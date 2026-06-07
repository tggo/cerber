// Package translator converts between the OpenAI chat-completions dialect and the
// Anthropic Messages dialect, so cerber can accept OpenAI-shaped requests and
// route them to Anthropic. It is pure data transformation: no network, no I/O
// beyond the byte slices / readers it is handed.
package translator

import "time"

// defaultMaxTokens is used when an OpenAI request omits max_tokens, which
// Anthropic requires.
const defaultMaxTokens = 4096

// Translator performs OpenAI<->Anthropic conversions. The injectable clock makes
// generated timestamps deterministic in tests.
type Translator struct {
	now func() time.Time
}

// Option customizes a Translator.
type Option func(*Translator)

// WithClock injects a clock (for tests). Defaults to time.Now.
func WithClock(now func() time.Time) Option {
	return func(t *Translator) { t.now = now }
}

// New builds a Translator.
func New(opts ...Option) *Translator {
	t := &Translator{now: time.Now}
	for _, o := range opts {
		o(t)
	}
	return t
}

// finishReason maps an Anthropic stop_reason to an OpenAI finish_reason.
func finishReason(stopReason string) string {
	switch stopReason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "":
		return ""
	default:
		return "stop"
	}
}
