package openai

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
	p := New("https://api.openai.com/", store(t, "sk-key"), doer)

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
	p := New("https://api.openai.com", store(t, "k"), doer)
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
	p := New("https://api.openai.com", store(t, "k1", "k2"), doer)
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
	p := New("https://api.openai.com", store(t, "k"), doer)
	if _, err := p.Chat(context.Background(), []byte(`{}`), false, nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestName(t *testing.T) {
	if New("x", store(t, "k"), mocks.NewHTTPDoer(t)).Name() != "openai" {
		t.Error("name")
	}
}
