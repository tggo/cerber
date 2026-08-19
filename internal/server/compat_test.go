package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tggo/cerber/internal/compat"
	"github.com/tggo/cerber/internal/credential"
	"github.com/tggo/cerber/internal/provider"
	providermocks "github.com/tggo/cerber/internal/provider/mocks"

	"github.com/stretchr/testify/mock"
)

// captureChatter registers a chatter that records every body it is handed and
// answers with the queued responses in order.
func captureChatter(t *testing.T, s *Server, name string, responses ...*provider.Response) *[]string {
	t.Helper()
	c := providermocks.NewChatter(t)
	c.EXPECT().Name().Return(name).Maybe()
	var seen []string
	i := 0
	c.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, body []byte, _ bool, _ http.Header) (*provider.Response, error) {
			seen = append(seen, string(body))
			r := responses[min(i, len(responses)-1)]
			i++
			return r, nil
		}).Maybe()
	s.RegisterChatter(c)
	return &seen
}

func okResp() *provider.Response {
	return &provider.Response{
		Status: 200, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"object":"chat.completion"}`)), Credential: "c",
	}
}

func TestCompat_AutoRenamesTokenParam(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	seen := captureChatter(t, s, "openai", okResp())

	rec := do(t, s.Handler(), "POST", "/v1/chat/completions",
		`{"model":"gpt-5.2","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`, clientKey)
	if rec.Code != 200 {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	if len(*seen) != 1 {
		t.Fatalf("chatter calls = %d", len(*seen))
	}
	body := (*seen)[0]
	if !strings.Contains(body, `"max_completion_tokens":16`) || strings.Contains(body, `"max_tokens"`) {
		t.Errorf("upstream body = %s, want max_tokens renamed for a gpt-5 model", body)
	}
}

func TestCompat_AutoLeavesXAISpelling(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	seen := captureChatter(t, s, "grok", okResp())

	do(t, s.Handler(), "POST", "/v1/chat/completions",
		`{"model":"grok-4","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":16}`, clientKey)
	body := (*seen)[0]
	if !strings.Contains(body, `"max_tokens":16`) || strings.Contains(body, `"max_completion_tokens"`) {
		t.Errorf("upstream body = %s, want the xAI spelling", body)
	}
}

func TestCompat_RawForwardsBodyUntouched(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	seen := captureChatter(t, s, "openai", okResp())

	const sent = `{"model":"gpt-5.2","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`
	req := doHeaders(t, s.Handler(), "POST", "/v1/chat/completions", sent, clientKey,
		map[string]string{"X-Cerber-Compat": "raw"})
	if req.Code != 200 {
		t.Fatalf("code = %d: %s", req.Code, req.Body.String())
	}
	if !strings.Contains((*seen)[0], `"max_tokens":16`) {
		t.Errorf("upstream body = %s, want the caller's own spelling preserved", (*seen)[0])
	}
}

func TestCompat_RawRejectedForTranslatingTarget(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	rec := doHeaders(t, s.Handler(), "POST", "/v1/chat/completions",
		`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`, clientKey,
		map[string]string{"X-Cerber-Compat": "raw"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/v1/messages") {
		t.Errorf("error should point at the native endpoint: %s", rec.Body.String())
	}
}

func TestCompat_UnknownModeIsRejected(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	rec := doHeaders(t, s.Handler(), "POST", "/v1/chat/completions",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, clientKey,
		map[string]string{"X-Cerber-Compat": "passthrough"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestCompat_ForceDropsUnknownParameters(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	s.SetCompatMode("force")
	seen := captureChatter(t, s, "openai", okResp())

	do(t, s.Handler(), "POST", "/v1/chat/completions",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"tools":[],"tool_choice":"auto","made_up":1}`, clientKey)
	body := (*seen)[0]
	for _, gone := range []string{"made_up", "tools", "tool_choice"} {
		if strings.Contains(body, gone) {
			t.Errorf("force kept %q: %s", gone, body)
		}
	}
	if !strings.Contains(body, `"model":"gpt-4o"`) {
		t.Errorf("force dropped a real parameter: %s", body)
	}
}

func TestCompat_RetriesAndLearnsTokenParam(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	rejection := &provider.Response{
		Status: 400, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"error":{"message":"Unsupported parameter: 'max_tokens'. Use 'max_completion_tokens' instead."}}`)),
		Credential: "c",
	}
	seen := captureChatter(t, s, "openai", rejection, okResp())

	// gpt-4o is guessed as a max_tokens model, so the first attempt is "wrong"
	// and the upstream corrects us.
	rec := do(t, s.Handler(), "POST", "/v1/chat/completions",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`, clientKey)
	if rec.Code != 200 {
		t.Fatalf("code = %d, want the retry to succeed: %s", rec.Code, rec.Body.String())
	}
	if len(*seen) != 2 {
		t.Fatalf("chatter calls = %d, want 2 (attempt + retry)", len(*seen))
	}
	if !strings.Contains((*seen)[0], `"max_tokens":16`) {
		t.Errorf("first attempt = %s", (*seen)[0])
	}
	if !strings.Contains((*seen)[1], `"max_completion_tokens":16`) {
		t.Errorf("retry = %s, want the swapped spelling", (*seen)[1])
	}
	if got := s.norm.TokenParam("gpt-4o"); got != compat.MaxCompletionTokens {
		t.Errorf("learned = %q, want %q", got, compat.MaxCompletionTokens)
	}
}

func TestCompat_UnrelatedBadRequestIsRelayedIntact(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	bad := &provider.Response{
		Status: 400, Header: http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":"model not found"}`)),
		Credential: "c",
	}
	seen := captureChatter(t, s, "openai", bad)

	rec := do(t, s.Handler(), "POST", "/v1/chat/completions",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`, clientKey)
	if rec.Code != 400 {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "model not found") {
		t.Errorf("body = %s, want the upstream error replayed verbatim", rec.Body.String())
	}
	if len(*seen) != 1 {
		t.Errorf("chatter calls = %d, want no retry for an unrelated error", len(*seen))
	}
	if got := s.norm.TokenParam("gpt-4o"); got != compat.MaxTokens {
		t.Errorf("learned %q from an unrelated error", got)
	}
}

func TestCompat_FailedRetryRelaysTheOriginalError(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	mk := func() *provider.Response {
		return &provider.Response{
			Status: 400, Header: http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"max_tokens must be positive"}`)),
			Credential: "c",
		}
	}
	seen := captureChatter(t, s, "openai", mk(), mk())

	rec := do(t, s.Handler(), "POST", "/v1/chat/completions",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":-1}`, clientKey)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "must be positive") {
		t.Fatalf("resp = %d %s, want the original error", rec.Code, rec.Body.String())
	}
	if len(*seen) != 2 {
		t.Errorf("chatter calls = %d, want one retry attempt", len(*seen))
	}
	if got := s.norm.TokenParam("gpt-4o"); got != compat.MaxTokens {
		t.Errorf("learned %q from a retry that also failed", got)
	}
}

