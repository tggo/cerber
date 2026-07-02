package usage

import (
	"path/filepath"
	"testing"
	"time"
)

func fixedTracker() *Tracker {
	n := time.Unix(1000, 0)
	return New(WithClock(func() time.Time { return n }))
}

func TestRecordAndSnapshot(t *testing.T) {
	tr := fixedTracker()
	tr.Record(Event{Credential: "a", Model: "claude", InputTokens: 10, OutputTokens: 5})
	tr.Record(Event{Credential: "a", Model: "claude", InputTokens: 2, OutputTokens: 3})
	tr.Record(Event{Credential: "b", Model: "gpt", IsError: true})

	r := tr.Snapshot()
	if r.Totals.Requests != 3 || r.Totals.Errors != 1 {
		t.Errorf("totals = %+v", r.Totals)
	}
	if r.Totals.InputTokens != 12 || r.Totals.OutputTokens != 8 {
		t.Errorf("token totals = %+v", r.Totals)
	}
	// by_credential sorted by requests desc: a(2) before b(1)
	if len(r.ByCredential) != 2 || r.ByCredential[0].Name != "a" || r.ByCredential[0].Requests != 2 {
		t.Errorf("by_credential = %+v", r.ByCredential)
	}
	if r.ByCredential[1].Name != "b" || r.ByCredential[1].Errors != 1 {
		t.Errorf("by_credential[1] = %+v", r.ByCredential[1])
	}
	if r.SinceUnix != 1000 || r.GeneratedUnix != 1000 {
		t.Errorf("times = %d %d", r.SinceUnix, r.GeneratedUnix)
	}
}

func TestRecord_DefaultsUnknown(t *testing.T) {
	tr := fixedTracker()
	tr.Record(Event{}) // empty credential + model
	r := tr.Snapshot()
	if r.ByCredential[0].Name != "unknown" || r.ByModel[0].Name != "unknown" {
		t.Errorf("expected unknown keys: %+v %+v", r.ByCredential, r.ByModel)
	}
}

func TestSnapshot_SortStableByName(t *testing.T) {
	tr := fixedTracker()
	// equal request counts -> sorted by name asc
	tr.Record(Event{Credential: "z", Model: "m"})
	tr.Record(Event{Credential: "a", Model: "m"})
	r := tr.Snapshot()
	if r.ByCredential[0].Name != "a" || r.ByCredential[1].Name != "z" {
		t.Errorf("name sort = %+v", r.ByCredential)
	}
}

func TestPricingCost(t *testing.T) {
	tr := fixedTracker()
	tr.SetPricing(map[string]Price{"claude": {Input: 3, Output: 15}}) // per 1M
	tr.Record(Event{Credential: "a", Model: "claude", InputTokens: 1_000_000, OutputTokens: 2_000_000})
	tr.Record(Event{Credential: "a", Model: "unpriced", InputTokens: 5_000_000})
	r := tr.Snapshot()
	// claude: 1*3 + 2*15 = 33
	var claudeCost float64
	for _, e := range r.ByModel {
		if e.Name == "claude" {
			claudeCost = e.Cost
		}
		if e.Name == "unpriced" && e.Cost != 0 {
			t.Errorf("unpriced model should have 0 cost, got %v", e.Cost)
		}
	}
	if claudeCost != 33 {
		t.Errorf("claude cost = %v, want 33", claudeCost)
	}
	if r.TotalCost != 33 {
		t.Errorf("total cost = %v, want 33", r.TotalCost)
	}
}

func TestCost_PrefixMatch(t *testing.T) {
	tr := fixedTracker()
	tr.SetPricing(map[string]Price{
		"claude-opus": {Input: 5, Output: 25},
		"gpt-4o":      {Input: 2.5, Output: 10},
		"gpt-4o-mini": {Input: 0.15, Output: 0.6},
		"grok-4":      {Input: 3, Output: 15},
		"grok-4.20":   {Input: 2, Output: 6},
	})
	cases := []struct {
		model      string
		in, out    int64
		wantPerTok float64 // expected cost
	}{
		// dated variant matches the family prefix
		{"claude-opus-4-8", 1_000_000, 0, 5},
		// exact-and-longest: gpt-4o-mini beats the shorter gpt-4o prefix
		{"gpt-4o-mini-2026", 1_000_000, 0, 0.15},
		{"gpt-4o", 1_000_000, 0, 2.5},
		// longer prefix wins: grok-4.20-... -> grok-4.20, not grok-4
		{"grok-4.20-0309-non-reasoning", 1_000_000, 0, 2},
		{"grok-4.3", 1_000_000, 0, 3}, // only grok-4 prefixes it
		// no prefix -> free
		{"llama3.1:8b", 1_000_000, 1_000_000, 0},
	}
	for _, c := range cases {
		if got := tr.Cost(c.model, c.in, c.out); got != c.wantPerTok {
			t.Errorf("Cost(%q) = %v, want %v", c.model, got, c.wantPerTok)
		}
	}
}

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data", "usage.json")
	now := time.Unix(2000, 0)
	tr := New(WithClock(func() time.Time { return now }))
	tr.Record(Event{Credential: "a", Model: "m", InputTokens: 10, OutputTokens: 5})
	tr.Record(Event{Credential: "a", Model: "m", IsError: true})
	if err := tr.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := loaded.Snapshot()
	if r.Totals.Requests != 2 || r.Totals.Errors != 1 || r.Totals.InputTokens != 10 {
		t.Errorf("loaded totals = %+v", r.Totals)
	}
	// continues accumulating after load
	loaded.Record(Event{Credential: "a", Model: "m"})
	if loaded.Snapshot().Totals.Requests != 3 {
		t.Error("should accumulate after load")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	tr, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || tr == nil {
		t.Fatalf("missing file should give empty tracker: %v", err)
	}
	if tr.Snapshot().Totals.Requests != 0 {
		t.Error("expected empty")
	}
}

