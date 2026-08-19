package compat

import (
	"encoding/json"
	"strings"
	"sync"
)

// The two spellings of the output-length cap. Every OpenAI-compatible upstream
// accepts exactly one of them and 400s on the other.
const (
	MaxTokens           = "max_tokens"
	MaxCompletionTokens = "max_completion_tokens"
)

// completionTokenModels are the model-name markers whose upstream requires
// max_completion_tokens. OpenAI's reasoning and gpt-5 families rejected the older
// spelling; everything else — xAI, ollama, vLLM, older OpenAI chat models — still
// wants max_tokens. This table is a starting guess only: Normalizer.Learn
// overrides it from what the upstream actually said, so a model nobody listed
// here self-corrects after one request.
var completionTokenModels = []string{
	"gpt-5",
	"o1",
	"o3",
	"o4",
}

// openAIParams is the allowlist ModeForce keeps for an OpenAI-dialect upstream.
// Anything outside it is dropped, on the theory that a client which cannot pick
// the right token parameter also cannot be trusted to only send real ones.
var openAIParams = map[string]bool{
	"model": true, "messages": true, "stream": true, "stream_options": true,
	"temperature": true, "top_p": true, "n": true, "stop": true, "seed": true,
	"presence_penalty": true, "frequency_penalty": true, "logit_bias": true,
	"logprobs": true, "top_logprobs": true, "response_format": true, "user": true,
	"tools": true, "tool_choice": true, "parallel_tool_calls": true,
	"reasoning_effort": true,
	MaxTokens:          true, MaxCompletionTokens: true,
}

// Normalizer rewrites request bodies for a target upstream and remembers what
// each model actually accepts. It is safe for concurrent use.
type Normalizer struct {
	mu      sync.RWMutex
	learned map[string]string // model -> the token parameter that upstream accepted
}

// NewNormalizer builds an empty Normalizer.
func NewNormalizer() *Normalizer { return &Normalizer{learned: map[string]string{}} }

// TokenParam reports which output-length parameter the given model expects:
// what the upstream taught us if it ever corrected us, otherwise the static
// guess from the model name.
func (n *Normalizer) TokenParam(model string) string {
	n.mu.RLock()
	p, ok := n.learned[model]
	n.mu.RUnlock()
	if ok {
		return p
	}
	lower := strings.ToLower(model)
	for _, marker := range completionTokenModels {
		if strings.Contains(lower, marker) {
			return MaxCompletionTokens
		}
	}
	return MaxTokens
}

// Learn records that model wants param, so later requests get it right first
// time. Called after an upstream rejection identified by TokenParamRejection.
func (n *Normalizer) Learn(model, param string) {
	if model == "" || (param != MaxTokens && param != MaxCompletionTokens) {
		return
	}
	n.mu.Lock()
	n.learned[model] = param
	n.mu.Unlock()
}

// Learned returns a copy of the learned table, for diagnostics.
func (n *Normalizer) Learned() map[string]string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make(map[string]string, len(n.learned))
	for k, v := range n.learned {
		out[k] = v
	}
	return out
}

// Apply rewrites an OpenAI-dialect body for the given mode and model. ModeRaw
// returns the body untouched. It reports whether anything changed, so a caller
// can skip re-serializing an already-correct request.
//
// It is only for upstreams that speak the OpenAI dialect; a translating target
// gets its body rebuilt field by field by the translator, which is normalizing
// by construction.
func (n *Normalizer) Apply(mode Mode, model string, body []byte) (out []byte, changed bool, err error) {
	if mode == ModeRaw {
		return body, false, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		// Not an object we can reason about; leave it to the upstream to reject.
		return body, false, nil
	}

	if renameTokenParam(m, n.TokenParam(model)) {
		changed = true
	}
	if mode == ModeForce && coerce(m) {
		changed = true
	}
	if !changed {
		return body, false, nil
	}
	out, err = json.Marshal(m)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// SetTokenParam rewrites a body to use want as the output-length parameter,
// regardless of mode. Used for the one-shot retry after an upstream rejection,
// where the client's choice has already been proven wrong.
func SetTokenParam(body []byte, want string) ([]byte, bool, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false, nil
	}
	if !renameTokenParam(m, want) {
		return body, false, nil
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// renameTokenParam moves whichever token parameter is present to the `want`
// spelling. When both are present the wanted one wins and the other is dropped,
// because sending both is itself a 400 on some upstreams.
func renameTokenParam(m map[string]json.RawMessage, want string) bool {
	other := MaxTokens
	if want == MaxTokens {
		other = MaxCompletionTokens
	}
	v, hasOther := m[other]
	if !hasOther {
		return false
	}
	delete(m, other)
	if _, hasWant := m[want]; !hasWant {
		m[want] = v
	}
	return true
}

// coerce is the ModeForce extra: drop parameters the OpenAI dialect does not
// define, and remove degenerate tool fields that make upstreams 400.
func coerce(m map[string]json.RawMessage) bool {
	changed := false
	for k := range m {
		if !openAIParams[k] {
			delete(m, k)
			changed = true
		}
	}
	// An empty tools array is rejected by several upstreams; so is a tool_choice
	// naming a tool that was never declared.
	if raw, ok := m["tools"]; ok && emptyArray(raw) {
		delete(m, "tools")
		changed = true
	}
	if _, hasTools := m["tools"]; !hasTools {
		if _, ok := m["tool_choice"]; ok {
			delete(m, "tool_choice")
			changed = true
		}
		if _, ok := m["parallel_tool_calls"]; ok {
			delete(m, "parallel_tool_calls")
			changed = true
		}
	}
	return changed
}

func emptyArray(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "[]" || s == "null"
}

// TokenParamRejection inspects a failed upstream response and reports which
// token parameter to try instead. ok is false when the failure was about
// something else — the caller must not retry then.
//
// The rule is deliberately blunt: if a 400 talks about either spelling at all,
// the other one is worth exactly one attempt. Trying to read *which* spelling the
// upstream wants from the wording does not survive contact with reality — OpenAI
// names the rejected field and the remedy, xAI names only the rejected field, and
// a third upstream will phrase it a third way. A retry that was not the problem
// simply fails again and the original error is what the client sees.
func TokenParamRejection(status int, body []byte, sent string) (want string, ok bool) {
	if status != 400 && status != 422 {
		return "", false
	}
	if sent != MaxTokens && sent != MaxCompletionTokens {
		return "", false
	}
	text := strings.ToLower(string(body))
	if !strings.Contains(text, MaxTokens) && !strings.Contains(text, MaxCompletionTokens) {
		return "", false
	}
	if sent == MaxTokens {
		return MaxCompletionTokens, true
	}
	return MaxTokens, true
}

// SentTokenParam reports which token parameter a body carries, or "" for neither.
func SentTokenParam(body []byte) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	if _, ok := m[MaxCompletionTokens]; ok {
		return MaxCompletionTokens
	}
	if _, ok := m[MaxTokens]; ok {
		return MaxTokens
	}
	return ""
}
