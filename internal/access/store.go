package access

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// keyPrefix marks cerber-issued client keys.
const keyPrefix = "cer_"

// ErrNameTaken is returned by Add when a key with that name already exists.
var ErrNameTaken = errors.New("access: key name already in use")

// ErrNotFound is returned when no managed key has the given name.
var ErrNotFound = errors.New("access: no key with that name")

// KeyInfo is a redacted view of a managed key — never contains the secret. Last4
// is the final four characters of the key, enough to tell keys apart in the UI.
// Limits and Usage expose the key's governance config and current window state so
// the dashboard can render budgets/rate-limits without revealing the secret.
type KeyInfo struct {
	Name       string    `json:"name"`
	Enabled    bool      `json:"enabled"`
	Last4      string    `json:"last4"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	Limits     Limits    `json:"limits"`
	Usage      Usage     `json:"usage"`
}

// keyEntry is the persisted form, including the secret. It is only ever written
// to the keys file (0600) and never returned through the API. Limits is the
// per-key governance config; counters is its rolling-window runtime state
// (persisted so budgets/rate-limits survive a restart).
type keyEntry struct {
	Name       string    `json:"name"`
	Key        string    `json:"key"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	Limits     Limits    `json:"limits,omitempty"`
	Counters   counters  `json:"counters,omitempty"`
}

// Store is a mutable, file-backed set of client API keys managed at runtime
// (created/enabled/disabled/deleted via the dashboard). It is consulted in
// addition to the static config keys, so an env-seeded key keeps working
// regardless of the store's contents. Safe for concurrent use.
type Store struct {
	mu       sync.Mutex
	path     string
	entries  []*keyEntry
	now      func() time.Time
	defaults Limits // applied to newly-created keys when no explicit limits are given
}

// SetDefaultLimits sets the limits applied to keys created via Add (i.e. through
// the dashboard) when no explicit limits are supplied. A zero Limits means new
// keys are unlimited. Existing keys are unaffected.
func (s *Store) SetDefaultLimits(l Limits) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defaults = l
}

// LoadStore reads the keys file at path (JSON array of entries). A missing file
// yields an empty store. path is remembered so mutations persist back to it.
func LoadStore(path string) (*Store, error) {
	s := &Store{path: path, now: time.Now}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("read keys file: %w", err)
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.entries); err != nil {
		return nil, fmt.Errorf("parse keys file: %w", err)
	}
	return s, nil
}

// Allow reports whether presented matches an enabled managed key. The scan is
// constant-time and always visits every entry, leaking neither key contents nor
// which key matched. A match stamps the key's last-used time (persisted lazily).
func (s *Store) Allow(presented string) bool {
	if s == nil || presented == "" {
		return false
	}
	pb := []byte(presented)
	s.mu.Lock()
	defer s.mu.Unlock()
	matchedIdx := -1
	for i, e := range s.entries {
		if !e.Enabled {
			continue
		}
		if subtle.ConstantTimeCompare(pb, []byte(e.Key)) == 1 {
			matchedIdx = i
		}
	}
	if matchedIdx >= 0 {
		s.entries[matchedIdx].LastUsedAt = s.now()
		return true
	}
	return false
}

// Identify returns the name of the enabled managed key matching presented, and
// whether one matched. Like Allow it scans every entry and stamps the matched
// key's last-used time, but additionally yields the key's name so the caller can
// enforce per-key governance (Admit/Charge). The scan is constant-time.
func (s *Store) Identify(presented string) (string, bool) {
	if s == nil || presented == "" {
		return "", false
	}
	pb := []byte(presented)
	s.mu.Lock()
	defer s.mu.Unlock()
	matchedIdx := -1
	for i, e := range s.entries {
		if !e.Enabled {
			continue
		}
		if subtle.ConstantTimeCompare(pb, []byte(e.Key)) == 1 {
			matchedIdx = i
		}
	}
	if matchedIdx >= 0 {
		s.entries[matchedIdx].LastUsedAt = s.now()
		return s.entries[matchedIdx].Name, true
	}
	return "", false
}

// Add creates a new enabled key with a freshly generated secret and persists the
// store. It returns the full secret (the only time it is ever exposed) and a
// redacted info record. Names must be unique and non-empty.
func (s *Store) Add(name string) (string, KeyInfo, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", KeyInfo{}, errors.New("access: key name is required")
	}
	key, err := generateKey()
	if err != nil {
		return "", KeyInfo{}, err
	}
	s.mu.Lock()
	for _, e := range s.entries {
		if e.Name == name {
			s.mu.Unlock()
			return "", KeyInfo{}, ErrNameTaken
		}
	}
	e := &keyEntry{Name: name, Key: key, Enabled: true, CreatedAt: s.now(), Limits: s.defaults}
	s.entries = append(s.entries, e)
	info := infoOf(e)
	err = s.saveLocked()
	s.mu.Unlock()
	return key, info, err
}

// SetEnabled enables or disables the named key and persists. Returns ErrNotFound
// if no key matches.
func (s *Store) SetEnabled(name string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.Name == name {
			e.Enabled = enabled
			return s.saveLocked()
		}
	}
	return ErrNotFound
}

// Delete removes the named key and persists. Returns ErrNotFound if absent.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.entries {
		if e.Name == name {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			return s.saveLocked()
		}
	}
	return ErrNotFound
}

// List returns redacted info for every managed key, sorted by name.
func (s *Store) List() []KeyInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]KeyInfo, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, infoOf(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Len reports how many managed keys exist.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Save persists the store to its file (atomic write). Used by the periodic saver
// so lazily-stamped last-used times reach disk.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

// saveLocked writes the entries to the keys file atomically. The caller holds mu.
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil // in-memory only (tests)
	}
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func infoOf(e *keyEntry) KeyInfo {
	last4 := e.Key
	if len(last4) > 4 {
		last4 = last4[len(last4)-4:]
	}
	return KeyInfo{
		Name: e.Name, Enabled: e.Enabled, Last4: last4,
		CreatedAt: e.CreatedAt, LastUsedAt: e.LastUsedAt,
		Limits: e.Limits, Usage: e.Counters.usage(),
	}
}

// generateKey returns a new "cer_"-prefixed key with 32 hex chars of entropy.
func generateKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("access: generate key: %w", err)
	}
	return keyPrefix + hex.EncodeToString(b), nil
}
