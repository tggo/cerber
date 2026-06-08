package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tggo/cerber/internal/access"
	"github.com/tggo/cerber/internal/config"
	"github.com/tggo/cerber/internal/credential"
	"github.com/tggo/cerber/internal/provider"
	providermocks "github.com/tggo/cerber/internal/provider/mocks"
	"github.com/tggo/cerber/internal/server/mocks"

	"github.com/stretchr/testify/mock"
)

const clientKey = "client-key"

func newStore(t *testing.T, n int) *credential.Store {
	t.Helper()
	var cfgs []config.Credential
	for i := 0; i < n; i++ {
		cfgs = append(cfgs, config.Credential{Type: config.CredentialAPIKey, Key: "k"})
	}
	s, err := credential.NewStore(cfgs)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newServer(t *testing.T, store *credential.Store) (*Server, *mocks.Upstream) {
	up := mocks.NewUpstream(t)
	s := New(access.New([]string{clientKey}), store, up, nil, nil)
	return s, up
}

func oauthStore(t *testing.T, accessToken string, expiresAt time.Time) *credential.Store {
	t.Helper()
	s, err := credential.NewStore([]config.Credential{{
		Type: config.CredentialOAuth, Name: "o", AccessToken: accessToken,
		RefreshToken: "refresh-0", ExpiresAt: expiresAt,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func resp(status int, ct, body string) *http.Response {
	h := http.Header{}
	if ct != "" {
		h.Set("Content-Type", ct)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func do(t *testing.T, h http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestHealthz(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	rec := do(t, s.Handler(), "GET", "/healthz", "", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("healthz = %d %q", rec.Code, rec.Body.String())
	}
}

func TestAuth_Rejected(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	h := s.Handler()
	for _, path := range []string{"/v1/messages", "/v1/chat/completions"} {
		rec := do(t, h, "POST", path, `{}`, "wrong")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with bad key = %d, want 401", path, rec.Code)
		}
	}
}

func TestAllowLocalhost(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	s.SetAllowLocalhost(true)
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(resp(200, "application/json", `{"id":"ok"}`), nil)
	h := s.Handler()

	// loopback with NO key -> allowed
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`))
	r.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Errorf("loopback no-key = %d, want 200", rec.Code)
	}

	// non-loopback with NO key -> still 401
	r2 := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`))
	r2.RemoteAddr = "8.8.8.8:5555"
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, r2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("remote no-key = %d, want 401", rec2.Code)
	}
}

func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:80": true, "[::1]:80": true, "8.8.8.8:80": false,
		"192.0.2.1:1234": false, "garbage": false,
	}
	for in, want := range cases {
		if got := isLoopback(in); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNative_Passthrough(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	up.EXPECT().Send(mock.Anything, mock.Anything, false, mock.Anything, mock.Anything).
		Return(resp(200, "application/json", `{"id":"msg_1"}`), nil)
	rec := do(t, s.Handler(), "POST", "/v1/messages", `{"model":"claude","stream":false}`, clientKey)
	if rec.Code != 200 {
		t.Fatalf("code %d", rec.Code)
	}
	if rec.Body.String() != `{"id":"msg_1"}` {
		t.Errorf("body %q", rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("ct %q", rec.Header().Get("Content-Type"))
	}
}

func TestNative_StreamFlagDetected(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	var gotStream bool
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ []byte, stream bool, _ *credential.Credential, _ http.Header) (*http.Response, error) {
			gotStream = stream
			return resp(200, "text/event-stream", "event: x\n"), nil
		})
	do(t, s.Handler(), "POST", "/v1/messages", `{"stream":true}`, clientKey)
	if !gotStream {
		t.Error("stream flag not detected from body")
	}
}

func TestNative_ForwardsUpstreamHeaders(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.15")
	h.Set("Transfer-Encoding", "chunked") // hop-by-hop, must be dropped
	up.EXPECT().Send(mock.Anything, mock.Anything, false, mock.Anything, mock.Anything).
		Return(&http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(`{"id":"x"}`))}, nil)
	rec := do(t, s.Handler(), "POST", "/v1/messages", `{"model":"c"}`, clientKey)
	if got := rec.Header().Get("Anthropic-Ratelimit-Unified-5h-Utilization"); got != "0.15" {
		t.Errorf("ratelimit header not forwarded: %q", got)
	}
	if rec.Header().Get("Transfer-Encoding") != "" {
		t.Error("hop-by-hop Transfer-Encoding should be dropped")
	}
}

func TestNative_StreamRecordsUsage(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	stream := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1234,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":56}}\n\n"
	up.EXPECT().Send(mock.Anything, mock.Anything, true, mock.Anything, mock.Anything).
		Return(resp(200, "text/event-stream", stream), nil)
	h := s.Handler()
	rec := do(t, h, "POST", "/v1/messages", `{"model":"claude-x","stream":true}`, clientKey)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"text":"hi"`) {
		t.Fatalf("stream relay = %d %q", rec.Code, rec.Body.String())
	}
	stats := do(t, h, "GET", "/admin/stats", "", clientKey)
	if !strings.Contains(stats.Body.String(), `"input_tokens":1234`) || !strings.Contains(stats.Body.String(), `"output_tokens":56`) {
		t.Errorf("streaming usage not recorded: %s", stats.Body.String())
	}
}

func TestParseAnthropicStreamUsage(t *testing.T) {
	in, out := parseAnthropicStreamUsage([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`))
	if in != 10 {
		t.Errorf("input = %d", in)
	}
	// cache tokens are included in input (Claude Code caches its big tool/system prompt)
	inC, _ := parseAnthropicStreamUsage([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_creation_input_tokens":2000,"cache_read_input_tokens":48000}}}`))
	if inC != 50010 {
		t.Errorf("input with cache = %d, want 50010", inC)
	}
	_, out = parseAnthropicStreamUsage([]byte(`{"type":"message_delta","usage":{"output_tokens":42}}`))
	if out != 42 {
		t.Errorf("output = %d", out)
	}
	if i, o := parseAnthropicStreamUsage([]byte(`not json`)); i != 0 || o != 0 {
		t.Errorf("bad json = %d %d", i, o)
	}
}

func TestOpenAI_NonStreamTranslated(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	anthropicResp := `{"id":"msg_9","model":"claude","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`
	up.EXPECT().Send(mock.Anything, mock.Anything, false, mock.Anything, mock.Anything).
		Return(resp(200, "application/json", anthropicResp), nil)
	rec := do(t, s.Handler(), "POST", "/v1/chat/completions",
		`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`, clientKey)
	if rec.Code != 200 {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	b := rec.Body.String()
	if !strings.Contains(b, `"object":"chat.completion"`) || !strings.Contains(b, `"content":"hi"`) {
		t.Errorf("not translated: %s", b)
	}
}

func TestOpenAI_BadRequest(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	rec := do(t, s.Handler(), "POST", "/v1/chat/completions", `{"messages":[]}`, clientKey)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code %d, want 400", rec.Code)
	}
}

func TestOpenAI_StreamTranslated(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	stream := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"c\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"yo\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	up.EXPECT().Send(mock.Anything, mock.Anything, true, mock.Anything, mock.Anything).
		Return(resp(200, "text/event-stream", stream), nil)
	rec := do(t, s.Handler(), "POST", "/v1/chat/completions",
		`{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`, clientKey)
	if rec.Code != 200 {
		t.Fatalf("code %d", rec.Code)
	}
	b := rec.Body.String()
	if !strings.Contains(b, `chat.completion.chunk`) || !strings.Contains(b, `"content":"yo"`) || !strings.Contains(b, "[DONE]") {
		t.Errorf("stream not translated: %s", b)
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Errorf("ct %q", rec.Header().Get("Content-Type"))
	}
}

func TestOpenAI_UpstreamErrorRelayed(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(resp(400, "application/json", `{"error":"bad model"}`), nil)
	rec := do(t, s.Handler(), "POST", "/v1/chat/completions",
		`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`, clientKey)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "bad model") {
		t.Errorf("relay = %d %q", rec.Code, rec.Body.String())
	}
}

