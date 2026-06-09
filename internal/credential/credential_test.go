package credential

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tggo/cerber/internal/config"
)

func apiKeyCfg(name, key string) config.Credential {
	return config.Credential{Type: config.CredentialAPIKey, Name: name, Key: key}
}

func TestNewStore_Empty(t *testing.T) {
	if _, err := NewStore(nil); err == nil {
		t.Fatal("expected error for empty store")
	}
}

func TestNewStore_UnsupportedType(t *testing.T) {
	_, err := NewStore([]config.Credential{{Type: "weird"}})
	if err == nil {
		t.Fatal("expected error for unsupported type")
	}
}

func TestCredential_AccessorsAndRedaction(t *testing.T) {
	s, err := NewStore([]config.Credential{
		apiKeyCfg("a", "sk-secret"),
		{Type: config.CredentialOAuth, AccessToken: "tok-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 2 {
		t.Fatalf("Len = %d", s.Len())
	}
	c1, _ := s.Next()
	if c1.Kind() != KindAPIKey || c1.APIKey() != "sk-secret" || c1.Name() != "a" {
		t.Errorf("apikey cred wrong: %+v", c1)
	}
	c2, _ := s.Next()
	if c2.Kind() != KindOAuth || c2.AccessToken() != "tok-secret" || c2.Name() != "cred-1" {
		t.Errorf("oauth cred wrong: name=%s", c2.Name())
	}
	// String never leaks secrets.
	for _, c := range []*Credential{c1, c2} {
		if strings.Contains(c.String(), "secret") {
			t.Errorf("String leaked secret: %s", c.String())
		}
	}
}

func TestNext_RoundRobin(t *testing.T) {
	s, _ := NewStore([]config.Credential{apiKeyCfg("a", "1"), apiKeyCfg("b", "2"), apiKeyCfg("c", "3")})
	got := []string{}
	for i := 0; i < 5; i++ {
		c, err := s.Next()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, c.Name())
	}
	want := "a b c a b"
	if strings.Join(got, " ") != want {
		t.Errorf("rotation = %q, want %q", strings.Join(got, " "), want)
	}
}

func TestCooldown_SkipsAndRecovers(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }
	s, _ := NewStore([]config.Credential{apiKeyCfg("a", "1"), apiKeyCfg("b", "2")}, WithClock(clock))

	a, _ := s.Next() // a
	s.Cooldown(a, time.Minute)

	// With a cooling down, the next several calls return only b.
	for i := 0; i < 3; i++ {
		c, err := s.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if c.Name() != "b" {
			t.Fatalf("got %s, want b while a cools down", c.Name())
		}
	}
	// After cooldown expires, a is back in rotation.
	now = now.Add(2 * time.Minute)
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		c, _ := s.Next()
		seen[c.Name()] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("expected both back in rotation, saw %v", seen)
	}
}

func TestCooldown_AllUnavailable(t *testing.T) {
	now := time.Unix(0, 0)
	s, _ := NewStore([]config.Credential{apiKeyCfg("a", "1")}, WithClock(func() time.Time { return now }))
	c, _ := s.Next()
	s.Cooldown(c, time.Hour)
	if _, err := s.Next(); err != ErrNoneAvailable {
		t.Fatalf("err = %v, want ErrNoneAvailable", err)
	}
}

