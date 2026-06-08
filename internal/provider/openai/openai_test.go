package openai

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
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{"Content-Type": {"application/json"}}}
}

func TestChat_Passthrough(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var captured *http.Request
	var sentBody []byte
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		captured = r
		sentBody, _ = io.ReadAll(r.Body)
		return resp(200, `{"object":"chat.completion"}`), nil
	})
	p := New("openai", "https://api.openai.com/", store(t, "sk-key"), doer)

	in := []byte(`{"model":"gpt-4o","messages":[]}`)
	out, err := p.Chat(context.Background(), in, false, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer out.Body.Close()

	if captured.URL.String() != "https://api.openai.com/v1/chat/completions" {
		t.Errorf("url = %s", captured.URL)
	}
	if got := captured.Header.Get("Authorization"); got != "Bearer sk-key" {
		t.Errorf("auth = %q", got)
	}
	if string(sentBody) != string(in) {
		t.Errorf("body forwarded = %q", sentBody)
	}
	if out.Status != 200 || out.Credential != "a" {
		t.Errorf("response = %+v", out)
	}
	b, _ := io.ReadAll(out.Body)
	if !strings.Contains(string(b), "chat.completion") {
		t.Errorf("passthrough body = %s", b)
	}
}

func TestChat_StreamAccept(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var captured *http.Request
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		captured = r
		return resp(200, "data: x"), nil
	})
	p := New("openai", "https://api.openai.com", store(t, "k"), doer)
	out, err := p.Chat(context.Background(), []byte(`{"stream":true}`), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	out.Body.Close()
	if got := captured.Header.Get("Accept"); got != "text/event-stream" {
		t.Errorf("accept = %q", got)
	}
}

func TestChat_RotatesOn429(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	n := 0
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		n++
		if n == 1 {
			return resp(429, `{"error":"rate"}`), nil
		}
		return resp(200, `{"ok":true}`), nil
	})
	p := New("openai", "https://api.openai.com", store(t, "k1", "k2"), doer)
	out, err := p.Chat(context.Background(), []byte(`{}`), false, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	out.Body.Close()
	if out.Credential != "b" || n != 2 {
		t.Errorf("expected rotation to b after 429 (n=%d cred=%s)", n, out.Credential)
	}
}

func TestChat_TransportError(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	doer.EXPECT().Do(mock.Anything).Return(nil, errors.New("dial"))
	p := New("openai", "https://api.openai.com", store(t, "k"), doer)
	if _, err := p.Chat(context.Background(), []byte(`{}`), false, nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestName(t *testing.T) {
	if New("openai", "x", store(t, "k"), mocks.NewHTTPDoer(t)).Name() != "openai" {
		t.Error("name")
	}
}
func TestProbeCredential_Models(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var gotURL, gotAuth string
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		gotAuth = r.Header.Get("Authorization")
		return resp(200, `{"object":"list","data":[{"id":"llama3.1:8b"},{"id":"qwen3:9b"},{"id":""}]}`), nil
	})
	p := New("ollama", "http://gpu0:11434", store(t, "sk-k"), doer)
	cred, _ := p.store.Next()
	models, err := p.ProbeCredential(context.Background(), cred)
	if err != nil {
		t.Fatalf("ProbeCredential: %v", err)
	}
	if gotURL != "http://gpu0:11434/v1/models" {
		t.Errorf("url = %s", gotURL)
	}
	if gotAuth != "Bearer sk-k" {
		t.Errorf("auth = %q", gotAuth)
	}
	if len(models) != 2 || models[0] != "llama3.1:8b" || models[1] != "qwen3:9b" {
		t.Errorf("models = %v (empty ids dropped)", models)
	}
}

func TestProbeCredential_InvalidAndError(t *testing.T) {
	// 401 -> ErrInvalidCredential
	d1 := mocks.NewHTTPDoer(t)
	d1.EXPECT().Do(mock.Anything).Return(resp(401, `{}`), nil)
	p1 := New("openai", "https://api.openai.com", store(t, "bad"), d1)
	c1, _ := p1.store.Next()
	if _, err := p1.ProbeCredential(context.Background(), c1); !errors.Is(err, provider.ErrInvalidCredential) {
		t.Errorf("401 err = %v, want ErrInvalidCredential", err)
	}
	// transport error -> plain error (not ErrInvalidCredential)
	d2 := mocks.NewHTTPDoer(t)
	d2.EXPECT().Do(mock.Anything).Return(nil, errors.New("dial fail"))
	p2 := New("openai", "https://api.openai.com", store(t, "k"), d2)
	c2, _ := p2.store.Next()
	if _, err := p2.ProbeCredential(context.Background(), c2); err == nil || errors.Is(err, provider.ErrInvalidCredential) {
		t.Errorf("transport err = %v, want plain error", err)
	}
}

func TestImages_Passthrough(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var gotURL, gotAuth string
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		gotAuth = r.Header.Get("Authorization")
		return resp(200, `{"data":[{"url":"https://img/x.jpg","mime_type":"image/jpeg"}]}`), nil
	})
	p := New("grok", "https://api.x.ai", store(t, "xai-key"), doer)
	out, err := p.Images(context.Background(), []byte(`{"model":"grok-imagine-image","prompt":"cat"}`), nil)
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	defer out.Body.Close()
	if gotURL != "https://api.x.ai/v1/images/generations" {
		t.Errorf("url = %s", gotURL)
	}
	if gotAuth != "Bearer xai-key" {
		t.Errorf("auth = %q", gotAuth)
	}
	b, _ := io.ReadAll(out.Body)
	if out.Status != 200 || !strings.Contains(string(b), "https://img/x.jpg") {
		t.Errorf("relay = %d %s", out.Status, b)
	}
}
