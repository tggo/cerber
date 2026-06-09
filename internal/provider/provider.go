// Package provider defines the abstractions shared by upstream provider clients.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/tggo/cerber/internal/credential"
)

// HTTPDoer is the minimal HTTP client surface a provider needs. It is an
// interface so provider clients can be unit-tested against generated mocks
// (mockery) without touching the network. *http.Client satisfies it.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Response is an OpenAI-dialect response produced by a Chatter. Body is the
// OpenAI-format response stream/JSON; the caller must Close it.
type Response struct {
	Status       int
	Header       http.Header
	Body         io.ReadCloser
	Credential   string
	InputTokens  int64
	OutputTokens int64
}

// Chatter handles an OpenAI chat-completions request and returns an OpenAI-format
// response, performing any provider-specific translation internally.
type Chatter interface {
	Name() string
	Chat(ctx context.Context, openaiBody []byte, stream bool, clientHeader http.Header) (*Response, error)
}

// ErrInvalidCredential is returned by a Prober when the upstream rejects a
// credential's auth (e.g. 401/403) — i.e. the key/token is bad, as opposed to a
// transport or server error.
var ErrInvalidCredential = errors.New("provider: credential rejected by upstream")

// Prober is an optional capability: validate a single credential against the
// upstream and report the model IDs it can access. A nil error means the
// credential is valid; ErrInvalidCredential means it was rejected; any other
// error is a transport/unknown failure. models may be empty even when valid
// (e.g. an upstream that has no model-listing for that auth kind).
type Prober interface {
	ProbeCredential(ctx context.Context, c *credential.Credential) (models []string, err error)
}

// BaseURLer is an optional capability exposing a provider's upstream base URL
// for the management UI (safe to display).
type BaseURLer interface {
	BaseURL() string
}

// ImageGenerator is an optional capability: a provider that can serve the
// OpenAI-compatible /v1/images/generations endpoint (e.g. xAI/Grok, OpenAI).
type ImageGenerator interface {
	Images(ctx context.Context, openaiBody []byte, clientHeader http.Header) (*Response, error)
}

// BadRequestError marks a client-side error (e.g. an untranslatable request) so
// callers can map it to HTTP 400 instead of a 502 upstream error.
type BadRequestError struct{ Err error }

func (e *BadRequestError) Error() string { return e.Err.Error() }
func (e *BadRequestError) Unwrap() error { return e.Err }

// Rotate sends through each credential in turn, sidelining (cooldown) any that
// fail with a transport error or an auth/rate-limit status, until one succeeds.
// It returns the successful response, the credential name used, and an error.
// The returned response's Body must be closed by the caller.
func Rotate(ctx context.Context, store *credential.Store, cooldown time.Duration, send func(*credential.Credential) (*http.Response, error)) (*http.Response, string, error) {
	return RotateFiltered(ctx, store, cooldown, nil, send)
}

// RotateFiltered is Rotate restricted to credentials matching match (nil = any),
// so a client can pin a specific account/subscription (see credential.MatchHeader).
func RotateFiltered(ctx context.Context, store *credential.Store, cooldown time.Duration, match func(*credential.Credential) bool, send func(*credential.Credential) (*http.Response, error)) (*http.Response, string, error) {
	var lastErr error
	var lastCred string
	for i, n := 0, store.Len(); i < n; i++ {
		cred, err := store.NextOf(match)
		if err != nil {
			return nil, lastCred, err // ErrNoneAvailable
		}
		lastCred = cred.Name()
		resp, err := send(cred)
		if err != nil {
			if ctx.Err() != nil { // client canceled — don't penalize the credential
				return nil, lastCred, ctx.Err()
			}
			lastErr = err
			store.Cooldown(cred, cooldown)
			continue
		}
		if isCredFailure(resp.StatusCode) {
			_ = resp.Body.Close()
			store.Cooldown(cred, cooldown)
			lastErr = fmt.Errorf("upstream auth/rate-limit status %d", resp.StatusCode)
			continue
		}
		return resp, cred.Name(), nil
	}
	if lastErr == nil {
		lastErr = errors.New("no credentials available")
	}
	return nil, lastCred, lastErr
}

func isCredFailure(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}
