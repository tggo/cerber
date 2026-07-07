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
	"io"
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

// ImagesPath is the OpenAI-compatible image-generation endpoint (xAI/Grok serves
// it too: grok-imagine-image*).
const ImagesPath = "/v1/images/generations"

// defaultCooldown sidelines a credential after an auth/rate-limit failure.
const defaultCooldown = 60 * time.Second

// hostGate is one upstream host plus its own concurrency semaphore. Different
// boxes have different capacity (a GPU box may serve 2 concurrent requests, a
// CPU box 1), so the cap is per host, not per provider. sem == nil = unlimited.
type hostGate struct {
	url string
	sem chan struct{}
}

// Provider routes OpenAI-format requests to an OpenAI-compatible upstream. The
// same implementation serves OpenAI and any OpenAI-compatible API (e.g. xAI/Grok)
// — only the name and base URL differ.
type Provider struct {
	name     string
	baseURL  string // primary host, for ProbeCredential + BaseURL() display
	store    *credential.Store
	http     provider.HTTPDoer
	cooldown time.Duration

	// hosts is the ordered upstream list: primary first, then failover targets
	// (e.g. a second ollama box). On a transport error or 5xx from one host the
	// same request is retried against the next — host-level failover, transparent
	// to the model name. Each host carries its own in-flight semaphore: a slot is
	// held for the whole request (including while the client streams the response
	// body) and released when the body is closed, so requests beyond a host's cap
	// queue until a connection frees. Always has at least one entry.
	hosts []hostGate

	// penalizeTransport, when false, keeps a transport error / exhausted-host 5xx
	// from sidelining the credential (see provider.RotateOpts) — for keyless local
	// providers where that failure is a capacity signal, not a bad credential.
	penalizeTransport bool

	// Pending option state, resolved into hosts by normalize() after options run.
	primaryCap   int          // WithConcurrency: cap for base_url (+ legacy fallbacks)
	fallbackURLs []string     // WithFallbackBaseURLs: legacy extra hosts
	hostConfigs  []HostConfig // WithHosts: explicit per-host list (takes precedence)

	// qm observes concurrency-gate behaviour (in-flight count, queue depth and
	// wait). nil = no observation.
	qm QueueMetrics

	// Optional OAuth refresh (e.g. xAI/Grok subscription tokens). nil = api-key
	// only. refreshSkew refreshes this long before expiry.
	refresh     func(ctx context.Context, refreshToken string) (credential.OAuthTokens, error)
	persist     func(name string, tok credential.OAuthTokens)
	refreshSkew time.Duration
	now         func() time.Time
}

// QueueMetrics observes a provider's concurrency-gate behaviour. All methods
// must be safe for concurrent use; *metrics.Metrics satisfies it.
type QueueMetrics interface {
	QueueDepthInc(provider string)
	QueueDepthDec(provider string)
	QueueWait(provider string, seconds float64)
	InflightInc(provider string)
	InflightDec(provider string)
}

// Option configures a Provider at construction.
type Option func(*Provider)

// HostConfig is one upstream host with its own concurrency cap, for WithHosts.
type HostConfig struct {
	URL         string
	Concurrency int // max in-flight to THIS host; <= 0 = unlimited
}

// WithConcurrency caps the number of in-flight requests this provider sends to
// the primary host at once (e.g. an ArliAI plan that allows n concurrent
// streams). n <= 0 leaves it unlimited. Requests beyond the cap queue and wait
// for an in-flight one to finish (its response body to be closed). Ignored when
// WithHosts is used (per-host caps are given there instead).
func WithConcurrency(n int) Option {
	return func(p *Provider) { p.primaryCap = n }
}

// WithQueueMetrics attaches a QueueMetrics observer so the provider reports its
// in-flight count, queue depth, and queue-wait time (labelled by provider name).
func WithQueueMetrics(qm QueueMetrics) Option {
	return func(p *Provider) { p.qm = qm }
}

// WithFallbackBaseURLs sets additional upstream hosts tried (in order) when the
// primary base URL is unreachable or 5xx — e.g. a CPU ollama box that backs up
// the GPU one for embeddings. Each is trimmed of a trailing slash; empties drop.
// The primary's WithConcurrency cap (if any) applies to each fallback too; for
// distinct per-host caps use WithHosts. Ignored when WithHosts is used.
func WithFallbackBaseURLs(urls []string) Option {
	return func(p *Provider) {
		for _, u := range urls {
			if u = strings.TrimRight(u, "/"); u != "" {
				p.fallbackURLs = append(p.fallbackURLs, u)
			}
		}
	}
}

