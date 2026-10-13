// Package modelblock is cerber's single, global model blocklist: a set of masks
// (exact model ids or "*" globs) that no caller may use. The list is seeded from
// config (immutable at runtime) and extended by the operator through the admin
// API; the operator part is persisted to a JSON file so it survives restarts.
//
// A nil *List is valid and blocks nothing.
package modelblock

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// Entry sources.
const (
	SourceConfig   = "config"   // from blocked_models in config; not removable at runtime
	SourceOperator = "operator" // added through the admin API; persisted
)

// maxMaskLen bounds a mask; real model ids are far shorter.
const maxMaskLen = 128

// ErrConfigEntry is returned by Remove for a mask that comes from config.
var ErrConfigEntry = errors.New("modelblock: mask comes from config and cannot be removed at runtime")

// Entry is one mask in the list.
type Entry struct {
	Mask   string `json:"mask"`
	Source string `json:"source"`
}

// List is the blocklist. Safe for concurrent use.
type List struct {
	mu       sync.RWMutex
	path     string   // "" = operator entries are in-memory only
	config   []string // normalized, deduped
	operator []string // normalized, deduped, sorted
}

// Normalize validates and canonicalises a mask: trimmed, lower-cased, non-empty,
// no whitespace/control characters, at most 128 bytes. "*" is the only wildcard.
func Normalize(mask string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(mask))
	switch {
	case m == "":
		return "", errors.New("mask is required")
	case len(m) > maxMaskLen:
		return "", fmt.Errorf("mask is longer than %d characters", maxMaskLen)
	}
	for _, r := range m {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", errors.New("mask must not contain whitespace or control characters")
		}
	}
	return m, nil
}

// New builds an in-memory list seeded with config masks. An invalid config mask
// is an error (a typo must not silently leave a model unblocked).
func New(seed []string) (*List, error) {
	l := &List{}
	for _, s := range seed {
		m, err := Normalize(s)
		if err != nil {
			return nil, fmt.Errorf("modelblock: blocked_models entry %q: %w", s, err)
		}
		if !contains(l.config, m) {
			l.config = append(l.config, m)
		}
	}
	return l, nil
}

// Load builds a list seeded with config masks plus the operator masks persisted
// at path. A missing file is an empty operator list; a corrupt one is an error
// rather than an empty list, because silently dropping the list would unblock
// every model the operator blocked. An empty path means in-memory only.
func Load(path string, seed []string) (*List, error) {
	l, err := New(seed)
	if err != nil {
		return nil, err
	}
	l.path = path
	if path == "" {
		return l, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return l, nil
		}
		return nil, fmt.Errorf("modelblock: read %s: %w", path, err)
	}
	var masks []string
	if err := json.Unmarshal(data, &masks); err != nil {
		return nil, fmt.Errorf("modelblock: parse %s: %w", path, err)
	}
	for _, s := range masks {
		m, err := Normalize(s)
		if err != nil {
			return nil, fmt.Errorf("modelblock: %s entry %q: %w", path, s, err)
		}
		if !contains(l.config, m) && !contains(l.operator, m) {
			l.operator = append(l.operator, m)
		}
	}
	sort.Strings(l.operator)
	return l, nil
}

// Add validates mask and adds it as an operator entry, persisting first; on a
// write failure the in-memory list is unchanged. added is false when the mask is
// already present (from config or operator).
func (l *List) Add(mask string) (norm string, added bool, err error) {
	norm, err = Normalize(mask)
	if err != nil {
		return "", false, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if contains(l.config, norm) || contains(l.operator, norm) {
		return norm, false, nil
	}
	next := append(append([]string{}, l.operator...), norm)
	sort.Strings(next)
	if err := l.write(next); err != nil {
		return norm, false, err
	}
	l.operator = next
	return norm, true, nil
}

// Remove drops an operator mask, persisting first. found is false when the mask
// is not present; a config mask yields ErrConfigEntry.
func (l *List) Remove(mask string) (found bool, err error) {
	norm, err := Normalize(mask)
	if err != nil {
		return false, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if contains(l.config, norm) {
		return true, ErrConfigEntry
	}
	if !contains(l.operator, norm) {
		return false, nil
	}
	next := make([]string, 0, len(l.operator))
	for _, m := range l.operator {
		if m != norm {
			next = append(next, m)
		}
	}
	if err := l.write(next); err != nil {
		return true, err
	}
	l.operator = next
	return true, nil
}

// Entries lists config masks (in config order) then operator masks (sorted).
func (l *List) Entries() []Entry {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Entry, 0, len(l.config)+len(l.operator))
	for _, m := range l.config {
		out = append(out, Entry{Mask: m, Source: SourceConfig})
	}
	for _, m := range l.operator {
		out = append(out, Entry{Mask: m, Source: SourceOperator})
	}
	return out
}

// Blocked reports whether any of the given model names (e.g. the requested name
// and its alias-resolved canonical name) matches a mask, returning the first
// matching mask. Empty names are ignored.
func (l *List) Blocked(models ...string) (mask string, ok bool) {
	if l == nil {
		return "", false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, model := range models {
		if strings.TrimSpace(model) == "" {
			continue
		}
		for _, m := range l.config {
			if Match(m, model) {
				return m, true
			}
		}
		for _, m := range l.operator {
			if Match(m, model) {
				return m, true
			}
		}
	}
	return "", false
}

// Match reports whether model is covered by mask, case-insensitively. A mask with
// "*" is a glob. A mask without "*" matches the exact id and any dated/suffixed
// variant that continues with "-" (claude-haiku-4-5 covers claude-haiku-4-5-20251001
// but not claude-haiku-4-50).
func Match(mask, model string) bool {
	mask = strings.ToLower(strings.TrimSpace(mask))
	model = strings.ToLower(strings.TrimSpace(model))
	if mask == "" || model == "" {
		return false
	}
	if !strings.Contains(mask, "*") {
		return model == mask || strings.HasPrefix(model, mask+"-")
	}
	return glob(mask, model)
}

// glob matches s against a pattern whose only metacharacter is "*" (any run,
// including across "/" and "-").
func glob(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return strings.HasSuffix(s, last)
}

func (l *List) write(operator []string) error {
	if l.path == "" {
		return nil
	}
	if operator == nil {
		operator = []string{}
	}
	data, err := json.MarshalIndent(operator, "", "  ")
	if err != nil {
		return fmt.Errorf("modelblock: marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return fmt.Errorf("modelblock: create dir: %w", err)
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("modelblock: write: %w", err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("modelblock: replace: %w", err)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