func TestDispatch_RotatesOnRateLimit(t *testing.T) {
	s, up := newServer(t, newStore(t, 2))
	calls := 0
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ []byte, _ bool, _ *credential.Credential, _ http.Header) (*http.Response, error) {
			calls++
			if calls == 1 {
				return resp(429, "application/json", `{"error":"rate"}`), nil
			}
			return resp(200, "application/json", `{"id":"ok"}`), nil
		})
	rec := do(t, s.Handler(), "POST", "/v1/messages", `{}`, clientKey)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("expected rotation to succeed: %d %q", rec.Code, rec.Body.String())
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (rotated once)", calls)
	}
}

func TestDispatch_AllCredsFail_502(t *testing.T) {
	s, up := newServer(t, newStore(t, 2))
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(resp(401, "application/json", `{"error":"unauth"}`), nil)
	rec := do(t, s.Handler(), "POST", "/v1/messages", `{}`, clientKey)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code %d, want 502", rec.Code)
	}
}

func TestDispatch_TransportError_502(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errors.New("dial fail"))
	rec := do(t, s.Handler(), "POST", "/v1/messages", `{}`, clientKey)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code %d, want 502", rec.Code)
	}
}

func TestOpenAI_UpstreamUntranslatable_502(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	up.EXPECT().Send(mock.Anything, mock.Anything, false, mock.Anything, mock.Anything).
		Return(resp(200, "application/json", `{not valid json`), nil)
	rec := do(t, s.Handler(), "POST", "/v1/chat/completions",
		`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`, clientKey)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code %d, want 502", rec.Code)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestReadBody_Error_400(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Body = io.NopCloser(errReader{})
	r.Header.Set("Authorization", "Bearer "+clientKey)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code %d, want 400", rec.Code)
	}
}