// TestCompat_ToolsReachAnthropic is the regression this whole path exists for:
// tools sent to the OpenAI endpoint used to be dropped on the way to Claude, so
// the model answered that it had no such functions.
func TestCompat_ToolsReachAnthropic(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, body []byte, _ bool, _ *credential.Credential, _ http.Header) (*http.Response, error) {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(body, &m); err != nil {
				t.Errorf("upstream body: %v", err)
			}
			if _, ok := m["tools"]; !ok {
				t.Errorf("tools missing from the Anthropic request: %s", body)
			}
			if !strings.Contains(string(m["tools"]), `"input_schema"`) {
				t.Errorf("tools = %s, want Anthropic's input_schema spelling", m["tools"])
			}
			return resp(200, "application/json",
				`{"id":"m","model":"claude-sonnet-5","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"get_weather","input":{"city":"Kyiv"}}],"usage":{"input_tokens":1,"output_tokens":1}}`), nil
		})

	rec := do(t, s.Handler(), "POST", "/v1/chat/completions",
		`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`,
		clientKey)
	if rec.Code != 200 {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"tool_calls"`) || !strings.Contains(rec.Body.String(), "get_weather") {
		t.Errorf("response = %s, want the tool call translated back", rec.Body.String())
	}
}

// doHeaders is do() with extra request headers.
func doHeaders(t *testing.T, h http.Handler, method, path, body, key string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}
