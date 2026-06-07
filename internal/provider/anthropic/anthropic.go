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

// forwardedHeaders are client request headers passed through to Anthropic so
// faithful clients (e.g. Claude Code) keep working. anthropic-beta is required
// for features like context_management that the client also sets in the body.
var forwardedHeaders = []string{"anthropic-beta"}

// Send POSTs an Anthropic Messages request with the given raw JSON body, applying
// cred's auth headers and forwarding a safelist of client headers. stream
// controls the Accept header. clientHeader may be nil. The returned response's
// Body is owned by the caller and must be closed.
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
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", c.version)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	applyAuth(req, cred)
	forwardClientHeaders(req, clientHeader, cred)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: upstream request: %w", err)
	}
	return resp, nil
}

// forwardClientHeaders copies safelisted client headers onto the upstream
// request. For OAuth, the required oauth beta is merged with any client betas.
func forwardClientHeaders(req *http.Request, client http.Header, cred *credential.Credential) {
	if client == nil {
		return
	}
	for _, h := range forwardedHeaders {
		v := client.Get(h)
		if v == "" {
			continue
		}
		if h == "anthropic-beta" && cred.Kind() == credential.KindOAuth {
			req.Header.Set(h, mergeBetas(oauthBetas, v))
		} else {
			req.Header.Set(h, v)
		}
	}
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

// applyAuth sets the credential-specific auth headers.
func applyAuth(req *http.Request, cred *credential.Credential) {
	switch cred.Kind() {
	case credential.KindOAuth:
		req.Header.Del("x-api-key")
		req.Header.Set("Authorization", "Bearer "+cred.AccessToken())
		req.Header.Set("anthropic-beta", oauthBetas)
	default: // KindAPIKey
		req.Header.Set("x-api-key", cred.APIKey())
	}
}
