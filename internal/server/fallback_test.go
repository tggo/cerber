package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tggo/cerber/internal/config"
	"github.com/tggo/cerber/internal/provider"
	providermocks "github.com/tggo/cerber/internal/provider/mocks"

	"github.com/stretchr/testify/mock"
)

// chatterResp builds an OpenAI-format provider.Response.
func chatterResp(status int, body string) *provider.Response {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return &provider.Response{
		Status: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Credential: "oai",
	}
}

func registerChatter(t *testing.T, s *Server, name string) *providermocks.Chatter {
	t.Helper()
	c := providermocks.NewChatter(t)
	c.EXPECT().Name().Return(name)
	s.RegisterChatter(c)
	return c
}

func TestOpenAITargets(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	s.SetFallbacks([]config.Fallback{{Model: "claude", To: []string{"gpt-4o", "gemini-2"}}})

	// Header override wins over config.
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Cerber-Fallback", "a, b ,, c")
	if got := s.openAITargets(r, "claude-x"); strings.Join(got, ",") != "claude-x,a,b,c" {
		t.Errorf("header targets = %v", got)
	}
	// Config chain by prefix match.
	r2 := httptest.NewRequest("POST", "/", nil)
	if got := s.openAITargets(r2, "claude-opus"); strings.Join(got, ",") != "claude-opus,gpt-4o,gemini-2" {
		t.Errorf("config targets = %v", got)
	}
	// No matching chain → just the model.
	if got := s.openAITargets(r2, "gpt-4o"); strings.Join(got, ",") != "gpt-4o" {
		t.Errorf("no-chain targets = %v", got)
	}
}

func TestFallback_AnthropicDownToChatter(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	chatter := registerChatter(t, s, "openai")
	s.SetFallbacks([]config.Fallback{{Model: "claude", To: []string{"gpt-4o"}}})

	// Primary (anthropic) returns 503 → retryable; fallback chatter succeeds.
	up.EXPECT().Send(mock.Anything, mock.Anything, false, mock.Anything, mock.Anything).
		Return(resp(503, "application/json", `{"error":"overloaded"}`), nil).Once()
	chatter.EXPECT().Chat(mock.Anything,
		mock.MatchedBy(func(b []byte) bool { return strings.Contains(string(b), "gpt-4o") }),
		false, mock.Anything).
		Return(chatterResp(200, `{"id":"cmpl","object":"chat.completion"}`), nil).Once()

	rec := do(t, s.Handler(), "POST", "/v1/chat/completions", `{"model":"claude-x","stream":false,"messages":[{"role":"user","content":"hi"}]}`, clientKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("fallback result = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cmpl") {
		t.Errorf("body = %q, want chatter response", rec.Body.String())
	}
}

func TestFallback_4xxIsTerminal(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	// Chatter registered but must NOT be called: a 4xx is a client error.
	registerChatter(t, s, "openai")
	s.SetFallbacks([]config.Fallback{{Model: "claude", To: []string{"gpt-4o"}}})

	up.EXPECT().Send(mock.Anything, mock.Anything, false, mock.Anything, mock.Anything).
		Return(resp(400, "application/json", `{"error":"bad request"}`), nil).Once()

	rec := do(t, s.Handler(), "POST", "/v1/chat/completions", `{"model":"claude-x","stream":false,"messages":[{"role":"user","content":"hi"}]}`, clientKey)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("4xx result = %d, want 400 (no fallback)", rec.Code)
	}
}

func TestFallback_HeaderOverride(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	chatter := registerChatter(t, s, "openai")

	// No config fallbacks; the client supplies the chain via header.
	up.EXPECT().Send(mock.Anything, mock.Anything, false, mock.Anything, mock.Anything).
		Return(resp(503, "application/json", `{"error":"down"}`), nil).Once()
	chatter.EXPECT().Chat(mock.Anything, mock.Anything, false, mock.Anything).
		Return(chatterResp(200, `{"id":"viahdr"}`), nil).Once()

	r := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"claude-x","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Authorization", "Bearer "+clientKey)
	r.Header.Set("X-Cerber-Fallback", "gpt-4o")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "viahdr") {
		t.Fatalf("header fallback = %d %q", rec.Code, rec.Body.String())
	}
}

func TestFallback_SkipsUnroutableTarget(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	chatter := registerChatter(t, s, "openai")
	// Chain: anthropic (down) → unroutable junk (skipped) → gpt-4o (succeeds).
	s.SetFallbacks([]config.Fallback{{Model: "claude", To: []string{"zzz-unknown-model", "gpt-4o"}}})

	up.EXPECT().Send(mock.Anything, mock.Anything, false, mock.Anything, mock.Anything).
		Return(resp(503, "application/json", `{"error":"down"}`), nil).Once()
	chatter.EXPECT().Chat(mock.Anything,
		mock.MatchedBy(func(b []byte) bool { return strings.Contains(string(b), "gpt-4o") }),
		false, mock.Anything).
		Return(chatterResp(200, `{"id":"ok"}`), nil).Once()

	rec := do(t, s.Handler(), "POST", "/v1/chat/completions", `{"model":"claude-x","stream":false,"messages":[{"role":"user","content":"hi"}]}`, clientKey)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("skip-unroutable = %d %q", rec.Code, rec.Body.String())
	}
}

func TestFallback_AllExhausted(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	chatter := registerChatter(t, s, "openai")
	s.SetFallbacks([]config.Fallback{{Model: "claude", To: []string{"gpt-4o"}}})

	// Both targets fail; the last target's error is surfaced.
	up.EXPECT().Send(mock.Anything, mock.Anything, false, mock.Anything, mock.Anything).
		Return(resp(503, "application/json", `{"error":"down"}`), nil).Once()
	chatter.EXPECT().Chat(mock.Anything, mock.Anything, false, mock.Anything).
		Return(chatterResp(503, `{"error":"also down"}`), nil).Once()

	rec := do(t, s.Handler(), "POST", "/v1/chat/completions", `{"model":"claude-x","stream":false,"messages":[{"role":"user","content":"hi"}]}`, clientKey)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("all-exhausted result = %d, want 503", rec.Code)
	}
}
