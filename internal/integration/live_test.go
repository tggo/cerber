//go:build integration

// Package integration holds live tests that hit the real Anthropic API through a
// full in-process cerber server. They are excluded from the normal unit-test run
// and the coverage gate (build tag `integration`); run them with `make integration`.
//
// They require a real Anthropic API key in PLAYGROUND_API_KEY (loaded from .env
// if present). Without it, the tests skip rather than fail.
package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tggo/cerber/internal/access"
	"github.com/tggo/cerber/internal/config"
	"github.com/tggo/cerber/internal/credential"
	"github.com/tggo/cerber/internal/provider/anthropic"
	"github.com/tggo/cerber/internal/provider/gemini"
	"github.com/tggo/cerber/internal/provider/openai"
	"github.com/tggo/cerber/internal/server"

	"go.uber.org/zap"
)

const (
	clientKey = "itest-client-key"
	// A small, cheap current model for the smoke calls.
	testModel = "claude-haiku-4-5-20251001"
	baseURL   = "https://api.anthropic.com"
)

func apiKey(t *testing.T) string {
	t.Helper()
	_ = config.LoadEnvFile("../../.env")
	k := os.Getenv("PLAYGROUND_API_KEY")
	if k == "" {
		t.Skip("PLAYGROUND_API_KEY not set; skipping live Anthropic integration test")
	}
	return k
}

// liveServer wires a real cerber server backed by the playground key and returns
// its base URL.
func liveServer(t *testing.T, key string) string {
	t.Helper()
	store, err := credential.NewStore([]config.Credential{
		{Type: config.CredentialAPIKey, Name: "playground", Key: key},
	})
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Timeout: 60 * time.Second}
	client := anthropic.New(baseURL, "2023-06-01", hc)
	refresher := anthropic.NewRefresher(baseURL, hc)
	srv := server.New(access.New([]string{clientKey}), store, client, refresher, zap.NewNop())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

func post(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+clientKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func openaiKey(t *testing.T) string {
	t.Helper()
	_ = config.LoadEnvFile("../../.env")
	k := os.Getenv("OPENAI_KEY")
	if k == "" {
		t.Skip("OPENAI_KEY not set; skipping live OpenAI integration test")
	}
	return k
}

// liveServerWithOpenAI wires a cerber server with both Anthropic (playground) and
// a real OpenAI provider, returning its base URL.
func liveServerWithOpenAI(t *testing.T, anthropicKey, oaiKey string) string {
	t.Helper()
	store, err := credential.NewStore([]config.Credential{{Type: config.CredentialAPIKey, Name: "playground", Key: anthropicKey}})
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Timeout: 60 * time.Second}
	srv := server.New(access.New([]string{clientKey}), store,
		anthropic.New(baseURL, "2023-06-01", hc), anthropic.NewRefresher(baseURL, hc), zap.NewNop())

	ostore, err := credential.NewStore([]config.Credential{{Type: config.CredentialAPIKey, Name: "oai", Key: oaiKey}})
	if err != nil {
		t.Fatal(err)
	}
	srv.RegisterChatter(openai.New("openai", "https://api.openai.com", ostore, &http.Client{Timeout: 60 * time.Second}))

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestLive_OpenAIRoute(t *testing.T) {
	url := liveServerWithOpenAI(t, apiKey(t), openaiKey(t)) + "/v1/chat/completions"
	body := `{"model":"gpt-4o-mini","max_tokens":8,"messages":[{"role":"user","content":"Reply with exactly the word: pong"}]}`
	status, raw := post(t, url, body)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	var r struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("parse: %v (%s)", err, raw)
	}
	if r.Object != "chat.completion" || len(r.Choices) == 0 || strings.TrimSpace(r.Choices[0].Message.Content) == "" {
		t.Fatalf("unexpected openai response: %s", raw)
	}
	t.Logf("openai route OK: %q", r.Choices[0].Message.Content)
}

func geminiKey(t *testing.T) string {
	t.Helper()
	_ = config.LoadEnvFile("../../.env")
	k := os.Getenv("GEMINI_KEY")
	if k == "" {
		t.Skip("GEMINI_KEY not set; skipping live Gemini integration test")
	}
	return k
}

