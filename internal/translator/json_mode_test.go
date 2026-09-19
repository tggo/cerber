package translator

import (
	"encoding/json"
	"testing"
)

// OpenAI's response_format has no Anthropic equivalent, and asking for JSON in the system prompt is
// not a contract — Haiku answers such an instruction with a markdown-fenced object followed by a
// paragraph of prose, which every strict json.Unmarshal on the client rejects. cerber therefore
// forces a tool call, whose argument block *is* the object.
func TestOpenAIToAnthropic_JSONObjectBecomesAForcedTool(t *testing.T) {
	t.Parallel()

	out, _, err := New().OpenAIToAnthropic([]byte(`{
		"model":"claude-haiku-4-5-20251001",
		"response_format":{"type":"json_object"},
		"messages":[{"role":"user","content":"is this abusive? answer {\"ok\":bool}"}]
	}`))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	var got struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
		ToolChoice struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tool_choice"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != JSONResponseToolName {
		t.Fatalf("expected the synthetic json tool, got %+v", got.Tools)
	}
	if got.ToolChoice.Type != "tool" || got.ToolChoice.Name != JSONResponseToolName {
		t.Fatalf("the tool must be forced, got %+v", got.ToolChoice)
	}
	if len(got.Tools[0].InputSchema) == 0 {
		t.Fatal("an Anthropic tool without an input_schema is rejected upstream")
	}
}

// json_schema carries a shape; it is handed to Anthropic as the tool's own schema rather than
// flattened to "any object", so the model is constrained to what the caller actually asked for.
func TestOpenAIToAnthropic_JSONSchemaIsPassedThrough(t *testing.T) {
	t.Parallel()

	out, _, err := New().OpenAIToAnthropic([]byte(`{
		"model":"claude-haiku-4-5-20251001",
		"response_format":{"type":"json_schema","json_schema":{"name":"verdict",
			"schema":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}}},
		"messages":[{"role":"user","content":"verdict?"}]
	}`))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	var got struct {
		Tools []struct {
			InputSchema struct {
				Required []string `json:"required"`
			} `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got.Tools) != 1 || len(got.Tools[0].InputSchema.Required) != 1 || got.Tools[0].InputSchema.Required[0] != "ok" {
		t.Fatalf("the caller's schema must reach Anthropic, got %+v", got.Tools)
	}
}

// A caller who sent their own tools keeps them: forcing ours would suppress theirs, and silently
// dropping a client's tool calls is worse than prose they can still parse.
func TestOpenAIToAnthropic_JSONModeWithClientToolsFallsBackToAnInstruction(t *testing.T) {
	t.Parallel()

	out, _, err := New().OpenAIToAnthropic([]byte(`{
		"model":"claude-haiku-4-5-20251001",
		"response_format":{"type":"json_object"},
		"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],
		"messages":[{"role":"user","content":"weather?"}]
	}`))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	var got struct {
		System string `json:"system"`
		Tools  []struct {
			Name string `json:"name"`
		} `json:"tools"`
		ToolChoice *struct{} `json:"tool_choice"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "get_weather" {
		t.Fatalf("the client's own tools must survive, got %+v", got.Tools)
	}
	if got.System == "" {
		t.Fatal("expected a JSON instruction in the system prompt as the fallback")
	}
}

// The client asked for JSON *content*, so the forced call must come back as message.content — never
// as a tool call it never declared and cannot answer.
func TestAnthropicToOpenAI_ForcedJSONToolBecomesContent(t *testing.T) {
	t.Parallel()

	out, err := New().AnthropicToOpenAI([]byte(`{
		"id":"msg_1","model":"claude-haiku-4-5-20251001","stop_reason":"tool_use",
		"content":[
			{"type":"text","text":"Here is the verdict:"},
			{"type":"tool_use","id":"tu_1","name":"` + JSONResponseToolName + `","input":{"ok":true}}
		],
		"usage":{"input_tokens":10,"output_tokens":5}
	}`))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	var got struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []any   `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	msg := got.Choices[0].Message
	if len(msg.ToolCalls) != 0 {
		t.Fatalf("the synthetic tool must not surface as a call, got %+v", msg.ToolCalls)
	}
	if msg.Content == nil {
		t.Fatal("expected the object as content")
	}
	var verdict struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(*msg.Content), &verdict); err != nil {
		t.Fatalf("content must be bare JSON a client can unmarshal, got %q: %v", *msg.Content, err)
	}
	if !verdict.OK {
		t.Fatal("the object's own value must survive")
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Fatalf(`a json_object answer ends with "stop", not %q`, got.Choices[0].FinishReason)
	}
}

// A genuine client tool is untouched by any of the above.
func TestAnthropicToOpenAI_RealToolCallStillSurfaces(t *testing.T) {
	t.Parallel()

	out, err := New().AnthropicToOpenAI([]byte(`{
		"id":"msg_2","model":"claude-haiku-4-5-20251001","stop_reason":"tool_use",
		"content":[{"type":"tool_use","id":"tu_2","name":"get_weather","input":{"city":"Kyiv"}}],
		"usage":{"input_tokens":1,"output_tokens":1}
	}`))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	var got struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	calls := got.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].Function.Name != "get_weather" {
		t.Fatalf("expected the real tool call, got %+v", calls)
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf(`expected "tool_calls", got %q`, got.Choices[0].FinishReason)
	}
}
