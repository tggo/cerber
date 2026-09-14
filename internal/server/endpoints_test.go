package server

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tggo/cerber/internal/config"
	"github.com/tggo/cerber/internal/credential"
	provmocks "github.com/tggo/cerber/internal/provider/mocks"
	"github.com/tggo/cerber/internal/provider/openai"

	"github.com/stretchr/testify/mock"
)

// openaiProvider builds a real OpenAI provider backed by a mock HTTPDoer that
// returns the given response for any request, capturing the last request URL.
func openaiProvider(t *testing.T, status int, respBody string, urlOut *string) *openai.Provider {
	t.Helper()
	doer := provmocks.NewHTTPDoer(t)
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		if urlOut != nil {
			*urlOut = r.URL.String()
		}
		h := http.Header{"Content-Type": {"application/json"}}
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(respBody))}, nil
	}).Maybe()
	st, err := credential.NewStore([]config.Credential{{Type: config.CredentialAPIKey, Name: "a", Key: "sk"}})
	if err != nil {
		t.Fatal(err)
	}
	return openai.New("openai", "https://api.openai.com", st, doer)
}

// perplexityProvider is openaiProvider named "perplexity" on api.perplexity.ai,
// capturing the last upstream URL and request body.
func perplexityProvider(t *testing.T, status int, respBody string, urlOut, bodyOut *string) *openai.Provider {
	t.Helper()
	doer := provmocks.NewHTTPDoer(t)
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		*urlOut = r.URL.String()
		b, _ := io.ReadAll(r.Body)
		*bodyOut = string(b)
		h := http.Header{"Content-Type": {"application/json"}}
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(respBody))}, nil
	}).Maybe()
	st, err := credential.NewStore([]config.Credential{{Type: config.CredentialAPIKey, Name: "px", Key: "pplx"}})
	if err != nil {
		t.Fatal(err)
	}
	return openai.New("perplexity", "https://api.perplexity.ai", st, doer, openai.WithChatPath("/chat/completions"))
}

func TestSearch_ForwardsToPerplexity(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	var gotURL, gotBody string
	s.RegisterChatter(perplexityProvider(t, 200, `{"results":[{"title":"Go","url":"https://go.dev"}]}`, &gotURL, &gotBody))
	in := `{"query":"go 1.26","max_results":3}`
	rec := do(t, s.Handler(), "POST", "/v1/search", in, clientKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/search = %d: %s", rec.Code, rec.Body.String())
	}
	if gotURL != "https://api.perplexity.ai/search" {
		t.Errorf("upstream url = %q", gotURL)
	}
	if gotBody != in {
		t.Errorf("upstream body = %q, want passthrough", gotBody)
	}
	if !strings.Contains(rec.Body.String(), "https://go.dev") {
		t.Errorf("body = %q", rec.Body.String())
	}
	var searches int64
	for _, e := range s.Usage().Snapshot().ByModel {
		if e.Name == "perplexity-search" {
			searches = e.Requests
		}
	}
	if searches != 1 {
		t.Errorf("perplexity-search requests = %d, want 1", searches)
	}
}

