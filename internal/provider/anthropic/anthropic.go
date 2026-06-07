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

// Send POSTs an Anthropic Messages request with the given raw JSON body, applying
// cred's auth headers. stream controls the Accept header. The returned response's
// Body is owned by the caller and must be closed.
func (c *Client) Send(ctx context.Context, body []byte, stream bool, cred *credential.Credential) (*http.Response, error) {
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

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: upstream request: %w", err)
	}
	return resp, nil
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
