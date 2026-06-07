package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cerber/internal/access"
	"cerber/internal/config"
	"cerber/internal/credential"
	"cerber/internal/provider"
	providermocks "cerber/internal/provider/mocks"
	"cerber/internal/server/mocks"

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
		`{"model":"c","stream":true,"messages":[{"role":"user","content":"hi"}]}`, clientKey)
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
		`{"model":"c","messages":[{"role":"user","content":"hi"}]}`, clientKey)
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
		`{"model":"c","messages":[{"role":"user","content":"hi"}]}`, clientKey)
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
	s.SetRoutes([]config.Route{{Prefix: "custom-", Provider: "openai"}})
	cases := map[string]string{
		"claude-sonnet-4-6": "anthropic",
		"gpt-4o":            "openai",
		"o3-mini":           "openai",
		"chatgpt-x":         "openai",
		"gemini-2.5-flash":  "gemini",
		"custom-model":      "openai", // config override
		"mystery":           "anthropic",
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
