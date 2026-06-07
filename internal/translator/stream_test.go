package translator

import (
	"errors"
	"strings"
	"testing"
)

const anthropicStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_7","model":"claude-x"}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}

event: message_stop
data: {"type":"message_stop"}
`

func TestStream_FullSequence(t *testing.T) {
	var buf strings.Builder
	flushes := 0
	err := fixedTr().StreamAnthropicToOpenAI(&buf, strings.NewReader(anthropicStream), func() { flushes++ })
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// role chunk, two content chunks, final stop chunk, DONE
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Error("missing role chunk")
	}
	if !strings.Contains(out, `"content":"Hel"`) || !strings.Contains(out, `"content":"lo"`) {
		t.Error("missing content chunks")
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Error("missing finish reason")
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Errorf("must end with DONE, got:\n%s", out)
	}
	if !strings.Contains(out, "chatcmpl-msg_7") {
		t.Error("id not propagated")
	}
	if flushes == 0 {
		t.Error("flush never called")
	}
}

func TestStream_EOFWithoutMessageStop(t *testing.T) {
	in := `data: {"type":"message_start","message":{"id":"m","model":"x"}}
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}
`
	var buf strings.Builder
	if err := fixedTr().StreamAnthropicToOpenAI(&buf, strings.NewReader(in), nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"finish_reason":"stop"`) || !strings.Contains(out, "[DONE]") {
		t.Errorf("EOF should still finish + DONE, got:\n%s", out)
	}
}

func TestStream_IgnoresNoiseAndBadJSON(t *testing.T) {
	in := `: comment line
event: only-event-no-data
data: not-json
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"x"}}
data: {"type":"message_stop"}
`
	var buf strings.Builder
	if err := fixedTr().StreamAnthropicToOpenAI(&buf, strings.NewReader(in), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"content":"x"`) {
		t.Errorf("should have emitted the one good delta:\n%s", buf.String())
	}
}

// errWriter fails after allowed successful writes, to exercise error paths.
type errWriter struct{ ok int }

func (w *errWriter) Write(p []byte) (int, error) {
	if w.ok <= 0 {
		return 0, errors.New("write fail")
	}
	w.ok--
	return len(p), nil
}

func TestStream_WriteErrorPropagates(t *testing.T) {
	// Fails on the very first emit (the role chunk).
	err := fixedTr().StreamAnthropicToOpenAI(&errWriter{ok: 0}, strings.NewReader(anthropicStream), nil)
	if err == nil {
		t.Fatal("expected write error to propagate")
	}
}

func TestStream_WriteErrorDuringDone(t *testing.T) {
	// Allow role + 2 content + final stop chunk writes, fail on the [DONE] write.
	err := fixedTr().StreamAnthropicToOpenAI(&errWriter{ok: 4}, strings.NewReader(anthropicStream), nil)
	if err == nil {
		t.Fatal("expected write error on DONE to propagate")
	}
}
