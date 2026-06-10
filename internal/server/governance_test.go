package server

import (
	"net/http"
	"testing"

	"github.com/tggo/cerber/internal/access"
	"github.com/tggo/cerber/internal/server/mocks"
	"github.com/tggo/cerber/internal/usage"

	"github.com/stretchr/testify/mock"
)

// managedKeyServer wires a server whose only accepted key is a managed
// (dashboard) key carrying the given limits, returning the server, the upstream
// mock, and the key's secret.
func managedKeyServer(t *testing.T, limits access.Limits) (*Server, *mocks.Upstream, string) {
	t.Helper()
	s, up := newServer(t, newStore(t, 1))
	st, err := access.LoadStore("") // in-memory (no path)
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := st.Add("mk")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetLimits("mk", limits); err != nil {
		t.Fatal(err)
	}
	// Replace the static checker so only the managed key authorizes (forcing the
	// governance path), keep the managed store for limits.
	s.access = access.New(nil)
	s.SetClientKeyStore(st)
	return s, up, secret
}

func TestGovernance_RequestLimit(t *testing.T) {
	s, up, key := managedKeyServer(t, access.Limits{MaxRequests: 1, RatePeriod: "minute"})
	up.EXPECT().Send(mock.Anything, mock.Anything, false, mock.Anything, mock.Anything).
		Return(resp(200, "application/json", `{"id":"msg_1"}`), nil).Once()
	h := s.Handler()

	rec := do(t, h, "POST", "/v1/messages", `{"model":"claude","stream":false}`, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", rec.Code)
	}
	rec = do(t, h, "POST", "/v1/messages", `{"model":"claude","stream":false}`, key)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429", rec.Code)
	}
}

func TestGovernance_BudgetExceeded(t *testing.T) {
	s, _, key := managedKeyServer(t, access.Limits{MaxCostUSD: 1.0, BudgetPeriod: "hour"})
	// Push the key over its budget before the request; the gate must reject it
	// without ever calling upstream (no Send expectation registered).
	s.keys.Charge("mk", 2.0, 0)
	rec := do(t, s.Handler(), "POST", "/v1/messages", `{"model":"claude","stream":false}`, key)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("over-budget request = %d, want 402", rec.Code)
	}
}

func TestGovernance_ChargeFromPricing(t *testing.T) {
	s, up, key := managedKeyServer(t, access.Limits{MaxCostUSD: 100, BudgetPeriod: "hour"})
	s.usage.SetPricing(map[string]usage.Price{"claude": {Input: 1_000_000, Output: 1_000_000}})
	up.EXPECT().Send(mock.Anything, mock.Anything, false, mock.Anything, mock.Anything).
		Return(resp(200, "application/json",
			`{"id":"m","usage":{"input_tokens":3,"output_tokens":4}}`), nil).Once()
	rec := do(t, s.Handler(), "POST", "/v1/messages", `{"model":"claude","stream":false}`, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("request = %d, want 200", rec.Code)
	}
	// Pricing is 1.0 per token (1e6 per 1M); 7 tokens => $7 charged to the key.
	if u := s.keys.List()[0].Usage; u.CostUSD != 7 || u.Tokens != 7 {
		t.Errorf("charged usage = %+v, want cost 7 tokens 7", u)
	}
}

func TestGovernance_AdminSetLimits(t *testing.T) {
	s, _, key := managedKeyServer(t, access.Limits{})
	h := s.Handler()
	body := `{"max_requests":5,"rate_period":"minute"}`

	// No auth → 401 (no management key configured falls back to the client check).
	if rec := do(t, h, "POST", "/admin/keys/mk/limits", body, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("admin without key = %d, want 401", rec.Code)
	}
	// With the managed key → applied.
	if rec := do(t, h, "POST", "/admin/keys/mk/limits", body, key); rec.Code != http.StatusOK {
		t.Fatalf("admin set limits = %d, want 200", rec.Code)
	}
	if got := s.keys.List()[0].Limits.MaxRequests; got != 5 {
		t.Errorf("limits after admin set = %d, want 5", got)
	}
	// Unknown key → 404; invalid period → 400.
	if rec := do(t, h, "POST", "/admin/keys/ghost/limits", body, key); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown key = %d, want 404", rec.Code)
	}
	if rec := do(t, h, "POST", "/admin/keys/mk/limits", `{"rate_period":"bad"}`, key); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad period = %d, want 400", rec.Code)
	}
}