type nonFlusher struct{ http.ResponseWriter }

func TestFlusher_NoopWhenUnsupported(t *testing.T) {
	// nonFlusher embeds ResponseWriter but does not implement http.Flusher.
	f := flusher(nonFlusher{httptest.NewRecorder()})
	f() // must not panic
}

func TestRefresh_BeforeSendWhenExpiring(t *testing.T) {
	now := time.Unix(1000, 0)
	store := oauthStore(t, "stale", now.Add(10*time.Second)) // within skew -> needs refresh
	up := mocks.NewUpstream(t)
	ref := mocks.NewRefresher(t)
	s := New(access.New([]string{clientKey}), store, up, ref, nil)
	s.now = func() time.Time { return now }

	ref.EXPECT().Refresh(mock.Anything, "refresh-0").
		Return(credential.OAuthTokens{AccessToken: "fresh", RefreshToken: "refresh-1", ExpiresAt: now.Add(time.Hour)}, nil)

	var sentToken string
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ []byte, _ bool, cred *credential.Credential, _ http.Header) (*http.Response, error) {
			sentToken = cred.AccessToken()
			return resp(200, "application/json", `{"id":"ok"}`), nil
		})

	rec := do(t, s.Handler(), "POST", "/v1/messages", `{}`, clientKey)
	if rec.Code != 200 {
		t.Fatalf("code %d", rec.Code)
	}
	if sentToken != "fresh" {
		t.Errorf("expected refreshed token to be used, got %q", sentToken)
	}
}

func TestRefresh_PersistsTokens(t *testing.T) {
	now := time.Unix(1000, 0)
	store := oauthStore(t, "stale", now.Add(time.Second))
	up := mocks.NewUpstream(t)
	ref := mocks.NewRefresher(t)
	s := New(access.New([]string{clientKey}), store, up, ref, nil)
	s.now = func() time.Time { return now }

	var persistedName string
	var persistedTok credential.OAuthTokens
	s.SetTokenPersister(func(name string, tok credential.OAuthTokens) {
		persistedName, persistedTok = name, tok
	})
	ref.EXPECT().Refresh(mock.Anything, mock.Anything).
		Return(credential.OAuthTokens{AccessToken: "fresh", RefreshToken: "r1", ExpiresAt: now.Add(time.Hour)}, nil)
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(resp(200, "application/json", `{"id":"ok"}`), nil)

	do(t, s.Handler(), "POST", "/v1/messages", `{}`, clientKey)
	if persistedName != "o" || persistedTok.AccessToken != "fresh" {
		t.Errorf("persist = %q %+v", persistedName, persistedTok)
	}
}

func TestRefresh_NotTriggeredWhenValid(t *testing.T) {
	now := time.Unix(1000, 0)
	store := oauthStore(t, "valid", now.Add(time.Hour)) // far from expiry
	up := mocks.NewUpstream(t)
	ref := mocks.NewRefresher(t) // expects no Refresh call
	s := New(access.New([]string{clientKey}), store, up, ref, nil)
	s.now = func() time.Time { return now }
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(resp(200, "application/json", `{"id":"ok"}`), nil)
	if rec := do(t, s.Handler(), "POST", "/v1/messages", `{}`, clientKey); rec.Code != 200 {
		t.Fatalf("code %d", rec.Code)
	}
}

func TestRefresh_FailureSidelinesCredential_502(t *testing.T) {
	now := time.Unix(1000, 0)
	store := oauthStore(t, "stale", now.Add(time.Second))
	up := mocks.NewUpstream(t) // Send must never be called
	ref := mocks.NewRefresher(t)
	s := New(access.New([]string{clientKey}), store, up, ref, nil)
	s.now = func() time.Time { return now }
	ref.EXPECT().Refresh(mock.Anything, mock.Anything).Return(credential.OAuthTokens{}, errors.New("refresh boom"))
	if rec := do(t, s.Handler(), "POST", "/v1/messages", `{}`, clientKey); rec.Code != http.StatusBadGateway {
		t.Errorf("code %d, want 502", rec.Code)
	}
}

