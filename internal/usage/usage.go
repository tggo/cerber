// Package usage tracks request and token counts per credential and per model.
// It is an in-memory, concurrency-safe aggregate exposed via a snapshot (for the
// /admin/stats endpoint and the dashboard). Token counts are recorded when the
// response exposes them (non-streaming); request/error counts are always tracked.
package usage

import (
	"sort"
	"sync"
	"time"
)

// Stat is the aggregate for one key (a credential or a model).
type Stat struct {
	Requests     int64     `json:"requests"`
	Errors       int64     `json:"errors"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	LastUsed     time.Time `json:"last_used"`
}

// Entry is a named Stat for JSON output.
type Entry struct {
	Name string `json:"name"`
	Stat
}

// Report is a point-in-time snapshot.
type Report struct {
	Totals        Stat    `json:"totals"`
	ByCredential  []Entry `json:"by_credential"`
	ByModel       []Entry `json:"by_model"`
	SinceUnix     int64   `json:"since_unix"`
	GeneratedUnix int64   `json:"generated_unix"`
}

// Event is one recorded request outcome.
type Event struct {
	Credential   string
	Model        string
	IsError      bool
	InputTokens  int64
	OutputTokens int64
}

// Tracker accumulates usage. The zero value is not usable; call New.
type Tracker struct {
	mu           sync.Mutex
	byCredential map[string]*Stat
	byModel      map[string]*Stat
	totals       Stat
	since        time.Time
	now          func() time.Time
}

// Option customizes a Tracker.
type Option func(*Tracker)

// WithClock injects a clock (for tests). Defaults to time.Now.
func WithClock(now func() time.Time) Option {
	return func(t *Tracker) { t.now = now }
}

// New builds an empty Tracker.
func New(opts ...Option) *Tracker {
	t := &Tracker{
		byCredential: map[string]*Stat{},
		byModel:      map[string]*Stat{},
		now:          time.Now,
	}
	for _, o := range opts {
		o(t)
	}
	t.since = t.now()
	return t
}

// Record adds one event to the aggregates.
func (t *Tracker) Record(e Event) {
	cred := e.Credential
	if cred == "" {
		cred = "unknown"
	}
	model := e.Model
	if model == "" {
		model = "unknown"
	}
	now := t.now()

	t.mu.Lock()
	defer t.mu.Unlock()
	apply(&t.totals, e, now)
	apply(get(t.byCredential, cred), e, now)
	apply(get(t.byModel, model), e, now)
}

func get(m map[string]*Stat, k string) *Stat {
	s, ok := m[k]
	if !ok {
		s = &Stat{}
		m[k] = s
	}
	return s
}

func apply(s *Stat, e Event, now time.Time) {
	s.Requests++
	if e.IsError {
		s.Errors++
	}
	s.InputTokens += e.InputTokens
	s.OutputTokens += e.OutputTokens
	s.LastUsed = now
}

// Snapshot returns a sorted, immutable view of the current aggregates.
func (t *Tracker) Snapshot() Report {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Report{
		Totals:        t.totals,
		ByCredential:  entries(t.byCredential),
		ByModel:       entries(t.byModel),
		SinceUnix:     t.since.Unix(),
		GeneratedUnix: t.now().Unix(),
	}
}

// entries returns map contents sorted by request count (desc), then name.
func entries(m map[string]*Stat) []Entry {
	out := make([]Entry, 0, len(m))
	for name, s := range m {
		out = append(out, Entry{Name: name, Stat: *s})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Name < out[j].Name
	})
	return out
}
