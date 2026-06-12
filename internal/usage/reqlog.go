package usage

import "time"

// defaultRecentCap bounds the in-memory recent-request log.
const defaultRecentCap = 1000

// RequestEvent is one recorded API request, for the "who recently called which
// model" view. It carries client IP/User-Agent, so it is kept ONLY in a bounded
// in-memory ring and is never written to disk (unlike the aggregates).
type RequestEvent struct {
	Time         time.Time `json:"time"`
	IP           string    `json:"ip,omitempty"`
	UserAgent    string    `json:"user_agent,omitempty"`
	Client       string    `json:"client,omitempty"` // managed key name / "config" / "localhost"
	Endpoint     string    `json:"endpoint,omitempty"`
	Model        string    `json:"model,omitempty"`
	Credential   string    `json:"credential,omitempty"` // upstream account/provider used
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	Cost         float64   `json:"cost"`
	Error        bool      `json:"error,omitempty"`
}

// RecordRequest appends a per-request event to the bounded recent-log, dropping
// the oldest once the cap is reached.
func (t *Tracker) RecordRequest(e RequestEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.recentCap <= 0 {
		t.recentCap = defaultRecentCap
	}
	t.recent = append(t.recent, e)
	if over := len(t.recent) - t.recentCap; over > 0 {
		t.recent = append(t.recent[:0], t.recent[over:]...)
	}
}

// RecentRequests returns up to limit most-recent events, newest first,
// optionally filtered to a single model. limit<=0 returns all retained events.
func (t *Tracker) RecentRequests(model string, limit int) []RequestEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]RequestEvent, 0, len(t.recent))
	for i := len(t.recent) - 1; i >= 0; i-- {
		e := t.recent[i]
		if model != "" && e.Model != model {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}
