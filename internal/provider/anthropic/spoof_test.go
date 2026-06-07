package anthropic

import (
	"encoding/json"
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
