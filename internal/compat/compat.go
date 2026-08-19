// Package compat resolves how much cerber is allowed to rewrite an inbound
// OpenAI-dialect request before it reaches an upstream.
//
// Upstreams that all claim to speak "the OpenAI API" disagree on the details:
// gpt-5 rejects max_tokens and demands max_completion_tokens, xAI rejects
// max_completion_tokens and demands max_tokens, and there is no way to know
// which from the model name alone for a model nobody has seen yet. A client can
// discover that by retrying and remembering, but every client then has to
// implement the same dance. cerber sits where that knowledge belongs, so it does
// the dance once — while still letting a client that knows better opt out.
//
// The package is pure data transformation: no network, no I/O.
package compat

import "fmt"

// Mode selects how aggressively an inbound request may be rewritten.
type Mode string

const (
	// ModeRaw forwards the caller's body byte-for-byte. Only meaningful for an
	// OpenAI-dialect upstream — a target needing dialect translation cannot honour
	// it, and the server rejects the combination rather than pretending.
	ModeRaw Mode = "raw"

	// ModeAuto rewrites only what is deterministically known to be wrong for the
	// resolved target: the max_tokens / max_completion_tokens spelling. Unknown
	// parameters pass through, so a provider-specific extension keeps working
	// without cerber having to learn about it first.
	ModeAuto Mode = "auto"

	// ModeForce is ModeAuto plus coercion for clients that cannot adapt: keys the
	// target does not accept are dropped rather than forwarded, and degenerate
	// values (an empty tools array, a tool_choice with no tools) are removed.
	ModeForce Mode = "force"
)

// DefaultMode is used when neither the request nor the config picks one.
const DefaultMode = ModeAuto

// ParseMode validates a mode string. An empty string yields def. The accepted
// spellings are the three constants; anything else is an error so a typo in a
// header surfaces instead of silently selecting the default.
func ParseMode(s string, def Mode) (Mode, error) {
	switch Mode(s) {
	case "":
		if def == "" {
			return DefaultMode, nil
		}
		return def, nil
	case ModeRaw:
		return ModeRaw, nil
	case ModeAuto:
		return ModeAuto, nil
	case ModeForce:
		return ModeForce, nil
	default:
		return "", fmt.Errorf("compat: unknown mode %q (want raw, auto or force)", s)
	}
}

// TranslatingTargets are the providers whose upstream speaks a different dialect,
// so the request body is rebuilt by the translator rather than forwarded. Raw
// mode is impossible for these.
func Translating(target string) bool {
	return target == "anthropic" || target == "gemini"
}
