package gemini

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

func store(t *testing.T, keys ...string) *credential.Store {
	t.Helper()
	var cfgs []config.Credential
	for i, k := range keys {
		cfgs = append(cfgs, config.Credential{Type: config.CredentialAPIKey, Name: string(rune('a' + i)), Key: k})
	}
	s, err := credential.NewStore(cfgs)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func resp(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

const okGemini = `{"candidates":[{"content":{"parts":[{"text":"pong"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1}}`

func TestChat_NonStreamTranslated(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var captured *http.Request
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		captured = r
		return resp(200, okGemini), nil
	})
	p := New("https://generativelanguage.googleapis.com", store(t, "gk"), doer)

	out, err := p.Chat(context.Background(), []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`), false, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer out.Body.Close()

	if captured.URL.String() != "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Errorf("url = %s", captured.URL)
	}
	if got := captured.Header.Get("x-goog-api-key"); got != "gk" {
		t.Errorf("api key header = %q", got)
	}
	body, _ := io.ReadAll(out.Body)
	if !strings.Contains(string(body), `"object":"chat.completion"`) || !strings.Contains(string(body), `"content":"pong"`) {
		t.Errorf("translated body = %s", body)
	}
	if !strings.Contains(string(body), `"total_tokens":3`) {
		t.Errorf("usage missing: %s", body)
	}
}

func TestChat_StreamURLAndTranslate(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var captured *http.Request
	stream := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"yo\"}]},\"finishReason\":\"STOP\"}]}\n\n"
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		captured = r
		return resp(200, stream), nil
	})
	p := New("https://generativelanguage.googleapis.com", store(t, "gk"), doer)

	out, err := p.Chat(context.Background(), []byte(`{"model":"gemini-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	if !strings.Contains(captured.URL.String(), ":streamGenerateContent") || !strings.Contains(captured.URL.RawQuery, "alt=sse") {
		t.Errorf("stream url = %s", captured.URL)
	}
	body, _ := io.ReadAll(out.Body)
	if !strings.Contains(string(body), "chat.completion.chunk") || !strings.Contains(string(body), `"content":"yo"`) || !strings.Contains(string(body), "[DONE]") {
		t.Errorf("translated stream = %s", body)
	}
}

func TestChat_BadRequest(t *testing.T) {
	p := New("https://x", store(t, "gk"), mocks.NewHTTPDoer(t))
	_, err := p.Chat(context.Background(), []byte(`{"messages":[]}`), false, nil) // no model
	var bad *provider.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("expected BadRequestError, got %v", err)
	}
}

func TestChat_UpstreamErrorRelayed(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	doer.EXPECT().Do(mock.Anything).Return(resp(400, `{"error":{"message":"bad model"}}`), nil)
	p := New("https://x", store(t, "gk"), doer)
	out, err := p.Chat(context.Background(), []byte(`{"model":"g","messages":[{"role":"user","content":"x"}]}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	if out.Status != 400 {
		t.Errorf("status = %d", out.Status)
	}
}

func TestChat_TransportError(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	doer.EXPECT().Do(mock.Anything).Return(nil, errors.New("dial"))
	p := New("https://x", store(t, "gk"), doer)
	if _, err := p.Chat(context.Background(), []byte(`{"model":"g","messages":[{"role":"user","content":"x"}]}`), false, nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestName(t *testing.T) {
	if New("x", store(t, "k"), mocks.NewHTTPDoer(t)).Name() != "gemini" {
		t.Error("name")
	}
}

func TestProbeCredential(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var gotURL string
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		return resp(200, `{"models":[{"name":"models/gemini-2.5-flash"},{"name":"models/gemini-2.5-pro"}]}`), nil
	})
	p := New("https://generativelanguage.googleapis.com", store(t, "gk"), doer)
	cred, _ := p.store.Next()
	models, err := p.ProbeCredential(context.Background(), cred)
	if err != nil {
		t.Fatalf("ProbeCredential: %v", err)
	}
	if !strings.Contains(gotURL, "/v1beta/models?key=gk") {
		t.Errorf("url = %s", gotURL)
	}
	if len(models) != 2 || models[0] != "gemini-2.5-flash" || models[1] != "gemini-2.5-pro" {
		t.Errorf("models = %v (models/ prefix must be stripped)", models)
	}
}

func TestProbeCredential_BadKey(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	doer.EXPECT().Do(mock.Anything).Return(resp(400, `{"error":{"status":"INVALID_ARGUMENT"}}`), nil)
	p := New("https://generativelanguage.googleapis.com", store(t, "bad"), doer)
	cred, _ := p.store.Next()
	if _, err := p.ProbeCredential(context.Background(), cred); !errors.Is(err, provider.ErrInvalidCredential) {
		t.Errorf("400 err = %v, want ErrInvalidCredential", err)
	}
}
