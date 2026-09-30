package credential

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// DisabledSet is the durable list of credential names an operator disabled at
// runtime (dashboard / POST /admin/accounts/.../disable). Every provider Store
// shares one set, so a single file covers all of them. It holds names only — no
// secrets — so a leak of the file reveals nothing credential-bearing.
//
// A nil *DisabledSet is valid and means "in-memory only": Has reports false and
// Set is a no-op.
type DisabledSet struct {
	mu    sync.Mutex
	path  string
	names map[string]bool
}

// LoadDisabledSet reads the set from path. A missing file yields an empty set. A
// corrupt file is an error rather than an empty set: silently treating it as
// empty would re-enable every disabled account — the very thing this prevents.
func LoadDisabledSet(path string) (*DisabledSet, error) {
	d := &DisabledSet{path: path, names: map[string]bool{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return d, nil
		}
		return nil, fmt.Errorf("credential: read disabled set: %w", err)
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return nil, fmt.Errorf("credential: parse disabled set %s: %w", path, err)
	}
	for _, n := range names {
		d.names[n] = true
	}
	return d, nil
}

// Has reports whether name is persisted as disabled.
func (d *DisabledSet) Has(name string) bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.names[name]
}

// Set records name as disabled (or enabled again) and writes the file
// atomically (temp + rename). On a write error the in-memory set is unchanged.
func (d *DisabledSet) Set(name string, disabled bool) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.names[name] == disabled {
		return nil
	}
	next := make(map[string]bool, len(d.names)+1)
	for k := range d.names {
		next[k] = true
	}
	if disabled {
		next[name] = true
	} else {
		delete(next, name)
	}
	if err := d.write(next); err != nil {
		return err
	}
	d.names = next
	return nil
}

func (d *DisabledSet) write(names map[string]bool) error {
	list := make([]string, 0, len(names))
	for n := range names {
		list = append(list, n)
	}
	sort.Strings(list)
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return fmt.Errorf("credential: marshal disabled set: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		return fmt.Errorf("credential: create disabled set dir: %w", err)
	}
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("credential: write disabled set: %w", err)
	}
	if err := os.Rename(tmp, d.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("credential: replace disabled set: %w", err)
	}
	return nil
}
