package credential

import (
	"strings"
	"testing"
	"time"

	"cerber/internal/config"
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