func TestStats_RecordsAndServes(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	up.EXPECT().Send(mock.Anything, mock.Anything, false, mock.Anything, mock.Anything).
		Return(resp(200, "application/json", `{"id":"m","usage":{"input_tokens":7,"output_tokens":11}}`), nil)
	h := s.Handler()
	if rec := do(t, h, "POST", "/v1/messages", `{"model":"claude-x"}`, clientKey); rec.Code != 200 {
		t.Fatalf("native code %d", rec.Code)
	}
	rec := do(t, h, "GET", "/admin/stats", "", clientKey)
	if rec.Code != 200 {
		t.Fatalf("stats code %d", rec.Code)
	}
	var rep struct {
		Totals struct {
			Requests     int64 `json:"requests"`
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"totals"`
		ByModel []struct {
			Name string `json:"name"`
		} `json:"by_model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Totals.Requests != 1 || rep.Totals.InputTokens != 7 || rep.Totals.OutputTokens != 11 {
		t.Errorf("totals = %+v", rep.Totals)
	}
	if len(rep.ByModel) == 0 || rep.ByModel[0].Name != "claude-x" {
		t.Errorf("by_model = %+v", rep.ByModel)
	}
}

func TestCatchAll(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	// no upstream proxy -> unknown path 404
	if rec := do(t, s.Handler(), "GET", "/api/claude_code/settings", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("without proxy = %d, want 404", rec.Code)
	}

	// with upstream proxy -> forwarded
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "proxied:"+r.URL.Path)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	s.SetUpstreamProxy(target, nil, nil)
	rec := do(t, s.Handler(), "GET", "/api/claude_code/settings", "", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "proxied:/api/claude_code/settings") {
		t.Errorf("proxied = %d %q", rec.Code, rec.Body.String())
	}
}

