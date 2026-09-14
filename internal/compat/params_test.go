package compat

import (
	"encoding/json"
	"testing"
)

func TestTokenParamStaticGuess(t *testing.T) {
	n := NewNormalizer()
	tests := []struct {
		model string
		want  string
	}{
		{"gpt-5.2", MaxCompletionTokens},
		{"GPT-5-mini", MaxCompletionTokens},
		{"o3-pro", MaxCompletionTokens},
		{"gpt-4o", MaxTokens},
		{"grok-4", MaxTokens},
		{"qwen3-coder", MaxTokens},
		{"", MaxTokens},
	}
	for _, tc := range tests {
		if got := n.TokenParam(tc.model); got != tc.want {
			t.Errorf("TokenParam(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}

func TestLearnOverridesGuess(t *testing.T) {
	n := NewNormalizer()
	// A model the static table would guess wrong.
	n.Learn("gpt-5.2", MaxTokens)
	if got := n.TokenParam("gpt-5.2"); got != MaxTokens {
		t.Errorf("after Learn, TokenParam = %q, want %q", got, MaxTokens)
	}
	if got := n.Learned()["gpt-5.2"]; got != MaxTokens {
		t.Errorf("Learned() = %v", n.Learned())
	}
	// Junk is ignored so a bad parse cannot poison the table.
	n.Learn("", MaxTokens)
	n.Learn("grok-4", "maximum_tokens")
	if len(n.Learned()) != 1 {
		t.Errorf("Learned() = %v, want only the valid entry", n.Learned())
	}
}

func TestApply(t *testing.T) {
	tests := []struct {
		name        string
		mode        Mode
		model       string
		body        string
		wantChanged bool
		wantKeys    map[string]bool // key -> must be present
	}{
		{
			name: "raw leaves the body alone even when wrong",
			mode: ModeRaw, model: "gpt-5.2",
			body:        `{"model":"gpt-5.2","max_tokens":16}`,
			wantChanged: false,
		},
		{
			name: "auto renames max_tokens for a completion-token model",
			mode: ModeAuto, model: "gpt-5.2",
			body:        `{"model":"gpt-5.2","max_tokens":16}`,
			wantChanged: true,
			wantKeys:    map[string]bool{MaxCompletionTokens: true, MaxTokens: false},
		},
		{
			name: "auto renames max_completion_tokens back for xAI",
			mode: ModeAuto, model: "grok-4",
			body:        `{"model":"grok-4","max_completion_tokens":16}`,
			wantChanged: true,
			wantKeys:    map[string]bool{MaxTokens: true, MaxCompletionTokens: false},
		},
		{
			name: "already correct is left untouched",
			mode: ModeAuto, model: "grok-4",
			body:        `{"model":"grok-4","max_tokens":16}`,
			wantChanged: false,
		},
		{
			name: "both spellings collapse to the wanted one",
			mode: ModeAuto, model: "gpt-5.2",
			body:        `{"max_tokens":16,"max_completion_tokens":32}`,
			wantChanged: true,
			wantKeys:    map[string]bool{MaxCompletionTokens: true, MaxTokens: false},
		},
		{
			name: "auto keeps unknown provider extensions",
			mode: ModeAuto, model: "grok-4",
			body:        `{"model":"grok-4","max_tokens":1,"search_parameters":{"mode":"on"}}`,
			wantChanged: false,
			wantKeys:    map[string]bool{"search_parameters": true},
		},
		{
			name: "force drops unknown parameters",
			mode: ModeForce, model: "grok-4",
			body:        `{"model":"grok-4","max_tokens":1,"search_parameters":{"mode":"on"}}`,
			wantChanged: true,
			wantKeys:    map[string]bool{"search_parameters": false, "model": true, MaxTokens: true},
		},
		{
			name: "force drops an empty tools array and its orphaned selector",
			mode: ModeForce, model: "grok-4",
			body:        `{"model":"grok-4","tools":[],"tool_choice":"auto","parallel_tool_calls":true}`,
			wantChanged: true,
			wantKeys:    map[string]bool{"tools": false, "tool_choice": false, "parallel_tool_calls": false},
		},
		{
			name: "force keeps real tools",
			mode: ModeForce, model: "grok-4",
			body:        `{"model":"grok-4","tools":[{"type":"function","function":{"name":"x"}}],"tool_choice":"auto"}`,
			wantChanged: false,
			wantKeys:    map[string]bool{"tools": true, "tool_choice": true},
		},
		{
			name: "a non-object body is passed through",
			mode: ModeForce, model: "grok-4",
			body:        `[1,2,3]`,
			wantChanged: false,
		},
	}
	n := NewNormalizer()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, changed, err := n.Apply(tc.mode, tc.model, []byte(tc.body))
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v (out=%s)", changed, tc.wantChanged, out)
			}
			if tc.wantKeys == nil {
				return
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("unmarshal result: %v", err)
			}
			for k, want := range tc.wantKeys {
				if _, got := m[k]; got != want {
					t.Errorf("key %q present = %v, want %v (out=%s)", k, got, want, out)
				}
			}
		})
	}
}

