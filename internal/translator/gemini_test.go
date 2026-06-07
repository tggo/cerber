package translator

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenAIToGemini_Basic(t *testing.T) {
	in := `{"model":"gemini-2.5-flash","temperature":0.7,"stop":"END","stream":true,
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"hello"},
			{"role":"user","content":"more"}
		]}`
	out, model, stream, err := fixedTr().OpenAIToGemini([]byte(in))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if model != "gemini-2.5-flash" || !stream {
		t.Errorf("model/stream = %q %v", model, stream)
	}
	var gr geminiRequest
	json.Unmarshal(out, &gr)
	if gr.SystemInstruction == nil || gr.SystemInstruction.Parts[0].Text != "be brief" {
		t.Errorf("system = %+v", gr.SystemInstruction)
	}
	if len(gr.Contents) != 3 {
		t.Fatalf("contents = %d", len(gr.Contents))
	}
	if gr.Contents[0].Role != "user" || gr.Contents[1].Role != "model" || gr.Contents[2].Role != "user" {
		t.Errorf("roles = %v", gr.Contents)
	}
	if gr.GenerationConfig == nil || *gr.GenerationConfig.Temperature != 0.7 ||
		len(gr.GenerationConfig.StopSequences) != 1 {
		t.Errorf("genconfig = %+v", gr.GenerationConfig)
	}
}

func TestOpenAIToGemini_ImageInline(t *testing.T) {
	in := `{"model":"g","messages":[{"role":"user","content":[
		{"type":"text","text":"see"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}
	]}]}`
	out, _, _, err := fixedTr().OpenAIToGemini([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var gr geminiRequest
	json.Unmarshal(out, &gr)
	parts := gr.Contents[0].Parts
	if len(parts) != 2 || parts[1].InlineData == nil ||
		parts[1].InlineData.MimeType != "image/png" || parts[1].InlineData.Data != "QUJD" {
		t.Errorf("parts = %+v", parts)
	}
}

func TestOpenAIToGemini_Errors(t *testing.T) {
	cases := map[string]string{
		"bad json":    `{`,
		"no model":    `{"messages":[{"role":"user","content":"x"}]}`,
		"no messages": `{"model":"g","messages":[]}`,
		"bad role":    `{"model":"g","messages":[{"role":"tool","content":"x"}]}`,
		"only system": `{"model":"g","messages":[{"role":"system","content":"x"}]}`,
		"http image":  `{"model":"g","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`,
		"bad part":    `{"model":"g","messages":[{"role":"user","content":[{"type":"audio"}]}]}`,
		"bad stop":    `{"model":"g","stop":{"x":1},"messages":[{"role":"user","content":"x"}]}`,
		"bad content": `{"model":"g","messages":[{"role":"user","content":5}]}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := fixedTr().OpenAIToGemini([]byte(in)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestGeminiToOpenAI(t *testing.T) {
	in := `{"candidates":[{"content":{"parts":[{"text":"Pong"},{"text":"!"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5},"responseId":"abc"}`
	out, err := fixedTr().GeminiToOpenAI([]byte(in), "gemini-x")
	if err != nil {
		t.Fatal(err)
	}
	var r openaiResponse
	json.Unmarshal(out, &r)
	if r.ID != "chatcmpl-abc" || r.Model != "gemini-x" {
		t.Errorf("id/model = %q %q", r.ID, r.Model)
	}
	if r.Choices[0].Message.Content != "Pong!" || r.Choices[0].FinishReason != "stop" {
		t.Errorf("choice = %+v", r.Choices[0])
	}
	if r.Usage.TotalTokens != 8 {
		t.Errorf("usage = %+v", r.Usage)
	}
}

func TestGeminiToOpenAI_EmptyAndError(t *testing.T) {
	out, err := fixedTr().GeminiToOpenAI([]byte(`{"candidates":[]}`), "g")
	if err != nil {
		t.Fatal(err)
	}
	var r openaiResponse
	json.Unmarshal(out, &r)
	if r.ID != "chatcmpl-gemini" || r.Choices[0].Message.Content != "" {
		t.Errorf("empty = %+v", r)
	}
	if _, err := fixedTr().GeminiToOpenAI([]byte(`{`), "g"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestGeminiFinish(t *testing.T) {
	cases := map[string]string{"STOP": "stop", "MAX_TOKENS": "length", "SAFETY": "content_filter", "": "", "WEIRD": "stop"}
	for in, want := range cases {
		if got := geminiFinish(in); got != want {
			t.Errorf("geminiFinish(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStreamGeminiToOpenAI(t *testing.T) {
	in := `data: {"candidates":[{"content":{"parts":[{"text":"Po"}]}}]}

data: {"candidates":[{"content":{"parts":[{"text":"ng"}]},"finishReason":"STOP"}]}

`
	var buf strings.Builder
	if err := fixedTr().StreamGeminiToOpenAI(&buf, strings.NewReader(in), "gemini-x", nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"role":"assistant"`) || !strings.Contains(out, `"content":"Po"`) ||
		!strings.Contains(out, `"content":"ng"`) || !strings.Contains(out, `"finish_reason":"stop"`) ||
		!strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Errorf("stream = %s", out)
	}
}

func TestStreamGeminiToOpenAI_IgnoresNoise(t *testing.T) {
	in := ": ping\ndata: not-json\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"x\"}]}}]}\n"
	var buf strings.Builder
	if err := fixedTr().StreamGeminiToOpenAI(&buf, strings.NewReader(in), "g", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"content":"x"`) || !strings.Contains(buf.String(), "[DONE]") {
		t.Errorf("out = %s", buf.String())
	}
}

func TestStreamGeminiToOpenAI_WriteError(t *testing.T) {
	in := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"x\"}]}}]}\n"
	if err := fixedTr().StreamGeminiToOpenAI(&errWriter{ok: 0}, strings.NewReader(in), "g", nil); err == nil {
		t.Fatal("expected write error")
	}
}
