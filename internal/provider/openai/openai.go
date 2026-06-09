// Package openai is cerber's client for OpenAI-compatible chat completions. The
// inbound dialect and the OpenAI upstream are the same format, so this provider
// is a credential-injecting passthrough — no translation. It only contacts the
// configured base URL (see AUDIT.md).
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tggo/cerber/internal/credential"
	"github.com/tggo/cerber/internal/provider"
)

// ChatPath is the OpenAI chat-completions endpoint.
const ChatPath = "/v1/chat/completions"

// ModelsPath is the OpenAI-compatible model-listing endpoint (served by ollama
// and vLLM too), used by Probe for liveness + model discovery.
const ModelsPath = "/v1/models"

// ImagesPath is the OpenAI-compatible image-generation endpoint (xAI/Grok serves
// it too: grok-imagine-image*).
const ImagesPath = "/v1/images/generations"

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

	// Optional OAuth refresh (e.g. xAI/Grok subscription tokens). nil = api-key
	// only. refreshSkew refreshes this long before expiry.
	refresh     func(ctx context.Context, refreshToken string) (credential.OAuthTokens, error)
	persist     func(name string, tok credential.OAuthTokens)
	refreshSkew time.Duration
	now         func() time.Time
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

// headerGet safely reads a header value (clientHeader may be nil).
func headerGet(h http.Header, key string) string {
	if h == nil {
		return ""
	}
	return h.Get(key)
}

// SetOAuthRefresh enables OAuth-token refresh for this provider's credentials
// (used for xAI/Grok subscription tokens). refresh exchanges a refresh token for
// new tokens; persist (optional) saves them to disk. Call before serving.
func (p *Provider) SetOAuthRefresh(refresh func(ctx context.Context, refreshToken string) (credential.OAuthTokens, error), persist func(name string, tok credential.OAuthTokens)) {
	p.refresh = refresh
	p.persist = persist
	p.refreshSkew = 60 * time.Second
	p.now = time.Now
}

// bearer returns the Authorization bearer value for a credential: the OAuth
// access token (refreshing it first if near expiry) or the API key.
func (p *Provider) bearer(ctx context.Context, cred *credential.Credential) string {
	if cred.Kind() != credential.KindOAuth {
		return cred.APIKey()
	}
	if p.refresh != nil && p.now != nil && cred.NeedsRefresh(p.now(), p.refreshSkew) {
		if tok, err := p.refresh(ctx, cred.RefreshToken()); err == nil {
			p.store.UpdateOAuth(cred, tok)
			if p.persist != nil {
				p.persist(cred.Name(), tok)
			}
		}
		// On refresh error, fall through with the current token; a 401 will
		// rotate/cooldown it via Rotate.
	}
	return cred.AccessToken()
}

// BaseURL returns the configured upstream base URL (safe to display).
func (p *Provider) BaseURL() string { return p.baseURL }

// Images forwards an OpenAI-format image-generation request upstream (rotating
// credentials) and returns the response unchanged. Non-streaming.
func (p *Provider) Images(ctx context.Context, body []byte, clientHeader http.Header) (*provider.Response, error) {
	match := credential.MatchHeader(headerGet(clientHeader, "X-Cerber-Cred"))
	resp, credName, err := provider.RotateFiltered(ctx, p.store, p.cooldown, match, func(cred *credential.Credential) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+ImagesPath, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("openai: build image request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+p.bearer(ctx, cred))
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

// ProbeCredential validates a single credential by calling GET /v1/models with
// its key and returns the model IDs it can access. A 401/403 yields
// provider.ErrInvalidCredential; other non-200 / transport / decode failures
// yield a plain error; success returns the model list (possibly empty).
func (p *Provider) ProbeCredential(ctx context.Context, c *credential.Credential) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+ModelsPath, nil)
	if err != nil {
		return nil, fmt.Errorf("openai: build models request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c != nil {
		if tok := p.bearer(ctx, c); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, provider.ErrInvalidCredential
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai: models probe status %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("openai: decode models: %w", err)
	}
	models := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

// Chat forwards an OpenAI chat-completions request upstream, rotating across
// credentials, and returns the OpenAI-format response unchanged.
func (p *Provider) Chat(ctx context.Context, body []byte, stream bool, clientHeader http.Header) (*provider.Response, error) {
	match := credential.MatchHeader(headerGet(clientHeader, "X-Cerber-Cred"))
	resp, credName, err := provider.RotateFiltered(ctx, p.store, p.cooldown, match, func(cred *credential.Credential) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+ChatPath, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("openai: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+p.bearer(ctx, cred))
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
