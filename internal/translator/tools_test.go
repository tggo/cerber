package translator

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const weatherTool = `{"type":"function","function":{"name":"get_weather","description":"look it up","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}`

// decodeAnthropic converts an OpenAI body and unmarshals the Anthropic result.
func decodeAnthropic(t *testing.T, body string) anthropicRequest {
	t.Helper()
	out, _, err := fixedTr().OpenAIToAnthropic([]byte(body))
	if err != nil {
		t.Fatalf("OpenAIToAnthropic: %v", err)
	}
	var ar anthropicRequest
	if err := json.Unmarshal(out, &ar); err != nil {
		t.Fatalf("unmarshal anthropic request: %v", err)
	}
	return ar
}

func TestOpenAIToAnthropic_ToolDeclarations(t *testing.T) {
	ar := decodeAnthropic(t, `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"weather?"}],"tools":[`+weatherTool+`]}`)
	if len(ar.Tools) != 1 {
		t.Fatalf("tools = %+v, want 1", ar.Tools)
	}
	got := ar.Tools[0]
	if got.Name != "get_weather" || got.Description != "look it up" {
		t.Errorf("tool = %+v", got)
	}
	if !strings.Contains(string(got.InputSchema), `"city"`) {
		t.Errorf("input_schema = %s", got.InputSchema)
	}
}

func TestOpenAIToAnthropic_ToolWithoutParametersGetsEmptySchema(t *testing.T) {
	ar := decodeAnthropic(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"ping"}}]}`)
	if string(ar.Tools[0].InputSchema) != emptyObjectSchema {
		t.Errorf("input_schema = %s, want %s", ar.Tools[0].InputSchema, emptyObjectSchema)
	}
}

func TestOpenAIToAnthropic_ToolChoice(t *testing.T) {
	tests := []struct {
		name         string
		choice       string
		parallel     string
		wantType     string
		wantName     string
		wantDisabled bool
	}{
		{name: "absent", choice: "", wantType: ""},
		{name: "auto", choice: `"auto"`, wantType: "auto"},
		{name: "none", choice: `"none"`, wantType: "none"},
		{name: "required maps to any", choice: `"required"`, wantType: "any"},
		{name: "named function", choice: `{"type":"function","function":{"name":"get_weather"}}`, wantType: "tool", wantName: "get_weather"},
		{name: "anthropic-style object passes through", choice: `{"type":"tool","name":"get_weather"}`, wantType: "tool", wantName: "get_weather"},
		{name: "object any maps to required", choice: `{"type":"any"}`, wantType: "any"},
		{name: "parallel false alone still carries", choice: "", parallel: "false", wantType: "auto", wantDisabled: true},
		{name: "parallel false with a choice", choice: `"required"`, parallel: "false", wantType: "any", wantDisabled: true},
		{name: "parallel true is the default and adds nothing", choice: `"auto"`, parallel: "true", wantType: "auto"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":"hi"}]`
			if tc.choice != "" {
				body += `,"tool_choice":` + tc.choice
			}
			if tc.parallel != "" {
				body += `,"parallel_tool_calls":` + tc.parallel
			}
			body += `}`
			ar := decodeAnthropic(t, body)
			if tc.wantType == "" {
				if ar.ToolChoice != nil {
					t.Fatalf("tool_choice = %+v, want nil", ar.ToolChoice)
				}
				return
			}
			if ar.ToolChoice == nil {
				t.Fatal("tool_choice = nil")
			}
			if ar.ToolChoice.Type != tc.wantType || ar.ToolChoice.Name != tc.wantName {
				t.Errorf("tool_choice = %+v, want type %q name %q", ar.ToolChoice, tc.wantType, tc.wantName)
			}
			disabled := ar.ToolChoice.DisableParallelToolUse != nil && *ar.ToolChoice.DisableParallelToolUse
			if disabled != tc.wantDisabled {
				t.Errorf("disable_parallel_tool_use = %v, want %v", disabled, tc.wantDisabled)
			}
		})
	}
}

