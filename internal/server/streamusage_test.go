package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tggo/cerber/internal/config"
	"github.com/tggo/cerber/internal/credential"
	provmocks "github.com/tggo/cerber/internal/provider/mocks"
	"github.com/tggo/cerber/internal/provider/openai"
	"github.com/tggo/cerber/internal/usage"

	"github.com/stretchr/testify/mock"
)

// openAIStream is what OpenAI streams with include_usage on: "usage":null on
// every content chunk, then a usage-only chunk before [DONE].
const openAIStream = "data: {\"choices\":[{\"delta\":{\"content\":\"po\"}}],\"usage\":null}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"ng\"}}],\"usage\":null}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":13}}\n\n" +
	"data: [DONE]\n\n"

// scriptedOpenAI is an "openai" provider whose mock upstream answers each call
// with the next (status, body) pair and records every request body it was sent.
func scriptedOpenAI(t *testing.T, sent *[]string, replies ...[2]string) *openai.Provider {
	t.Helper()
	doer := provmocks.NewHTTPDoer(t)
	call := 0
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		*sent = append(*sent, string(b))
		rep := replies[min(call, len(replies)-1)]
		call++
		status := 200
		if rep[0] != "200" {
			status = 400
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(rep[1]))}, nil
	}).Maybe()
	st, err := credential.NewStore([]config.Credential{{Type: config.CredentialAPIKey, Name: "a", Key: "sk"}})
	if err != nil {
		t.Fatal(err)
	}
	return openai.New("openai", "https://api.openai.com", st, doer)
}

func includeUsage(t *testing.T, body string) (set, value bool) {
	t.Helper()
	var m struct {
		StreamOptions *struct {
			IncludeUsage *bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if m.StreamOptions == nil || m.StreamOptions.IncludeUsage == nil {
		return false, false
	}
	return true, *m.StreamOptions.IncludeUsage
}

func TestStreamUsage_InjectedStrippedAndCharged(t *testing.T) {
	for _, tc := range []struct {
		name, compat, body string
		wantInjected       bool
		wantBody           string
		wantIn, wantOut    int64
	}{
		{"client didn't ask: injected, usage chunk stripped", "", `{"model":"gpt-4o","stream":true,"messages":[]}`, true,
			strings.Replace(openAIStream, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":13}}\n\n", "", 1), 9, 13},
		{"client asked for usage: untouched, chunk kept", "", `{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true},"messages":[]}`, false, openAIStream, 9, 13},
		{"client declined usage: injected, chunk stripped", "force", `{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":false},"messages":[]}`, true,
			strings.Replace(openAIStream, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":13}}\n\n", "", 1), 9, 13},
		{"raw mode: never injected", "raw", `{"model":"gpt-4o","stream":true,"messages":[]}`, false, "data: {\"choices\":[{\"delta\":{}}]}\n\ndata: [DONE]\n\n", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newServer(t, newStore(t, 1))
			s.usage.SetPricing(map[string]usage.Price{"gpt-4o": {Input: 1e6, Output: 1e6}})
			var sent []string
			upstream := openAIStream
			if tc.compat == "raw" {
				upstream = tc.wantBody
			}
			s.RegisterChatter(scriptedOpenAI(t, &sent, [2]string{"200", upstream}))
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+clientKey)
			if tc.compat != "" {
				req.Header.Set("X-Cerber-Compat", tc.compat)
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			if len(sent) != 1 {
				t.Fatalf("upstream calls = %d, want 1", len(sent))
			}
			set, val := includeUsage(t, sent[0])
			if tc.wantInjected && !(set && val) {
				t.Errorf("include_usage not injected: %s", sent[0])
			}
			if !tc.wantInjected && sent[0] != tc.body {
				t.Errorf("body rewritten = %s, want untouched %s", sent[0], tc.body)
			}
			if rec.Body.String() != tc.wantBody {
				t.Errorf("relayed = %q\nwant      %q", rec.Body.String(), tc.wantBody)
			}
			evs, _ := s.usage.RecentRequests(usage.RequestFilter{}, 0, 1)
			if len(evs) != 1 || evs[0].InputTokens != tc.wantIn || evs[0].OutputTokens != tc.wantOut || !near(evs[0].Cost, float64(tc.wantIn+tc.wantOut)) {
				t.Errorf("recorded = %+v, want in=%d out=%d", evs, tc.wantIn, tc.wantOut)
			}
		})
	}
}

func TestStreamUsage_RejectedRetriesWithoutIt(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	var sent []string
	plain := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	s.RegisterChatter(scriptedOpenAI(t, &sent,
		[2]string{"400", `{"error":{"message":"Unrecognized request argument supplied: stream_options"}}`},
		[2]string{"200", plain}))
	body := `{"model":"gpt-4o","stream":true,"messages":[]}`
	rec := do(t, s.Handler(), "POST", "/v1/chat/completions", body, clientKey)
	if rec.Code != http.StatusOK || rec.Body.String() != plain {
		t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
	}
	if len(sent) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (injected, then retry)", len(sent))
	}
	if set, _ := includeUsage(t, sent[0]); !set {
		t.Errorf("first call lacks include_usage: %s", sent[0])
	}
	if sent[1] != body {
		t.Errorf("retry body = %s, want the client's own %s", sent[1], body)
	}
}

func TestStreamUsage_OtherRejectionNotRetried(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	var sent []string
	s.RegisterChatter(scriptedOpenAI(t, &sent, [2]string{"400", `{"error":{"message":"invalid messages"}}`}))
	rec := do(t, s.Handler(), "POST", "/v1/chat/completions", `{"model":"gpt-4o","stream":true,"messages":[]}`, clientKey)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid messages") {
		t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
	}
	if len(sent) != 1 {
		t.Errorf("upstream calls = %d, want 1", len(sent))
	}
}

func TestIsUsageOnlyChunk(t *testing.T) {
	for in, want := range map[string]bool{
		`{"choices":[],"usage":{"prompt_tokens":1}}`:             true,
		`{"choices":[],"usage":null}`:                            false,
		`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":1}}`: false, // Perplexity: content + usage
		`{"usage":{"prompt_tokens":1}}`:                          false, // no choices field
		`{"choices":[]}`:                                         false,
		`[DONE]`:                                                 false,
	} {
		if got := isUsageOnlyChunk([]byte(in)); got != want {
			t.Errorf("isUsageOnlyChunk(%s) = %v, want %v", in, got, want)
		}
	}
}
