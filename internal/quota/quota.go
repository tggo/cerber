// Package quota tracks per-credential rate-limit/quota state. For Anthropic it is
// populated passively from the Anthropic-Ratelimit-Unified-* response headers
// cerber already sees on every request — no separate polling.
package quota

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Window is one rate-limit window's state (e.g. 5h or 7d).
type Window struct {
	Status      string  `json:"status,omitempty"`     // e.g. "allowed"
	Utilization float64 `json:"utilization"`          // 0..1
	ResetUnix   int64   `json:"reset_unix,omitempty"` // when the window resets
}

// Snapshot is a credential's quota at a point in time.
type Snapshot struct {
	FiveHour    Window `json:"five_hour"`
	SevenDay    Window `json:"seven_day"`
	UpdatedUnix int64  `json:"updated_unix"`
}

// Tracker holds the latest quota snapshot per credential name.
type Tracker struct {
	mu  sync.Mutex
	m   map[string]Snapshot
	now func() time.Time
}

// Option customizes a Tracker.
type Option func(*Tracker)

// WithClock injects a clock (tests).
func WithClock(now func() time.Time) Option { return func(t *Tracker) { t.now = now } }

// New builds an empty Tracker.
func New(opts ...Option) *Tracker {
	t := &Tracker{m: map[string]Snapshot{}, now: time.Now}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Record extracts Anthropic unified rate-limit headers for credential cred. It is
// a no-op if none are present (e.g. non-Anthropic providers).
func (t *Tracker) Record(cred string, h http.Header) {
	if cred == "" || h == nil {
		return
	}
	five := window(h, "Anthropic-Ratelimit-Unified-5h-")
	seven := window(h, "Anthropic-Ratelimit-Unified-7d-")
	if five == (Window{}) && seven == (Window{}) {
		return
	}
	t.mu.Lock()
	t.m[cred] = Snapshot{FiveHour: five, SevenDay: seven, UpdatedUnix: t.now().Unix()}
	t.mu.Unlock()
}

// Get returns the snapshot for a credential, if any.
func (t *Tracker) Get(cred string) (Snapshot, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.m[cred]
	return s, ok
}

func window(h http.Header, prefix string) Window {
	var w Window
	w.Status = h.Get(prefix + "Status")
	if v := h.Get(prefix + "Utilization"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			w.Utilization = f
		}
	}
	if v := h.Get(prefix + "Reset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			w.ResetUnix = n
		}
	}
	return w
}