func TestLive_GeminiRoute(t *testing.T) {
	store, err := credential.NewStore([]config.Credential{{Type: config.CredentialAPIKey, Name: "playground", Key: apiKey(t)}})
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Timeout: 60 * time.Second}
	srv := server.New(access.New([]string{clientKey}), store,
		anthropic.New(baseURL, "2023-06-01", hc), anthropic.NewRefresher(baseURL, hc), zap.NewNop())
	gstore, err := credential.NewStore([]config.Credential{{Type: config.CredentialAPIKey, Name: "g", Key: geminiKey(t)}})
	if err != nil {
		t.Fatal(err)
	}
	srv.RegisterChatter(gemini.New("https://generativelanguage.googleapis.com", gstore, &http.Client{Timeout: 60 * time.Second}))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	body := `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"Reply with exactly the word: pong"}]}`
	status, raw := post(t, ts.URL+"/v1/chat/completions", body)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	var r struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("parse: %v (%s)", err, raw)
	}
	if r.Object != "chat.completion" || len(r.Choices) == 0 || strings.TrimSpace(r.Choices[0].Message.Content) == "" {
		t.Fatalf("unexpected gemini response: %s", raw)
	}
	t.Logf("gemini route OK: %q", r.Choices[0].Message.Content)
}

func grokKey(t *testing.T) string {
	t.Helper()
	_ = config.LoadEnvFile("../../.env")
	k := os.Getenv("GROK_API_KEY")
	if k == "" {
		t.Skip("GROK_API_KEY not set; skipping live Grok integration test")
	}
	return k
}

func TestLive_GrokRoute(t *testing.T) {
	store, err := credential.NewStore([]config.Credential{{Type: config.CredentialAPIKey, Name: "playground", Key: apiKey(t)}})
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Timeout: 60 * time.Second}
	srv := server.New(access.New([]string{clientKey}), store,
		anthropic.New(baseURL, "2023-06-01", hc), anthropic.NewRefresher(baseURL, hc), zap.NewNop())
	kstore, err := credential.NewStore([]config.Credential{{Type: config.CredentialAPIKey, Name: "grok", Key: grokKey(t)}})
	if err != nil {
		t.Fatal(err)
	}
	srv.RegisterChatter(openai.New("grok", "https://api.x.ai", kstore, &http.Client{Timeout: 60 * time.Second}))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	body := `{"model":"grok-4.3","messages":[{"role":"user","content":"Reply with exactly the word: pong"}]}`
	status, raw := post(t, ts.URL+"/v1/chat/completions", body)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	var r struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("parse: %v (%s)", err, raw)
	}
	if r.Object != "chat.completion" || len(r.Choices) == 0 || strings.TrimSpace(r.Choices[0].Message.Content) == "" {
		t.Fatalf("unexpected grok response: %s", raw)
	}
	t.Logf("grok route OK: %q", r.Choices[0].Message.Content)
}

func TestLive_NativeMessages(t *testing.T) {
	url := liveServer(t, apiKey(t)) + "/v1/messages"
	body := `{"model":"` + testModel + `","max_tokens":16,"messages":[{"role":"user","content":"Reply with exactly the word: pong"}]}`
	status, raw := post(t, url, body)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("parse: %v (%s)", err, raw)
	}
	if len(r.Content) == 0 || strings.TrimSpace(r.Content[0].Text) == "" {
		t.Fatalf("empty content: %s", raw)
	}
	t.Logf("native /v1/messages OK: stop=%s text=%q", r.StopReason, r.Content[0].Text)
}

func TestLive_OpenAIChatCompletions(t *testing.T) {
	url := liveServer(t, apiKey(t)) + "/v1/chat/completions"
	body := `{"model":"` + testModel + `","max_tokens":16,"messages":[{"role":"user","content":"Reply with exactly the word: pong"}]}`
	status, raw := post(t, url, body)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	var r struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("parse: %v (%s)", err, raw)
	}
	if r.Object != "chat.completion" {
		t.Fatalf("object = %q: %s", r.Object, raw)
	}
	if len(r.Choices) == 0 || strings.TrimSpace(r.Choices[0].Message.Content) == "" {
		t.Fatalf("empty choice: %s", raw)
	}
	t.Logf("openai /v1/chat/completions OK: finish=%s tokens=%d text=%q",
		r.Choices[0].FinishReason, r.Usage.TotalTokens, r.Choices[0].Message.Content)
}

func TestLive_StreamingChatCompletions(t *testing.T) {
	url := liveServer(t, apiKey(t)) + "/v1/chat/completions"
	body := `{"model":"` + testModel + `","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"count: 1 2 3"}]}`
	status, raw := post(t, url, body)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	s := string(raw)
	if !strings.Contains(s, "chat.completion.chunk") || !strings.Contains(s, "[DONE]") {
		t.Fatalf("not a valid translated stream: %s", s)
	}
	t.Logf("streaming OK: %d bytes, DONE present", len(raw))
}