func TestOpenAIToAnthropic_ToolRoundTrip(t *testing.T) {
	// A full second-round request: user turn, assistant tool_calls, two tool
	// results (parallel calls), then the follow-up.
	body := `{"model":"claude-sonnet-5","messages":[
		{"role":"user","content":"weather in Kyiv and Lviv?"},
		{"role":"assistant","content":"looking","tool_calls":[
			{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Kyiv\"}"}},
			{"id":"call_b","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Lviv\"}"}}]},
		{"role":"tool","tool_call_id":"call_a","content":"20C"},
		{"role":"tool","tool_call_id":"call_b","content":"18C"},
		{"role":"user","content":"thanks"}
	],"tools":[` + weatherTool + `]}`

	ar := decodeAnthropic(t, body)
	if len(ar.Messages) != 4 {
		t.Fatalf("messages = %d, want 4 (user, assistant, merged tool results, user): %+v", len(ar.Messages), ar.Messages)
	}

	asst := ar.Messages[1]
	if asst.Role != "assistant" || len(asst.Content) != 3 {
		t.Fatalf("assistant = %+v", asst)
	}
	if asst.Content[0].Type != "text" {
		t.Errorf("assistant block 0 = %+v, want the text preserved", asst.Content[0])
	}
	if asst.Content[1].Type != "tool_use" || asst.Content[1].ID != "call_a" || asst.Content[1].Name != "get_weather" {
		t.Errorf("assistant block 1 = %+v", asst.Content[1])
	}
	if string(asst.Content[1].Input) != `{"city":"Kyiv"}` {
		t.Errorf("input = %s", asst.Content[1].Input)
	}

	results := ar.Messages[2]
	if results.Role != "user" || len(results.Content) != 2 {
		t.Fatalf("tool results = %+v, want both merged into one user message", results)
	}
	if results.Content[0].ToolUseID != "call_a" || results.Content[1].ToolUseID != "call_b" {
		t.Errorf("tool_result ids = %q %q", results.Content[0].ToolUseID, results.Content[1].ToolUseID)
	}
	if !strings.Contains(string(results.Content[0].Content), "20C") {
		t.Errorf("tool_result content = %s", results.Content[0].Content)
	}
	if ar.Messages[3].Role != "user" || len(ar.Messages[3].Content) != 1 {
		t.Errorf("follow-up = %+v, want a separate user message", ar.Messages[3])
	}
}

func TestOpenAIToAnthropic_ToolCallWithoutText(t *testing.T) {
	ar := decodeAnthropic(t, `{"model":"m","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"ping","arguments":""}}]},
		{"role":"tool","tool_call_id":"c1","content":"pong"}]}`)
	asst := ar.Messages[1]
	if len(asst.Content) != 1 || asst.Content[0].Type != "tool_use" {
		t.Fatalf("assistant = %+v", asst)
	}
	if string(asst.Content[0].Input) != "{}" {
		t.Errorf("empty arguments = %s, want {}", asst.Content[0].Input)
	}
}

