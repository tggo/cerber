package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cerber/internal/usage"

	"github.com/prometheus/client_golang/prometheus"
)

func TestCollectorAndHandler(t *testing.T) {
	tr := usage.New()
	tr.Record(usage.Event{Credential: "acct-a", Model: "claude-x", InputTokens: 10, OutputTokens: 4})
	tr.Record(usage.Event{Credential: "acct-a", Model: "claude-x", IsError: true})

	rec := httptest.NewRecorder()
	Handler(tr).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("code %d", rec.Code)
	}
	out := rec.Body.String()
	want := []string{
		`cerber_requests_total{credential="acct-a"} 2`,
		`cerber_errors_total{credential="acct-a"} 1`,
		`cerber_input_tokens_total{credential="acct-a"} 10`,
		`cerber_output_tokens_total{credential="acct-a"} 4`,
		`cerber_requests_by_model_total{model="claude-x"} 2`,
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("metrics missing %q\n--- got ---\n%s", w, out)
		}
	}
}

func TestCollector_Describe(t *testing.T) {
	c := NewCollector(usage.New())
	ch := make(chan *prometheus.Desc, 8)
	c.Describe(ch)
	close(ch)
	n := 0
	for range ch {
		n++
	}
	if n != 5 {
		t.Errorf("Describe emitted %d descs, want 5", n)
	}
}