func TestNeedsRefresh(t *testing.T) {
	now := time.Unix(1000, 0)
	skew := time.Minute

	apiKey, _ := NewStore([]config.Credential{apiKeyCfg("a", "k")})
	c, _ := apiKey.Next()
	if c.NeedsRefresh(now, skew) {
		t.Error("api_key credential never needs refresh")
	}

	oauthZero, _ := NewStore([]config.Credential{{Type: config.CredentialOAuth, AccessToken: "t"}})
	oz, _ := oauthZero.Next()
	if oz.NeedsRefresh(now, skew) {
		t.Error("oauth with unknown expiry should not proactively refresh")
	}

	cases := []struct {
		name    string
		expires time.Time
		want    bool
	}{
		{"far future", now.Add(time.Hour), false},
		{"within skew", now.Add(30 * time.Second), true},
		{"already expired", now.Add(-time.Second), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := NewStore([]config.Credential{{Type: config.CredentialOAuth, AccessToken: "t", ExpiresAt: tc.expires}})
			cred, _ := s.Next()
			if got := cred.NeedsRefresh(now, skew); got != tc.want {
				t.Errorf("NeedsRefresh = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUpdateOAuth(t *testing.T) {
	exp := time.Unix(5000, 0)
	s, _ := NewStore([]config.Credential{{Type: config.CredentialOAuth, AccessToken: "old", RefreshToken: "r0", ExpiresAt: time.Unix(1, 0)}})
	c, _ := s.Next()

	s.UpdateOAuth(c, OAuthTokens{AccessToken: "new", RefreshToken: "r1", ExpiresAt: exp})
	if c.AccessToken() != "new" || c.RefreshToken() != "r1" || !c.ExpiresAt().Equal(exp) {
		t.Errorf("update failed: %s %s %v", c.AccessToken(), c.RefreshToken(), c.ExpiresAt())
	}

	// Empty refresh token preserves the existing one.
	s.UpdateOAuth(c, OAuthTokens{AccessToken: "newer", RefreshToken: "", ExpiresAt: exp})
	if c.RefreshToken() != "r1" {
		t.Errorf("empty refresh should preserve, got %q", c.RefreshToken())
	}

	// Nil / unknown credentials are ignored (no panic).
	s.UpdateOAuth(nil, OAuthTokens{})
	s.UpdateOAuth(&Credential{}, OAuthTokens{AccessToken: "x"})
}

func TestNextOf_FiltersByKind(t *testing.T) {
	s, _ := NewStore([]config.Credential{
		apiKeyCfg("k1", "x"),
		{Type: config.CredentialOAuth, Name: "o1", AccessToken: "t"},
		apiKeyCfg("k2", "y"),
	})
	isOAuth := func(c *Credential) bool { return c.Kind() == KindOAuth }
	for i := 0; i < 3; i++ {
		c, err := s.NextOf(isOAuth)
		if err != nil || c.Name() != "o1" {
			t.Fatalf("NextOf(oauth) = %v %v", c, err)
		}
	}
	isKey := func(c *Credential) bool { return c.Kind() == KindAPIKey }
	got := map[string]bool{}
	for i := 0; i < 4; i++ {
		c, _ := s.NextOf(isKey)
		got[c.Name()] = true
		if c.Kind() != KindAPIKey {
			t.Errorf("got non-key %s", c.Name())
		}
	}
	if !got["k1"] || !got["k2"] {
		t.Errorf("key rotation missed one: %v", got)
	}
}

func TestSetEnabledAndList(t *testing.T) {
	now := time.Unix(1000, 0)
	s, _ := NewStore([]config.Credential{apiKeyCfg("a", "1"), apiKeyCfg("b", "2")}, WithClock(func() time.Time { return now }))

	if !s.SetEnabled("a", false) {
		t.Fatal("SetEnabled should find 'a'")
	}
	if s.SetEnabled("nope", false) {
		t.Fatal("SetEnabled should not match unknown")
	}
	// disabled 'a' is skipped; only 'b' returned
	for i := 0; i < 3; i++ {
		c, err := s.Next()
		if err != nil || c.Name() != "b" {
			t.Fatalf("got %v %v, want b", c, err)
		}
	}
	// list reflects state
	list := s.List()
	byName := map[string]Info{}
	for _, in := range list {
		byName[in.Name] = in
	}
	if byName["a"].Enabled || !byName["b"].Enabled {
		t.Errorf("list enabled wrong: %+v", list)
	}
	// re-enable
	s.SetEnabled("a", true)
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		c, _ := s.Next()
		seen[c.Name()] = true
	}
	if !seen["a"] {
		t.Error("a should be back after enable")
	}
}

func TestNextOf_FillFirst(t *testing.T) {
	now := time.Unix(1000, 0)
	s, _ := NewStore([]config.Credential{apiKeyCfg("a", "1"), apiKeyCfg("b", "2")},
		WithFillFirst(true), WithClock(func() time.Time { return now }))
	// always prefers 'a' (fixed order, not round-robin)
	for i := 0; i < 3; i++ {
		c, _ := s.Next()
		if c.Name() != "a" {
			t.Fatalf("fill-first should keep returning a, got %s", c.Name())
		}
	}
	// when 'a' is disabled, fall to 'b'
	s.SetEnabled("a", false)
	c, _ := s.Next()
	if c.Name() != "b" {
		t.Errorf("got %s, want b", c.Name())
	}
}

func TestList_CoolingDown(t *testing.T) {
	now := time.Unix(1000, 0)
	s, _ := NewStore([]config.Credential{apiKeyCfg("a", "1")}, WithClock(func() time.Time { return now }))
	c, _ := s.Next()
	s.Cooldown(c, time.Minute)
	if !s.List()[0].CoolingDown {
		t.Error("expected cooling_down true")
	}
}

func TestNextOf_NoMatch(t *testing.T) {
	s, _ := NewStore([]config.Credential{apiKeyCfg("k", "x")})
	_, err := s.NextOf(func(c *Credential) bool { return c.Kind() == KindOAuth })
	if err != ErrNoneAvailable {
		t.Fatalf("err = %v, want ErrNoneAvailable", err)
	}
}

func TestCooldown_NoopCases(t *testing.T) {
	s, _ := NewStore([]config.Credential{apiKeyCfg("a", "1")})
	s.Cooldown(nil, time.Minute)           // nil credential
	s.Cooldown(&Credential{}, time.Minute) // unknown credential
	c, _ := s.Next()
	s.Cooldown(c, 0) // non-positive duration
	if _, err := s.Next(); err != nil {
		t.Fatalf("Next after noop cooldowns: %v", err)
	}
}

func TestEnsureFresh_Singleflight(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	s, err := NewStore([]config.Credential{{
		Type: config.CredentialOAuth, Name: "o", AccessToken: "old", RefreshToken: "rt",
		ExpiresAt: now.Add(-time.Minute), // already expired -> needs refresh
	}}, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	cred, _ := s.Next()
	var calls int32
	refresh := func() (OAuthTokens, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(15 * time.Millisecond) // widen the race window
		return OAuthTokens{AccessToken: "new", RefreshToken: "rt2", ExpiresAt: now.Add(time.Hour)}, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _, _ = s.EnsureFresh(cred, false, now, time.Minute, refresh) }()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("refresh called %d times, want 1 (singleflight)", got)
	}
	if cred.AccessToken() != "new" {
		t.Errorf("token not updated: %q", cred.AccessToken())
	}
}

func TestEnsureFresh_ForceVsProactive(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	s, _ := NewStore([]config.Credential{{
		Type: config.CredentialOAuth, Name: "o", AccessToken: "a", RefreshToken: "rt",
		ExpiresAt: now.Add(time.Hour), // fresh -> no proactive refresh
	}}, WithClock(func() time.Time { return now }))
	cred, _ := s.Next()
	refresh := func() (OAuthTokens, error) { return OAuthTokens{AccessToken: "b", ExpiresAt: now.Add(time.Hour)}, nil }

	if _, did, _ := s.EnsureFresh(cred, false, now, time.Minute, refresh); did {
		t.Error("proactive refresh should NOT run for a fresh token")
	}
	if _, did, err := s.EnsureFresh(cred, true, now, time.Minute, refresh); err != nil || !did {
		t.Errorf("force refresh should run: did=%v err=%v", did, err)
	}
	if cred.AccessToken() != "b" {
		t.Errorf("forced refresh didn't update token: %q", cred.AccessToken())
	}
}

func TestPenalizeBackoffAndReset(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	s, _ := NewStore([]config.Credential{{Type: config.CredentialAPIKey, Name: "a", Key: "k"}},
		WithClock(func() time.Time { return now }))
	cred, _ := s.Next()
	// exponential: 60s, 120s, 240s ...
	for i, want := range []time.Duration{60 * time.Second, 120 * time.Second, 240 * time.Second} {
		if got := s.Penalize(cred, 60*time.Second); got != want {
			t.Errorf("penalize #%d = %v, want %v", i+1, got, want)
		}
	}
	// cap at maxCooldown
	for i := 0; i < 20; i++ {
		s.Penalize(cred, 60*time.Second)
	}
	if got := s.Penalize(cred, 60*time.Second); got != maxCooldown {
		t.Errorf("capped penalize = %v, want %v", got, maxCooldown)
	}
	// while cooling, Next skips it
	if _, err := s.Next(); err != ErrNoneAvailable {
		t.Errorf("cooling cred should be skipped, got %v", err)
	}
	// success resets streak + clears cooldown
	s.MarkSuccess(cred)
	if _, err := s.Next(); err != nil {
		t.Errorf("after MarkSuccess cred should be available: %v", err)
	}
	if got := s.Penalize(cred, 60*time.Second); got != 60*time.Second {
		t.Errorf("after reset, first penalize = %v, want 60s", got)
	}
}