func TestRoot(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	h := s.Handler()
	// GET / and HEAD / (clients like Claude Code probe the base URL) -> 200
	if rec := do(t, h, "GET", "/", "", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "cerber") {
		t.Errorf("GET / = %d %q", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, "HEAD", "/", "", ""); rec.Code != 200 {
		t.Errorf("HEAD / = %d", rec.Code)
	}
	// Unknown paths still 404 (root handler is exact-match only).
	if rec := do(t, h, "GET", "/nope", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", rec.Code)
	}
}

func TestMetricsAndDashboardServed(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	h := s.Handler()
	if rec := do(t, h, "GET", "/metrics", "", ""); rec.Code != 200 {
		t.Errorf("metrics = %d", rec.Code)
	}
	rec := do(t, h, "GET", "/dashboard", "", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "cerber") ||
		rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("dashboard = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestAccounts_ListEnableDisable(t *testing.T) {
	store, err := credential.NewStore([]config.Credential{
		{Type: config.CredentialAPIKey, Name: "acct-a", Key: "a"},
		{Type: config.CredentialOAuth, Name: "acct-b", AccessToken: "t"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := New(access.New([]string{clientKey}), store, mocks.NewUpstream(t), nil, nil)
	h := s.Handler()

	// list
	rec := do(t, h, "GET", "/admin/accounts", "", clientKey)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "acct-a") || !strings.Contains(rec.Body.String(), `"kind":"oauth"`) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	// disable
	if rec := do(t, h, "POST", "/admin/accounts/acct-a/disable", "", clientKey); rec.Code != 200 {
		t.Fatalf("disable = %d", rec.Code)
	}
	// reflected in list
	rec = do(t, h, "GET", "/admin/accounts", "", clientKey)
	if !strings.Contains(rec.Body.String(), `"name":"acct-a","kind":"api_key","enabled":false`) {
		t.Errorf("disable not reflected: %s", rec.Body.String())
	}
	// enable back
	if rec := do(t, h, "POST", "/admin/accounts/acct-a/enable", "", clientKey); rec.Code != 200 {
		t.Fatalf("enable = %d", rec.Code)
	}
	// unknown -> 404
	if rec := do(t, h, "POST", "/admin/accounts/nope/disable", "", clientKey); rec.Code != http.StatusNotFound {
		t.Errorf("unknown = %d, want 404", rec.Code)
	}
	// auth required
	if rec := do(t, h, "GET", "/admin/accounts", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no key = %d, want 401", rec.Code)
	}
}

func TestClientKeys_CRUD(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	ks, err := access.LoadStore(filepath.Join(t.TempDir(), "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetClientKeyStore(ks)
	h := s.Handler()

	// create
	rec := do(t, h, "POST", "/admin/keys", `{"name":"laptop"}`, clientKey)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var created struct{ Name, Key string }
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "laptop" || !strings.HasPrefix(created.Key, "cer_") {
		t.Fatalf("created = %+v", created)
	}

	// the new key now authenticates a real request
	if rec := do(t, h, "GET", "/admin/keys", "", created.Key); rec.Code != 200 {
		t.Errorf("new key should authenticate, got %d", rec.Code)
	}

	// list shows it, redacted (no secret)
	rec = do(t, h, "GET", "/admin/keys", "", clientKey)
	if !strings.Contains(rec.Body.String(), `"name":"laptop"`) || strings.Contains(rec.Body.String(), created.Key) {
		t.Errorf("list leaked or missing key: %s", rec.Body.String())
	}

	// duplicate name -> 409
	if rec := do(t, h, "POST", "/admin/keys", `{"name":"laptop"}`, clientKey); rec.Code != http.StatusConflict {
		t.Errorf("dup = %d, want 409", rec.Code)
	}
	// empty name -> 400
	if rec := do(t, h, "POST", "/admin/keys", `{"name":""}`, clientKey); rec.Code != http.StatusBadRequest {
		t.Errorf("empty name = %d, want 400", rec.Code)
	}

	// disable -> key stops authenticating
	if rec := do(t, h, "POST", "/admin/keys/laptop/disable", "", clientKey); rec.Code != 200 {
		t.Fatalf("disable = %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/admin/keys", "", created.Key); rec.Code != http.StatusUnauthorized {
		t.Errorf("disabled key should be 401, got %d", rec.Code)
	}
	// enable back
	if rec := do(t, h, "POST", "/admin/keys/laptop/enable", "", clientKey); rec.Code != 200 {
		t.Fatalf("enable = %d", rec.Code)
	}

	// delete (DELETE verb)
	if rec := do(t, h, "DELETE", "/admin/keys/laptop", "", clientKey); rec.Code != 200 {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/admin/keys", "", created.Key); rec.Code != http.StatusUnauthorized {
		t.Errorf("deleted key should be 401, got %d", rec.Code)
	}
	// unknown -> 404
	if rec := do(t, h, "POST", "/admin/keys/ghost/disable", "", clientKey); rec.Code != http.StatusNotFound {
		t.Errorf("unknown = %d, want 404", rec.Code)
	}
	// auth required
	if rec := do(t, h, "GET", "/admin/keys", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no key = %d, want 401", rec.Code)
	}
}

func TestClientKeys_NotConfigured(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1)) // no SetClientKeyStore
	h := s.Handler()
	if rec := do(t, h, "GET", "/admin/keys", "", clientKey); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("list w/o store = %d, want 503", rec.Code)
	}
	if rec := do(t, h, "POST", "/admin/keys", `{"name":"x"}`, clientKey); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("create w/o store = %d, want 503", rec.Code)
	}
}

func TestManagementKey(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	s.SetManagementKey("mgmt-secret")
	h := s.Handler()
	// client key is NOT enough for /admin when a management key is set
	if rec := do(t, h, "GET", "/admin/accounts", "", clientKey); rec.Code != http.StatusUnauthorized {
		t.Errorf("client key on admin = %d, want 401", rec.Code)
	}
	// management key (Bearer) works
	if rec := do(t, h, "GET", "/admin/accounts", "", "mgmt-secret"); rec.Code != 200 {
		t.Errorf("mgmt key = %d, want 200", rec.Code)
	}
	// also via X-Cerber-Management header
	r := httptest.NewRequest("GET", "/admin/accounts", nil)
	r.Header.Set("X-Cerber-Management", "mgmt-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Errorf("X-Cerber-Management = %d, want 200", rec.Code)
	}
}

func TestStats_RequiresAuth(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	if rec := do(t, s.Handler(), "GET", "/admin/stats", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("stats without key = %d, want 401", rec.Code)
	}
}

func TestStats_RecordsErrors(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(resp(400, "application/json", `{"error":"bad"}`), nil)
	h := s.Handler()
	do(t, h, "POST", "/v1/messages", `{"model":"m"}`, clientKey)
	rec := do(t, h, "GET", "/admin/stats", "", clientKey)
	var rep struct {
		Totals struct {
			Requests int64 `json:"requests"`
			Errors   int64 `json:"errors"`
		} `json:"totals"`
	}
	json.Unmarshal(rec.Body.Bytes(), &rep)
	if rep.Totals.Requests != 1 || rep.Totals.Errors != 1 {
		t.Errorf("error stat = %+v", rep.Totals)
	}
}

func TestRoute(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	s.SetRoutes([]config.Route{
		{Prefix: "custom-", Provider: "openai"},
		{Prefix: "llama", Provider: "ollama"},
	})
	cases := map[string]string{
		"claude-sonnet-4-6": "anthropic",
		"gpt-4o":            "openai",
		"o3-mini":           "openai",
		"chatgpt-x":         "openai",
		"gemini-2.5-flash":  "gemini",
		"grok-2":            "grok",
		"custom-model":      "openai", // config override
		"llama3.1":          "ollama", // config override (arbitrary model name)
		"claude-3-5-haiku":  "anthropic",
		"mystery":           "", // unknown -> no provider (rejected by caller)
	}
	for model, want := range cases {
		if got := s.route(model); got != want {
			t.Errorf("route(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestOpenAI_RoutedToChatter(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	ch := providermocks.NewChatter(t)
	ch.EXPECT().Name().Return("openai").Maybe()
	s.RegisterChatter(ch)

	ch.EXPECT().Chat(mock.Anything, mock.Anything, false, mock.Anything).Return(&provider.Response{
		Status:       200,
		Header:       http.Header{"Content-Type": {"application/json"}},
		Body:         io.NopCloser(strings.NewReader(`{"object":"chat.completion","usage":{"prompt_tokens":3,"completion_tokens":4}}`)),
		Credential:   "oai-1",
		InputTokens:  0,
		OutputTokens: 0,
	}, nil)

	h := s.Handler()
	rec := do(t, h, "POST", "/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, clientKey)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "chat.completion") {
		t.Fatalf("routed resp = %d %s", rec.Code, rec.Body.String())
	}
	// usage parsed from the OpenAI response
	stats := do(t, h, "GET", "/admin/stats", "", clientKey)
	if !strings.Contains(stats.Body.String(), `"input_tokens":3`) || !strings.Contains(stats.Body.String(), `"output_tokens":4`) {
		t.Errorf("openai usage not recorded: %s", stats.Body.String())
	}
}

func TestOpenAI_RouteUnconfigured_501(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1)) // no openai chatter registered
	rec := do(t, s.Handler(), "POST", "/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, clientKey)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("code %d, want 501", rec.Code)
	}
}

func TestChatter_ErrorRelayed(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	ch := providermocks.NewChatter(t)
	ch.EXPECT().Name().Return("openai").Maybe()
	s.RegisterChatter(ch)
	ch.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(&provider.Response{
		Status: 400, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"error":"bad"}`)), Credential: "oai-1",
	}, nil)
	rec := do(t, s.Handler(), "POST", "/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"x"}]}`, clientKey)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "bad") {
		t.Errorf("relay = %d %s", rec.Code, rec.Body.String())
	}
}

func TestChatter_BadRequest_400(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	ch := providermocks.NewChatter(t)
	ch.EXPECT().Name().Return("gemini").Maybe()
	s.RegisterChatter(ch)
	ch.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, &provider.BadRequestError{Err: errors.New("missing model")})
	rec := do(t, s.Handler(), "POST", "/v1/chat/completions", `{"model":"gemini-x","messages":[{"role":"user","content":"x"}]}`, clientKey)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code %d, want 400", rec.Code)
	}
}

func TestCredFilter_HeaderRoutesByKind(t *testing.T) {
	store, err := credential.NewStore([]config.Credential{
		{Type: config.CredentialAPIKey, Name: "key1", Key: "k"},
		{Type: config.CredentialOAuth, Name: "oauth1", AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}
	up := mocks.NewUpstream(t)
	s := New(access.New([]string{clientKey}), store, up, nil, nil) // nil refresher: no refresh
	var gotKind credential.Kind
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ []byte, _ bool, cred *credential.Credential, _ http.Header) (*http.Response, error) {
			gotKind = cred.Kind()
			return resp(200, "application/json", `{"id":"ok"}`), nil
		})
	h := s.Handler()

	send := func(credHeader string) credential.Kind {
		r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer "+clientKey)
		if credHeader != "" {
			r.Header.Set("X-Cerber-Cred", credHeader)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != 200 {
			t.Fatalf("code %d", rec.Code)
		}
		return gotKind
	}

	if k := send("oauth"); k != credential.KindOAuth {
		t.Errorf("X-Cerber-Cred: oauth -> %s, want oauth", k)
	}
	if k := send("key"); k != credential.KindAPIKey {
		t.Errorf("X-Cerber-Cred: key -> %s, want api_key", k)
	}
}

func TestCredFilter_ByName(t *testing.T) {
	store, err := credential.NewStore([]config.Credential{
		{Type: config.CredentialAPIKey, Name: "acct-a", Key: "a"},
		{Type: config.CredentialAPIKey, Name: "acct-b", Key: "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	up := mocks.NewUpstream(t)
	s := New(access.New([]string{clientKey}), store, up, nil, nil)
	var gotName string
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ []byte, _ bool, cred *credential.Credential, _ http.Header) (*http.Response, error) {
			gotName = cred.Name()
			return resp(200, "application/json", `{"id":"ok"}`), nil
		})
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+clientKey)
	r.Header.Set("X-Cerber-Cred", "acct-b")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	if rec.Code != 200 || gotName != "acct-b" {
		t.Errorf("name select = %d %q", rec.Code, gotName)
	}
}

func TestCredFilter_OAuthRequestedButNone_503(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1)) // api_key only
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+clientKey)
	r.Header.Set("X-Cerber-Cred", "oauth")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code %d, want 503 (no oauth cred)", rec.Code)
	}
}

