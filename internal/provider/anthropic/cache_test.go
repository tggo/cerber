package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// big returns a string long enough that estTokens(raw) comfortably exceeds a
// MinTokens of 1024 (est = bytes/4, so ~6000 bytes -> ~1500 est tokens).
func big() string { return strings.Repeat("lorem ipsum dolor sit amet ", 240) }

func countCacheControl(t *testing.T, body []byte) int {
	t.Helper()
	return bytes.Count(body, []byte(`"type":"ephemeral"`))
}

// mustValidJSON fails if body is not valid JSON (guards against corrupting the
// request shape).
func mustValidJSON(t *testing.T, body []byte) {
	t.Helper()
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("injected body is not valid JSON: %v\n%s", err, body)
	}
}

const minTok = 1024

func moderate() CacheOptions {
	return CacheOptions{Enabled: true, Strategy: CacheModerate, MinTokens: minTok}
}

func TestInject_Disabled_NoOp(t *testing.T) {
	in := []byte(`{"model":"x","system":"` + big() + `"}`)
	out, changed, err := InjectCacheBreakpoints(in, CacheOptions{Enabled: false})
	if err != nil || changed {
		t.Fatalf("disabled: changed=%v err=%v", changed, err)
	}
	if !bytes.Equal(out, in) {
		t.Error("disabled must return the original bytes")
	}
}

func TestInject_ClientCacheControl_Untouched(t *testing.T) {
	in := []byte(`{"system":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}`)
	out, changed, err := InjectCacheBreakpoints(in, moderate())
	if err != nil || changed {
		t.Fatalf("client cache_control: changed=%v err=%v", changed, err)
	}
	if !bytes.Equal(out, in) {
		t.Error("must not touch a request that already has cache_control")
	}
}

func TestInject_StringSystem_BecomesCachedBlock(t *testing.T) {
	sys := big()
	in := []byte(`{"model":"x","system":` + mustJSON(t, sys) + `,"messages":[]}`)
	out, changed, err := InjectCacheBreakpoints(in, moderate())
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	mustValidJSON(t, out)
	if n := countCacheControl(t, out); n != 1 {
		t.Fatalf("want 1 breakpoint, got %d: %s", n, out)
	}
	// system must now be an array whose (only) block carries the original text.
	var m map[string]json.RawMessage
	_ = json.Unmarshal(out, &m)
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(m["system"], &blocks); err != nil {
		t.Fatalf("system is not a block array: %v", err)
	}
	var text string
	_ = json.Unmarshal(blocks[0]["text"], &text)
	if text != sys {
		t.Errorf("system text mangled: got %q", text[:20])
	}
}

func TestInject_SmallSystem_BelowMin_NoOp(t *testing.T) {
	in := []byte(`{"system":"tiny prompt","messages":[]}`)
	_, changed, err := InjectCacheBreakpoints(in, moderate())
	if err != nil || changed {
		t.Fatalf("below-min system should be a no-op: changed=%v err=%v", changed, err)
	}
}

func TestInject_ArraySystem_LastBlockMarked(t *testing.T) {
	in := []byte(`{"system":[{"type":"text","text":"a"},{"type":"text","text":"` + big() + `"}]}`)
	out, changed, err := InjectCacheBreakpoints(in, moderate())
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(out, &m)
	var blocks []map[string]json.RawMessage
	_ = json.Unmarshal(m["system"], &blocks)
	if _, ok := blocks[0]["cache_control"]; ok {
		t.Error("only the LAST system block should be marked")
	}
	if _, ok := blocks[1]["cache_control"]; !ok {
		t.Error("last system block should be marked")
	}
}

