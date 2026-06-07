package server

import (
	"context"
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
	s := New(access.New([]string{clientKey}), store, up, nil)
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

func TestNative_Passthrough(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	up.EXPECT().Send(mock.Anything, mock.Anything, false, mock.Anything).
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
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ []byte, stream bool, _ *credential.Credential) (*http.Response, error) {
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
	up.EXPECT().Send(mock.Anything, mock.Anything, false, mock.Anything).
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
	up.EXPECT().Send(mock.Anything, mock.Anything, true, mock.Anything).
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
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
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
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ []byte, _ bool, _ *credential.Credential) (*http.Response, error) {
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
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(resp(401, "application/json", `{"error":"unauth"}`), nil)
	rec := do(t, s.Handler(), "POST", "/v1/messages", `{}`, clientKey)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code %d, want 502", rec.Code)
	}
}

func TestDispatch_TransportError_502(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errors.New("dial fail"))
	rec := do(t, s.Handler(), "POST", "/v1/messages", `{}`, clientKey)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code %d, want 502", rec.Code)
	}
}

func TestOpenAI_UpstreamUntranslatable_502(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	up.EXPECT().Send(mock.Anything, mock.Anything, false, mock.Anything).
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
	s := New(access.New([]string{clientKey}), store, up, ref)
	s.now = func() time.Time { return now }

	ref.EXPECT().Refresh(mock.Anything, "refresh-0").
		Return(credential.OAuthTokens{AccessToken: "fresh", RefreshToken: "refresh-1", ExpiresAt: now.Add(time.Hour)}, nil)

	var sentToken string
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ []byte, _ bool, cred *credential.Credential) (*http.Response, error) {
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

func TestRefresh_NotTriggeredWhenValid(t *testing.T) {
	now := time.Unix(1000, 0)
	store := oauthStore(t, "valid", now.Add(time.Hour)) // far from expiry
	up := mocks.NewUpstream(t)
	ref := mocks.NewRefresher(t) // expects no Refresh call
	s := New(access.New([]string{clientKey}), store, up, ref)
	s.now = func() time.Time { return now }
	up.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
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
	s := New(access.New([]string{clientKey}), store, up, ref)
	s.now = func() time.Time { return now }
	ref.EXPECT().Refresh(mock.Anything, mock.Anything).Return(credential.OAuthTokens{}, errors.New("refresh boom"))
	if rec := do(t, s.Handler(), "POST", "/v1/messages", `{}`, clientKey); rec.Code != http.StatusBadGateway {
		t.Errorf("code %d, want 502", rec.Code)
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
