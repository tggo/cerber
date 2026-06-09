// Package xai implements the xAI / Grok Build OAuth2 device-authorization flow
// (the same public flow the `grok` CLI uses) plus refresh-token exchange. No
// secrets are stored here; the caller persists the returned tokens (see
// internal/tokenstore). Device flow is used because cerber typically runs
// headless/remote — there is no localhost callback to land on.
package xai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tggo/cerber/internal/provider"
)

// OAuth endpoints (from https://auth.x.ai/.well-known/openid-configuration) and
// the public Grok CLI client id (not a secret; taken from the official
// installer). Scope requests offline_access so we get a refresh token.
const (
	DeviceCodeURL = "https://auth.x.ai/oauth2/device/code"
	TokenURL      = "https://auth.x.ai/oauth2/token"
	ClientID      = "b1a00492-073a-47ea-816f-4c329264a828"
	Scope         = "openid profile email offline_access api:access grok-cli:access"
)

// ErrAuthorizationPending means the user hasn't authorized yet — keep polling.
var ErrAuthorizationPending = errors.New("xai: authorization pending")

// ErrSlowDown means the poll interval should be increased.
var ErrSlowDown = errors.New("xai: slow down")

// DeviceCode is the device-authorization response.
type DeviceCode struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	Interval                int
	ExpiresIn               int
}

// Tokens is the result of a successful device authorization or refresh.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// StartDevice initiates the device-authorization flow and returns the codes to
// show the user.
func StartDevice(ctx context.Context, doer provider.HTTPDoer) (DeviceCode, error) {
	form := url.Values{"client_id": {ClientID}, "scope": {Scope}}
	body, err := post(ctx, doer, DeviceCodeURL, form)
	if err != nil {
		return DeviceCode{}, fmt.Errorf("xai: device code: %w", err)
	}
	var dr struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		Interval                int    `json:"interval"`
		ExpiresIn               int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &dr); err != nil {
		return DeviceCode{}, fmt.Errorf("xai: parse device code: %w", err)
	}
	if dr.DeviceCode == "" || dr.UserCode == "" {
		return DeviceCode{}, fmt.Errorf("xai: device code response missing fields")
	}
	if dr.Interval <= 0 {
		dr.Interval = 5
	}
	return DeviceCode(dr), nil
}

// PollToken makes one token-endpoint attempt for a device code. It returns
// ErrAuthorizationPending / ErrSlowDown while the user is still authorizing.
func PollToken(ctx context.Context, doer provider.HTTPDoer, deviceCode string, now func() time.Time) (Tokens, error) {
	if now == nil {
		now = time.Now
	}
	form := url.Values{
		"client_id":   {ClientID},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
	}
	return tokenRequest(ctx, doer, form, now)
}

// Refresh exchanges a refresh token for a fresh access token.
func Refresh(ctx context.Context, doer provider.HTTPDoer, refreshToken string, now func() time.Time) (Tokens, error) {
	if now == nil {
		now = time.Now
	}
	form := url.Values{
		"client_id":     {ClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	return tokenRequest(ctx, doer, form, now)
}

// tokenRequest posts to the token endpoint and maps the OAuth response/errors.
func tokenRequest(ctx context.Context, doer provider.HTTPDoer, form url.Values, now func() time.Time) (Tokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, fmt.Errorf("xai: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := doer.Do(req)
	if err != nil {
		return Tokens{}, fmt.Errorf("xai: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Tokens{}, fmt.Errorf("xai: read token response: %w", err)
	}
	var tr struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int    `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &tr)
	if resp.StatusCode != http.StatusOK {
		switch tr.Error {
		case "authorization_pending":
			return Tokens{}, ErrAuthorizationPending
		case "slow_down":
			return Tokens{}, ErrSlowDown
		default:
			msg := tr.Error
			if tr.ErrorDescription != "" {
				msg += ": " + tr.ErrorDescription
			}
			if msg == "" {
				msg = strings.TrimSpace(string(raw))
			}
			return Tokens{}, fmt.Errorf("xai: token endpoint status %d: %s", resp.StatusCode, msg)
		}
	}
	if tr.AccessToken == "" {
		return Tokens{}, fmt.Errorf("xai: token response missing access_token")
	}
	return Tokens{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}, nil
}

func post(ctx context.Context, doer provider.HTTPDoer, urlStr string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := doer.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}
