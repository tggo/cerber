package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cerber/internal/credential"
	"cerber/internal/provider"
)

// ClaudeCodeClientID is the public OAuth client id used by the Claude Code CLI.
// It is not a secret; it identifies the client application to Anthropic.
const ClaudeCodeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// tokenPath is the Anthropic OAuth token endpoint.
const tokenPath = "/v1/oauth/token"

// Refresher exchanges a Claude Code refresh token for a fresh access token. It
// only ever contacts the configured Anthropic base URL (see AUDIT.md).
type Refresher struct {
	baseURL string
	http    provider.HTTPDoer
	now     func() time.Time
}

// RefresherOption customizes a Refresher.
type RefresherOption func(*Refresher)

// WithRefresherClock injects a clock (for tests). Defaults to time.Now.
func WithRefresherClock(now func() time.Time) RefresherOption {
	return func(r *Refresher) { r.now = now }
}

// NewRefresher builds a Refresher against the given Anthropic base URL.
func NewRefresher(baseURL string, doer provider.HTTPDoer, opts ...RefresherOption) *Refresher {
	r := &Refresher{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    doer,
		now:     time.Now,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

type refreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// Refresh obtains a new token set. The returned RefreshToken may differ from the
// one passed in (Anthropic rotates refresh tokens), so callers must persist it.
func (r *Refresher) Refresh(ctx context.Context, refreshToken string) (credential.OAuthTokens, error) {
	var zero credential.OAuthTokens
	if refreshToken == "" {
		return zero, fmt.Errorf("anthropic: refresh requires a refresh_token")
	}
	reqBody, err := json.Marshal(map[string]string{
		"client_id":     ClaudeCodeClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	})
	if err != nil {
		return zero, fmt.Errorf("anthropic: marshal refresh request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+tokenPath, bytes.NewReader(reqBody))
	if err != nil {
		return zero, fmt.Errorf("anthropic: build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := r.http.Do(req)
	if err != nil {
		return zero, fmt.Errorf("anthropic: refresh request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return zero, fmt.Errorf("anthropic: read refresh response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("anthropic: refresh failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tr refreshResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return zero, fmt.Errorf("anthropic: parse refresh response: %w", err)
	}
	if tr.AccessToken == "" {
		return zero, fmt.Errorf("anthropic: refresh response missing access_token")
	}
	return credential.OAuthTokens{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    r.now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}, nil
}
