// Package usage tracks request and token counts per credential and per model.
// It is an in-memory, concurrency-safe aggregate exposed via a snapshot (for the
// /admin/stats endpoint and the dashboard). Token counts are recorded when the
// response exposes them (non-streaming); request/error counts are always tracked.
package usage

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

// Entry is a named Stat for JSON output. Cost is set for model entries when
// pricing is configured.
type Entry struct {
	Name string  `json:"name"`
	Cost float64 `json:"cost,omitempty"`
	Stat
}

// Bucket is one hour of usage (Unix = hour start), for time-series analytics.
type Bucket struct {
	Unix int64 `json:"unix"`
	Stat
}

// ClientReport is the cumulative usage of one client key (managed key name, or
// "config"/"localhost" for the operator's own callers): its totals plus a
// per-model breakdown with cost. It answers "which models did this key use, for
// how many tokens and how much money".
type ClientReport struct {
	Name string  `json:"name"`
	Cost float64 `json:"cost"` // sum of per-model cost (0 for unpriced models)
	Stat
	ByModel []Entry `json:"by_model"` // this key's usage split per model, with cost
}

// Report is a point-in-time snapshot.
type Report struct {
	Totals        Stat           `json:"totals"`
	TotalCost     float64        `json:"total_cost"`
	ByCredential  []Entry        `json:"by_credential"`
	ByModel       []Entry        `json:"by_model"`
	ByClient      []ClientReport `json:"by_client"` // per client key, with per-model breakdown + cost
	Series        []Bucket       `json:"series"`    // hourly, chronological
	SinceUnix     int64          `json:"since_unix"`
	GeneratedUnix int64          `json:"generated_unix"`
}

// retentionHours bounds how much hourly history is kept (~30 days).
const retentionHours = 24 * 30

// Price is per-model pricing in cost units per 1,000,000 tokens.
type Price struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

// Event is one recorded request outcome.
type Event struct {
	Credential   string
	Client       string // managed key name / "config" / "localhost"; "" = not attributed
	Model        string
	IsError      bool
	InputTokens  int64
	OutputTokens int64
}

// Tracker accumulates usage. The zero value is not usable; call New.
type Tracker struct {
	mu            sync.Mutex
	byCredential  map[string]*Stat
	byModel       map[string]*Stat
	byClient      map[string]map[string]*Stat           // client key -> model -> stat
	buckets       map[int64]*Stat                       // hour-start unix -> stat
	credBuckets   map[int64]map[string]*Stat            // hour-start unix -> credential -> stat
	modelBuckets  map[int64]map[string]*Stat            // hour-start unix -> model -> stat
	clientBuckets map[int64]map[string]map[string]*Stat // hour-start unix -> client -> model -> stat
	totals        Stat
	since         time.Time
	now           func() time.Time
	pricing       map[string]Price
	recent        []RequestEvent // bounded ring of per-request events (in-memory only)
	recentCap     int
}

// Option customizes a Tracker.
type Option func(*Tracker)

// WithClock injects a clock (for tests). Defaults to time.Now.
func WithClock(now func() time.Time) Option {
	return func(t *Tracker) { t.now = now }
}

// WithRecentCap sets how many recent per-request events are retained in memory
// (default 1000). n<=0 keeps the default.
func WithRecentCap(n int) Option {
	return func(t *Tracker) {
		if n > 0 {
			t.recentCap = n
		}
	}
}

// New builds an empty Tracker.
func New(opts ...Option) *Tracker {
	t := &Tracker{
		byCredential:  map[string]*Stat{},
		byModel:       map[string]*Stat{},
		byClient:      map[string]map[string]*Stat{},
		buckets:       map[int64]*Stat{},
		credBuckets:   map[int64]map[string]*Stat{},
		modelBuckets:  map[int64]map[string]*Stat{},
		clientBuckets: map[int64]map[string]map[string]*Stat{},
		pricing:       map[string]Price{},
		now:           time.Now,
		recentCap:     defaultRecentCap,
	}
	for _, o := range opts {
		o(t)
	}
	t.since = t.now()
	return t
}

