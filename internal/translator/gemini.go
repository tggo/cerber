package translator

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// --- Gemini request shapes ---

type geminiRequest struct {
	Contents          []geminiContent   `json:"contents"`
	SystemInstruction *geminiContent    `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenConfig  `json:"generationConfig,omitempty"`
	Tools             []geminiTool      `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig `json:"toolConfig,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFuncDecl `json:"functionDeclarations,omitempty"`
}

type geminiFuncDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type geminiToolConfig struct {
	FunctionCallingConfig geminiFuncCallConfig `json:"functionCallingConfig"`
}

type geminiFuncCallConfig struct {
	Mode                 string   `json:"mode"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	InlineData       *geminiInlineData       `json:"inlineData,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response,omitempty"`
}

type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiGenConfig struct {
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

// OpenAIToGemini converts an OpenAI chat-completions request into a Gemini
// generateContent request body. It also returns the model (for the URL path) and
// whether streaming was requested.
//
// Text and base64 (data: URI) image parts are supported (http(s) image URLs are
// not — Gemini needs inline data). Function calling is translated in full:
// `tools` become functionDeclarations, assistant `tool_calls` become
// functionCall parts, and role=="tool" messages become functionResponse parts.
func (t *Translator) OpenAIToGemini(body []byte) (out []byte, model string, stream bool, err error) {
	var in openaiRequest
	if err = json.Unmarshal(body, &in); err != nil {
		return nil, "", false, fmt.Errorf("translator: parse openai request: %w", err)
	}
	if in.Model == "" {
		return nil, "", false, fmt.Errorf("translator: openai request missing model")
	}
	if len(in.Messages) == 0 {
		return nil, "", false, fmt.Errorf("translator: openai request has no messages")
	}

	gr := geminiRequest{}
	if gr.Tools, err = geminiTools(in.Tools); err != nil {
		return nil, "", false, err
	}
	if gr.ToolConfig, err = geminiChoice(in.ToolChoice); err != nil {
		return nil, "", false, err
	}

	var systemParts []string
	// callNames remembers which function each synthetic tool_call_id refers to, so
	// a later role=="tool" message can be labelled — Gemini keys a functionResponse
	// by function name, while OpenAI keys it by call id.
	callNames := map[string]string{}
	mergingToolResults := false
	for i, m := range in.Messages {
		if m.Role == "tool" {
			part, perr := geminiToolResultPart(m, callNames)
			if perr != nil {
				return nil, "", false, fmt.Errorf("translator: message[%d]: %w", i, perr)
			}
			if mergingToolResults {
				last := &gr.Contents[len(gr.Contents)-1]
				last.Parts = append(last.Parts, part)
			} else {
				gr.Contents = append(gr.Contents, geminiContent{Role: "user", Parts: []geminiPart{part}})
				mergingToolResults = true
			}
			continue
		}
		mergingToolResults = false

		parts, text, perr := contentToGeminiParts(m.Content)
		if perr != nil {
			return nil, "", false, fmt.Errorf("translator: message[%d]: %w", i, perr)
		}
		switch m.Role {
		case "system":
			if text != "" {
				systemParts = append(systemParts, text)
			}
		case "user":
			gr.Contents = append(gr.Contents, geminiContent{Role: "user", Parts: parts})
		case "assistant":
			for _, tc := range m.ToolCalls {
				args, aerr := argumentsToObject(tc.Function.Arguments, tc.Function.Name)
				if aerr != nil {
					return nil, "", false, fmt.Errorf("translator: message[%d]: %w", i, aerr)
				}
				callNames[tc.ID] = tc.Function.Name
				parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{Name: tc.Function.Name, Args: args}})
			}
			gr.Contents = append(gr.Contents, geminiContent{Role: "model", Parts: parts})
		default:
			return nil, "", false, fmt.Errorf("translator: message[%d]: unsupported role %q", i, m.Role)
		}
	}
	if len(gr.Contents) == 0 {
		return nil, "", false, fmt.Errorf("translator: no user/assistant messages after conversion")
	}
	if len(systemParts) > 0 {
		gr.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: strings.Join(systemParts, "\n\n")}}}
	}

	cfg := geminiGenConfig{Temperature: in.Temperature, TopP: in.TopP}
	cfg.MaxOutputTokens = in.MaxTokens // nil unless caller set it (Gemini defaults otherwise)
	if cfg.StopSequences, err = parseStop(in.Stop); err != nil {
		return nil, "", false, err
	}
	if cfg.MaxOutputTokens != nil || cfg.Temperature != nil || cfg.TopP != nil || len(cfg.StopSequences) > 0 {
		gr.GenerationConfig = &cfg
	}

	out, err = json.Marshal(gr)
	if err != nil {
		return nil, "", false, fmt.Errorf("translator: marshal gemini request: %w", err)
	}
	return out, in.Model, in.Stream, nil
}

