package usage

import (
	"strings"
	"testing"
	"time"
)

func TestRecentRequests_RingAndOrder(t *testing.T) {
	tr := New(WithRecentCap(3))
	for _, m := range []string{"a", "b", "c", "d", "e"} {
		tr.RecordRequest(RequestEvent{Model: m})
	}
	got, total := tr.RecentRequests(RequestFilter{}, 0, 0)
	// cap=3 keeps the last three, newest first.
	if len(got) != 3 || total != 3 {
		t.Fatalf("len=%d total=%d, want 3/3 (capped)", len(got), total)
	}
	if got[0].Model != "e" || got[1].Model != "d" || got[2].Model != "c" {
		t.Errorf("order = %v, want e,d,c", []string{got[0].Model, got[1].Model, got[2].Model})
	}
}

func TestRecentRequests_Filter(t *testing.T) {
	tr := New(WithRecentCap(10))
	tr.RecordRequest(RequestEvent{Model: "x", Provider: "openai", Credential: "a", IP: "1.1.1.1"})
	tr.RecordRequest(RequestEvent{Model: "y", Provider: "anthropic", Credential: "b"})
	tr.RecordRequest(RequestEvent{Model: "x", Provider: "openai", Credential: "c", IP: "2.2.2.2"})

	x, total := tr.RecentRequests(RequestFilter{Model: "x"}, 0, 0)
	if len(x) != 2 || total != 2 || x[0].IP != "2.2.2.2" || x[1].IP != "1.1.1.1" {
		t.Fatalf("model filter = %+v (total %d)", x, total)
	}
	if got, _ := tr.RecentRequests(RequestFilter{Provider: "anthropic"}, 0, 0); len(got) != 1 || got[0].Model != "y" {
		t.Errorf("provider filter = %+v", got)
	}
	if got, _ := tr.RecentRequests(RequestFilter{Credential: "c"}, 0, 0); len(got) != 1 || got[0].Model != "x" {
		t.Errorf("credential filter = %+v", got)
	}
	if got, _ := tr.RecentRequests(RequestFilter{Model: "nope"}, 0, 0); len(got) != 0 {
		t.Errorf("unknown model = %+v", got)
	}
}

func TestRecentRequests_Pagination(t *testing.T) {
	tr := New(WithRecentCap(10))
	for _, m := range []string{"a", "b", "c", "d", "e"} { // newest = e
		tr.RecordRequest(RequestEvent{Model: m})
	}
	// page 1: newest 2
	p1, total := tr.RecentRequests(RequestFilter{}, 0, 2)
	if total != 5 || len(p1) != 2 || p1[0].Model != "e" || p1[1].Model != "d" {
		t.Fatalf("page1 = %+v total=%d", p1, total)
	}
	// page 2: next 2
	p2, _ := tr.RecentRequests(RequestFilter{}, 2, 2)
	if len(p2) != 2 || p2[0].Model != "c" || p2[1].Model != "b" {
		t.Fatalf("page2 = %+v", p2)
	}
	// offset past the end
	if p3, _ := tr.RecentRequests(RequestFilter{}, 5, 2); len(p3) != 0 {
		t.Errorf("page past end = %+v", p3)
	}
}

func TestRecentRequests_DefaultCap(t *testing.T) {
	tr := New() // default cap
	tr.RecordRequest(RequestEvent{Model: "a", Time: time.Unix(1, 0)})
	if got, total := tr.RecentRequests(RequestFilter{}, 0, 0); len(got) != 1 || total != 1 {
		t.Errorf("default-cap tracker dropped an event: %+v", got)
	}
}

func TestAnnotateRequest(t *testing.T) {
	tr := New()
	seq := tr.RecordRequest(RequestEvent{Model: "a"})
	tr.RecordRequest(RequestEvent{Model: "b"})
	tr.AnnotateRequest(seq, 429, "rate limited")
	tr.AnnotateRequest(0, 500, "ignored")    // no handle → no-op
	tr.AnnotateRequest(9999, 500, "ignored") // aged out / unknown → no-op

	got, _ := tr.RecentRequests(RequestFilter{Model: "a"}, 0, 0)
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].Status != 429 || got[0].Detail != "rate limited" || !got[0].Error {
		t.Errorf("annotated = %+v", got[0])
	}
	// Only the errored event matches the errors-only filter.
	errs, total := tr.RecentRequests(RequestFilter{ErrorsOnly: true}, 0, 0)
	if len(errs) != 1 || total != 1 || errs[0].Model != "a" {
		t.Errorf("errors-only = %+v (total %d), want just the annotated one", errs, total)
	}
}

func TestRecordRequest_TruncatesDetail(t *testing.T) {
	tr := New()
	long := strings.Repeat("x", maxDetail+50)
	tr.RecordRequest(RequestEvent{Model: "a", Detail: long})
	got, _ := tr.RecentRequests(RequestFilter{}, 0, 0)
	if want := strings.Repeat("x", maxDetail) + "…"; got[0].Detail != want {
		t.Errorf("detail len = %d, want truncated to %d + ellipsis", len(got[0].Detail), maxDetail)
	}
}