func TestSearch_Errors(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		configured bool
		upstream   int
		want       int
	}{
		{"not configured", `{"query":"x"}`, false, 0, http.StatusNotImplemented},
		{"invalid json", `{`, true, 0, http.StatusBadRequest},
		{"missing query", `{"max_results":3}`, true, 0, http.StatusBadRequest},
		{"empty query", `{"query":""}`, true, 0, http.StatusBadRequest},
		{"upstream 400 relayed", `{"query":"x"}`, true, 400, http.StatusBadRequest},
		{"upstream 401 exhausts keys", `{"query":"x"}`, true, 401, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newServer(t, newStore(t, 1))
			if tc.configured {
				var u, b string
				s.RegisterChatter(perplexityProvider(t, tc.upstream, `{"error":"nope"}`, &u, &b))
			}
			if rec := do(t, s.Handler(), "POST", "/v1/search", tc.body, clientKey); rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestSearch_RequiresAuth(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	if rec := do(t, s.Handler(), "POST", "/v1/search", `{"query":"x"}`, "wrong-key"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated search = %d, want 401", rec.Code)
	}
}

func TestSonarChat_RoutesToPerplexityChatPath(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	var gotURL, gotBody string
	s.RegisterChatter(perplexityProvider(t, 200, `{"object":"chat.completion","citations":["https://go.dev"],"usage":{"prompt_tokens":3,"completion_tokens":4}}`, &gotURL, &gotBody))
	rec := do(t, s.Handler(), "POST", "/v1/chat/completions", `{"model":"sonar","messages":[{"role":"user","content":"hi"}]}`, clientKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("sonar chat = %d: %s", rec.Code, rec.Body.String())
	}
	if gotURL != "https://api.perplexity.ai/chat/completions" {
		t.Errorf("upstream url = %q", gotURL)
	}
	if !strings.Contains(rec.Body.String(), "citations") {
		t.Errorf("citations not relayed: %s", rec.Body.String())
	}
}

func TestForwardEndpoints_RouteToProvider(t *testing.T) {
	for _, tc := range []struct {
		path    string
		model   string
		prefix  string
		wantURL string
	}{
		{"/v1/embeddings", "text-embedding-3-small", "text-embedding", "https://api.openai.com/v1/embeddings"},
		{"/v1/completions", "gpt-3.5-turbo-instruct", "gpt", "https://api.openai.com/v1/completions"},
		{"/v1/responses", "gpt-4o", "gpt", "https://api.openai.com/v1/responses"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			s, _ := newServer(t, newStore(t, 1))
			var gotURL string
			s.RegisterChatter(openaiProvider(t, 200, `{"object":"ok"}`, &gotURL))
			s.SetRoutes([]config.Route{{Prefix: tc.prefix, Provider: "openai"}})

			rec := do(t, s.Handler(), "POST", tc.path, `{"model":"`+tc.model+`","input":"hi"}`, clientKey)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d, want 200", tc.path, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "ok") {
				t.Errorf("body = %q", rec.Body.String())
			}
			if gotURL != tc.wantURL {
				t.Errorf("upstream url = %q, want %q", gotURL, tc.wantURL)
			}
		})
	}
}

func TestForwardEndpoints_AnthropicModelRejected(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	rec := do(t, s.Handler(), "POST", "/v1/embeddings", `{"model":"claude-3"}`, clientKey)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("anthropic model on /v1/embeddings = %d, want 400", rec.Code)
	}
}

func TestForwardEndpoints_UnknownModelRejected(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	rec := do(t, s.Handler(), "POST", "/v1/completions", `{"model":"totally-unknown-xyz"}`, clientKey)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown model = %d, want 400", rec.Code)
	}
}

func TestForwardEndpoints_ProviderWithoutForward(t *testing.T) {
	// A plain Chatter (no Forwarder capability) routed to → 501.
	s, _ := newServer(t, newStore(t, 1))
	c := provmocks.NewChatter(t)
	c.EXPECT().Name().Return("openai")
	s.RegisterChatter(c)
	s.SetRoutes([]config.Route{{Prefix: "emb", Provider: "openai"}})
	rec := do(t, s.Handler(), "POST", "/v1/embeddings", `{"model":"emb-1"}`, clientKey)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("non-forwarder = %d, want 501", rec.Code)
	}
}

func TestForwardEndpoints_UpstreamErrorRelayed(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	s.RegisterChatter(openaiProvider(t, 400, `{"error":"bad input"}`, nil))
	s.SetRoutes([]config.Route{{Prefix: "text-embedding", Provider: "openai"}})
	rec := do(t, s.Handler(), "POST", "/v1/embeddings", `{"model":"text-embedding-3-small"}`, clientKey)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("upstream 400 relayed = %d, want 400", rec.Code)
	}
}
