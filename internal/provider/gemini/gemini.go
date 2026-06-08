// Package gemini is cerber's client for Google's Generative Language (Gemini)
// API. It translates OpenAI chat-completions to/from Gemini generateContent and
// returns OpenAI-format responses. It only contacts the configured base URL
// (see AUDIT.md).
package gemini

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tggo/cerber/internal/credential"
	"github.com/tggo/cerber/internal/provider"
	"github.com/tggo/cerber/internal/translator"
)

const defaultCooldown = 60 * time.Second

// Provider routes OpenAI-format requests to the Gemini upstream.
type Provider struct {
	baseURL  string
	store    *credential.Store
	http     provider.HTTPDoer
	tr       *translator.Translator
	cooldown time.Duration
}

// New builds a Provider. baseURL is the Gemini origin (e.g.
// https://generativelanguage.googleapis.com).
func New(baseURL string, store *credential.Store, doer provider.HTTPDoer) *Provider {
	return &Provider{
		baseURL:  strings.TrimRight(baseURL, "/"),
		store:    store,
		http:     doer,
		tr:       translator.New(),
		cooldown: defaultCooldown,
	}
}

// Name identifies this provider.
func (p *Provider) Name() string { return "gemini" }

// Chat translates the OpenAI request to Gemini, sends it (rotating credentials),
// and translates the response back to OpenAI format.
func (p *Provider) Chat(ctx context.Context, body []byte, stream bool, _ http.Header) (*provider.Response, error) {
	geminiBody, model, _, err := p.tr.OpenAIToGemini(body)
	if err != nil {
		return nil, &provider.BadRequestError{Err: err}
	}

	method := "generateContent"
	if stream {
		method = "streamGenerateContent"
	}
	url := fmt.Sprintf("%s/v1beta/models/%s:%s", p.baseURL, model, method)
	if stream {
		url += "?alt=sse"
	}

	resp, credName, err := provider.Rotate(ctx, p.store, p.cooldown, func(cred *credential.Credential) (*http.Response, error) {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(geminiBody))
		if rerr != nil {
			return nil, fmt.Errorf("gemini: build request: %w", rerr)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-goog-api-key", cred.APIKey())
		return p.http.Do(req)
	})
	if err != nil {
		return nil, err
	}

	// Upstream (Gemini) errors are relayed as-is.
	if resp.StatusCode != http.StatusOK {
		return &provider.Response{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body, Credential: credName}, nil
	}

	if stream {
		pr, pw := io.Pipe()
		go func() {
			terr := p.tr.StreamGeminiToOpenAI(pw, resp.Body, model, nil)
			_ = resp.Body.Close()
			_ = pw.CloseWithError(terr)
		}()
		h := http.Header{"Content-Type": {"text/event-stream"}}
		return &provider.Response{Status: http.StatusOK, Header: h, Body: pr, Credential: credName}, nil
	}

	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("gemini: read response: %w", err)
	}
	out, err := p.tr.GeminiToOpenAI(raw, model)
	if err != nil {
		return nil, fmt.Errorf("gemini: translate response: %w", err)
	}
	h := http.Header{"Content-Type": {"application/json"}}
	return &provider.Response{Status: http.StatusOK, Header: h, Body: io.NopCloser(bytes.NewReader(out)), Credential: credName}, nil
}
