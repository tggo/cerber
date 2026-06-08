// Package openai is cerber's client for OpenAI-compatible chat completions. The
// inbound dialect and the OpenAI upstream are the same format, so this provider
// is a credential-injecting passthrough — no translation. It only contacts the
// configured base URL (see AUDIT.md).
package openai

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tggo/cerber/internal/credential"
	"github.com/tggo/cerber/internal/provider"
)

// ChatPath is the OpenAI chat-completions endpoint.
const ChatPath = "/v1/chat/completions"

// defaultCooldown sidelines a credential after an auth/rate-limit failure.
const defaultCooldown = 60 * time.Second

// Provider routes OpenAI-format requests to an OpenAI-compatible upstream. The
// same implementation serves OpenAI and any OpenAI-compatible API (e.g. xAI/Grok)
// — only the name and base URL differ.
type Provider struct {
	name     string
	baseURL  string
	store    *credential.Store
	http     provider.HTTPDoer
	cooldown time.Duration
}

// New builds a Provider with the given name (e.g. "openai", "grok") and base URL
// (e.g. https://api.openai.com, https://api.x.ai).
func New(name, baseURL string, store *credential.Store, doer provider.HTTPDoer) *Provider {
	return &Provider{
		name:     name,
		baseURL:  strings.TrimRight(baseURL, "/"),
		store:    store,
		http:     doer,
		cooldown: defaultCooldown,
	}
}

// Name identifies this provider.
func (p *Provider) Name() string { return p.name }

// Chat forwards an OpenAI chat-completions request upstream, rotating across
// credentials, and returns the OpenAI-format response unchanged.
func (p *Provider) Chat(ctx context.Context, body []byte, stream bool, clientHeader http.Header) (*provider.Response, error) {
	resp, credName, err := provider.Rotate(ctx, p.store, p.cooldown, func(cred *credential.Credential) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+ChatPath, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("openai: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cred.APIKey())
		if stream {
			req.Header.Set("Accept", "text/event-stream")
		} else {
			req.Header.Set("Accept", "application/json")
		}
		return p.http.Do(req)
	})
	if err != nil {
		return nil, err
	}
	return &provider.Response{
		Status:     resp.StatusCode,
		Header:     resp.Header,
		Body:       resp.Body,
		Credential: credName,
	}, nil
}
