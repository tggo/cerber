package quota

import (
	"net/http"
	"testing"
	"time"
)

func TestRecordAndGet(t *testing.T) {
	tr := New(WithClock(func() time.Time { return time.Unix(1000, 0) }))
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.15")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1780879800")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.02")
	tr.Record("acct-a", h)

	s, ok := tr.Get("acct-a")
	if !ok {
		t.Fatal("expected snapshot")
	}
	if s.FiveHour.Status != "allowed" || s.FiveHour.Utilization != 0.15 || s.FiveHour.ResetUnix != 1780879800 {
		t.Errorf("5h = %+v", s.FiveHour)
	}
	if s.SevenDay.Utilization != 0.02 {
		t.Errorf("7d = %+v", s.SevenDay)
	}
	if s.UpdatedUnix != 1000 {
		t.Errorf("updated = %d", s.UpdatedUnix)
	}
}

func TestRecord_NoHeaders(t *testing.T) {
	tr := New()
	tr.Record("a", http.Header{}) // no ratelimit headers
	if _, ok := tr.Get("a"); ok {
		t.Error("should not record without headers")
	}
	tr.Record("", nil)
	tr.Record("b", nil)
	if _, ok := tr.Get("b"); ok {
		t.Error("nil header should be no-op")
	}
}
