package server

import (
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/tggo/cerber/internal/access"
	"github.com/tggo/cerber/internal/usage"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// perplexityKeyServer wires a managed-key server with a Perplexity provider
// answering every request with respBody.
func perplexityKeyServer(t *testing.T, respBody string) (*Server, string) {
	t.Helper()
	s, _, key := managedKeyServer(t, access.Limits{MaxCostUSD: 100, BudgetPeriod: "hour"})
	var u, b string
	s.RegisterChatter(perplexityProvider(t, 200, respBody, &u, &b))
	return s, key
}

// lastRequest returns the newest request-log event.
func lastRequest(t *testing.T, s *Server) usage.RequestEvent {
	t.Helper()
	evs, _ := s.usage.RecentRequests(usage.RequestFilter{}, 0, 1)
	if len(evs) == 0 {
		t.Fatal("no request logged")
	}
	return evs[0]
}

func TestCost_SearchRequestFeeChargedToKey(t *testing.T) {
	s, key := perplexityKeyServer(t, `{"results":[]}`)
	s.usage.SetPricing(map[string]usage.Price{"perplexity-search": {Request: 0.005}})
	h := s.Handler()
	for i := 0; i < 2; i++ {
		if rec := do(t, h, "POST", "/v1/search", `{"query":"x"}`, key); rec.Code != http.StatusOK {
			t.Fatalf("search = %d", rec.Code)
		}
	}
	if u := s.keys.List()[0].Usage; !near(u.CostUSD, 0.01) {
		t.Errorf("key charged %v, want 0.01 (2 × $0.005)", u.CostUSD)
	}
	if e := lastRequest(t, s); !near(e.Cost, 0.005) {
		t.Errorf("request log cost = %v, want 0.005", e.Cost)
	}
	if got := s.usage.Snapshot().TotalCost; !near(got, 0.01) {
		t.Errorf("total cost = %v, want 0.01", got)
	}
}

func TestCost_ReportedCostOverridesPricing(t *testing.T) {
	const sse = "data: {\"choices\":[{\"delta\":{\"content\":\"po\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"cost\":{\"request_cost\":0.005,\"total_cost\":0.00501}}}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":5,\"cost\":{\"request_cost\":0.005,\"total_cost\":0.0144}}}\n\n" +
		"data: [DONE]\n\n"
	const completed = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":28,\"output_tokens\":353,\"cost\":{\"total_cost\":0.00014}}}}"
	for _, tc := range []struct {
		name, path, body, resp string
		wantIn, wantOut        int64
		wantCost               float64
	}{
		{"chat non-stream", "/v1/chat/completions", `{"model":"sonar","messages":[{"role":"user","content":"hi"}]}`,
			`{"object":"chat.completion","usage":{"prompt_tokens":3,"completion_tokens":27,"cost":{"request_cost":0.014,"total_cost":0.01441}}}`, 3, 27, 0.01441},
		{"chat stream: last usage chunk wins", "/v1/chat/completions", `{"model":"sonar","stream":true,"stream_options":{"include_usage":true},"messages":[]}`, sse, 2, 5, 0.0144},
		{"responses stream: response.completed, no trailing newline", "/v1/responses", `{"model":"openai/gpt-5-mini","stream":true,"input":"hi"}`, completed, 28, 353, 0.00014},
		{"responses non-stream", "/v1/responses", `{"model":"openai/gpt-5-mini","input":"hi"}`,
			`{"object":"response","usage":{"input_tokens":10,"output_tokens":20,"cost":{"total_cost":0.002}}}`, 10, 20, 0.002},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, key := perplexityKeyServer(t, tc.resp)
			s.setProviderModels("perplexity", []string{"openai/gpt-5-mini"})
			// Token pricing that would give a very different number: the reported cost must win.
			s.usage.SetPricing(map[string]usage.Price{"sonar": {Input: 1e6, Output: 1e6, Request: 1}, "openai/": {Input: 1e6, Output: 1e6}})
			rec := do(t, s.Handler(), "POST", tc.path, tc.body, key)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			if rec.Body.String() != tc.resp {
				t.Errorf("body not relayed unchanged: %q", rec.Body.String())
			}
			e := lastRequest(t, s)
			if e.InputTokens != tc.wantIn || e.OutputTokens != tc.wantOut || !near(e.Cost, tc.wantCost) {
				t.Errorf("logged in=%d out=%d cost=%v, want %d/%d/%v", e.InputTokens, e.OutputTokens, e.Cost, tc.wantIn, tc.wantOut, tc.wantCost)
			}
			if u := s.keys.List()[0].Usage; !near(u.CostUSD, tc.wantCost) {
				t.Errorf("key charged %v, want %v", u.CostUSD, tc.wantCost)
			}
			if got := s.usage.Snapshot().TotalCost; !near(got, tc.wantCost) {
				t.Errorf("snapshot total = %v, want %v", got, tc.wantCost)
			}
		})
	}
}

func TestSSEUsage_EdgeCases(t *testing.T) {
	// A usage event split across writes is reassembled.
	sc := &sseUsage{}
	full := "data: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":8}}\n"
	for i := 0; i < len(full); i += 5 {
		_, _ = sc.Write([]byte(full[i:min(i+5, len(full))]))
	}
	if u := sc.usage(); u.in != 7 || u.out != 8 {
		t.Errorf("split write usage = %+v", u)
	}

	// An oversized line is skipped without losing the earlier usage event.
	sc = &sseUsage{}
	_, _ = sc.Write([]byte("data: {\"usage\":{\"prompt_tokens\":1}}\n"))
	_, _ = sc.Write([]byte("data: {\"usage\":\"" + strings.Repeat("x", maxSSELine+10) + "\"}\n"))
	if u := sc.usage(); u.in != 1 {
		t.Errorf("after overflow usage = %+v, want in=1", u)
	}

	// No usage event, or an unparsable one, yields zero.
	for _, in := range []string{"data: {\"choices\":[]}\n", "data: {\"usage\": broken\n", ": comment \"usage\"\n"} {
		sc = &sseUsage{}
		_, _ = sc.Write([]byte(in))
		if u := sc.usage(); u != (upstreamUsage{}) {
			t.Errorf("%q usage = %+v, want zero", in, u)
		}
	}
}
