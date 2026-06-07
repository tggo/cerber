// Package claude implements the Claude Code OAuth2 (PKCE) primitives: building
// the authorization URL, and exchanging an authorization code for tokens. It is
// the same public flow the Claude Code CLI uses. No secrets are stored here; the
// caller persists the returned tokens (see internal/tokenstore).
package claude

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cerber/internal/provider"
)

// OAuth endpoints and the public Claude Code client id (not a secret).
const (
	AuthURL             = "https://claude.ai/oauth/authorize"
	TokenURL            = "https://api.anthropic.com/v1/oauth/token"
	ClientID            = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	DefaultCallbackPort = 54545
	scope               = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
)

// RedirectURI returns the OAuth redirect for a given local callback port.
func RedirectURI(port int) string {
	return fmt.Sprintf("http://localhost:%d/callback", port)
}

// PKCE holds a PKCE verifier and its S256 challenge.
type PKCE struct {
	Verifier  string
	Challenge string
}

// NewPKCE generates a fresh PKCE pair.
func NewPKCE() (PKCE, error) {
	v, err := randomURLSafe(96)
	if err != nil {
		return PKCE{}, fmt.Errorf("claude: pkce verifier: %w", err)
	}
	sum := sha256.Sum256([]byte(v))
	return PKCE{Verifier: v, Challenge: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

// NewState generates a random CSRF state value.
func NewState() (string, error) {
	s, err := randomURLSafe(32)
	if err != nil {
		return "", fmt.Errorf("claude: state: %w", err)
	}
	return s, nil
}

func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// BuildAuthURL constructs the authorization URL for the given state, PKCE, and
// callback port.
func BuildAuthURL(state string, p PKCE, port int) string {
	params := url.Values{
		"code":                  {"true"},
		"client_id":             {ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {RedirectURI(port)},
		"scope":                 {scope},
		"code_challenge":        {p.Challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	return AuthURL + "?" + params.Encode()
}

// Tokens is the result of a successful authorization-code exchange.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	Email        string
	OrgName      string
	OrgUUID      string
	ExpiresAt    time.Time
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Account      struct {
		EmailAddress string `json:"email_address"`
	} `json:"account"`
	Organization struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
}

// Exchange swaps an authorization code (plus the PKCE verifier) for tokens. The
// code may arrive as "code#state"; the embedded state, if present, is used.
func Exchange(ctx context.Context, doer provider.HTTPDoer, code, state, verifier string, port int, now func() time.Time) (Tokens, error) {
	if now == nil {
		now = time.Now
	}
	codePart, statePart := splitCodeState(code)
	if statePart != "" {
		state = statePart
	}
	reqBody, _ := json.Marshal(map[string]string{
		"code":          codePart,
		"state":         state,
		"grant_type":    "authorization_code",
		"client_id":     ClientID,
		"redirect_uri":  RedirectURI(port),
		"code_verifier": verifier,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(string(reqBody)))
	if err != nil {
		return Tokens{}, fmt.Errorf("claude: build exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := doer.Do(req)
	if err != nil {
		return Tokens{}, fmt.Errorf("claude: exchange request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Tokens{}, fmt.Errorf("claude: read exchange response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Tokens{}, fmt.Errorf("claude: exchange failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return Tokens{}, fmt.Errorf("claude: parse exchange response: %w", err)
	}
	if tr.AccessToken == "" {
		return Tokens{}, fmt.Errorf("claude: exchange response missing access_token")
	}
	return Tokens{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		Email:        tr.Account.EmailAddress,
		OrgName:      tr.Organization.Name,
		OrgUUID:      tr.Organization.UUID,
		ExpiresAt:    now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}, nil
}

func splitCodeState(code string) (string, string) {
	if i := strings.IndexByte(code, '#'); i >= 0 {
		return code[:i], code[i+1:]
	}
	return code, ""
}
