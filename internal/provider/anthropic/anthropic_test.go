package anthropic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"cerber/internal/config"
	"cerber/internal/credential"
	"cerber/internal/provider/mocks"

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

	resp, err := c.Send(context.Background(), []byte(`{"model":"claude"}`), false, cred)
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

	resp, err := c.Send(context.Background(), []byte(`{}`), true, cred)
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
	resp, err := c.Send(context.Background(), []byte(`{"system":"hi"}`), false, cred)
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
	if _, err := c.Send(context.Background(), []byte(`{bad json`), false, cred); err == nil {
		t.Fatal("expected injection error for malformed oauth body")
	}
}

func TestSend_NilCredential(t *testing.T) {
	c := New("https://api.anthropic.com", "v", mocks.NewHTTPDoer(t))
	if _, err := c.Send(context.Background(), []byte(`{}`), false, nil); err == nil {
		t.Fatal("expected error for nil credential")
	}
}

func TestSend_UpstreamError(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	doer.EXPECT().Do(mock.Anything).Return(nil, errors.New("dial fail"))
	store := mustStore(t, config.Credential{Type: config.CredentialAPIKey, Key: "k"})
	cred, _ := store.Next()
	c := New("https://api.anthropic.com", "v", doer)
	if _, err := c.Send(context.Background(), []byte(`{}`), false, cred); err == nil {
		t.Fatal("expected error from upstream failure")
	}
}

func TestSend_BadURL(t *testing.T) {
	store := mustStore(t, config.Credential{Type: config.CredentialAPIKey, Key: "k"})
	cred, _ := store.Next()
	// Control character in URL makes http.NewRequest fail.
	c := New("http://\x7f", "v", mocks.NewHTTPDoer(t))
	if _, err := c.Send(context.Background(), []byte(`{}`), false, cred); err == nil {
		t.Fatal("expected error for invalid URL")
	}
}
