// Package credential is cerber's trust-critical store of upstream provider
// credentials. Secrets are held unexported and are only ever returned through
// explicit accessor methods so they can be applied as outbound auth headers to
// the owning provider. Nothing here logs a secret, and String/redaction never
// expose one (see CLAUDE.md).
package credential

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"cerber/internal/config"
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

// Credential is a single upstream account credential. Secret material is
// unexported; read it only via the accessor methods.
type Credential struct {
	name        string
	kind        Kind
	apiKey      string
	accessToken string
}

// Name returns the human label for this credential (safe to log).
func (c *Credential) Name() string { return c.name }

// Kind returns the auth mechanism.
func (c *Credential) Kind() Kind { return c.kind }

// APIKey returns the x-api-key secret (empty unless KindAPIKey).
func (c *Credential) APIKey() string { return c.apiKey }

// AccessToken returns the OAuth bearer token (empty unless KindOAuth).
func (c *Credential) AccessToken() string { return c.accessToken }

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
		return &Credential{name: name, kind: KindOAuth, accessToken: cc.AccessToken}, nil
	default:
		return nil, fmt.Errorf("credential %q: unsupported type %q", name, cc.Type)
	}
}

type entry struct {
	cred          *Credential
	cooldownUntil time.Time
}

// Store holds a provider's credentials and hands them out round-robin, skipping
// any that are temporarily in cooldown. It is safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	entries []*entry
	idx     int
	now     func() time.Time
}

// Option customizes a Store.
type Option func(*Store)

// WithClock injects a clock (for tests). Defaults to time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
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
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := len(s.entries)
	for i := 0; i < n; i++ {
		e := s.entries[s.idx]
		s.idx = (s.idx + 1) % n
		if now.Before(e.cooldownUntil) {
			continue
		}
		return e.cred, nil
	}
	return nil, ErrNoneAvailable
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
