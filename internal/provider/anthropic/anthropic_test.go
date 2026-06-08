package anthropic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tggo/cerber/internal/config"
	"github.com/tggo/cerber/internal/credential"
	"github.com/tggo/cerber/internal/provider"
	"github.com/tggo/cerber/internal/provider/mocks"

	"github.com/stretchr/testify/mock"
)

func mustStore(t *testing.T, cfgs ...config.Credential) *credential.Store {
	t.Helper()
	s, err := credential.NewStore(cfgs)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func okResp() *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}
}

func TestSend_APIKeyHeaders(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var captured *http.Request
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		captured = r
		return okResp(), nil
	})

	store := mustStore(t, config.Credential{Type: config.CredentialAPIKey, Key: "sk-ant-123"})
	cred, _ := store.Next()
	c := New("https://api.anthropic.com/", "2023-06-01", doer)

	resp, err := c.Send(context.Background(), []byte(`{"model":"claude"}`), false, cred, http.Header{})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	resp.Body.Close()

	if captured.URL.String() != "https://api.anthropic.com/v1/messages" {
		t.Errorf("url = %s", captured.URL)
	}
	if captured.Method != http.MethodPost {
		t.Errorf("method = %s", captured.Method)
	}
	if got := captured.Header.Get("x-api-key"); got != "sk-ant-123" {
		t.Errorf("x-api-key = %q", got)
	}
	if got := captured.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("version = %q", got)
	}
	if got := captured.Header.Get("Accept"); got != "application/json" {
		t.Errorf("accept = %q, want application/json (non-stream)", got)
	}
	if captured.Header.Get("Authorization") != "" {
		t.Error("api_key cred must not set Authorization")
	}
	body, _ := io.ReadAll(captured.Body)
	if string(body) != `{"model":"claude"}` {
		t.Errorf("body = %q", body)
	}
}

func TestSend_OAuthHeadersAndStream(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var captured *http.Request
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		captured = r
		return okResp(), nil
	})

	store := mustStore(t, config.Credential{Type: config.CredentialOAuth, AccessToken: "tok-xyz"})
	cred, _ := store.Next()
	c := New("https://api.anthropic.com", "2023-06-01", doer)

	resp, err := c.Send(context.Background(), []byte(`{}`), true, cred, http.Header{})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	resp.Body.Close()

	if got := captured.Header.Get("Authorization"); got != "Bearer tok-xyz" {
		t.Errorf("authorization = %q", got)
	}
	if got := captured.Header.Get("anthropic-beta"); got != oauthBetas {
		t.Errorf("anthropic-beta = %q", got)
	}
	if captured.Header.Get("x-api-key") != "" {
		t.Error("oauth cred must not set x-api-key")
	}
	if got := captured.Header.Get("Accept"); got != "text/event-stream" {
		t.Errorf("accept = %q, want text/event-stream (stream)", got)
	}
	// OAuth requests must carry the Claude Code agent system prefix.
	body, _ := io.ReadAll(captured.Body)
	if !strings.Contains(string(body), claudeCodeAgentPrompt) {
		t.Errorf("oauth request missing claude code system prefix: %s", body)
	}
}

func TestSend_APIKeyDoesNotInjectSystem(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var captured *http.Request
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		captured = r
		return okResp(), nil
	})
	store := mustStore(t, config.Credential{Type: config.CredentialAPIKey, Key: "k"})
	cred, _ := store.Next()
	c := New("https://api.anthropic.com", "v", doer)
	resp, err := c.Send(context.Background(), []byte(`{"system":"hi"}`), false, cred, http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	body, _ := io.ReadAll(captured.Body)
	if strings.Contains(string(body), claudeCodeAgentPrompt) {
		t.Errorf("api_key request must not be modified: %s", body)
	}
}

func TestSend_OAuthInjectionErrorOnBadBody(t *testing.T) {
	store := mustStore(t, config.Credential{Type: config.CredentialOAuth, AccessToken: "t"})
	cred, _ := store.Next()
	c := New("https://api.anthropic.com", "v", mocks.NewHTTPDoer(t))
	if _, err := c.Send(context.Background(), []byte(`{bad json`), false, cred, http.Header{}); err == nil {
		t.Fatal("expected injection error for malformed oauth body")
	}
}

