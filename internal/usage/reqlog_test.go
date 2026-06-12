package usage

import (
	"testing"
	"time"
)

func TestRecentRequests_RingAndOrder(t *testing.T) {
	tr := New(WithRecentCap(3))
	for _, m := range []string{"a", "b", "c", "d", "e"} {
		tr.RecordRequest(RequestEvent{Model: m})
	}
	got := tr.RecentRequests("", 0)
	// cap=3 keeps the last three, newest first.
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (capped)", len(got))
	}
	if got[0].Model != "e" || got[1].Model != "d" || got[2].Model != "c" {
		t.Errorf("order = %v, want e,d,c", []string{got[0].Model, got[1].Model, got[2].Model})
	}
}

func TestRecentRequests_FilterAndLimit(t *testing.T) {
	tr := New(WithRecentCap(10))
	tr.RecordRequest(RequestEvent{Model: "x", IP: "1.1.1.1"})
	tr.RecordRequest(RequestEvent{Model: "y"})
	tr.RecordRequest(RequestEvent{Model: "x", IP: "2.2.2.2"})

	x := tr.RecentRequests("x", 0)
	if len(x) != 2 || x[0].IP != "2.2.2.2" || x[1].IP != "1.1.1.1" {
		t.Fatalf("model filter = %+v", x)
	}
	if got := tr.RecentRequests("", 1); len(got) != 1 || got[0].Model != "x" {
		t.Errorf("limit = %+v", got)
	}
	if got := tr.RecentRequests("nope", 0); len(got) != 0 {
		t.Errorf("unknown model = %+v", got)
	}
}

func TestRecentRequests_DefaultCap(t *testing.T) {
	tr := New() // default cap
	tr.RecordRequest(RequestEvent{Model: "a", Time: time.Unix(1, 0)})
	if got := tr.RecentRequests("", 0); len(got) != 1 {
		t.Errorf("default-cap tracker dropped an event: %+v", got)
	}
}