func contentToGeminiParts(raw json.RawMessage) ([]geminiPart, string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []geminiPart{{Text: s}}, s, nil
	}
	var parts []openaiPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, "", fmt.Errorf("content must be a string or array of parts")
	}
	var out []geminiPart
	var texts []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, geminiPart{Text: p.Text})
			texts = append(texts, p.Text)
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				return nil, "", fmt.Errorf("image_url part missing url")
			}
			media, data, ok := parseDataURI(p.ImageURL.URL)
			if !ok {
				return nil, "", fmt.Errorf("gemini supports only base64 data: image URLs")
			}
			out = append(out, geminiPart{InlineData: &geminiInlineData{MimeType: media, Data: data}})
		default:
			return nil, "", fmt.Errorf("unsupported content part type %q", p.Type)
		}
	}
	return out, strings.Join(texts, ""), nil
}

// --- Gemini response shapes ---

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
	ResponseID string `json:"responseId"`
}

// GeminiToOpenAI converts a non-streaming Gemini response into an OpenAI
// chat-completion response. model labels the response.
func (t *Translator) GeminiToOpenAI(body []byte, model string) ([]byte, error) {
	var in geminiResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("translator: parse gemini response: %w", err)
	}
	var text strings.Builder
	var calls []openaiToolCall
	finish := ""
	if len(in.Candidates) > 0 {
		for _, p := range in.Candidates[0].Content.Parts {
			text.WriteString(p.Text)
			if p.FunctionCall != nil {
				calls = append(calls, geminiCallToOpenAI(*p.FunctionCall, len(calls)))
			}
		}
		finish = geminiFinish(in.Candidates[0].FinishReason)
	}
	// Gemini reports STOP even when the turn is a function call; OpenAI clients
	// branch on finish_reason, so correct it here.
	if len(calls) > 0 && (finish == "stop" || finish == "") {
		finish = "tool_calls"
	}
	msg := openaiRespMsg{Role: "assistant", ToolCalls: calls}
	if s := text.String(); s != "" || len(calls) == 0 {
		msg.Content = &s
	}
	resp := openaiResponse{
		ID:      geminiChatID(in.ResponseID),
		Object:  "chat.completion",
		Created: t.now().Unix(),
		Model:   model,
		Choices: []openaiChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: finish,
		}},
		Usage: openaiUsage{
			PromptTokens:     in.UsageMetadata.PromptTokenCount,
			CompletionTokens: in.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      in.UsageMetadata.PromptTokenCount + in.UsageMetadata.CandidatesTokenCount,
		},
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("translator: marshal openai response: %w", err)
	}
	return out, nil
}

// geminiFinish maps a Gemini finishReason to an OpenAI finish_reason.
func geminiFinish(reason string) string {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
		return "content_filter"
	case "":
		return ""
	default:
		return "stop"
	}
}

func geminiChatID(responseID string) string {
	if responseID == "" {
		return "chatcmpl-gemini"
	}
	return "chatcmpl-" + responseID
}

