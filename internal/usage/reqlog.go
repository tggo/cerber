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
	// Status/Detail are the failure as the client saw it: the HTTP status cerber
	// returned and a short message extracted from the error body. They are filled
	// after the fact (the handler records the event before the error response is
	// written) via AnnotateRequest, keyed by Seq.
	Status int    `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`
	Seq    uint64 `json:"-"` // internal handle for AnnotateRequest
}

// maxDetail caps how much of an error body is retained per event.
const maxDetail = 512

// RequestFilter narrows the recent-request log. Empty fields match anything.
type RequestFilter struct {
	Model      string
	Provider   string
	Credential string
	Client     string
	ErrorsOnly bool
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
	case f.ErrorsOnly && !e.Error:
		return false
	}
	return true
}

// RecordRequest appends a per-request event to the bounded recent-log, dropping
// the oldest once the cap is reached. It returns the event's sequence number,
// with which AnnotateRequest can later attach the error the client was served.
func (t *Tracker) RecordRequest(e RequestEvent) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.recentCap <= 0 {
		t.recentCap = defaultRecentCap
	}
	t.recentSeq++
	e.Seq = t.recentSeq
	e.Detail = truncate(e.Detail)
	t.recent = append(t.recent, e)
	if over := len(t.recent) - t.recentCap; over > 0 {
		t.recent = append(t.recent[:0], t.recent[over:]...)
	}
	return e.Seq
}

// AnnotateRequest attaches the HTTP status and error message the client was
// served to an already-recorded event (identified by the sequence number
// RecordRequest returned), and marks it as errored for status >= 400. A seq of 0
// or an event that has already aged out of the ring is a no-op.
func (t *Tracker) AnnotateRequest(seq uint64, status int, detail string) {
	if seq == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.recent) - 1; i >= 0; i-- {
		if t.recent[i].Seq != seq {
			continue
		}
		t.recent[i].Status = status
		if detail != "" {
			t.recent[i].Detail = truncate(detail)
		}
		if status >= 400 {
			t.recent[i].Error = true
		}
		return
	}
}

// truncate bounds a stored error message.
func truncate(s string) string {
	if len(s) <= maxDetail {
		return s
	}
	return s[:maxDetail] + "…"
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
