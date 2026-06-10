// Package catalog resolves client-facing model aliases to the canonical model
// names that providers actually serve. It is a lightweight, in-memory lookup
// built from config — no network, no remote fetch. Discovery of which provider
// serves a model lives in the server (it owns the probe results); the catalog's
// job is purely alias → canonical normalisation so a stable client-facing name
// (e.g. "opus") can map to whatever exact upstream id is current
// (e.g. "claude-opus-4-20250514").
package catalog

import "strings"

// Catalog maps model aliases to canonical model names.
type Catalog struct {
	aliases map[string]string
}

// New builds a Catalog from an alias→canonical map. Keys and values are trimmed;
// empty entries are skipped. A nil/empty map yields a catalog that resolves every
// name to itself.
func New(aliases map[string]string) *Catalog {
	m := make(map[string]string, len(aliases))
	for k, v := range aliases {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" || v == "" {
			continue
		}
		m[k] = v
	}
	return &Catalog{aliases: m}
}

// Canonical returns the canonical model name for model: the configured alias
// target if one exists, otherwise model unchanged. Resolution is single-hop (an
// alias whose target is itself another alias is not followed) and case-sensitive.
// A nil catalog resolves every name to itself.
func (c *Catalog) Canonical(model string) string {
	if c == nil {
		return model
	}
	if canon, ok := c.aliases[model]; ok {
		return canon
	}
	return model
}

// Aliases returns a copy of the configured alias map (for display, e.g. /llm.md).
func (c *Catalog) Aliases() map[string]string {
	if c == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(c.aliases))
	for k, v := range c.aliases {
		out[k] = v
	}
	return out
}
