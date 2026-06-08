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

// ModelLister is an optional capability: a Chatter that knows which model IDs
// its upstream serves (e.g. discovered via /v1/models). The server uses it to
// route a request to the provider that actually has the requested model, without
// relying on name-prefix configuration.
type ModelLister interface {
	Models() []string
}

// Inspectable is an optional capability for provider health/discovery reporting
// in the management API. checkedAt is zero if the provider has never been probed.
type Inspectable interface {
	Name() string
	BaseURL() string
	Models() []string
	Health() (alive bool, checkedAt time.Time, errMsg string)
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
	var lastErr error
	var lastCred string
	for i, n := 0, store.Len(); i < n; i++ {
		cred, err := store.Next()
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