func TestConcurrentRecord(t *testing.T) {
	tr := fixedTracker()
	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				tr.Record(Event{Credential: "a", Model: "m", OutputTokens: 1})
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}
	if r := tr.Snapshot(); r.Totals.Requests != 1000 || r.Totals.OutputTokens != 1000 {
		t.Errorf("concurrent totals = %+v", r.Totals)
	}
}

func TestSeriesBuckets(t *testing.T) {
	base := time.Date(2026, 6, 8, 10, 30, 0, 0, time.UTC)
	now := base
	tr := New(WithClock(func() time.Time { return now }))
	tr.Record(Event{Credential: "a", Model: "m", InputTokens: 5})
	now = base.Add(70 * time.Minute) // next hour bucket
	tr.Record(Event{Credential: "a", Model: "m", InputTokens: 3})
	tr.Record(Event{Credential: "a", Model: "m"})

	r := tr.Snapshot()
	if len(r.Series) != 2 {
		t.Fatalf("series = %d buckets, want 2", len(r.Series))
	}
	if r.Series[0].Unix >= r.Series[1].Unix {
		t.Error("series not chronological")
	}
	if r.Series[0].Requests != 1 || r.Series[1].Requests != 2 {
		t.Errorf("bucket requests = %d, %d", r.Series[0].Requests, r.Series[1].Requests)
	}
	if r.Series[0].Unix != base.Truncate(time.Hour).Unix() {
		t.Error("bucket key should be hour start")
	}
}

func TestByClient_BreakdownAndCost(t *testing.T) {
	tr := fixedTracker()
	tr.SetPricing(map[string]Price{"claude": {Input: 3, Output: 15}})
	// gandalf uses two models; laptop uses one; an empty client is not attributed.
	tr.Record(Event{Client: "gandalf", Model: "claude", InputTokens: 1_000_000, OutputTokens: 1_000_000})
	tr.Record(Event{Client: "gandalf", Model: "gpt", InputTokens: 500_000})
	tr.Record(Event{Client: "laptop", Model: "claude", InputTokens: 2_000_000})
	tr.Record(Event{Model: "claude", InputTokens: 9}) // no client -> not in byClient

	r := tr.Snapshot()
	if len(r.ByClient) != 2 {
		t.Fatalf("by_client = %+v", r.ByClient)
	}
	// sorted by requests desc: gandalf(2) before laptop(1)
	g := r.ByClient[0]
	if g.Name != "gandalf" || g.Requests != 2 {
		t.Fatalf("by_client[0] = %+v", g)
	}
	// cost: claude 1M in*3 + 1M out*15 = 18; gpt unpriced = 0 -> total 18
	if g.Cost != 18 {
		t.Errorf("gandalf cost = %v, want 18", g.Cost)
	}
	if g.InputTokens != 1_500_000 || g.OutputTokens != 1_000_000 {
		t.Errorf("gandalf tokens = %+v", g.Stat)
	}
	// per-model breakdown present, claude carries cost, gpt is 0
	if len(g.ByModel) != 2 {
		t.Fatalf("gandalf by_model = %+v", g.ByModel)
	}
	byName := map[string]Entry{}
	for _, m := range g.ByModel {
		byName[m.Name] = m
	}
	if byName["claude"].Cost != 18 || byName["gpt"].Cost != 0 {
		t.Errorf("per-model cost = %+v", g.ByModel)
	}
}

func TestClientUsage_LookupAndMiss(t *testing.T) {
	tr := fixedTracker()
	tr.Record(Event{Client: "gandalf", Model: "m", InputTokens: 7})
	if _, ok := tr.ClientUsage("ghost"); ok {
		t.Error("unknown client should return ok=false")
	}
	rep, ok := tr.ClientUsage("gandalf")
	if !ok || rep.Name != "gandalf" || rep.InputTokens != 7 || len(rep.ByModel) != 1 {
		t.Errorf("ClientUsage = %+v ok=%v", rep, ok)
	}
}

func TestByClient_PersistRoundTrip(t *testing.T) {
	now := time.Unix(1000, 0)
	tr := New(WithClock(func() time.Time { return now }))
	tr.Record(Event{Client: "gandalf", Model: "claude", InputTokens: 4, OutputTokens: 2})
	path := filepath.Join(t.TempDir(), "usage.json")
	if err := tr.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	rep, ok := loaded.ClientUsage("gandalf")
	if !ok || rep.InputTokens != 4 || rep.OutputTokens != 2 {
		t.Errorf("after load, ClientUsage = %+v ok=%v", rep, ok)
	}
}
