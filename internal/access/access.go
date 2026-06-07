// Package access controls which clients may call cerber. A client presents an
// API key (Authorization: Bearer <key> or x-api-key: <key>) which is matched,
// in constant time, against the configured allow-list.
package access

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// Checker matches presented client keys against the configured allow-list.
type Checker struct {
	keys []string
}

// New builds a Checker from the configured client keys.
func New(keys []string) *Checker {
	cp := make([]string, len(keys))
	copy(cp, keys)
	return &Checker{keys: cp}
}

// Allow reports whether presented matches any configured key. The comparison is
// constant-time and always scans every key, so it leaks neither key contents
// nor which key matched.
func (c *Checker) Allow(presented string) bool {
	if presented == "" {
		return false
	}
	pb := []byte(presented)
	matched := 0
	for _, k := range c.keys {
		if subtle.ConstantTimeCompare(pb, []byte(k)) == 1 {
			matched = 1
		}
	}
	return matched == 1
}

// FromRequest extracts the presented client key, preferring the Authorization
// bearer token and falling back to the x-api-key header. Returns "" if absent.
func FromRequest(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		if rest, ok := strings.CutPrefix(auth, "Bearer "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}
