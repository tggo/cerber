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
	Provider     string    `json:"provider,omitempty"` // provider that served the model (anthropic/openai/…)
	Model        string    `json:"model,omitempty"`
	Credential   string    `json:"credential,omitempty"` // upstream account used
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	Cost         float64   `json:"cost"`
	Error        bool      `json:"error,omitempty"`
}

// RequestFilter narrows the recent-request log. Empty fields match anything.
type RequestFilter struct {
	Model      string
	Provider   string
	Credential string
	Client     string
}

func (f RequestFilter) match(e RequestEvent) bool {
	switch {
	case f.Model != "" && e.Model != f.Model:
		return false
	case f.Provider != "" && e.Provider != f.Provider:
		return false
	case f.Credential != "" && e.Credential != f.Credential:
		return false
	case f.Client != "" && e.Client != f.Client:
		return false
	}
	return true
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

// RecentRequests returns a page of recent events matching f, newest first:
// it skips the first offset matches and returns up to limit of the rest
// (limit<=0 returns all remaining). The second return value is the total number
// of matches (for pagination UIs), independent of offset/limit.
func (t *Tracker) RecentRequests(f RequestFilter, offset, limit int) ([]RequestEvent, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]RequestEvent, 0, limit)
	total := 0
	for i := len(t.recent) - 1; i >= 0; i-- {
		e := t.recent[i]
		if !f.match(e) {
			continue
		}
		total++
		if total <= offset {
			continue
		}
		if limit > 0 && len(out) >= limit {
			continue // keep counting total, stop collecting
		}
		out = append(out, e)
	}
	return out, total
}
