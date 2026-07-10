package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

func systemOf(t *testing.T, body []byte) []systemBlock {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	blocks, err := parseSystem(m["system"])
	if err != nil {
		t.Fatal(err)
	}
	return blocks
}

func TestInjectClaudeCodeSystem(t *testing.T) {
	t.Run("no system field", func(t *testing.T) {
		out, err := injectClaudeCodeSystem([]byte(`{"model":"c","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		b := systemOf(t, out)
		if len(b) != 1 || b[0].Text != claudeCodeAgentPrompt {
			t.Errorf("blocks = %+v", b)
		}
	})

	t.Run("string system preserved after agent", func(t *testing.T) {
		out, err := injectClaudeCodeSystem([]byte(`{"system":"be brief","model":"c"}`))
		if err != nil {
			t.Fatal(err)
		}
		b := systemOf(t, out)
		if len(b) != 2 || b[0].Text != claudeCodeAgentPrompt || b[1].Text != "be brief" {
			t.Errorf("blocks = %+v", b)
		}
	})

	t.Run("array system preserved", func(t *testing.T) {
		out, err := injectClaudeCodeSystem([]byte(`{"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		b := systemOf(t, out)
		if len(b) != 3 || b[0].Text != claudeCodeAgentPrompt || b[1].Text != "a" || b[2].Text != "b" {
			t.Errorf("blocks = %+v", b)
		}
	})

	t.Run("idempotent when already prefixed", func(t *testing.T) {
		in := []byte(`{"system":[{"type":"text","text":"` + claudeCodeAgentPrompt + `"},{"type":"text","text":"x"}]}`)
		out, err := injectClaudeCodeSystem(in)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != string(in) {
			t.Errorf("should be unchanged:\n in=%s\nout=%s", in, out)
		}
	})

	t.Run("null system", func(t *testing.T) {
		out, err := injectClaudeCodeSystem([]byte(`{"system":null}`))
		if err != nil {
			t.Fatal(err)
		}
		b := systemOf(t, out)
		if len(b) != 1 || b[0].Text != claudeCodeAgentPrompt {
			t.Errorf("blocks = %+v", b)
		}
	})

	t.Run("empty string system", func(t *testing.T) {
		out, err := injectClaudeCodeSystem([]byte(`{"system":""}`))
		if err != nil {
			t.Fatal(err)
		}
		if b := systemOf(t, out); len(b) != 1 {
			t.Errorf("blocks = %+v", b)
		}
	})

	t.Run("bad json body", func(t *testing.T) {
		if _, err := injectClaudeCodeSystem([]byte(`{bad`)); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("invalid system type", func(t *testing.T) {
		if _, err := injectClaudeCodeSystem([]byte(`{"system":123}`)); err == nil {
			t.Fatal("expected error")
		}
	})
}

// firstUserContent returns the first user message's content, decoded as either a
// raw string or a slice of blocks, for assertions.
func firstUserContent(t *testing.T, body []byte) (string, []systemBlock) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(m["messages"], &msgs); err != nil {
		t.Fatal(err)
	}
	for _, msg := range msgs {
		var role string
		_ = json.Unmarshal(msg["role"], &role)
		if role != "user" {
			continue
		}
		var s string
		if err := json.Unmarshal(msg["content"], &s); err == nil {
			return s, nil
		}
		var blocks []systemBlock
		if err := json.Unmarshal(msg["content"], &blocks); err != nil {
			t.Fatalf("content not string or blocks: %v", err)
		}
		return "", blocks
	}
	t.Fatal("no user message")
	return "", nil
}

func TestRequestModel(t *testing.T) {
	if got := requestModel([]byte(`{"model":"claude-sonnet-5"}`)); got != "claude-sonnet-5" {
		t.Errorf("model = %q", got)
	}
	if got := requestModel([]byte(`{}`)); got != "" {
		t.Errorf("missing model = %q", got)
	}
	if got := requestModel([]byte(`{bad`)); got != "" {
		t.Errorf("bad body model = %q", got)
	}
}

func TestOAuthSystemForModel(t *testing.T) {
	// Every gated family (incl. dated variants) routes to the full cloak.
	for _, model := range []string{
		"claude-sonnet-5",
		"claude-sonnet-5-20250929",
		"claude-opus-4-8",
		"claude-haiku-4-5-20251001",
	} {
		body := []byte(`{"model":"` + model + `","system":"be brief","messages":[{"role":"user","content":"hi"}]}`)
		out, err := oauthSystemForModel(body)(body)
		if err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		if b := systemOf(t, out); len(b) != 1 || b[0].Text != claudeCodeAgentPrompt {
			t.Errorf("%s: cloak should leave only agent block, got %+v", model, b)
		}
		if s, _ := firstUserContent(t, out); !strings.Contains(s, "be brief") || !strings.Contains(s, "system-reminder") {
			t.Errorf("%s: relocated system missing from user message: %q", model, s)
		}
	}

	// A non-gated model keeps the system in place (prefix behaviour).
	prefixBody := []byte(`{"model":"claude-3-5-haiku-20241022","system":"be brief","messages":[{"role":"user","content":"hi"}]}`)
	out2, err := oauthSystemForModel(prefixBody)(prefixBody)
	if err != nil {
		t.Fatal(err)
	}
	if b := systemOf(t, out2); len(b) != 2 || b[1].Text != "be brief" {
		t.Errorf("prefix should preserve system, got %+v", b)
	}
}

func TestCloakClaudeCode(t *testing.T) {
	t.Run("relocates string system into first user message", func(t *testing.T) {
		in := []byte(`{"model":"claude-sonnet-5","system":"you are pi agent","messages":[{"role":"user","content":"about repo?"}]}`)
		out, err := cloakClaudeCode(in)
		if err != nil {
			t.Fatal(err)
		}
		b := systemOf(t, out)
		if len(b) != 1 || b[0].Text != claudeCodeAgentPrompt {
			t.Errorf("system[] = %+v, want only agent block", b)
		}
		s, blocks := firstUserContent(t, out)
		if blocks != nil {
			t.Fatalf("string content should stay a string, got blocks %+v", blocks)
		}
		if !strings.Contains(s, "you are pi agent") || !strings.HasSuffix(s, "about repo?") {
			t.Errorf("user content = %q", s)
		}
	})

	t.Run("relocates array system and preserves user blocks", func(t *testing.T) {
		in := []byte(`{"system":[{"type":"text","text":"sys A"},{"type":"text","text":"sys B"}],` +
			`"messages":[{"role":"user","content":[{"type":"text","text":"orig"}]}]}`)
		out, err := cloakClaudeCode(in)
		if err != nil {
			t.Fatal(err)
		}
		if b := systemOf(t, out); len(b) != 1 || b[0].Text != claudeCodeAgentPrompt {
			t.Errorf("system[] = %+v", b)
		}
		_, blocks := firstUserContent(t, out)
		if len(blocks) != 2 || blocks[0].Type != "text" || blocks[1].Text != "orig" {
			t.Errorf("blocks = %+v", blocks)
		}
		if !strings.Contains(blocks[0].Text, "sys A") || !strings.Contains(blocks[0].Text, "sys B") {
			t.Errorf("relocated block = %q", blocks[0].Text)
		}
	})

	t.Run("no system leaves only agent block", func(t *testing.T) {
		out, err := cloakClaudeCode([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if b := systemOf(t, out); len(b) != 1 || b[0].Text != claudeCodeAgentPrompt {
			t.Errorf("blocks = %+v", b)
		}
		if s, _ := firstUserContent(t, out); s != "hi" {
			t.Errorf("user content should be untouched, got %q", s)
		}
	})

	t.Run("agent prompt in system is not relocated", func(t *testing.T) {
		in := []byte(`{"system":[{"type":"text","text":"` + claudeCodeAgentPrompt + `"}],"messages":[{"role":"user","content":"hi"}]}`)
		out, err := cloakClaudeCode(in)
		if err != nil {
			t.Fatal(err)
		}
		if s, _ := firstUserContent(t, out); s != "hi" {
			t.Errorf("nothing should be relocated, user content = %q", s)
		}
	})

	t.Run("no user message falls back to system in place", func(t *testing.T) {
		in := []byte(`{"system":"keep me","messages":[{"role":"assistant","content":"prev"}]}`)
		out, err := cloakClaudeCode(in)
		if err != nil {
			t.Fatal(err)
		}
		b := systemOf(t, out)
		if len(b) != 2 || b[0].Text != claudeCodeAgentPrompt || b[1].Text != "keep me" {
			t.Errorf("fallback system = %+v", b)
		}
	})

	t.Run("no messages field falls back", func(t *testing.T) {
		out, err := cloakClaudeCode([]byte(`{"system":"keep me"}`))
		if err != nil {
			t.Fatal(err)
		}
		if b := systemOf(t, out); len(b) != 2 || b[1].Text != "keep me" {
			t.Errorf("fallback system = %+v", b)
		}
	})

	t.Run("bad json body", func(t *testing.T) {
		if _, err := cloakClaudeCode([]byte(`{bad`)); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("invalid system type", func(t *testing.T) {
		if _, err := cloakClaudeCode([]byte(`{"system":123,"messages":[]}`)); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("invalid messages type", func(t *testing.T) {
		if _, err := cloakClaudeCode([]byte(`{"system":"x","messages":123}`)); err == nil {
			t.Fatal("expected error")
		}
	})
}