func TestInject_StrategyBreakpointCounts(t *testing.T) {
	tools := `[{"name":"a","description":"` + big() + `"},{"name":"b","description":"` + big() + `"}]`
	sys := mustJSON(t, big())
	// two messages with block-form content, each large.
	msgs := `[` +
		`{"role":"user","content":[{"type":"text","text":"` + big() + `"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"ok"}]},` +
		`{"role":"user","content":[{"type":"text","text":"` + big() + `"}]}` +
		`]`
	in := []byte(`{"tools":` + tools + `,"system":` + sys + `,"messages":` + msgs + `}`)

	cases := []struct {
		strat CacheStrategy
		want  int
	}{
		{CacheConservative, 2}, // system + tools
		{CacheModerate, 3},     // + 1 message
		{CacheAggressive, 4},   // + 2nd message
	}
	for _, c := range cases {
		out, changed, err := InjectCacheBreakpoints(in, CacheOptions{Enabled: true, Strategy: c.strat, MinTokens: minTok})
		if err != nil || !changed {
			t.Fatalf("%s: changed=%v err=%v", c.strat, changed, err)
		}
		mustValidJSON(t, out)
		if n := countCacheControl(t, out); n != c.want {
			t.Errorf("%s: want %d breakpoints, got %d", c.strat, c.want, n)
		}
	}
}

func TestInject_StringMessageContent_NotReshaped(t *testing.T) {
	// system below min, tools absent, messages are string-content -> nothing to do.
	in := []byte(`{"system":"tiny","messages":[{"role":"user","content":"` + big() + `"}]}`)
	_, changed, err := InjectCacheBreakpoints(in, CacheOptions{Enabled: true, Strategy: CacheAggressive, MinTokens: minTok})
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("string message content must never be reshaped into blocks")
	}
}

func TestInject_CumulativeGate_MessagesOnly(t *testing.T) {
	// small system + small tools individually below min, but a big block message
	// pushes the cumulative prefix over the threshold -> one message breakpoint.
	in := []byte(`{"system":"tiny","tools":[{"name":"x"}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"` + big() + `"}]}]}`)
	out, changed, err := InjectCacheBreakpoints(in, moderate())
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if n := countCacheControl(t, out); n != 1 {
		t.Fatalf("want 1 (message) breakpoint, got %d: %s", n, out)
	}
}

func TestInject_MalformedBody_ReturnsOriginalAndError(t *testing.T) {
	in := []byte(`{not json`)
	out, changed, err := InjectCacheBreakpoints(in, moderate())
	if err == nil {
		t.Fatal("want error on malformed body")
	}
	if changed || !bytes.Equal(out, in) {
		t.Error("malformed body must be returned unchanged so the caller can forward it")
	}
}

func TestInject_NothingQualifies_ReturnsOriginal(t *testing.T) {
	in := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	out, changed, err := InjectCacheBreakpoints(in, moderate())
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !bytes.Equal(out, in) {
		t.Error("no-op must return original bytes")
	}
}

func TestInject_ToolsOnly_Conservative(t *testing.T) {
	in := []byte(`{"tools":[{"name":"a","description":"` + big() + `"}],"messages":[]}`)
	out, changed, err := InjectCacheBreakpoints(in, CacheOptions{Enabled: true, Strategy: CacheConservative, MinTokens: minTok})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if n := countCacheControl(t, out); n != 1 {
		t.Fatalf("tools-only: want 1 breakpoint, got %d", n)
	}
}

func TestInject_NonObjectArrayElem_Skipped(t *testing.T) {
	// tools whose last element is not an object can't be marked; with no system
	// present there is nothing else to place, so the request is left unchanged.
	in := []byte(`{"tools":["` + big() + `"],"messages":[]}`)
	_, changed, err := InjectCacheBreakpoints(in, CacheOptions{Enabled: true, Strategy: CacheConservative, MinTokens: minTok})
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("non-object tool element must be skipped, not corrupted")
	}
}

func TestStrategyBudget(t *testing.T) {
	for s, want := range map[CacheStrategy]int{
		CacheConservative: 2, CacheModerate: 3, CacheAggressive: 4, "": 3, "bogus": 3,
	} {
		if got := s.budget(); got != want {
			t.Errorf("budget(%q) = %d, want %d", s, got, want)
		}
	}
}

// mustJSON marshals a Go value to a compact JSON string for embedding in test
// request bodies (handles escaping of the large repeated text).
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
