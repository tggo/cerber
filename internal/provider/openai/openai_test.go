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

func TestProbe_DiscoversModelsAndHealth(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var gotURL string
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		return resp(200, `{"object":"list","data":[{"id":"llama3.1:8b"},{"id":"supergemma4-26b:latest"},{"id":""}]}`), nil
	})
	p := New("ollama", "http://gpu0:11434", store(t, "x"), doer)

	// before probe: not alive, never checked, no models
	if alive, at, _ := p.Health(); alive || !at.IsZero() {
		t.Errorf("pre-probe health = %v %v", alive, at)
	}
	if len(p.Models()) != 0 {
		t.Error("pre-probe models should be empty")
	}

	if err := p.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if gotURL != "http://gpu0:11434/v1/models" {
		t.Errorf("probe url = %s", gotURL)
	}
	alive, at, errMsg := p.Health()
	if !alive || at.IsZero() || errMsg != "" {
		t.Errorf("post-probe health = %v %v %q", alive, at, errMsg)
	}
	got := p.Models()
	if len(got) != 2 || got[0] != "llama3.1:8b" || got[1] != "supergemma4-26b:latest" {
		t.Errorf("models = %v (empty ids must be dropped)", got)
	}
}

func TestProbe_ErrorsRecordUnhealthy(t *testing.T) {
	// transport error
	d1 := mocks.NewHTTPDoer(t)
	d1.EXPECT().Do(mock.Anything).Return(nil, errors.New("dial fail"))
	p1 := New("ollama", "http://x", store(t, "k"), d1)
	if err := p1.Probe(context.Background()); err == nil {
		t.Error("transport error should propagate")
	}
	if alive, at, msg := p1.Health(); alive || at.IsZero() || msg == "" {
		t.Errorf("after transport error: %v %v %q", alive, at, msg)
	}

	// non-200
	d2 := mocks.NewHTTPDoer(t)
	d2.EXPECT().Do(mock.Anything).Return(resp(503, ``), nil)
	p2 := New("ollama", "http://x", store(t, "k"), d2)
	if err := p2.Probe(context.Background()); err == nil {
		t.Error("non-200 should error")
	}
	if alive, _, msg := p2.Health(); alive || !strings.Contains(msg, "503") {
		t.Errorf("after 503: alive=%v msg=%q", alive, msg)
	}
}
