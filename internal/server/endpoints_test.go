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
