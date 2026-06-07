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
	Contents          []geminiContent  `json:"contents"`
	SystemInstruction *geminiContent   `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenConfig `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text       string            `json:"text,omitempty"`
	InlineData *geminiInlineData `json:"inlineData,omitempty"`
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
// Slice scope: text and base64 (data: URI) image parts are supported; http(s)
// image URLs and tools are not (use a different provider for those).
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
	var systemParts []string
	for i, m := range in.Messages {
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
	finish := ""
	if len(in.Candidates) > 0 {
		for _, p := range in.Candidates[0].Content.Parts {
			text.WriteString(p.Text)
		}
		finish = geminiFinish(in.Candidates[0].FinishReason)
	}
	resp := openaiResponse{
		ID:      geminiChatID(in.ResponseID),
		Object:  "chat.completion",
		Created: t.now().Unix(),
		Model:   model,
		Choices: []openaiChoice{{
			Index:        0,
			Message:      openaiRespMsg{Role: "assistant", Content: text.String()},
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