func TestSend_ForwardsClientBeta_APIKey(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var captured *http.Request
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		captured = r
		return okResp(), nil
	})
	store := mustStore(t, config.Credential{Type: config.CredentialAPIKey, Key: "k"})
	cred, _ := store.Next()
	c := New("https://api.anthropic.com", "v", doer)

	h := http.Header{}
	h.Set("anthropic-beta", "context-management-2025-06-27,prompt-caching-2024-07-31")
	resp, err := c.Send(context.Background(), []byte(`{}`), false, cred, h)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := captured.Header.Get("anthropic-beta"); got != "context-management-2025-06-27,prompt-caching-2024-07-31" {
		t.Errorf("client beta not forwarded: %q", got)
	}
}

func TestSend_MergesClientBeta_OAuth(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var captured *http.Request
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		captured = r
		return okResp(), nil
	})
	store := mustStore(t, config.Credential{Type: config.CredentialOAuth, AccessToken: "t"})
	cred, _ := store.Next()
	c := New("https://api.anthropic.com", "v", doer)

	h := http.Header{}
	h.Set("anthropic-beta", "context-management-2025-06-27,oauth-2025-04-20") // dup oauth beta
	resp, err := c.Send(context.Background(), []byte(`{}`), false, cred, h)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := captured.Header.Get("anthropic-beta")
	if got != "oauth-2025-04-20,context-management-2025-06-27" {
		t.Errorf("merged beta = %q", got)
	}
}

func TestSend_NilCredential(t *testing.T) {
	c := New("https://api.anthropic.com", "v", mocks.NewHTTPDoer(t))
	if _, err := c.Send(context.Background(), []byte(`{}`), false, nil, nil); err == nil {
		t.Fatal("expected error for nil credential")
	}
}

func TestSend_UpstreamError(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	doer.EXPECT().Do(mock.Anything).Return(nil, errors.New("dial fail"))
	store := mustStore(t, config.Credential{Type: config.CredentialAPIKey, Key: "k"})
	cred, _ := store.Next()
	c := New("https://api.anthropic.com", "v", doer)
	if _, err := c.Send(context.Background(), []byte(`{}`), false, cred, http.Header{}); err == nil {
		t.Fatal("expected error from upstream failure")
	}
}

func TestSend_BadURL(t *testing.T) {
	store := mustStore(t, config.Credential{Type: config.CredentialAPIKey, Key: "k"})
	cred, _ := store.Next()
	// Control character in URL makes http.NewRequest fail.
	c := New("http://\x7f", "v", mocks.NewHTTPDoer(t))
	if _, err := c.Send(context.Background(), []byte(`{}`), false, cred, http.Header{}); err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestProbeCredential_APIKeyModels(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var url, apiKey string
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		url = r.URL.String()
		apiKey = r.Header.Get("x-api-key")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(
			`{"data":[{"id":"claude-sonnet-4-6"},{"id":"claude-3-5-haiku"},{"id":""}]}`))}, nil
	})
	c := New("https://api.anthropic.com", "2023-06-01", doer)
	store := mustStore(t, config.Credential{Type: config.CredentialAPIKey, Key: "sk-ant"})
	cred, _ := store.Next()
	models, err := c.ProbeCredential(context.Background(), cred)
	if err != nil {
		t.Fatalf("ProbeCredential: %v", err)
	}
	if url != "https://api.anthropic.com/v1/models" || apiKey != "sk-ant" {
		t.Errorf("url=%s key=%s", url, apiKey)
	}
	if len(models) != 2 || models[0] != "claude-sonnet-4-6" {
		t.Errorf("models = %v", models)
	}
}

func TestProbeCredential_APIKeyInvalid(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	doer.EXPECT().Do(mock.Anything).Return(&http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{}`))}, nil)
	c := New("https://api.anthropic.com", "2023-06-01", doer)
	s := mustStore(t, config.Credential{Type: config.CredentialAPIKey, Key: "sk-bad"})
	cred, _ := s.Next()
	if _, err := c.ProbeCredential(context.Background(), cred); !errors.Is(err, provider.ErrInvalidCredential) {
		t.Errorf("401 err = %v, want ErrInvalidCredential", err)
	}
}

func TestProbeCredential_OAuthStateCheck(t *testing.T) {
	// OAuth is validated by state (no network call): a present token is healthy.
	doer := mocks.NewHTTPDoer(t) // no Do() expectation — must not be called
	c := New("https://api.anthropic.com", "2023-06-01", doer)
	s := mustStore(t, config.Credential{Type: config.CredentialOAuth, AccessToken: "tok"})
	cred, _ := s.Next()
	if models, err := c.ProbeCredential(context.Background(), cred); err != nil || len(models) != 0 {
		t.Errorf("oauth with token: models=%v err=%v (want healthy, no models)", models, err)
	}
}
