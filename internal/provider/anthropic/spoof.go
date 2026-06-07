package anthropic

import "encoding/json"

// claudeCodeAgentPrompt is the system prefix Anthropic requires on requests
// authenticated with a Claude Code OAuth token. Without it as the first system
// block, OAuth requests are rejected.
const claudeCodeAgentPrompt = "You are Claude Code, Anthropic's official CLI for Claude."

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
