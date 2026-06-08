// Package gemini is cerber's client for Google's Generative Language (Gemini)
// API. It translates OpenAI chat-completions to/from Gemini generateContent and
// returns OpenAI-format responses. It only contacts the configured base URL
// (see AUDIT.md).
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
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

// BaseURL returns the configured upstream base URL (safe to display).
func (p *Provider) BaseURL() string { return p.baseURL }

// ProbeCredential validates a Gemini API key via GET /v1beta/models?key=… and
// returns the model IDs (with the "models/" prefix stripped). A 400/401/403
// yields provider.ErrInvalidCredential.
func (p *Provider) ProbeCredential(ctx context.Context, c *credential.Credential) ([]string, error) {
	if c == nil || c.APIKey() == "" {
		return nil, provider.ErrInvalidCredential
	}
	url := fmt.Sprintf("%s/v1beta/models?key=%s&pageSize=200", p.baseURL, c.APIKey())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("gemini: build models request: %w", err)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Gemini returns 400 INVALID_ARGUMENT for a malformed key and 403 for a
	// disabled/forbidden one; treat both as a bad credential.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest {
		return nil, provider.ErrInvalidCredential
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gemini: models probe status %d", resp.StatusCode)
	}
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gemini: decode models: %w", err)
	}
	models := make([]string, 0, len(out.Models))
	for _, m := range out.Models {
		if id := strings.TrimPrefix(m.Name, "models/"); id != "" {
			models = append(models, id)
		}
	}
	return models, nil
}

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
