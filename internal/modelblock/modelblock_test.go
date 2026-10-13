package modelblock

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		mask, model string
		want        bool
	}{
		{"claude-haiku-4-5", "claude-haiku-4-5", true},
		{"claude-haiku-4-5", "claude-haiku-4-5-20251001", true}, // dated variant
		{"claude-haiku-4-5", "claude-haiku-4-50", false},        // not a "-" continuation
		{"claude-haiku-4-5", "claude-haiku-4", false},
		{"claude-haiku-4-5", "x-claude-haiku-4-5", false},
		{"CLAUDE-Haiku-4-5", "claude-haiku-4-5-20251001", true}, // case
		{"claude-haiku-4-5", "Claude-HAIKU-4-5", true},
		{"claude-opus-*", "claude-opus-4-8", true},
		{"claude-opus-*", "claude-opus-", true},
		{"claude-opus-*", "claude-opus", false},
		{"claude-opus-*", "claude-sonnet-5", false},
		{"*opus*", "claude-opus-5-5", true},
		{"*-latest", "omni-moderation-latest", true},
		{"*-latest", "omni-moderation-latest-x", false},
		{"a*b*c", "a--b--c", true},
		{"a*b*c", "a--c--b", false},
		{"a*a", "a", false},
		{"openai/*", "openai/gpt-5-mini", true}, // crosses "/"
		{"*", "anything", true},
		{"claude-fable-*", "claude-fable-1", true},
		{"", "x", false},
		{"x", "", false},
	}
	for _, c := range cases {
		if got := Match(c.mask, c.model); got != c.want {
			t.Errorf("Match(%q,%q)=%v want %v", c.mask, c.model, got, c.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct {
		in, want string
		bad      bool
	}{
		{"  Claude-Opus-* ", "claude-opus-*", false},
		{"gpt-4o", "gpt-4o", false},
		{"", "", true},
		{"   ", "", true},
		{"a b", "", true},
		{"a\tb", "", true},
		{"a\x00b", "", true},
		{strings.Repeat("x", 129), "", true},
	}
	for _, c := range cases {
		got, err := Normalize(c.in)
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("Normalize(%q)=%q,%v", c.in, got, err)
		}
	}
}

func TestBlockedMatchesAnyName(t *testing.T) {
	l, err := New([]string{"claude-opus-*"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Add("claude-haiku-4-5"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		names []string
		mask  string
		ok    bool
	}{
		{[]string{"claude-opus-4-8"}, "claude-opus-*", true},
		{[]string{"opus", "claude-opus-4-8"}, "claude-opus-*", true}, // alias then canonical
		{[]string{"claude-haiku-4-5-20251001"}, "claude-haiku-4-5", true},
		{[]string{"", "  "}, "", false},
		{[]string{"gpt-4o"}, "", false},
		{nil, "", false},
	}
	for _, c := range cases {
		mask, ok := l.Blocked(c.names...)
		if mask != c.mask || ok != c.ok {
			t.Errorf("Blocked(%v)=%q,%v want %q,%v", c.names, mask, ok, c.mask, c.ok)
		}
	}
	var nilList *List
	if _, ok := nilList.Blocked("x"); ok || nilList.Entries() != nil {
		t.Error("nil list must block nothing")
	}
}

func TestNewRejectsBadSeed(t *testing.T) {
	if _, err := New([]string{"ok", "bad mask"}); err == nil {
		t.Error("expected error for whitespace in seed")
	}
	l, err := New([]string{"A", "a", " a "})
	if err != nil || len(l.Entries()) != 1 {
		t.Errorf("seed dedupe: %v %v", l.Entries(), err)
	}
}

func TestAddRemovePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "blocked.json")
	l, err := Load(path, []string{"claude-fable-*"})
	if err != nil {
		t.Fatal(err)
	}
	if _, added, err := l.Add("Claude-Haiku-4-5"); err != nil || !added {
		t.Fatalf("add: %v %v", added, err)
	}
	if _, added, _ := l.Add("claude-haiku-4-5"); added {
		t.Error("duplicate add must not report added")
	}
	if _, added, _ := l.Add("claude-fable-*"); added {
		t.Error("config duplicate must not be added as operator")
	}
	if _, _, err := l.Add("bad mask"); err == nil {
		t.Error("expected validation error")
	}

	// Round trip: a fresh Load sees the operator entry; config entry stays config.
	l2, err := Load(path, []string{"claude-fable-*"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{{"claude-fable-*", SourceConfig}, {"claude-haiku-4-5", SourceOperator}}
	if got := l2.Entries(); !reflect.DeepEqual(got, want) {
		t.Fatalf("entries=%v want %v", got, want)
	}

	// Config seed that is also persisted is not duplicated as operator.
	l3, _ := Load(path, []string{"claude-haiku-4-5"})
	if got := l3.Entries(); len(got) != 1 || got[0].Source != SourceConfig {
		t.Errorf("entries=%v", got)
	}

	if found, err := l2.Remove("claude-fable-*"); !found || !errors.Is(err, ErrConfigEntry) {
		t.Errorf("config remove: %v %v", found, err)
	}
	if found, err := l2.Remove("nope"); found || err != nil {
		t.Errorf("missing remove: %v %v", found, err)
	}
	if _, err := l2.Remove("a b"); err == nil {
		t.Error("expected validation error on remove")
	}
	if found, err := l2.Remove("CLAUDE-haiku-4-5"); !found || err != nil {
		t.Fatalf("remove: %v %v", found, err)
	}
	l4, _ := Load(path, nil)
	if len(l4.Entries()) != 0 {
		t.Errorf("after remove: %v", l4.Entries())
	}
	data, _ := os.ReadFile(path)
	if strings.TrimSpace(string(data)) != "[]" {
		t.Errorf("file=%q want []", data)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte("{not json"), 0o600)
	if _, err := Load(bad, nil); err == nil {
		t.Error("corrupt file must be an error")
	}
	inv := filepath.Join(dir, "inv.json")
	_ = os.WriteFile(inv, []byte(`["a b"]`), 0o600)
	if _, err := Load(inv, nil); err == nil {
		t.Error("invalid persisted mask must be an error")
	}
	if _, err := Load(dir, nil); err == nil { // a directory is unreadable as a file
		t.Error("read error must surface")
	}
	if _, err := Load("", []string{"x y"}); err == nil {
		t.Error("bad seed must surface")
	}
	l, err := Load("", nil) // in-memory
	if err != nil {
		t.Fatal(err)
	}
	if _, added, err := l.Add("x"); !added || err != nil {
		t.Errorf("in-memory add: %v %v", added, err)
	}
}

func TestPersistFailureLeavesStateUnchanged(t *testing.T) {
	dir := t.TempDir()
	// Parent "directory" is a regular file, so MkdirAll/WriteFile must fail.
	blocker := filepath.Join(dir, "file")
	_ = os.WriteFile(blocker, []byte("x"), 0o600)
	l := &List{path: filepath.Join(blocker, "blocked.json")}
	if _, added, err := l.Add("x"); err == nil || added {
		t.Errorf("add should fail: %v %v", added, err)
	}
	if len(l.Entries()) != 0 {
		t.Error("failed add must not change the list")
	}
	l.operator = []string{"x"}
	if found, err := l.Remove("x"); !found || err == nil {
		t.Errorf("remove should fail: %v %v", found, err)
	}
	if len(l.Entries()) != 1 {
		t.Error("failed remove must not change the list")
	}
	// rename failure: target path is a non-empty directory.
	target := filepath.Join(dir, "t.json")
	_ = os.MkdirAll(filepath.Join(target, "child"), 0o755)
	l2 := &List{path: target}
	if _, _, err := l2.Add("y"); err == nil {
		t.Error("rename failure expected")
	}
}
