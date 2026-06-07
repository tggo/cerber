// Package provider defines the abstractions shared by upstream provider clients.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"cerber/internal/credential"
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

// Rotate sends through each credential in turn, sidelining (cooldown) any that
// fail with a transport error or an auth/rate-limit status, until one succeeds.
// It returns the successful response, the credential name used, and an error.
// The returned response's Body must be closed by the caller.
func Rotate(store *credential.Store, cooldown time.Duration, send func(*credential.Credential) (*http.Response, error)) (*http.Response, string, error) {
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
