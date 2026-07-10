package anthropic

import (
	"encoding/json"
	"strings"
)

// claudeCodeAgentPrompt is the system prefix Anthropic requires on requests
// authenticated with a Claude Code OAuth token. Without it as the first system
// block, OAuth requests are rejected.
const claudeCodeAgentPrompt = "You are Claude Code, Anthropic's official CLI for Claude."

// fullCloakModelMarker gates the aggressive Claude Code cloak (see cloakClaudeCode)
// to requests whose model id contains this substring. Anthropic fingerprints the
// system[] content of OAuth traffic: a third-party agent's system prompt (tool
// guidelines, custom identity) is billed to metered "extra usage" instead of the
// subscription plan. The cloak moves that content out of system[] so the request
// classifies as genuine Claude Code and rides the plan. Scoped to one model on
// purpose — it is a targeted spoof, not a blanket rewrite of every request.
const fullCloakModelMarker = "claude-sonnet-5"

// oauthSystemForModel returns the OAuth system-prompt transform to apply for the
// given request body's model. Requests matching fullCloakModelMarker get the full
// cloak; all others get the minimal one-line prefix (injectClaudeCodeSystem).
func oauthSystemForModel(body []byte) func([]byte) ([]byte, error) {
	if strings.Contains(requestModel(body), fullCloakModelMarker) {
		return cloakClaudeCode
	}
	return injectClaudeCodeSystem
}

// requestModel extracts the top-level "model" field from a Messages request body.
// Returns "" if absent or unparseable.
func requestModel(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Model
}

// injectClaudeCodeSystem ensures the request's `system` field begins with the
// Claude Code agent block, preserving any caller-supplied system content after
// it. It is idempotent. This is the minimal spoof required for OAuth tokens to
// be accepted; cerber deliberately does not replicate Claude Code's full
// fingerprint (billing headers, static prompt, tool renaming).
func injectClaudeCodeSystem(body []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}

	agent := systemBlock{Type: "text", Text: claudeCodeAgentPrompt}
	blocks := []systemBlock{agent}

	if raw, ok := m["system"]; ok {
		existing, err := parseSystem(raw)
		if err != nil {
			return nil, err
		}
		// Idempotent: if already prefixed with the agent block, return unchanged.
		if len(existing) > 0 && existing[0].Type == "text" && existing[0].Text == claudeCodeAgentPrompt {
			return body, nil
		}
		blocks = append(blocks, existing...)
	}

	encoded, err := json.Marshal(blocks)
	if err != nil {
		return nil, err
	}
	m["system"] = encoded
	return json.Marshal(m)
}

// cloakClaudeCode makes an OAuth request classify as genuine Claude Code so it is
// billed to the subscription plan rather than metered "extra usage". Anthropic
// keys that decision on system[] content, so cloakClaudeCode:
//
//  1. sets system[] to ONLY the Claude Code agent block, and
//  2. relocates the caller's original system content, verbatim, into the first
//     user message wrapped in a <system-reminder> block (which is NOT billing-
//     classified), preserving the client's instructions.
//
// Tools are left untouched — empirically the system-prompt relocation alone is
// sufficient, so no response-side tool renaming is needed. It is idempotent: the
// agent block is not treated as relocatable content. If there is no user message
// to host the reminder, it falls back to injectClaudeCodeSystem's behaviour
// (agent block + system kept in place) so no content is silently dropped.
func cloakClaudeCode(body []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}

	var relocate []string
	if raw, ok := m["system"]; ok {
		existing, err := parseSystem(raw)
		if err != nil {
			return nil, err
		}
		for _, b := range existing {
			if b.Type != "text" {
				continue
			}
			if strings.TrimSpace(b.Text) == "" || b.Text == claudeCodeAgentPrompt {
				continue
			}
			relocate = append(relocate, b.Text)
		}
	}

	agent := []systemBlock{{Type: "text", Text: claudeCodeAgentPrompt}}
	encAgent, err := json.Marshal(agent)
	if err != nil {
		return nil, err
	}

	// Nothing to relocate: system is already clean, just enforce the agent block.
	if len(relocate) == 0 {
		m["system"] = encAgent
		return json.Marshal(m)
	}

	reminder := "<system-reminder>\n" +
		"As you answer the user's questions, you can use the following context from the system:\n" +
		strings.Join(relocate, "\n\n") + "\n\n" +
		"IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this context unless it is highly relevant to your task.\n" +
		"</system-reminder>\n"

	msgs, ok, err := prependToFirstUserMessage(m["messages"], reminder)
	if err != nil {
		return nil, err
	}
	if !ok {
		// No user message to carry the reminder — fall back to keeping the system
		// content in place (after the agent block) rather than dropping it.
		blocks := append(agent, systemBlock{Type: "text", Text: strings.Join(relocate, "\n\n")})
		enc, err := json.Marshal(blocks)
		if err != nil {
			return nil, err
		}
		m["system"] = enc
		return json.Marshal(m)
	}

	m["system"] = encAgent
	m["messages"] = msgs
	return json.Marshal(m)
}

// prependToFirstUserMessage prepends text to the content of the first user
// message in a Messages `messages` array. It reports ok=false (leaving the input
// untouched) if there is no user message. String content stays a string; block-
// array content gets a leading text block.
func prependToFirstUserMessage(raw json.RawMessage, text string) (json.RawMessage, bool, error) {
	if len(raw) == 0 {
		return raw, false, nil
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil, false, err
	}
	for i, mraw := range msgs {
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(mraw, &msg); err != nil {
			return nil, false, err
		}
		var role string
		_ = json.Unmarshal(msg["role"], &role)
		if role != "user" {
			continue
		}
		newContent, err := prependToContent(msg["content"], text)
		if err != nil {
			return nil, false, err
		}
		msg["content"] = newContent
		encMsg, err := json.Marshal(msg)
		if err != nil {
			return nil, false, err
		}
		msgs[i] = encMsg
		out, err := json.Marshal(msgs)
		if err != nil {
			return nil, false, err
		}
		return out, true, nil
	}
	return raw, false, nil
}

// prependToContent prepends text to a Messages content value, which is either a
// string or an array of content blocks.
func prependToContent(content json.RawMessage, text string) (json.RawMessage, error) {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return json.Marshal(text + s)
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, err
	}
	block, err := json.Marshal(systemBlock{Type: "text", Text: text})
	if err != nil {
		return nil, err
	}
	return json.Marshal(append([]json.RawMessage{block}, blocks...))
}

type systemBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

// parseSystem normalizes an Anthropic `system` value (string | []block) into
// blocks. A non-empty string becomes a single text block; null/empty yields none.
func parseSystem(raw json.RawMessage) ([]systemBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []systemBlock{{Type: "text", Text: s}}, nil
	}
	var blocks []systemBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}