// WithHosts sets the full ordered upstream list, each host with its own
// concurrency cap — for a provider spread across boxes of differing capacity
// (e.g. ollama on a GPU box cap 2 + a CPU box cap 1). The first host is primary,
// the rest are failover targets. Takes precedence over the base URL passed to
// New and over WithConcurrency/WithFallbackBaseURLs. Empty URLs are dropped.
func WithHosts(hosts []HostConfig) Option {
	return func(p *Provider) {
		for _, h := range hosts {
			if u := strings.TrimRight(h.URL, "/"); u != "" {
				p.hostConfigs = append(p.hostConfigs, HostConfig{URL: u, Concurrency: h.Concurrency})
			}
		}
	}
}

// WithTransportPenaltyDisabled keeps a transport error / exhausted-host 5xx from
// sidelining the credential — for a keyless local provider (ollama/vLLM) where
// such a failure is a capacity/host signal, not a bad credential. Without it, an
// overload of the single dummy credential would 503 the whole provider until the
// exponential backoff expires. See provider.RotateOpts.PenalizeTransport.
func WithTransportPenaltyDisabled() Option {
	return func(p *Provider) { p.penalizeTransport = false }
}

// New builds a Provider with the given name (e.g. "openai", "grok") and base URL
// (e.g. https://api.openai.com, https://api.x.ai).
func New(name, baseURL string, store *credential.Store, doer provider.HTTPDoer, opts ...Option) *Provider {
	p := &Provider{
		name:              name,
		baseURL:           strings.TrimRight(baseURL, "/"),
		store:             store,
		http:              doer,
		cooldown:          defaultCooldown,
		penalizeTransport: true,
	}
	for _, opt := range opts {
		opt(p)
	}
	p.normalize()
	return p
}

// normalize resolves the pending option state (base URL, WithConcurrency,
// WithFallbackBaseURLs, WithHosts) into the ordered hosts list with per-host
// semaphores. Always leaves at least one host; sets baseURL to the primary.
func (p *Provider) normalize() {
	newSem := func(n int) chan struct{} {
		if n > 0 {
			return make(chan struct{}, n)
		}
		return nil
	}
	if len(p.hostConfigs) > 0 {
		for _, h := range p.hostConfigs {
			p.hosts = append(p.hosts, hostGate{url: h.URL, sem: newSem(h.Concurrency)})
		}
		p.baseURL = p.hosts[0].url
		return
	}
	p.hosts = []hostGate{{url: p.baseURL, sem: newSem(p.primaryCap)}}
	for _, u := range p.fallbackURLs {
		p.hosts = append(p.hosts, hostGate{url: u, sem: newSem(p.primaryCap)})
	}
}

// acquire takes host h's concurrency slot (recording queue depth/wait and
// in-flight count), blocking — respecting ctx cancellation — until one is free.
// It returns a release closure the caller must run exactly once (on failure
// immediately; on success when the response body is closed), or nil when there
// is nothing to gate (unlimited host and no metrics observer).
func (p *Provider) acquire(ctx context.Context, sem chan struct{}) (func(), error) {
	if sem == nil && p.qm == nil {
		return nil, nil
	}
	if p.qm != nil {
		p.qm.QueueDepthInc(p.name)
	}
	start := p.clock()
	if sem != nil {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			if p.qm != nil {
				p.qm.QueueDepthDec(p.name)
			}
			return nil, ctx.Err()
		}
	}
	if p.qm != nil {
		p.qm.QueueDepthDec(p.name)
		p.qm.QueueWait(p.name, p.clock().Sub(start).Seconds())
		p.qm.InflightInc(p.name)
	}
	return func() {
		if sem != nil {
			<-sem
		}
		if p.qm != nil {
			p.qm.InflightDec(p.name)
		}
	}, nil
}

