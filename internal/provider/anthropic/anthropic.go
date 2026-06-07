// Package anthropic is cerber's client for the Anthropic Messages API. It builds
// the upstream request, applies a credential's auth headers, and returns the raw
// upstream response for the caller to relay or translate. It only ever talks to
// the configured Anthropic base URL (see AUDIT.md).
package anthropic

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	"cerber/internal/credential"
	"cerber/internal/provider"
)

// MessagesPath is the Anthropic Messages endpoint.
const MessagesPath = "/v1/messages"

// oauthBetas are the anthropic-beta features Claude Code OAuth tokens are
// authorized for. Sent only with OAuth credentials.
const oauthBetas = "oauth-2025-04-20"

// Client issues Anthropic Messages requests.
type Client struct {
	baseURL string
	version string
	http    provider.HTTPDoer
}

// New builds a Client. baseURL is the Anthropic origin (e.g.
// https://api.anthropic.com); version is the anthropic-version header value.
func New(baseURL, version string, doer provider.HTTPDoer) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		version: version,
		http:    doer,
	}
}

// managedRequestHeaders are headers cerber sets itself; client copies of these
// are not forwarded (auth is injected, the rest are connection-specific).
var managedRequestHeaders = map[string]bool{
	"authorization": true, "x-api-key": true, "host": true,
	"content-length": true, "connection": true, "keep-alive": true,
	"proxy-authenticate": true, "proxy-authorization": true, "te": true,
	"trailer": true, "transfer-encoding": true, "upgrade": true,
}

// Send POSTs an Anthropic Messages request with the given raw JSON body. cerber
// is a transparent proxy: it forwards ALL client request headers (so Claude
// Code's version/betas/user-agent reach Anthropic unchanged), then injects only
// the credential auth. stream is informational; the client's own Accept is kept.
// clientHeader may be nil. The returned response's Body must be closed.
func (c *Client) Send(ctx context.Context, body []byte, stream bool, cred *credential.Credential, clientHeader http.Header) (*http.Response, error) {
	if cred == nil {
		return nil, fmt.Errorf("anthropic: nil credential")
	}
	if cred.Kind() == credential.KindOAuth {
		injected, err := injectClaudeCodeSystem(body)
		if err != nil {
			return nil, fmt.Errorf("anthropic: inject claude code system: %w", err)
		}
		body = injected
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+MessagesPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}

	// Forward all client headers (except the ones cerber manages).
	for k, vs := range clientHeader {
		if managedRequestHeaders[strings.ToLower(k)] {
			continue
		}
		req.Header[k] = append([]string(nil), vs...)
	}
	// Fill defaults only if the client didn't provide them.
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", c.version)
	}
	if req.Header.Get("Accept") == "" {
		if stream {
			req.Header.Set("Accept", "text/event-stream")
		} else {
			req.Header.Set("Accept", "application/json")
		}
	}
	applyAuth(req, cred)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: upstream request: %w", err)
	}
	return resp, nil
}

// mergeBetas unions two comma-separated beta lists, preserving order and
// dropping duplicates and blanks.
func mergeBetas(a, b string) string {
	seen := map[string]bool{}
	var out []string
	for _, list := range []string{a, b} {
		for _, item := range strings.Split(list, ",") {
			item = strings.TrimSpace(item)
			if item == "" || seen[item] {
				continue
			}
			seen[item] = true
			out = append(out, item)
		}
	}
	return strings.Join(out, ",")
}

// applyAuth sets the credential-specific auth headers, preserving any client
// betas (merging the required oauth beta for OAuth credentials).
func applyAuth(req *http.Request, cred *credential.Credential) {
	switch cred.Kind() {
	case credential.KindOAuth:
		req.Header.Del("x-api-key")
		req.Header.Set("Authorization", "Bearer "+cred.AccessToken())
		req.Header.Set("anthropic-beta", mergeBetas(oauthBetas, req.Header.Get("anthropic-beta")))
	default: // KindAPIKey
		req.Header.Set("x-api-key", cred.APIKey())
	}
}
