package translator

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func decodeGemini(t *testing.T, body string) geminiRequest {
	t.Helper()
	out, _, _, err := fixedTr().OpenAIToGemini([]byte(body))
	if err != nil {
		t.Fatalf("OpenAIToGemini: %v", err)
	}
	var gr geminiRequest
	if err := json.Unmarshal(out, &gr); err != nil {
		t.Fatalf("unmarshal gemini request: %v", err)
	}
	return gr
}

func TestOpenAIToGemini_FunctionDeclarations(t *testing.T) {
	gr := decodeGemini(t, `{"model":"gemini-3","messages":[{"role":"user","content":"weather?"}],"tools":[`+weatherTool+`]}`)
	if len(gr.Tools) != 1 || len(gr.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("tools = %+v", gr.Tools)
	}
	decl := gr.Tools[0].FunctionDeclarations[0]
	if decl.Name != "get_weather" || decl.Description != "look it up" {
		t.Errorf("decl = %+v", decl)
	}
	if !strings.Contains(string(decl.Parameters), `"city"`) {
		t.Errorf("parameters = %s", decl.Parameters)
	}
}

func TestOpenAIToGemini_ToolConfig(t *testing.T) {
	tests := []struct {
		name        string
		choice      string
		wantMode    string
		wantAllowed []string
		wantNil     bool
	}{
		{name: "absent", choice: "", wantNil: true},
		{name: "auto", choice: `"auto"`, wantMode: "AUTO"},
		{name: "none", choice: `"none"`, wantMode: "NONE"},
		{name: "required", choice: `"required"`, wantMode: "ANY"},
		{name: "named function pins the allowed list", choice: `{"type":"function","function":{"name":"get_weather"}}`, wantMode: "ANY", wantAllowed: []string{"get_weather"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"g","messages":[{"role":"user","content":"hi"}]`
			if tc.choice != "" {
				body += `,"tool_choice":` + tc.choice
			}
			body += `}`
			gr := decodeGemini(t, body)
			if tc.wantNil {
				if gr.ToolConfig != nil {
					t.Fatalf("toolConfig = %+v, want nil", gr.ToolConfig)
				}
				return
			}
			if gr.ToolConfig == nil {
				t.Fatal("toolConfig = nil")
			}
			cfg := gr.ToolConfig.FunctionCallingConfig
			if cfg.Mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", cfg.Mode, tc.wantMode)
			}
			if strings.Join(cfg.AllowedFunctionNames, ",") != strings.Join(tc.wantAllowed, ",") {
				t.Errorf("allowedFunctionNames = %v, want %v", cfg.AllowedFunctionNames, tc.wantAllowed)
			}
		})
	}
}

func TestOpenAIToGemini_ToolRoundTrip(t *testing.T) {
	gr := decodeGemini(t, `{"model":"gemini-3","messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","tool_calls":[
			{"id":"call_0_get_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Kyiv\"}"}},
			{"id":"call_1_get_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Lviv\"}"}}]},
		{"role":"tool","tool_call_id":"call_0_get_weather","content":"20C"},
		{"role":"tool","tool_call_id":"call_1_get_weather","content":"18C"}]}`)

	if len(gr.Contents) != 3 {
		t.Fatalf("contents = %d, want 3 (user, model, merged results): %+v", len(gr.Contents), gr.Contents)
	}
	model := gr.Contents[1]
	if model.Role != "model" || len(model.Parts) != 2 {
		t.Fatalf("model turn = %+v", model)
	}
	if model.Parts[0].FunctionCall == nil || model.Parts[0].FunctionCall.Name != "get_weather" {
		t.Errorf("part 0 = %+v", model.Parts[0])
	}
	if string(model.Parts[0].FunctionCall.Args) != `{"city":"Kyiv"}` {
		t.Errorf("args = %s", model.Parts[0].FunctionCall.Args)
	}
	results := gr.Contents[2]
	if results.Role != "user" || len(results.Parts) != 2 {
		t.Fatalf("results = %+v", results)
	}
	for i, want := range []string{"20C", "18C"} {
		fr := results.Parts[i].FunctionResponse
		if fr == nil || fr.Name != "get_weather" {
			t.Fatalf("result %d = %+v", i, results.Parts[i])
		}
		if !strings.Contains(string(fr.Response), want) {
			t.Errorf("result %d response = %s, want it to carry %q", i, fr.Response, want)
		}
	}
}

func TestOpenAIToGemini_ToolResultNameFallbacks(t *testing.T) {
	// No preceding assistant turn: the name is recovered from the synthetic id.
	gr := decodeGemini(t, `{"model":"g","messages":[{"role":"tool","tool_call_id":"call_0_lookup","content":"x"}]}`)
	if got := gr.Contents[0].Parts[0].FunctionResponse.Name; got != "lookup" {
		t.Errorf("name from id = %q, want lookup", got)
	}
	// Neither history nor a parseable id: fall back to the message's own name.
	gr = decodeGemini(t, `{"model":"g","messages":[{"role":"tool","tool_call_id":"xyz","name":"lookup","content":"x"}]}`)
	if got := gr.Contents[0].Parts[0].FunctionResponse.Name; got != "lookup" {
		t.Errorf("name from name field = %q, want lookup", got)
	}
}

func TestOpenAIToGemini_ToolErrors(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"non-function tool", `{"model":"g","messages":[{"role":"user","content":"x"}],"tools":[{"type":"web_search"}]}`, "unsupported tool type"},
		{"tool without a name", `{"model":"g","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{}}]}`, "missing function name"},
		{"bad tool_choice", `{"model":"g","messages":[{"role":"user","content":"x"}],"tool_choice":"whenever"}`, "unsupported tool_choice"},
		{"tool result with nothing to key on", `{"model":"g","messages":[{"role":"tool","content":"x"}]}`, "missing tool_call_id"},
		{"tool result with an unusable id", `{"model":"g","messages":[{"role":"tool","tool_call_id":"xyz","content":"x"}]}`, "cannot determine which function"},
		{"bad tool call arguments", `{"model":"g","messages":[{"role":"assistant","tool_calls":[{"id":"c","function":{"name":"f","arguments":"nope"}}]}]}`, "not valid JSON"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := fixedTr().OpenAIToGemini([]byte(tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestGeminiToOpenAI_FunctionCall(t *testing.T) {
	out, err := fixedTr().GeminiToOpenAI([]byte(`{"responseId":"r1","candidates":[{"content":{"parts":[
		{"text":"checking"},
		{"functionCall":{"name":"get_weather","args":{"city":"Kyiv"}}}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4}}`), "gemini-3")
	if err != nil {
		t.Fatal(err)
	}
	var r openaiResponse
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatal(err)
	}
	msg := r.Choices[0].Message
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v", msg.ToolCalls)
	}
	call := msg.ToolCalls[0]
	if call.Function.Name != "get_weather" || call.Function.Arguments != `{"city":"Kyiv"}` {
		t.Errorf("call = %+v", call)
	}
	if call.ID != "call_0_get_weather" {
		t.Errorf("id = %q, want one that survives a round trip", call.ID)
	}
	if r.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls despite Gemini reporting STOP", r.Choices[0].FinishReason)
	}
}

func TestStreamGeminiToOpenAI_FunctionCall(t *testing.T) {
	in := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`,
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"Kyiv"}}}]},"finishReason":"STOP"}]}`,
		"",
	}, "\n")
	var buf bytes.Buffer
	if err := fixedTr().StreamGeminiToOpenAI(&buf, strings.NewReader(in), "gemini-3", nil); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, `"name":"get_weather"`) || !strings.Contains(got, `{\"city\":\"Kyiv\"}`) {
		t.Errorf("tool call missing from:\n%s", got)
	}
	if !strings.Contains(got, `"finish_reason":"tool_calls"`) {
		t.Errorf("finish_reason not corrected in:\n%s", got)
	}
}

func TestGeminiCallName(t *testing.T) {
	tests := []struct{ id, want string }{
		{"call_0_get_weather", "get_weather"},
		{"call_12_f", "f"},
		{"toolu_01", ""},
		{"call_nounderscore", ""},
	}
	for _, tc := range tests {
		if got := geminiCallName(tc.id); got != tc.want {
			t.Errorf("geminiCallName(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}