func TestDispatch_ClientCancelNoCooldown(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	call := 0
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ []byte, _ bool, _ *credential.Credential, _ http.Header) (*http.Response, error) {
			call++
			if call == 1 {
				return nil, context.Canceled
			}
			return resp(200, "application/json", `{"id":"ok"}`), nil
		})
	h := s.Handler()

	// first request with an already-canceled context (client went away)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r1 := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`)).WithContext(ctx)
	r1.Header.Set("Authorization", "Bearer "+clientKey)
	h.ServeHTTP(httptest.NewRecorder(), r1)

	// the single credential must NOT have been cooled down -> next request works
	if rec := do(t, h, "POST", "/v1/messages", `{}`, clientKey); rec.Code != 200 {
		t.Errorf("credential cooled after client cancel: got %d", rec.Code)
	}
	if call != 2 {
		t.Errorf("calls = %d, want 2", call)
	}
}

func TestDispatch_NoneAvailable_503(t *testing.T) {
	store := newStore(t, 1)
	// Put the only credential into cooldown so Next() returns ErrNoneAvailable.
	c, _ := store.Next()
	store.Cooldown(c, time.Hour)
	s, _ := newServer(t, store)
	rec := do(t, s.Handler(), "POST", "/v1/messages", `{}`, clientKey)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code %d, want 503", rec.Code)
	}
}

