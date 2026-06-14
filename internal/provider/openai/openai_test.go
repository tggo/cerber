package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestForward_Passthrough(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var captured *http.Request
	var sentBody []byte
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		captured = r
		sentBody, _ = io.ReadAll(r.Body)
		return resp(200, `{"object":"list","data":[{"embedding":[0.1]}]}`), nil
	})
	p := New("openai", "https://api.openai.com/", store(t, "sk-key"), doer)

	in := []byte(`{"model":"text-embedding-3-small","input":"hi"}`)
	out, err := p.Forward(context.Background(), "/v1/embeddings", in, false, nil)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer out.Body.Close()
	if captured.URL.String() != "https://api.openai.com/v1/embeddings" {
		t.Errorf("url = %s", captured.URL)
	}
	if captured.Header.Get("Authorization") != "Bearer sk-key" {
		t.Errorf("auth = %q", captured.Header.Get("Authorization"))
	}
	if string(sentBody) != string(in) {
		t.Errorf("body forwarded = %q", sentBody)
	}
	if out.Status != 200 || out.Credential != "a" {
		t.Errorf("response = %+v", out)
	}
}

func TestForward_StreamAccept(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var captured *http.Request
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		captured = r
		return resp(200, "data: x"), nil
	})
	p := New("openai", "https://api.openai.com", store(t, "k"), doer)
	out, err := p.Forward(context.Background(), "/v1/responses", []byte(`{"stream":true}`), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	out.Body.Close()
	if got := captured.Header.Get("Accept"); got != "text/event-stream" {
		t.Errorf("accept = %q", got)
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

func oauthStore(t *testing.T, access, refresh string, exp time.Time) *credential.Store {
	t.Helper()
	s, err := credential.NewStore([]config.Credential{{
		Type: config.CredentialOAuth, Name: "sub", AccessToken: access, RefreshToken: refresh, ExpiresAt: exp,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestChat_OAuthBearerAndRefresh(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var auth string
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		auth = r.Header.Get("Authorization")
		return resp(200, `{"ok":true}`), nil
	})
	// token already expired -> refresh must run and the NEW access token is used
	p := New("grok", "https://api.x.ai", oauthStore(t, "old-at", "rt", time.Unix(1, 0)), doer)
	var refreshed bool
	p.SetOAuthRefresh(func(_ context.Context, rt string) (credential.OAuthTokens, error) {
		refreshed = (rt == "rt")
		return credential.OAuthTokens{AccessToken: "new-at", RefreshToken: "rt2", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}, nil)

	out, err := p.Chat(context.Background(), []byte(`{"model":"grok-4.3"}`), false, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	out.Body.Close()
	if !refreshed {
		t.Error("expected refresh to run for expired oauth token")
	}
	if auth != "Bearer new-at" {
		t.Errorf("auth = %q, want Bearer new-at", auth)
	}
}

func TestChat_OAuthNoRefreshWhenFresh(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var auth string
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		auth = r.Header.Get("Authorization")
		return resp(200, `{}`), nil
	})
	p := New("grok", "https://api.x.ai", oauthStore(t, "fresh-at", "rt", time.Now().Add(time.Hour)), doer)
	p.SetOAuthRefresh(func(context.Context, string) (credential.OAuthTokens, error) {
		t.Fatal("refresh must NOT run for a fresh token")
		return credential.OAuthTokens{}, nil
	}, nil)
	out, err := p.Chat(context.Background(), []byte(`{}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	out.Body.Close()
	if auth != "Bearer fresh-at" {
		t.Errorf("auth = %q", auth)
	}
}

func TestChat_PinsCredentialByHeader(t *testing.T) {
	doer := mocks.NewHTTPDoer(t)
	var auth string
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		auth = r.Header.Get("Authorization")
		return resp(200, `{}`), nil
	})
	// two keys; pin the second by name via X-Cerber-Cred
	p := New("grok", "https://api.x.ai", store(t, "k1", "k2"), doer)
	h := http.Header{}
	h.Set("X-Cerber-Cred", "b") // store names creds a,b,...
	out, err := p.Chat(context.Background(), []byte(`{}`), false, h)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	out.Body.Close()
	if auth != "Bearer k2" {
		t.Errorf("auth = %q, want Bearer k2 (pinned cred b)", auth)
	}
}

// doerFunc adapts a function to provider.HTTPDoer for concurrency tests.
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// TestConcurrency_QueuesUntilBodyClosed verifies WithConcurrency(1) serialises
// requests: a second request stays queued (its upstream Do is never invoked)
// until the first releases its slot by closing the response body.
func TestConcurrency_QueuesUntilBodyClosed(t *testing.T) {
	var calls int32
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return resp(200, `{"ok":1}`), nil
	})
	p := New("arliai", "https://api.arliai.com", store(t, "k"), doer, WithConcurrency(1))
	body := []byte(`{"model":"m","messages":[]}`)

	// A takes the only slot and holds it (body not yet closed).
	a, err := p.Chat(context.Background(), body, false, nil)
	if err != nil {
		t.Fatalf("A Chat: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("after A: Do calls = %d, want 1", got)
	}

	// B blocks in acquire because A holds the slot.
	done := make(chan struct{})
	go func() {
		b, err := p.Chat(context.Background(), body, false, nil)
		if err == nil {
			b.Body.Close()
		}
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("B proceeded while A held the slot")
	case <-time.After(50 * time.Millisecond):
		// expected: B still queued
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("B's Do ran while queued: calls = %d", got)
	}

	// Releasing A's slot lets B proceed.
	a.Body.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("B did not proceed after the slot freed")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("final Do calls = %d, want 2", got)
	}
}

// TestConcurrency_QueuedCtxCancel verifies a request waiting for a slot bails
// out when its context is canceled, without ever hitting the upstream.
func TestConcurrency_QueuedCtxCancel(t *testing.T) {
	var calls int32
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return resp(200, `{"ok":1}`), nil
	})
	p := New("arliai", "https://api.arliai.com", store(t, "k"), doer, WithConcurrency(1))
	body := []byte(`{"model":"m","messages":[]}`)

	a, err := p.Chat(context.Background(), body, false, nil)
	if err != nil {
		t.Fatalf("A Chat: %v", err)
	}
	defer a.Body.Close()

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := p.Chat(ctx, body, false, nil)
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond) // let B reach acquire
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("B err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("B did not return after ctx cancel")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("canceled B hit upstream: calls = %d", got)
	}
}

// TestConcurrency_Unlimited confirms the default (no option) keeps requests
// unserialised: two can be in flight with neither body closed.
func TestConcurrency_Unlimited(t *testing.T) {
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		return resp(200, `{"ok":1}`), nil
	})
	p := New("openai", "https://api.openai.com", store(t, "k"), doer)
	body := []byte(`{"model":"m","messages":[]}`)

	a, err := p.Chat(context.Background(), body, false, nil)
	if err != nil {
		t.Fatalf("A: %v", err)
	}
	b, err := p.Chat(context.Background(), body, false, nil) // must not block
	if err != nil {
		t.Fatalf("B: %v", err)
	}
	a.Body.Close()
	b.Body.Close()
}
