// Package credential is cerber's trust-critical store of upstream provider
// credentials. Secrets are held unexported and are only ever returned through
// explicit accessor methods so they can be applied as outbound auth headers to
// the owning provider. Nothing here logs a secret, and String/redaction never
// expose one (see CLAUDE.md).
package credential

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tggo/cerber/internal/config"
)

// Kind is the auth mechanism a credential uses.
type Kind string

const (
	// KindAPIKey applies an x-api-key header.
	KindAPIKey Kind = "api_key"
	// KindOAuth applies a Bearer access token (Claude Code OAuth).
	KindOAuth Kind = "oauth"
)

// ErrNoneAvailable is returned when every credential is in cooldown.
var ErrNoneAvailable = errors.New("credential: no credential available")

// OAuthTokens is the result of refreshing an OAuth credential.
type OAuthTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// Credential is a single upstream account credential. Secret material is
// unexported; read it only via the accessor methods. OAuth token fields are
// mutable (refreshed in place) and guarded by an internal mutex, so a Credential
// must always be used via pointer and never copied.
type Credential struct {
	name   string
	kind   Kind
	apiKey string // immutable

	mu           sync.RWMutex
	accessToken  string
	refreshToken string
	expiresAt    time.Time
	refreshGen   uint64 // bumped on every token update; used to dedup refreshes

	// refreshMu serializes token refresh for this credential so concurrent
	// requests don't each spend the (single-use, rotating) refresh token — which
	// would burn it and 401/cooldown the account. See Store.EnsureFresh.
	refreshMu sync.Mutex
}

// Name returns the human label for this credential (safe to log).
func (c *Credential) Name() string { return c.name }

// Kind returns the auth mechanism.
func (c *Credential) Kind() Kind { return c.kind }

// APIKey returns the x-api-key secret (empty unless KindAPIKey).
func (c *Credential) APIKey() string { return c.apiKey }

// AccessToken returns the current OAuth bearer token (empty unless KindOAuth).
func (c *Credential) AccessToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.accessToken
}

// RefreshToken returns the current OAuth refresh token.
func (c *Credential) RefreshToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.refreshToken
}

// ExpiresAt returns the OAuth access-token expiry (zero if unknown).
func (c *Credential) ExpiresAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.expiresAt
}

// NeedsRefresh reports whether this OAuth credential's access token is expired or
// within skew of expiring at now. API-key credentials and OAuth credentials with
// an unknown (zero) expiry never need a proactive refresh.
func (c *Credential) NeedsRefresh(now time.Time, skew time.Duration) bool {
	if c.kind != KindOAuth {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.expiresAt.IsZero() {
		return false
	}
	return !now.Before(c.expiresAt.Add(-skew))
}

// updateOAuth replaces the mutable OAuth token state.
func (c *Credential) updateOAuth(tok OAuthTokens) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		c.refreshToken = tok.RefreshToken
	}
	c.expiresAt = tok.ExpiresAt
	c.refreshGen++
}

// String is a redacted identifier — it never contains secret material.
func (c *Credential) String() string {
	return fmt.Sprintf("credential(%s,%s)", c.name, c.kind)
}

// newCredential builds a Credential from config, assigning a fallback name.
func newCredential(cc config.Credential, idx int) (*Credential, error) {
	name := cc.Name
	if name == "" {
		name = fmt.Sprintf("cred-%d", idx)
	}
	switch cc.Type {
	case config.CredentialAPIKey:
		return &Credential{name: name, kind: KindAPIKey, apiKey: cc.Key}, nil
	case config.CredentialOAuth:
		return &Credential{
			name:         name,
			kind:         KindOAuth,
			accessToken:  cc.AccessToken,
			refreshToken: cc.RefreshToken,
			expiresAt:    cc.ExpiresAt,
		}, nil
	default:
		return nil, fmt.Errorf("credential %q: unsupported type %q", name, cc.Type)
	}
}

type entry struct {
	cred          *Credential
	cooldownUntil time.Time
	disabled      bool

	// Health from the latest probe (see SetHealth). healthAt zero = never probed.
	healthChecked bool
	healthy       bool
	healthErr     string
	healthAt      time.Time
}

// Info is a redacted snapshot of a credential's state for orchestration/listing.
type Info struct {
	Name            string    `json:"name"`
	Kind            Kind      `json:"kind"`
	Enabled         bool      `json:"enabled"`
	CoolingDown     bool      `json:"cooling_down"`
	HealthChecked   bool      `json:"health_checked"`
	Healthy         bool      `json:"healthy"`
	HealthError     string    `json:"health_error,omitempty"`
	HealthCheckedAt time.Time `json:"health_checked_at,omitempty"`
}

// Store holds a provider's credentials and hands them out round-robin, skipping
// any that are temporarily in cooldown. It is safe for concurrent use.
type Store struct {
	mu        sync.Mutex
	entries   []*entry
	idx       int
	now       func() time.Time
	fillFirst bool
}

// Option customizes a Store.
type Option func(*Store)

// WithClock injects a clock (for tests). Defaults to time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// WithFillFirst selects credentials in fixed order (use the first available,
// only move on when it is unavailable) instead of round-robin.
func WithFillFirst(v bool) Option {
	return func(s *Store) { s.fillFirst = v }
}

