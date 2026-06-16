package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tggo/cerber/internal/usage"

	"github.com/prometheus/client_golang/prometheus"
)

func TestCollectorAndHandler(t *testing.T) {
	tr := usage.New()
	tr.SetPricing(map[string]usage.Price{"claude-x": {Input: 1_000_000, Output: 1_000_000}}) // $1/token
	tr.Record(usage.Event{Credential: "acct-a", Model: "claude-x", InputTokens: 10, OutputTokens: 4})
	tr.Record(usage.Event{Credential: "acct-a", Model: "claude-x", IsError: true})

	rec := httptest.NewRecorder()
	Handler(tr, "v1.2.3", nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
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
		`cerber_cost_usd_total{model="claude-x"} 14`, // 10 in + 4 out at $1/token
		`cerber_input_tokens_by_model_total{model="claude-x"} 10`,
		`cerber_output_tokens_by_model_total{model="claude-x"} 4`,
		`cerber_build_info{version="v1.2.3"} 1`,
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("metrics missing %q\n--- got ---\n%s", w, out)
		}
	}
}

// TestHandlerWithLiveMetrics confirms the live instruments are exposed when a
// *Metrics is passed, and that HTTP + queue observations show up in the output.
func TestHandlerWithLiveMetrics(t *testing.T) {
	tr := usage.New()
	m := NewMetrics()
	m.ObserveHTTP("/v1/chat/completions", "arliai", 200, 2.4)
	m.ObserveHTTP("/v1/chat/completions", "", 503, 0.01) // empty provider -> "none"
	m.QueueDepthInc("arliai")
	m.QueueWait("arliai", 5.0)
	m.InflightInc("arliai")

	rec := httptest.NewRecorder()
	Handler(tr, "dev", m).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("code %d", rec.Code)
	}
	out := rec.Body.String()
	want := []string{
		`cerber_http_requests_total{path="/v1/chat/completions",provider="arliai",status="200"} 1`,
		`cerber_http_requests_total{path="/v1/chat/completions",provider="none",status="503"} 1`,
		`cerber_http_request_duration_seconds_count{path="/v1/chat/completions",provider="arliai",status="200"} 1`,
		`cerber_provider_queue_depth{provider="arliai"} 1`,
		`cerber_provider_inflight_requests{provider="arliai"} 1`,
		`cerber_provider_queue_wait_seconds_count{provider="arliai"} 1`,
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("metrics missing %q\n--- got ---\n%s", w, out)
		}
	}
}

// TestMetrics_NilSafe verifies a nil *Metrics no-ops instead of panicking (so
// providers can be wired with metrics disabled).
func TestMetrics_NilSafe(t *testing.T) {
	var m *Metrics
	m.ObserveHTTP("/p", "x", 200, 1)
	m.QueueWait("x", 1)
	m.QueueDepthInc("x")
	m.QueueDepthDec("x")
	m.InflightInc("x")
	m.InflightDec("x")
}

func TestMetrics_GaugesUpAndDown(t *testing.T) {
	m := NewMetrics()
	m.QueueDepthInc("p")
	m.QueueDepthDec("p")
	m.InflightInc("p")
	m.InflightDec("p")

	rec := httptest.NewRecorder()
	Handler(usage.New(), "dev", m).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	out := rec.Body.String()
	for _, w := range []string{
		`cerber_provider_queue_depth{provider="p"} 0`,
		`cerber_provider_inflight_requests{provider="p"} 0`,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("metrics missing %q\n--- got ---\n%s", w, out)
		}
	}
}

func TestCollector_Describe(t *testing.T) {
	c := NewCollector(usage.New(), "dev")
	ch := make(chan *prometheus.Desc, 16)
	c.Describe(ch)
	close(ch)
	n := 0
	for range ch {
		n++
	}
	if n != 9 {
		t.Errorf("Describe emitted %d descs, want 9", n)
	}
}
