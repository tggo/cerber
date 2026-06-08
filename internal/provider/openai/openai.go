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
	"sync"
	"time"

	"github.com/tggo/cerber/internal/credential"
	"github.com/tggo/cerber/internal/provider"
)

// ChatPath is the OpenAI chat-completions endpoint.
const ChatPath = "/v1/chat/completions"

// ModelsPath is the OpenAI-compatible model-listing endpoint (served by ollama
// and vLLM too), used by Probe for liveness + model discovery.
const ModelsPath = "/v1/models"

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

	// Probe state (liveness + discovered models), refreshed by Probe.
	mu        sync.RWMutex
	models    []string
	alive     bool
	checkedAt time.Time
	probeErr  string
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

// BaseURL returns the configured upstream base URL (safe to display).
func (p *Provider) BaseURL() string { return p.baseURL }

// Models returns the set of model IDs the upstream advertised at the last Probe
// (empty until the first successful probe). A copy, safe to retain.
func (p *Provider) Models() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, len(p.models))
	copy(out, p.models)
	return out
}

// Health reports the last probe result: whether the upstream was reachable, when
// it was checked (zero = never probed), and the last error (empty if healthy).
func (p *Provider) Health() (alive bool, checkedAt time.Time, errMsg string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.alive, p.checkedAt, p.probeErr
}

// Probe refreshes liveness and the discovered model set by calling GET
// /v1/models on the upstream. It is best-effort: a non-nil error also records an
// unhealthy state. Auth is attached when a credential is available (local
// servers ignore it).
func (p *Provider) Probe(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+ModelsPath, nil)
	if err != nil {
		return fmt.Errorf("openai: build models request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if cred, cerr := p.store.Next(); cerr == nil && cred.APIKey() != "" {
		req.Header.Set("Authorization", "Bearer "+cred.APIKey())
	}
	resp, err := p.http.Do(req)
	if err != nil {
		p.record(false, nil, err.Error())
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("status %d", resp.StatusCode)
		p.record(false, nil, msg)
		return fmt.Errorf("openai: models probe %s", msg)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		p.record(false, nil, "decode models: "+err.Error())
		return err
	}
	models := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	p.record(true, models, "")
	return nil
}

// record updates probe state. models==nil leaves the previous set intact.
func (p *Provider) record(alive bool, models []string, errMsg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.alive = alive
	p.checkedAt = time.Now()
	p.probeErr = errMsg
	if models != nil {
		p.models = models
	}
}

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
