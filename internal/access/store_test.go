package access

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStore_AddAllowLifecycle(t *testing.T) {
	s, err := LoadStore(filepath.Join(t.TempDir(), "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	key, info, err := s.Add("laptop")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !strings.HasPrefix(key, keyPrefix) || len(key) < 20 {
		t.Errorf("weak key %q", key)
	}
	if info.Name != "laptop" || !info.Enabled || info.Last4 != key[len(key)-4:] {
		t.Errorf("info = %+v", info)
	}
	if !s.Allow(key) {
		t.Error("freshly created key should be allowed")
	}
	if s.Allow("nope") || s.Allow("") {
		t.Error("unknown/empty key must be denied")
	}
	// disable -> denied; enable -> allowed again
	if err := s.SetEnabled("laptop", false); err != nil {
		t.Fatal(err)
	}
	if s.Allow(key) {
		t.Error("disabled key must be denied")
	}
	if err := s.SetEnabled("laptop", true); err != nil {
		t.Fatal(err)
	}
	if !s.Allow(key) {
		t.Error("re-enabled key should be allowed")
	}
	// delete -> denied
	if err := s.Delete("laptop"); err != nil {
		t.Fatal(err)
	}
	if s.Allow(key) || s.Len() != 0 {
		t.Error("deleted key must be gone")
	}
}

func TestStore_Errors(t *testing.T) {
	s, _ := LoadStore("")
	if _, _, err := s.Add("  "); err == nil {
		t.Error("empty name should error")
	}
	if _, _, err := s.Add("dup"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Add("dup"); err != ErrNameTaken {
		t.Errorf("want ErrNameTaken, got %v", err)
	}
	if err := s.SetEnabled("ghost", true); err != ErrNotFound {
		t.Errorf("SetEnabled ghost = %v", err)
	}
	if err := s.Delete("ghost"); err != ErrNotFound {
		t.Errorf("Delete ghost = %v", err)
	}
}

func TestStore_NilSafe(t *testing.T) {
	var s *Store
	if s.Allow("x") {
		t.Error("nil store must deny")
	}
}

func TestStore_Persistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "keys.json")
	s, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := s.Add("ci")
	if err != nil {
		t.Fatal(err)
	}
	// File was written by Add (atomic, under a freshly-created dir).
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("keys file not written: %v", err)
	}
	// Reload from disk: the key still authenticates and survives a Save round-trip.
	s2, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Allow(key) {
		t.Error("persisted key not allowed after reload")
	}
	if got := s2.List(); len(got) != 1 || got[0].Name != "ci" {
		t.Errorf("reloaded list = %+v", got)
	}
	if err := s2.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func TestStore_LoadMissingAndBad(t *testing.T) {
	if s, err := LoadStore(filepath.Join(t.TempDir(), "absent.json")); err != nil || s.Len() != 0 {
		t.Errorf("missing file should give empty store, got len=%d err=%v", s.Len(), err)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte("{not json"), 0o600)
	if _, err := LoadStore(bad); err == nil {
		t.Error("malformed file should error")
	}
}

func TestStore_ListSortedAndLastUsed(t *testing.T) {
	s, _ := LoadStore("")
	s.now = func() time.Time { return time.Unix(1000, 0) }
	kb, _, _ := s.Add("bravo")
	s.Add("alpha")
	list := s.List()
	if len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "bravo" {
		t.Fatalf("not sorted by name: %+v", list)
	}
	if !list[0].LastUsedAt.IsZero() {
		t.Error("unused key should have zero last-used")
	}
	if !s.Allow(kb) {
		t.Fatal("bravo should be allowed")
	}
	if got := s.List(); got[1].LastUsedAt.IsZero() {
		t.Error("Allow should stamp last-used")
	}
}