// fakeOllama is a Chatter that also implements provider.Prober + BaseURLer, to
// drive ProbeAll (key validation + model discovery) without a live upstream.
type fakeOllama struct {
	models []string
}

func (f *fakeOllama) Name() string    { return "ollama" }
func (f *fakeOllama) BaseURL() string { return "http://gpu0:11434" }
func (f *fakeOllama) ProbeCredential(context.Context, *credential.Credential) ([]string, error) {
	return f.models, nil
}
func (f *fakeOllama) Chat(context.Context, []byte, bool, http.Header) (*provider.Response, error) {
	return nil, errors.New("not used")
}

// registerOllama wires a fake ollama provider (chatter + store) and runs a probe
// so its models are discovered.
func registerOllama(t *testing.T, s *Server, models ...string) {
	t.Helper()
	s.RegisterChatter(&fakeOllama{models: models})
	ostore, err := credential.NewStore([]config.Credential{{Type: config.CredentialAPIKey, Name: "ollama", Key: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	s.RegisterProviderStore("ollama", ostore)
	s.ProbeAll(context.Background())
}

func TestRoute_DiscoveredModel(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	registerOllama(t, s, "supergemma4-26b:latest", "hf.co/x/qwen:Q5")
	// no prefix matches these arbitrary names — discovery must catch them
	if got := s.route("supergemma4-26b:latest"); got != "ollama" {
		t.Errorf("route(supergemma4) = %q, want ollama", got)
	}
	if got := s.route("hf.co/x/qwen:Q5"); got != "ollama" {
		t.Errorf("route(hf.co/...) = %q, want ollama", got)
	}
	// an unknown model resolves to no provider (the caller rejects it)
	if got := s.route("mystery-model"); got != "" {
		t.Errorf("route(mystery) = %q, want empty", got)
	}
	// claude* is matched by prefix, not by a catch-all default
	if got := s.route("claude-opus-4-8"); got != "anthropic" {
		t.Errorf("route(claude) = %q, want anthropic", got)
	}
}

func TestOpenAI_UnknownModelRejected(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	h := s.Handler()
	rec := do(t, h, "POST", "/v1/chat/completions", `{"model":"totally-unknown","messages":[]}`, clientKey)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown model = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no provider configured") {
		t.Errorf("unexpected error body: %s", rec.Body.String())
	}
}

func TestProvidersView(t *testing.T) {
	s, _ := newServer(t, newStore(t, 2)) // anthropic store: 2 creds
	registerOllama(t, s, "llama3.1:8b")
	h := s.Handler()

	rec := do(t, h, "GET", "/admin/providers", "", clientKey)
	if rec.Code != 200 {
		t.Fatalf("providers = %d", rec.Code)
	}
	var d struct {
		Providers []struct {
			Name         string
			BaseURL      string `json:"base_url"`
			Credentials  int
			HealthyCreds int `json:"healthy_credentials"`
			Models       []string
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	var ollama, anth bool
	for _, p := range d.Providers {
		if p.Name == "ollama" {
			ollama = true
			if p.BaseURL == "" || len(p.Models) != 1 || p.Credentials != 1 || p.HealthyCreds != 1 {
				t.Errorf("ollama view = %+v", p)
			}
		}
		if p.Name == "anthropic" {
			anth = true
			if p.Credentials != 2 {
				t.Errorf("anthropic creds = %d, want 2", p.Credentials)
			}
		}
	}
	if !ollama || !anth {
		t.Errorf("providers must include ollama and anthropic: %+v", d.Providers)
	}
	// auth required
	if rec := do(t, h, "GET", "/admin/providers", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no key = %d, want 401", rec.Code)
	}
}

// fakeKeyProber marks credentials whose key is "bad" as invalid, others valid.
type fakeKeyProber struct{ name string }

func (f *fakeKeyProber) Name() string { return f.name }
func (f *fakeKeyProber) ProbeCredential(_ context.Context, c *credential.Credential) ([]string, error) {
	if c.APIKey() == "bad" {
		return nil, provider.ErrInvalidCredential
	}
	return []string{"m1"}, nil
}
func (f *fakeKeyProber) Chat(context.Context, []byte, bool, http.Header) (*provider.Response, error) {
	return nil, errors.New("not used")
}

func TestProbeAll_KeyHealth(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	s.RegisterChatter(&fakeKeyProber{name: "openai"})
	store, _ := credential.NewStore([]config.Credential{
		{Type: config.CredentialAPIKey, Name: "good-key", Key: "ok"},
		{Type: config.CredentialAPIKey, Name: "bad-key", Key: "bad"},
	})
	s.RegisterProviderStore("openai", store)
	s.ProbeAll(context.Background())

	rec := do(t, s.Handler(), "GET", "/admin/accounts", "", clientKey)
	body := rec.Body.String()
	if !strings.Contains(body, `"name":"good-key"`) || !strings.Contains(body, `"name":"bad-key"`) {
		t.Fatalf("accounts: %s", body)
	}
	var d struct {
		Accounts []struct {
			Name          string
			HealthChecked bool   `json:"health_checked"`
			Healthy       bool   `json:"healthy"`
			HealthError   string `json:"health_error"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	for _, a := range d.Accounts {
		switch a.Name {
		case "good-key":
			if !a.HealthChecked || !a.Healthy {
				t.Errorf("good-key should be healthy: %+v", a)
			}
		case "bad-key":
			if !a.HealthChecked || a.Healthy || a.HealthError == "" {
				t.Errorf("bad-key should be unhealthy with error: %+v", a)
			}
		}
	}
}

func TestAccounts_AcrossProviders(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	ostore, _ := credential.NewStore([]config.Credential{{Type: config.CredentialAPIKey, Name: "openai-1", Key: "x"}})
	s.RegisterProviderStore("openai", ostore)
	h := s.Handler()

	rec := do(t, h, "GET", "/admin/accounts", "", clientKey)
	if !strings.Contains(rec.Body.String(), `"provider":"openai"`) || !strings.Contains(rec.Body.String(), `"provider":"anthropic"`) {
		t.Fatalf("accounts must tag provider: %s", rec.Body.String())
	}
	// disable a credential that lives in the openai store (not anthropic)
	if rec := do(t, h, "POST", "/admin/accounts/openai-1/disable", "", clientKey); rec.Code != 200 {
		t.Fatalf("disable openai-1 = %d", rec.Code)
	}
	rec = do(t, h, "GET", "/admin/accounts", "", clientKey)
	if !strings.Contains(rec.Body.String(), `"name":"openai-1","kind":"api_key","enabled":false`) {
		t.Errorf("openai-1 disable not reflected: %s", rec.Body.String())
	}
}

func TestLLMDoc(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	registerOllama(t, s, "llama3.1:8b", "supergemma4-26b:latest")
	h := s.Handler()

	// public: readable with no key (so a browser/agent can discover usage)
	if rec := do(t, h, "GET", "/llm.md", "", ""); rec.Code != 200 {
		t.Errorf("no key = %d, want 200 (public)", rec.Code)
	}

	rec := do(t, h, "GET", "/llm.md", "", clientKey)
	if rec.Code != 200 {
		t.Fatalf("llm.md = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("content-type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"/v1/chat/completions", "/v1/messages", "Authorization: Bearer",
		"claude*", "llama3.1:8b", "supergemma4-26b:latest", // discovered models listed
	} {
		if !strings.Contains(body, want) {
			t.Errorf("llm.md missing %q\n%s", want, body)
		}
	}
	// the request host appears as the base URL
	if !strings.Contains(body, "example.com") {
		t.Errorf("base URL (host) not reflected: %s", body)
	}
}

func TestFavicon(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	h := s.Handler()
	for _, path := range []string{"/favicon.ico", "/favicon.svg"} {
		rec := do(t, h, "GET", path, "", "") // public, no key
		if rec.Code != 200 {
			t.Errorf("%s = %d, want 200", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml" {
			t.Errorf("%s content-type = %q", path, ct)
		}
		if !strings.Contains(rec.Body.String(), "<svg") {
			t.Errorf("%s body not svg", path)
		}
	}
}
