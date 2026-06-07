// Package provider defines the abstractions shared by upstream provider clients.
package provider

import "net/http"

// HTTPDoer is the minimal HTTP client surface a provider needs. It is an
// interface so provider clients can be unit-tested against generated mocks
// (mockery) without touching the network. *http.Client satisfies it.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}
