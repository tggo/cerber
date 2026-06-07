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