// SetPricing installs per-model pricing (cost per 1M tokens) for cost reporting.
func (t *Tracker) SetPricing(p map[string]Price) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pricing = map[string]Price{}
	for k, v := range p {
		t.pricing[k] = v
	}
}

// Cost returns the configured cost for a model's input/output tokens, or 0 when
// no pricing is set for the model. Used by per-key budget enforcement.
func (t *Tracker) Cost(model string, in, out int64) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.modelCost(model, Stat{InputTokens: in, OutputTokens: out})
}

// priceFor resolves a model's price: an exact match wins, otherwise the
// longest configured key that is a prefix of the model (so a family key like
// "claude-opus" prices every dated variant "claude-opus-4-8", "claude-opus-…").
func (t *Tracker) priceFor(model string) (Price, bool) {
	if p, ok := t.pricing[model]; ok {
		return p, true
	}
	best := ""
	for k := range t.pricing {
		if len(k) > len(best) && strings.HasPrefix(model, k) {
			best = k
		}
	}
	if best == "" {
		return Price{}, false
	}
	return t.pricing[best], true
}

// modelCost computes the cost for a model's tokens (0 if no pricing).
func (t *Tracker) modelCost(model string, s Stat) float64 {
	p, ok := t.priceFor(model)
	if !ok {
		return 0
	}
	return float64(s.InputTokens)/1e6*p.Input + float64(s.OutputTokens)/1e6*p.Output
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

	hour := now.Truncate(time.Hour).Unix()

	t.mu.Lock()
	defer t.mu.Unlock()
	apply(&t.totals, e, now)
	apply(get(t.byCredential, cred), e, now)
	apply(get(t.byModel, model), e, now)
	if e.Client != "" {
		cm := t.byClient[e.Client]
		if cm == nil {
			cm = map[string]*Stat{}
			t.byClient[e.Client] = cm
		}
		apply(get(cm, model), e, now)
	}

	b, ok := t.buckets[hour]
	if !ok {
		b = &Stat{}
		t.buckets[hour] = b
		t.pruneBuckets(now)
	}
	apply(b, e, now)

	cb := t.credBuckets[hour]
	if cb == nil {
		cb = map[string]*Stat{}
		t.credBuckets[hour] = cb
	}
	apply(get(cb, cred), e, now)

	mb := t.modelBuckets[hour]
	if mb == nil {
		mb = map[string]*Stat{}
		t.modelBuckets[hour] = mb
	}
	apply(get(mb, model), e, now)

	if e.Client != "" {
		clb := t.clientBuckets[hour]
		if clb == nil {
			clb = map[string]map[string]*Stat{}
			t.clientBuckets[hour] = clb
		}
		cm := clb[e.Client]
		if cm == nil {
			cm = map[string]*Stat{}
			clb[e.Client] = cm
		}
		apply(get(cm, model), e, now)
	}
}