// StreamGeminiToOpenAI reads a Gemini streamGenerateContent SSE stream from r and
// writes an OpenAI chat.completion.chunk SSE stream to w. It always emits a
// terminating "data: [DONE]".
func (t *Translator) StreamGeminiToOpenAI(w io.Writer, r io.Reader, model string, flush func()) error {
	created := t.now().Unix()
	id := geminiChatID("")
	emittedRole := false
	finalReason := ""
	done := false
	toolCalls := 0

	emit := func(choice openaiStreamChoice) error {
		chunk := openaiStreamChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model, Choices: []openaiStreamChoice{choice}}
		b, err := json.Marshal(chunk)
		if err != nil {
			return fmt.Errorf("translator: marshal chunk: %w", err)
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		if flush != nil {
			flush()
		}
		return nil
	}
	finish := func() error {
		if done {
			return nil
		}
		done = true
		fr := geminiFinish(finalReason)
		if fr == "" {
			fr = "stop"
		}
		if toolCalls > 0 && fr == "stop" {
			fr = "tool_calls"
		}
		if err := emit(openaiStreamChoice{Index: 0, Delta: openaiDelta{}, FinishReason: &fr}); err != nil {
			return err
		}
		_, err := io.WriteString(w, "data: [DONE]\n\n")
		if flush != nil {
			flush()
		}
		return err
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		var ev geminiResponse
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		if !emittedRole {
			emittedRole = true
			if err := emit(openaiStreamChoice{Index: 0, Delta: openaiDelta{Role: "assistant"}}); err != nil {
				return err
			}
		}
		if len(ev.Candidates) > 0 {
			for _, p := range ev.Candidates[0].Content.Parts {
				if p.Text != "" {
					if err := emit(openaiStreamChoice{Index: 0, Delta: openaiDelta{Content: p.Text}}); err != nil {
						return err
					}
				}
				if p.FunctionCall == nil {
					continue
				}
				// Gemini streams a function call whole, so one chunk carries the
				// complete name and arguments rather than a fragment sequence.
				call := geminiCallToOpenAI(*p.FunctionCall, toolCalls)
				delta := openaiDelta{ToolCalls: []openaiToolCallDelta{{
					Index: toolCalls,
					ID:    call.ID,
					Type:  "function",
					Function: openaiToolCallFuncDelta{
						Name:      call.Function.Name,
						Arguments: call.Function.Arguments,
					},
				}}}
				toolCalls++
				if err := emit(openaiStreamChoice{Index: 0, Delta: delta}); err != nil {
					return err
				}
			}
			if ev.Candidates[0].FinishReason != "" {
				finalReason = ev.Candidates[0].FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("translator: read gemini stream: %w", err)
	}
	return finish()
}

// geminiTools converts OpenAI tool declarations into Gemini functionDeclarations.
// Gemini takes a single tool entry holding all declarations.
func geminiTools(tools []openaiTool) ([]geminiTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	decls := make([]geminiFuncDecl, 0, len(tools))
	for i, t := range tools {
		if t.Type != "" && t.Type != "function" {
			return nil, fmt.Errorf("translator: tools[%d]: unsupported tool type %q", i, t.Type)
		}
		if t.Function.Name == "" {
			return nil, fmt.Errorf("translator: tools[%d]: missing function name", i)
		}
		decls = append(decls, geminiFuncDecl{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  toolSchemaOrEmpty(t.Function.Parameters),
		})
	}
	return []geminiTool{{FunctionDeclarations: decls}}, nil
}

// geminiChoice maps OpenAI tool_choice onto Gemini's functionCallingConfig.
func geminiChoice(raw json.RawMessage) (*geminiToolConfig, error) {
	kind, name, err := toolChoiceKind(raw)
	if err != nil {
		return nil, fmt.Errorf("translator: %w", err)
	}
	cfg := geminiFuncCallConfig{}
	switch kind {
	case "":
		return nil, nil
	case "auto":
		cfg.Mode = "AUTO"
	case "none":
		cfg.Mode = "NONE"
	case "required":
		cfg.Mode = "ANY"
	case "tool":
		cfg.Mode = "ANY"
		cfg.AllowedFunctionNames = []string{name}
	}
	return &geminiToolConfig{FunctionCallingConfig: cfg}, nil
}

// geminiCallID synthesizes the call id OpenAI requires and Gemini does not
// provide. The function name is embedded so the id survives a round trip: when
// the client replays the matching role=="tool" message, geminiToolResultPart can
// recover the name Gemini needs to key the functionResponse.
func geminiCallID(index int, name string) string {
	return fmt.Sprintf("call_%d_%s", index, name)
}

// geminiCallName recovers the function name from an id made by geminiCallID.
// Returns "" for an id in any other shape.
func geminiCallName(id string) string {
	rest, ok := strings.CutPrefix(id, "call_")
	if !ok {
		return ""
	}
	_, name, ok := strings.Cut(rest, "_")
	if !ok {
		return ""
	}
	return name
}

// geminiCallToOpenAI converts one Gemini functionCall into an OpenAI tool call at
// the given tool-call index.
func geminiCallToOpenAI(fc geminiFunctionCall, index int) openaiToolCall {
	return openaiToolCall{
		ID:   geminiCallID(index, fc.Name),
		Type: "function",
		Function: openaiToolCallFunc{
			Name:      fc.Name,
			Arguments: toolArguments(fc.Args),
		},
	}
}

// geminiToolResultPart converts an OpenAI role=="tool" message into a Gemini
// functionResponse part. The function name comes from the request's own history
// when available, then from the id's embedded name, then from the message's
// `name` field — one of the three is always present for a well-formed client.
func geminiToolResultPart(m openaiMessage, callNames map[string]string) (geminiPart, error) {
	if m.ToolCallID == "" && m.Name == "" {
		return geminiPart{}, fmt.Errorf("tool message missing tool_call_id")
	}
	name := callNames[m.ToolCallID]
	if name == "" {
		name = geminiCallName(m.ToolCallID)
	}
	if name == "" {
		name = m.Name
	}
	if name == "" {
		return geminiPart{}, fmt.Errorf("tool message %q: cannot determine which function it answers", m.ToolCallID)
	}
	_, text, err := contentToGeminiParts(m.Content)
	if err != nil {
		return geminiPart{}, err
	}
	// Gemini requires an object; a raw tool output string is wrapped so arbitrary
	// text survives without the client having to pre-encode it.
	resp, err := json.Marshal(map[string]string{"result": text})
	if err != nil {
		return geminiPart{}, err
	}
	return geminiPart{FunctionResponse: &geminiFunctionResponse{Name: name, Response: resp}}, nil
}
