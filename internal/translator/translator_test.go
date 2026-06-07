package translator

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func fixedTr() *Translator {
	return New(WithClock(func() time.Time { return time.Unix(1700000000, 0) }))
}

// ---------- request ----------

func TestOpenAIToAnthropic_Basic(t *testing.T) {
	in := `{
		"model":"claude-x",
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"system","content":"and kind"},
			{"role":"user","content":"hi"}
		],
		"temperature":0.5,"top_p":0.9,"stop":"END","stream":true
	}`
	out, stream, err := fixedTr().OpenAIToAnthropic([]byte(in))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !stream {
		t.Error("stream should be true")
	}
	var ar anthropicRequest
	if err := json.Unmarshal(out, &ar); err != nil {
		t.Fatal(err)
	}
	if ar.Model != "claude-x" {
		t.Errorf("model %q", ar.Model)
	}
	if ar.System != "be brief\n\nand kind" {
		t.Errorf("system %q", ar.System)
	}
	if ar.MaxTokens != defaultMaxTokens {
		t.Errorf("max_tokens %d, want default", ar.MaxTokens)
	}
	if len(ar.Messages) != 1 || ar.Messages[0].Role != "user" || ar.Messages[0].Content[0].Text != "hi" {
		t.Errorf("messages %+v", ar.Messages)
	}
	if len(ar.StopSequences) != 1 || ar.StopSequences[0] != "END" {
		t.Errorf("stop %v", ar.StopSequences)
	}
	if *ar.Temperature != 0.5 || *ar.TopP != 0.9 {
		t.Errorf("temp/top_p %v %v", ar.Temperature, ar.TopP)
	}
}

func TestOpenAIToAnthropic_MaxTokensAndStopArray(t *testing.T) {
	in := `{"model":"m","max_tokens":10,"stop":["a","b"],"messages":[{"role":"user","content":"x"}]}`
	out, _, err := fixedTr().OpenAIToAnthropic([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var ar anthropicRequest
	json.Unmarshal(out, &ar)
	if ar.MaxTokens != 10 {
		t.Errorf("max_tokens %d", ar.MaxTokens)
	}
	if len(ar.StopSequences) != 2 {
		t.Errorf("stop %v", ar.StopSequences)
	}
}

func TestOpenAIToAnthropic_MultimodalParts(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"look"},
		{"type":"image_url","image_url":{"url":"https://img/x.png"}},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}
	]}]}`
	out, _, err := fixedTr().OpenAIToAnthropic([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var ar anthropicRequest
	json.Unmarshal(out, &ar)
	blocks := ar.Messages[0].Content
	if len(blocks) != 3 {
		t.Fatalf("blocks %d", len(blocks))
	}
	if blocks[1].Source.Type != "url" || blocks[1].Source.URL != "https://img/x.png" {
		t.Errorf("url image %+v", blocks[1].Source)
	}
	if blocks[2].Source.Type != "base64" || blocks[2].Source.MediaType != "image/png" || blocks[2].Source.Data != "QUJD" {
		t.Errorf("b64 image %+v", blocks[2].Source)
	}
}

func TestOpenAIToAnthropic_Errors(t *testing.T) {
	cases := map[string]string{
		"bad json":      `{`,
		"no model":      `{"messages":[{"role":"user","content":"x"}]}`,
		"no messages":   `{"model":"m","messages":[]}`,
		"bad role":      `{"model":"m","messages":[{"role":"tool","content":"x"}]}`,
		"only system":   `{"model":"m","messages":[{"role":"system","content":"x"}]}`,
		"bad stop":      `{"model":"m","stop":{"x":1},"messages":[{"role":"user","content":"x"}]}`,
		"bad content":   `{"model":"m","messages":[{"role":"user","content":123}]}`,
		"img no url":    `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":""}}]}]}`,
		"bad part type": `{"model":"m","messages":[{"role":"user","content":[{"type":"audio"}]}]}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := fixedTr().OpenAIToAnthropic([]byte(in)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestOpenAIToAnthropic_NullContent(t *testing.T) {
	// null content on a user message -> empty blocks, but message kept.
	in := `{"model":"m","messages":[{"role":"user","content":null},{"role":"user","content":"hi"}]}`
	out, _, err := fixedTr().OpenAIToAnthropic([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "hi") {
		t.Errorf("out %s", out)
	}
}

func TestImageBlock_DataURIFallbacks(t *testing.T) {
	cases := map[string]struct{ wantType, wantURL string }{
		"data:image/png;base64,QUJD": {"base64", ""},                      // proper base64 -> base64 source
		"data:image/png,rawnotb64":   {"url", "data:image/png,rawnotb64"}, // not ;base64 -> url source
		"data:nocomma":               {"url", "data:nocomma"},             // malformed -> url source
		"https://x/y.png":            {"url", "https://x/y.png"},
	}
	for url, want := range cases {
		t.Run(url, func(t *testing.T) {
			b := imageBlock(url)
			if b.Source.Type != want.wantType {
				t.Errorf("type = %q, want %q", b.Source.Type, want.wantType)
			}
			if want.wantURL != "" && b.Source.URL != want.wantURL {
				t.Errorf("url = %q, want %q", b.Source.URL, want.wantURL)
			}
		})
	}
}

// ---------- response ----------

func TestAnthropicToOpenAI_Basic(t *testing.T) {
	in := `{"id":"msg_1","model":"claude-x","content":[
		{"type":"text","text":"Hello "},
		{"type":"text","text":"world"},
		{"type":"thinking","text":"ignored"}
	],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5}}`
	out, err := fixedTr().AnthropicToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var r openaiResponse
	json.Unmarshal(out, &r)
	if r.ID != "chatcmpl-msg_1" {
		t.Errorf("id %q", r.ID)
	}
	if r.Created != 1700000000 {
		t.Errorf("created %d", r.Created)
	}
	if r.Choices[0].Message.Content != "Hello world" {
		t.Errorf("content %q", r.Choices[0].Message.Content)
	}
	if r.Choices[0].FinishReason != "stop" {
		t.Errorf("finish %q", r.Choices[0].FinishReason)
	}
	if r.Usage.TotalTokens != 8 {
		t.Errorf("total %d", r.Usage.TotalTokens)
	}
}

func TestAnthropicToOpenAI_NoIDAndErrors(t *testing.T) {
	out, err := fixedTr().AnthropicToOpenAI([]byte(`{"content":[],"stop_reason":"max_tokens"}`))
	if err != nil {
		t.Fatal(err)
	}
	var r openaiResponse
	json.Unmarshal(out, &r)
	if r.ID != "chatcmpl-cerber" {
		t.Errorf("id %q", r.ID)
	}
	if r.Choices[0].FinishReason != "length" {
		t.Errorf("finish %q", r.Choices[0].FinishReason)
	}
	if _, err := fixedTr().AnthropicToOpenAI([]byte(`{`)); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestFinishReason(t *testing.T) {
	cases := map[string]string{
		"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length",
		"tool_use": "tool_calls", "": "", "weird": "stop",
	}
	for in, want := range cases {
		if got := finishReason(in); got != want {
			t.Errorf("finishReason(%q) = %q, want %q", in, got, want)
		}
	}
}
