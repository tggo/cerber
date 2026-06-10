package catalog

import (
	"reflect"
	"testing"
)

func TestCanonical(t *testing.T) {
	c := New(map[string]string{
		"opus":    "claude-opus-4-20250514",
		" sonnet": "claude-sonnet-4 ", // trimmed on both sides
		"blank":   "",                 // skipped (empty target)
		"":        "x",                // skipped (empty alias)
	})
	cases := map[string]string{
		"opus":   "claude-opus-4-20250514",
		"sonnet": "claude-sonnet-4",
		"blank":  "blank", // not registered → identity
		"gpt-4o": "gpt-4o",
	}
	for in, want := range cases {
		if got := c.Canonical(in); got != want {
			t.Errorf("Canonical(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonicalSingleHop(t *testing.T) {
	// An alias pointing at another alias is not chained.
	c := New(map[string]string{"a": "b", "b": "c"})
	if got := c.Canonical("a"); got != "b" {
		t.Errorf("Canonical(a) = %q, want b (single-hop)", got)
	}
}

func TestNilCatalog(t *testing.T) {
	var c *Catalog
	if got := c.Canonical("anything"); got != "anything" {
		t.Errorf("nil Canonical = %q", got)
	}
	if got := c.Aliases(); len(got) != 0 {
		t.Errorf("nil Aliases = %v", got)
	}
}

func TestAliasesCopy(t *testing.T) {
	c := New(map[string]string{"x": "y"})
	a := c.Aliases()
	a["x"] = "tampered"
	if c.Canonical("x") != "y" {
		t.Error("Aliases() returned a live reference, not a copy")
	}
	if !reflect.DeepEqual(c.Aliases(), map[string]string{"x": "y"}) {
		t.Errorf("Aliases = %v", c.Aliases())
	}
}