// clock returns the provider's clock (time.Now unless injected for tests).
func (p *Provider) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// sendHosts tries the request against each host in order (primary then failover
// targets), advancing on a transport error or a 5xx status — host-level failover
// for when a box is down. Each attempt first takes that host's concurrency slot
// (queuing if the host is at capacity): on a failed attempt the slot is released
// before trying the next host; on the returned (successful or single-host 5xx)
// response the slot is held until the caller closes the body. build is called
// fresh per host (so the request body is re-created each attempt). A non-5xx
// response is returned as-is (incl. 4xx, a client error identical on every host).
// If all hosts fail it returns the last error.
func (p *Provider) sendHosts(ctx context.Context, build func(base string) (*http.Request, error)) (*http.Response, error) {
	multi := len(p.hosts) > 1
	var lastErr error
	for _, h := range p.hosts {
		req, err := build(h.url)
		if err != nil {
			return nil, err // build error is request-shaped, not host-related — don't failover
		}
		release, err := p.acquire(ctx, h.sem)
		if err != nil {
			return nil, err // ctx canceled while queued for a slot
		}
		resp, err := p.http.Do(req)
		if err != nil {
			if release != nil {
				release()
			}
			if ctx.Err() != nil { // client canceled — don't thrash the remaining hosts
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 && multi {
			_ = resp.Body.Close()
			if release != nil {
				release()
			}
			lastErr = fmt.Errorf("openai: host %s returned %d", h.url, resp.StatusCode)
			continue
		}
		if release != nil {
			resp.Body = &gatedBody{ReadCloser: resp.Body, release: release}
		}
		return resp, nil
	}
	return nil, lastErr
}

// gatedRotate rotates across credentials (per-host concurrency gating happens
// inside the send callback via sendHosts) and returns the OpenAI-format response
// unchanged. Callers MUST close the returned Body — every server handler does
// (defer resp.Body.Close()) — which is also what releases the held host slot.
func (p *Provider) gatedRotate(ctx context.Context, match func(*credential.Credential) bool, send func(*credential.Credential) (*http.Response, error)) (*provider.Response, error) {
	resp, credName, err := provider.RotateFilteredOpts(ctx, p.store, p.cooldown, match, send,
		provider.RotateOpts{PenalizeTransport: p.penalizeTransport})
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

// gatedBody releases a concurrency slot when the underlying body is closed.
type gatedBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (g *gatedBody) Close() error {
	err := g.ReadCloser.Close()
	g.once.Do(g.release)
	return err
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
	if p.refresh != nil && p.now != nil {
		tok, did, err := p.store.EnsureFresh(cred, false, p.now(), p.refreshSkew, func() (credential.OAuthTokens, error) {
			return p.refresh(ctx, cred.RefreshToken())
		})
		if err == nil && did && p.persist != nil {
			p.persist(cred.Name(), tok)
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
	return p.gatedRotate(ctx, match, func(cred *credential.Credential) (*http.Response, error) {
		return p.sendHosts(ctx, func(base string) (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+ImagesPath, bytes.NewReader(body))
			if err != nil {
				return nil, fmt.Errorf("openai: build image request: %w", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json")
			req.Header.Set("Authorization", "Bearer "+p.bearer(ctx, cred))
			return req, nil
		})
	})
}

// Forward passes an OpenAI-compatible request through to a fixed sub-path on the
// provider (e.g. /v1/embeddings, /v1/completions, /v1/responses) with credential
// rotation, relaying the body unchanged. stream sets the Accept header so SSE
// passes through for endpoints that support it.
func (p *Provider) Forward(ctx context.Context, subpath string, body []byte, stream bool, clientHeader http.Header) (*provider.Response, error) {
	match := credential.MatchHeader(headerGet(clientHeader, "X-Cerber-Cred"))
	return p.gatedRotate(ctx, match, func(cred *credential.Credential) (*http.Response, error) {
		return p.sendHosts(ctx, func(base string) (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+subpath, bytes.NewReader(body))
			if err != nil {
				return nil, fmt.Errorf("openai: build forward request: %w", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+p.bearer(ctx, cred))
			if stream {
				req.Header.Set("Accept", "text/event-stream")
			} else {
				req.Header.Set("Accept", "application/json")
			}
			return req, nil
		})
	})
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
	return p.gatedRotate(ctx, match, func(cred *credential.Credential) (*http.Response, error) {
		return p.sendHosts(ctx, func(base string) (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+ChatPath, bytes.NewReader(body))
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
			return req, nil
		})
	})
}