// MatchHeader builds a credential matcher from an X-Cerber-Cred spec: "oauth" or
// "key"/"api_key" select by kind, anything else selects by exact name; "" (or a
// blank spec) returns nil (match any). Shared by the server and providers so a
// client can pin a specific account/subscription.
func MatchHeader(spec string) func(*Credential) bool {
	switch s := strings.TrimSpace(spec); strings.ToLower(s) {
	case "":
		return nil
	case "oauth":
		return func(c *Credential) bool { return c.Kind() == KindOAuth }
	case "key", "api_key", "apikey":
		return func(c *Credential) bool { return c.Kind() == KindAPIKey }
	default:
		return func(c *Credential) bool { return c.Name() == s }
	}
}

// NewStore builds a Store from validated config credentials.
func NewStore(cfgs []config.Credential, opts ...Option) (*Store, error) {
	if len(cfgs) == 0 {
		return nil, errors.New("credential: NewStore requires at least one credential")
	}
	s := &Store{now: time.Now}
	for _, o := range opts {
		o(s)
	}
	for i, cc := range cfgs {
		c, err := newCredential(cc, i)
		if err != nil {
			return nil, err
		}
		s.entries = append(s.entries, &entry{cred: c})
	}
	return s, nil
}

// Len reports how many credentials the store holds.
func (s *Store) Len() int { return len(s.entries) }

// Next returns the next available credential in round-robin order, skipping any
// still in cooldown. Returns ErrNoneAvailable if all are cooling down.
func (s *Store) Next() (*Credential, error) {
	return s.NextOf(nil)
}

// NextOf is like Next but only considers credentials for which match returns
// true (a nil match accepts any). Returns ErrNoneAvailable if none match and are
// available.
func (s *Store) NextOf(match func(*Credential) bool) (*Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := len(s.entries)
	for i := 0; i < n; i++ {
		var e *entry
		if s.fillFirst {
			e = s.entries[i] // fixed order: always prefer earlier entries
		} else {
			e = s.entries[s.idx]
			s.idx = (s.idx + 1) % n
		}
		if e.disabled || now.Before(e.cooldownUntil) {
			continue
		}
		if match != nil && !match(e.cred) {
			continue
		}
		return e.cred, nil
	}
	return nil, ErrNoneAvailable
}

// SetEnabled enables or disables the named credential at runtime (disabled
// credentials are skipped by Next/NextOf). Reports whether a credential matched.
func (s *Store) SetEnabled(name string, enabled bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.cred.Name() == name {
			e.disabled = !enabled
			return true
		}
	}
	return false
}

// List returns a redacted snapshot of all credentials and their state.
func (s *Store) List() []Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make([]Info, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, Info{
			Name:            e.cred.Name(),
			Kind:            e.cred.Kind(),
			Enabled:         !e.disabled,
			CoolingDown:     now.Before(e.cooldownUntil),
			HealthChecked:   e.healthChecked,
			Healthy:         e.healthy,
			HealthError:     e.healthErr,
			HealthCheckedAt: e.healthAt,
		})
	}
	return out
}

// All returns every credential in the store (including disabled ones), for
// out-of-band tasks like health probing. The returned slice is a fresh copy; the
// *Credential pointers are the live ones.
func (s *Store) All() []*Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Credential, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e.cred)
	}
	return out
}

// SetHealth records the result of probing a credential (valid/invalid + error),
// stamped with the current time. Unknown credentials are ignored.
func (s *Store) SetHealth(c *Credential, healthy bool, errMsg string) {
	if c == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.cred == c {
			e.healthChecked = true
			e.healthy = healthy
			e.healthErr = errMsg
			e.healthAt = s.now()
			return
		}
	}
}

// Cooldown sidelines a credential for the given duration (e.g. after a 429 or
// auth failure upstream). Unknown credentials are ignored.
func (s *Store) Cooldown(c *Credential, d time.Duration) {
	if c == nil || d <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	until := s.now().Add(d)
	for _, e := range s.entries {
		if e.cred == c {
			e.cooldownUntil = until
			return
		}
	}
}

// EnsureFresh refreshes a credential's OAuth token, serializing concurrent
// callers per credential so the rotating (single-use) refresh token is spent
// exactly once — concurrent refreshes would burn it and 401/cooldown the account.
//
// force=false: refresh only if within skew of expiry (proactive).
// force=true:  refresh regardless (e.g. recovering from an upstream 401).
//
// Dedup: each token update bumps a generation counter; a caller captures it
// before taking the lock and, if it changed while waiting, skips the refresh and
// uses the token the winner just obtained. refresh() does the provider-specific
// exchange. Returns the new tokens and whether a refresh actually ran.
func (s *Store) EnsureFresh(c *Credential, force bool, now time.Time, skew time.Duration, refresh func() (OAuthTokens, error)) (OAuthTokens, bool, error) {
	if c == nil {
		return OAuthTokens{}, false, nil
	}
	c.mu.RLock()
	gen := c.refreshGen
	c.mu.RUnlock()

	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.mu.RLock()
	changed := c.refreshGen != gen
	c.mu.RUnlock()
	if changed {
		return OAuthTokens{}, false, nil // refreshed by another goroutine while we waited
	}
	if !force && !c.NeedsRefresh(now, skew) {
		return OAuthTokens{}, false, nil
	}
	tok, err := refresh()
	if err != nil {
		return OAuthTokens{}, false, err
	}
	c.updateOAuth(tok)
	return tok, true, nil
}

// UpdateOAuth replaces a credential's OAuth token state after a refresh. Unknown
// credentials are ignored.
func (s *Store) UpdateOAuth(c *Credential, tok OAuthTokens) {
	if c == nil {
		return
	}
	s.mu.Lock()
	known := false
	for _, e := range s.entries {
		if e.cred == c {
			known = true
			break
		}
	}
	s.mu.Unlock()
	if known {
		c.updateOAuth(tok)
	}
}
