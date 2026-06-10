package access

import (
	"fmt"
	"strings"
	"time"
)

// Limits is the per-key governance config: a rolling cost budget plus rolling
// request/token rate limits. A zero value means the key is unlimited. Limits
// apply only to managed keys (the "cer_"-prefixed keys in the store); static
// config keys and loopback callers are the operator's own and bypass governance.
type Limits struct {
	// MaxCostUSD caps cumulative cost (computed from configured model pricing)
	// over BudgetPeriod. Zero = no cost budget.
	MaxCostUSD float64 `json:"max_cost_usd,omitempty"`
	// BudgetPeriod is the rolling window the cost budget resets over. One of
	// minute|hour|day|week|month. Empty defaults to "month".
	BudgetPeriod string `json:"budget_period,omitempty"`

	// MaxRequests caps requests over RatePeriod. Zero = no request limit.
	MaxRequests int64 `json:"max_requests,omitempty"`
	// MaxTokens caps total (input+output) tokens over RatePeriod. Zero = no
	// token limit.
	MaxTokens int64 `json:"max_tokens,omitempty"`
	// RatePeriod is the rolling window the rate limits reset over. One of
	// minute|hour|day|week|month. Empty defaults to "minute".
	RatePeriod string `json:"rate_period,omitempty"`
}

// empty reports whether no limit is set (the key is unlimited).
func (l Limits) empty() bool {
	return l.MaxCostUSD <= 0 && l.MaxRequests <= 0 && l.MaxTokens <= 0
}

// Validate reports whether the period strings are recognised. Empty periods are
// valid (they take the documented defaults).
func (l Limits) Validate() error {
	if l.BudgetPeriod != "" && periodDuration(l.BudgetPeriod) == 0 {
		return fmt.Errorf("access: invalid budget_period %q", l.BudgetPeriod)
	}
	if l.RatePeriod != "" && periodDuration(l.RatePeriod) == 0 {
		return fmt.Errorf("access: invalid rate_period %q", l.RatePeriod)
	}
	return nil
}

// budgetWindow returns the cost-budget window duration (default: month).
func (l Limits) budgetWindow() time.Duration {
	if l.BudgetPeriod == "" {
		return 30 * 24 * time.Hour
	}
	return periodDuration(l.BudgetPeriod)
}

// rateWindow returns the rate-limit window duration (default: minute).
func (l Limits) rateWindow() time.Duration {
	if l.RatePeriod == "" {
		return time.Minute
	}
	return periodDuration(l.RatePeriod)
}

// periodDuration maps a period name to its rolling duration, or 0 if unknown.
// Week/month are fixed rolling spans (7d / 30d), not calendar-aligned.
func periodDuration(p string) time.Duration {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "minute", "min":
		return time.Minute
	case "hour":
		return time.Hour
	case "day":
		return 24 * time.Hour
	case "week":
		return 7 * 24 * time.Hour
	case "month":
		return 30 * 24 * time.Hour
	default:
		return 0
	}
}

// counters is a key's rolling-window runtime state. It is persisted with the key
// so budgets and rate limits survive a restart.
type counters struct {
	BudgetStart time.Time `json:"budget_start,omitempty"`
	CostUSD     float64   `json:"cost_usd,omitempty"`
	RateStart   time.Time `json:"rate_start,omitempty"`
	Requests    int64     `json:"requests,omitempty"`
	Tokens      int64     `json:"tokens,omitempty"`
}

// usage returns the redacted public view of the counters.
func (c counters) usage() Usage {
	return Usage{
		CostUSD: c.CostUSD, Requests: c.Requests, Tokens: c.Tokens,
		BudgetWindowStart: c.BudgetStart, RateWindowStart: c.RateStart,
	}
}

// maybeReset zeros any window whose rolling span has elapsed at now.
func (c *counters) maybeReset(now time.Time, l Limits) {
	if bw := l.budgetWindow(); bw > 0 {
		if c.BudgetStart.IsZero() || now.Sub(c.BudgetStart) >= bw {
			c.BudgetStart = now
			c.CostUSD = 0
		}
	}
	if rw := l.rateWindow(); rw > 0 {
		if c.RateStart.IsZero() || now.Sub(c.RateStart) >= rw {
			c.RateStart = now
			c.Requests = 0
			c.Tokens = 0
		}
	}
}

// Usage is the current rolling-window consumption for a managed key, exposed via
// KeyInfo for the dashboard.
type Usage struct {
	CostUSD           float64   `json:"cost_usd"`
	Requests          int64     `json:"requests"`
	Tokens            int64     `json:"tokens"`
	BudgetWindowStart time.Time `json:"budget_window_start,omitempty"`
	RateWindowStart   time.Time `json:"rate_window_start,omitempty"`
}

// Decision is the outcome of an Admit check.
type Decision int

const (
	// Allowed means the request may proceed.
	Allowed Decision = iota
	// DeniedBudget means the key's cost budget is exhausted (map to HTTP 402).
	DeniedBudget
	// DeniedRate means the key's request/token rate limit is exceeded (HTTP 429).
	DeniedRate
)

// Admit checks the named key's limits and, if the request is allowed, reserves
// one request against the rate window. It does NOT persist on every call (like
// last-used stamping, the bumped counter is flushed lazily by the periodic
// Save); this keeps the hot path lock-light. An unknown name or a key with no
// limits is allowed. Token/cost are charged afterwards via Charge.
func (s *Store) Admit(name string) Decision {
	if s == nil {
		return Allowed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.find(name)
	if e == nil || e.Limits.empty() {
		return Allowed
	}
	now := s.now()
	e.Counters.maybeReset(now, e.Limits)
	if e.Limits.MaxCostUSD > 0 && e.Counters.CostUSD >= e.Limits.MaxCostUSD {
		return DeniedBudget
	}
	if e.Limits.MaxRequests > 0 && e.Counters.Requests >= e.Limits.MaxRequests {
		return DeniedRate
	}
	if e.Limits.MaxTokens > 0 && e.Counters.Tokens >= e.Limits.MaxTokens {
		return DeniedRate
	}
	e.Counters.Requests++
	return Allowed
}

// Charge records cost and token consumption against the named key after a
// request completes. A nil store, unknown name, or limit-free key is a no-op.
// Like Admit, it mutates in memory and relies on the periodic Save to persist.
func (s *Store) Charge(name string, costUSD float64, tokens int64) {
	if s == nil || (costUSD == 0 && tokens == 0) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.find(name)
	if e == nil || e.Limits.empty() {
		return
	}
	e.Counters.maybeReset(s.now(), e.Limits)
	if costUSD > 0 {
		e.Counters.CostUSD += costUSD
	}
	if tokens > 0 {
		e.Counters.Tokens += tokens
	}
}

// SetLimits replaces the named key's governance config and persists. Returns
// ErrNotFound if absent, or a validation error for an unrecognised period.
func (s *Store) SetLimits(name string, l Limits) error {
	if err := l.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.find(name)
	if e == nil {
		return ErrNotFound
	}
	e.Limits = l
	return s.saveLocked()
}

// find returns the entry with the given name, or nil. Caller holds mu.
func (s *Store) find(name string) *keyEntry {
	for _, e := range s.entries {
		if e.Name == name {
			return e
		}
	}
	return nil
}