func TestSetTokenParam(t *testing.T) {
	out, changed, err := SetTokenParam([]byte(`{"max_tokens":8}`), MaxCompletionTokens)
	if err != nil || !changed {
		t.Fatalf("SetTokenParam: changed=%v err=%v", changed, err)
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(out, &m)
	if string(m[MaxCompletionTokens]) != "8" {
		t.Errorf("out = %s", out)
	}
	if _, _, err := SetTokenParam([]byte(`not json`), MaxTokens); err != nil {
		t.Errorf("invalid body should pass through, got %v", err)
	}
	if _, changed, _ := SetTokenParam([]byte(`{"max_tokens":8}`), MaxTokens); changed {
		t.Error("already-correct body reported as changed")
	}
}

func TestTokenParamRejection(t *testing.T) {
	const openaiErr = `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.","param":"max_tokens"}}`
	const xaiErr = `{"error":"Unrecognized request argument supplied: max_completion_tokens"}`

	tests := []struct {
		name   string
		status int
		body   string
		sent   string
		want   string
		ok     bool
	}{
		{name: "openai asks for max_completion_tokens", status: 400, body: openaiErr, sent: MaxTokens, want: MaxCompletionTokens, ok: true},
		{name: "xai asks for max_tokens", status: 400, body: xaiErr, sent: MaxCompletionTokens, want: MaxTokens, ok: true},
		{name: "422 counts too", status: 422, body: openaiErr, sent: MaxTokens, want: MaxCompletionTokens, ok: true},
		{name: "a 500 is never this", status: 500, body: openaiErr, sent: MaxTokens},
		{name: "unrelated 400", status: 400, body: `{"error":"model not found"}`, sent: MaxTokens},
		{name: "an error naming only the sent spelling still swaps", status: 400, body: `{"error":"max_tokens is not supported"}`, sent: MaxTokens, want: MaxCompletionTokens, ok: true},
		{name: "no token param sent at all", status: 400, body: openaiErr, sent: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := TokenParamRejection(tc.status, []byte(tc.body), tc.sent)
			if ok != tc.ok || got != tc.want {
				t.Errorf("= %q,%v want %q,%v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestSentTokenParam(t *testing.T) {
	tests := []struct{ body, want string }{
		{`{"max_tokens":1}`, MaxTokens},
		{`{"max_completion_tokens":1}`, MaxCompletionTokens},
		{`{"max_tokens":1,"max_completion_tokens":1}`, MaxCompletionTokens},
		{`{"model":"x"}`, ""},
		{`nope`, ""},
	}
	for _, tc := range tests {
		if got := SentTokenParam([]byte(tc.body)); got != tc.want {
			t.Errorf("SentTokenParam(%s) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestRequestStreamUsage(t *testing.T) {
	for _, tc := range []struct {
		name, in     string
		wantInjected bool
		want         string // expected stream_options after (JSON), "" = body untouched
	}{
		{"stream without options", `{"model":"m","stream":true}`, true, `{"include_usage":true}`},
		{"keeps other stream_options keys", `{"stream":true,"stream_options":{"continuous_usage_stats":true}}`, true, `{"continuous_usage_stats":true,"include_usage":true}`},
		{"client declined", `{"stream":true,"stream_options":{"include_usage":false}}`, true, `{"include_usage":true}`},
		{"null stream_options", `{"stream":true,"stream_options":null}`, true, `{"include_usage":true}`},
		{"client already asked", `{"stream":true,"stream_options":{"include_usage":true}}`, false, ""},
		{"not streaming", `{"stream":false}`, false, ""},
		{"no stream field", `{"model":"m"}`, false, ""},
		{"stream not a bool", `{"stream":"yes"}`, false, ""},
		{"malformed stream_options", `{"stream":true,"stream_options":"x"}`, false, ""},
		{"not JSON", `{`, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, injected, err := RequestStreamUsage([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			if injected != tc.wantInjected {
				t.Fatalf("injected = %v, want %v", injected, tc.wantInjected)
			}
			if !injected {
				if string(out) != tc.in {
					t.Errorf("body changed: %s", out)
				}
				return
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatal(err)
			}
			if string(m["stream_options"]) != tc.want {
				t.Errorf("stream_options = %s, want %s", m["stream_options"], tc.want)
			}
		})
	}
}

func TestStreamOptionsRejection(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{400, `{"error":"Unrecognized request argument supplied: stream_options"}`, true},
		{422, `{"detail":"extra field Stream_Options not permitted"}`, true},
		{400, `{"error":"invalid messages"}`, false},
		{500, `stream_options exploded`, false},
	} {
		if got := StreamOptionsRejection(tc.status, []byte(tc.body)); got != tc.want {
			t.Errorf("StreamOptionsRejection(%d, %s) = %v, want %v", tc.status, tc.body, got, tc.want)
		}
	}
}