func TestOpenAIToAnthropic_ToolErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "non-function tool is rejected rather than dropped",
			body: `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"web_search"}]}`,
			want: "unsupported tool type",
		},
		{
			name: "tool without a name",
			body: `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{}}]}`,
			want: "missing function name",
		},
		{
			name: "unparseable tool_choice",
			body: `{"model":"m","messages":[{"role":"user","content":"x"}],"tool_choice":"whenever"}`,
			want: "unsupported tool_choice",
		},
		{
			name: "tool_choice object with no name",
			body: `{"model":"m","messages":[{"role":"user","content":"x"}],"tool_choice":{"type":"function"}}`,
			want: "missing function name",
		},
		{
			name: "tool_choice of an unknown type",
			body: `{"model":"m","messages":[{"role":"user","content":"x"}],"tool_choice":{"type":"magic"}}`,
			want: "unsupported tool_choice type",
		},
		{
			name: "tool_choice of the wrong JSON shape",
			body: `{"model":"m","messages":[{"role":"user","content":"x"}],"tool_choice":[1]}`,
			want: "must be a string or object",
		},
		{
			name: "tool result with no call id",
			body: `{"model":"m","messages":[{"role":"user","content":"x"},{"role":"tool","content":"y"}]}`,
			want: "missing tool_call_id",
		},
		{
			name: "tool call arguments that are not JSON",
			body: `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c","function":{"name":"f","arguments":"not json"}}]}]}`,
			want: "not valid JSON",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := fixedTr().OpenAIToAnthropic([]byte(tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestAnthropicToOpenAI_ToolUse(t *testing.T) {
	out, err := fixedTr().AnthropicToOpenAI([]byte(`{"id":"msg_1","model":"claude-sonnet-5","stop_reason":"tool_use","content":[
		{"type":"text","text":"checking"},
		{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Kyiv"}}],
		"usage":{"input_tokens":5,"output_tokens":7}}`))
	if err != nil {
		t.Fatal(err)
	}
	var r openaiResponse
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatal(err)
	}
	msg := r.Choices[0].Message
	if msgText(msg) != "checking" {
		t.Errorf("content = %q", msgText(msg))
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v", msg.ToolCalls)
	}
	call := msg.ToolCalls[0]
	if call.ID != "toolu_1" || call.Type != "function" || call.Function.Name != "get_weather" {
		t.Errorf("call = %+v", call)
	}
	if call.Function.Arguments != `{"city":"Kyiv"}` {
		t.Errorf("arguments = %q, want the JSON-encoded string form", call.Function.Arguments)
	}
	if r.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", r.Choices[0].FinishReason)
	}
}

func TestAnthropicToOpenAI_ToolOnlyTurnHasNullContent(t *testing.T) {
	out, err := fixedTr().AnthropicToOpenAI([]byte(`{"id":"m","model":"c","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t","name":"f"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"content":null`)) {
		t.Errorf("out = %s, want a null content on a tool-only turn", out)
	}
	var r openaiResponse
	_ = json.Unmarshal(out, &r)
	if r.Choices[0].Message.ToolCalls[0].Function.Arguments != "{}" {
		t.Errorf("absent input = %q, want {}", r.Choices[0].Message.ToolCalls[0].Function.Arguments)
	}
}

func TestStreamAnthropicToOpenAI_ToolCalls(t *testing.T) {
	in := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-5","usage":{"input_tokens":4}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Kyiv\"}"}}`,
		`data: {"type":"content_block_delta","index":9,"delta":{"type":"input_json_delta","partial_json":"orphan"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":11}}`,
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	var buf bytes.Buffer
	inTok, outTok, err := fixedTr().StreamAnthropicToOpenAI(&buf, strings.NewReader(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	if inTok != 4 || outTok != 11 {
		t.Errorf("usage = %d/%d", inTok, outTok)
	}
	got := buf.String()
	if strings.Contains(got, "orphan") {
		t.Error("emitted a fragment for a block that never started")
	}
	if !strings.Contains(got, `"tool_calls":[{"index":0,"id":"toolu_1","type":"function","function":{"name":"get_weather","arguments":""}}]`) {
		t.Errorf("missing the tool-call opener in:\n%s", got)
	}
	// The two argument fragments must arrive whole and in order.
	var args strings.Builder
	for _, line := range strings.Split(got, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var chunk openaiStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("chunk %q: %v", data, err)
		}
		for _, tc := range chunk.Choices[0].Delta.ToolCalls {
			if tc.Index != 0 {
				t.Errorf("tool call index = %d, want 0", tc.Index)
			}
			args.WriteString(tc.Function.Arguments)
		}
	}
	if args.String() != `{"city":"Kyiv"}` {
		t.Errorf("accumulated arguments = %q", args.String())
	}
	if !strings.Contains(got, `"finish_reason":"tool_calls"`) {
		t.Errorf("finish_reason missing in:\n%s", got)
	}
}