// pruneBuckets drops hourly buckets (overall, per-credential, per-model,
// per-client) older than the retention window.
func (t *Tracker) pruneBuckets(now time.Time) {
	cutoff := now.Add(-retentionHours * time.Hour).Unix()
	for k := range t.buckets {
		if k < cutoff {
			delete(t.buckets, k)
		}
	}
	for k := range t.credBuckets {
		if k < cutoff {
			delete(t.credBuckets, k)
		}
	}
	for k := range t.modelBuckets {
		if k < cutoff {
			delete(t.modelBuckets, k)
		}
	}
	for k := range t.clientBuckets {
		if k < cutoff {
			delete(t.clientBuckets, k)
		}
	}
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

// Snapshot returns a sorted, immutable view of the current aggregates, with
// per-model and total cost computed from configured pricing.
func (t *Tracker) Snapshot() Report {
	t.mu.Lock()
	defer t.mu.Unlock()
	byModel := entries(t.byModel)
	var total float64
	for i := range byModel {
		byModel[i].Cost = t.modelCost(byModel[i].Name, byModel[i].Stat)
		total += byModel[i].Cost
	}
	series := make([]Bucket, 0, len(t.buckets))
	for hour, st := range t.buckets {
		series = append(series, Bucket{Unix: hour, Stat: *st})
	}
	sort.Slice(series, func(i, j int) bool { return series[i].Unix < series[j].Unix })

	byClient := make([]ClientReport, 0, len(t.byClient))
	for name := range t.byClient {
		byClient = append(byClient, t.clientReportLocked(name))
	}
	sort.Slice(byClient, func(i, j int) bool {
		if byClient[i].Requests != byClient[j].Requests {
			return byClient[i].Requests > byClient[j].Requests
		}
		return byClient[i].Name < byClient[j].Name
	})

	return Report{
		Totals:        t.totals,
		TotalCost:     total,
		ByCredential:  entries(t.byCredential),
		ByModel:       byModel,
		ByClient:      byClient,
		Series:        series,
		SinceUnix:     t.since.Unix(),
		GeneratedUnix: t.now().Unix(),
	}
}

// SnapshotWindow returns a Report scoped to the last `window` of time, ending
// now: Totals, ByCredential, ByModel, ByClient and Series are computed by
// summing only the hourly buckets that fall in the window. window<=0 (or >=
// the full retention window) is equivalent to Snapshot (all-time).
func (t *Tracker) SnapshotWindow(window time.Duration) Report {
	if window <= 0 || window >= retentionHours*time.Hour {
		return t.Snapshot()
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	cutoff := now.Add(-window).Unix()

	totals := Stat{}
	series := make([]Bucket, 0, len(t.buckets))
	for hour, st := range t.buckets {
		if hour < cutoff {
			continue
		}
		totals.Requests += st.Requests
		totals.Errors += st.Errors
		totals.InputTokens += st.InputTokens
		totals.OutputTokens += st.OutputTokens
		if st.LastUsed.After(totals.LastUsed) {
			totals.LastUsed = st.LastUsed
		}
		series = append(series, Bucket{Unix: hour, Stat: *st})
	}
	sort.Slice(series, func(i, j int) bool { return series[i].Unix < series[j].Unix })

	byCredential := sumWindowedBuckets(t.credBuckets, cutoff)
	byModel := sumWindowedBuckets(t.modelBuckets, cutoff)
	var totalCost float64
	for i := range byModel {
		byModel[i].Cost = t.modelCost(byModel[i].Name, byModel[i].Stat)
		totalCost += byModel[i].Cost
	}

	byClientModel := map[string]map[string]*Stat{} // client -> model -> stat
	for hour, clients := range t.clientBuckets {
		if hour < cutoff {
			continue
		}
		for client, byModelStat := range clients {
			cm := byClientModel[client]
			if cm == nil {
				cm = map[string]*Stat{}
				byClientModel[client] = cm
			}
			for model, st := range byModelStat {
				apply2(get(cm, model), *st)
			}
		}
	}
	byClient := make([]ClientReport, 0, len(byClientModel))
	for name, byModelStat := range byClientModel {
		byClient = append(byClient, t.clientReportFromModels(name, byModelStat))
	}
	sort.Slice(byClient, func(i, j int) bool {
		if byClient[i].Requests != byClient[j].Requests {
			return byClient[i].Requests > byClient[j].Requests
		}
		return byClient[i].Name < byClient[j].Name
	})

	return Report{
		Totals:        totals,
		TotalCost:     totalCost,
		ByCredential:  byCredential,
		ByModel:       byModel,
		ByClient:      byClient,
		Series:        series,
		SinceUnix:     cutoff,
		GeneratedUnix: now.Unix(),
	}
}

// sumWindowedBuckets aggregates hourly key->Stat buckets at or after cutoff
// into sorted Entry totals per key.
func sumWindowedBuckets(buckets map[int64]map[string]*Stat, cutoff int64) []Entry {
	sums := map[string]*Stat{}
	for hour, byKey := range buckets {
		if hour < cutoff {
			continue
		}
		for key, st := range byKey {
			apply2(get(sums, key), *st)
		}
	}
	return entries(sums)
}

// apply2 accumulates src into dst (like apply, but from an existing Stat
// rather than a raw Event).
func apply2(dst *Stat, src Stat) {
	dst.Requests += src.Requests
	dst.Errors += src.Errors
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	if src.LastUsed.After(dst.LastUsed) {
		dst.LastUsed = src.LastUsed
	}
}

// ClientUsage returns the cumulative usage of one client key (per-model
// breakdown, tokens, and cost). The bool is false when the key has no recorded
// usage yet.
func (t *Tracker) ClientUsage(name string) (ClientReport, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.byClient[name]; !ok {
		return ClientReport{}, false
	}
	return t.clientReportLocked(name), true
}

// clientReportLocked builds the all-time report for one client key. The
// caller holds mu.
func (t *Tracker) clientReportLocked(name string) ClientReport {
	return t.clientReportFromModels(name, t.byClient[name])
}

// clientReportFromModels builds a ClientReport for one client key from a
// model->Stat breakdown (either the all-time map or a windowed sum). The
// caller holds mu (modelCost reads pricing).
func (t *Tracker) clientReportFromModels(name string, byModelStat map[string]*Stat) ClientReport {
	byModel := entries(byModelStat)
	var cr ClientReport
	cr.Name = name
	for i := range byModel {
		byModel[i].Cost = t.modelCost(byModel[i].Name, byModel[i].Stat)
		cr.Cost += byModel[i].Cost
		cr.Requests += byModel[i].Requests
		cr.Errors += byModel[i].Errors
		cr.InputTokens += byModel[i].InputTokens
		cr.OutputTokens += byModel[i].OutputTokens
		if byModel[i].LastUsed.After(cr.LastUsed) {
			cr.LastUsed = byModel[i].LastUsed
		}
	}
	cr.ByModel = byModel
	return cr
}

// persisted is the on-disk shape of a tracker's aggregates.
type persisted struct {
	Totals        Stat                                  `json:"totals"`
	ByCredential  map[string]*Stat                      `json:"by_credential"`
	ByModel       map[string]*Stat                      `json:"by_model"`
	ByClient      map[string]map[string]*Stat           `json:"by_client"`
	Buckets       map[int64]*Stat                       `json:"buckets"`
	CredBuckets   map[int64]map[string]*Stat            `json:"cred_buckets"`
	ModelBuckets  map[int64]map[string]*Stat            `json:"model_buckets"`
	ClientBuckets map[int64]map[string]map[string]*Stat `json:"client_buckets"`
	SinceUnix     int64                                 `json:"since_unix"`
}

// Save writes the aggregates to path (atomic via temp+rename) so usage survives
// restarts. Pricing is config-driven and not persisted.
func (t *Tracker) Save(path string) error {
	t.mu.Lock()
	data, err := json.Marshal(persisted{
		Totals: t.totals, ByCredential: t.byCredential, ByModel: t.byModel,
		ByClient: t.byClient, Buckets: t.buckets,
		CredBuckets: t.credBuckets, ModelBuckets: t.modelBuckets,
		ClientBuckets: t.clientBuckets, SinceUnix: t.since.Unix(),
	})
	t.mu.Unlock()
	if err != nil {
		return fmt.Errorf("usage: marshal: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("usage: mkdir: %w", err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("usage: write: %w", err)
	}
	return os.Rename(tmp, path)
}

// Load builds a Tracker from a saved file (missing file -> empty tracker).
func Load(path string, opts ...Option) (*Tracker, error) {
	t := New(opts...)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return t, nil
		}
		return nil, fmt.Errorf("usage: read: %w", err)
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("usage: parse: %w", err)
	}
	if p.ByCredential != nil {
		t.byCredential = p.ByCredential
	}
	if p.ByModel != nil {
		t.byModel = p.ByModel
	}
	if p.ByClient != nil {
		t.byClient = p.ByClient
	}
	if p.Buckets != nil {
		t.buckets = p.Buckets
	}
	if p.CredBuckets != nil {
		t.credBuckets = p.CredBuckets
	}
	if p.ModelBuckets != nil {
		t.modelBuckets = p.ModelBuckets
	}
	if p.ClientBuckets != nil {
		t.clientBuckets = p.ClientBuckets
	}
	t.totals = p.Totals
	if p.SinceUnix > 0 {
		t.since = time.Unix(p.SinceUnix, 0)
	}
	return t, nil
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
