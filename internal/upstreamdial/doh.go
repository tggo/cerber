// Package upstreamdial resolves upstream hostnames via DNS-over-HTTPS so cerber
// can reach the real provider even when the local /etc/hosts redirects that
// hostname to cerber itself (the TLS-impersonation setup, Docker only).
package upstreamdial

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// DefaultDoHEndpoint is Cloudflare's JSON DoH endpoint, addressed by IP so it is
// itself immune to /etc/hosts.
const DefaultDoHEndpoint = "https://1.1.1.1/dns-query"

// Doer is the minimal HTTP client surface (for testing).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Resolver resolves A records over DoH with a small TTL cache.
type Resolver struct {
	endpoint string
	doer     Doer
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	ip  string
	exp time.Time
}

// Option customizes a Resolver.
type Option func(*Resolver)

// WithClock injects a clock (tests).
func WithClock(now func() time.Time) Option { return func(r *Resolver) { r.now = now } }

// WithDoer injects the HTTP client (tests).
func WithDoer(d Doer) Option { return func(r *Resolver) { r.doer = d } }

// WithEndpoint overrides the DoH endpoint.
func WithEndpoint(ep string) Option { return func(r *Resolver) { r.endpoint = ep } }

// NewResolver builds a Resolver.
func NewResolver(opts ...Option) *Resolver {
	r := &Resolver{
		endpoint: DefaultDoHEndpoint,
		doer:     &http.Client{Timeout: 10 * time.Second},
		now:      time.Now,
		cache:    map[string]cacheEntry{},
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

type dohResponse struct {
	Answer []struct {
		Type int    `json:"type"`
		TTL  int    `json:"TTL"`
		Data string `json:"data"`
	} `json:"Answer"`
}

// Resolve returns an A-record IP for host, using the cache when fresh.
func (r *Resolver) Resolve(ctx context.Context, host string) (string, error) {
	now := r.now()
	r.mu.Lock()
	if e, ok := r.cache[host]; ok && now.Before(e.exp) {
		r.mu.Unlock()
		return e.ip, nil
	}
	r.mu.Unlock()

	u := r.endpoint + "?" + url.Values{"name": {host}, "type": {"A"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("doh: build request: %w", err)
	}
	req.Header.Set("Accept", "application/dns-json")
	resp, err := r.doer.Do(req)
	if err != nil {
		return "", fmt.Errorf("doh: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("doh: status %d", resp.StatusCode)
	}
	var dr dohResponse
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		return "", fmt.Errorf("doh: decode: %w", err)
	}
	ttl := 60
	for _, a := range dr.Answer {
		if a.Type == 1 && net.ParseIP(a.Data) != nil { // A record
			if a.TTL > 0 {
				ttl = a.TTL
			}
			if ttl < 30 {
				ttl = 30
			}
			r.mu.Lock()
			r.cache[host] = cacheEntry{ip: a.Data, exp: now.Add(time.Duration(ttl) * time.Second)}
			r.mu.Unlock()
			return a.Data, nil
		}
	}
	return "", fmt.Errorf("doh: no A record for %s", host)
}

// DialContext dials addr, resolving the host via DoH first (so it bypasses
// /etc/hosts). IP literals are dialed directly.
func (r *Resolver) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if net.ParseIP(host) != nil {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	ip, err := r.Resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(ip, port))
}
