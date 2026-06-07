package translator

import (
	"encoding/json"
	"fmt"
	"strings"
)

// --- Anthropic non-streaming response (fields we read) ---

type anthropicResponse struct {
	ID         string                  `json:"id"`
	Model      string                  `json:"model"`
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsage          `json:"usage"`
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// --- OpenAI non-streaming response ---

type openaiResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openaiChoice `json:"choices"`
	Usage   openaiUsage    `json:"usage"`
}

type openaiChoice struct {
	Index        int           `json:"index"`
	Message      openaiRespMsg `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type openaiRespMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// AnthropicToOpenAI converts a non-streaming Anthropic Messages response into an
// OpenAI chat-completion response. Text content blocks are concatenated.
func (t *Translator) AnthropicToOpenAI(body []byte) ([]byte, error) {
	var in anthropicResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("translator: parse anthropic response: %w", err)
	}

	var text strings.Builder
	for _, b := range in.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}

	resp := openaiResponse{
		ID:      chatID(in.ID),
		Object:  "chat.completion",
		Created: t.now().Unix(),
		Model:   in.Model,
		Choices: []openaiChoice{{
			Index:        0,
			Message:      openaiRespMsg{Role: "assistant", Content: text.String()},
			FinishReason: finishReason(in.StopReason),
		}},
		Usage: openaiUsage{
			PromptTokens:     in.Usage.InputTokens,
			CompletionTokens: in.Usage.OutputTokens,
			TotalTokens:      in.Usage.InputTokens + in.Usage.OutputTokens,
		},
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("translator: marshal openai response: %w", err)
	}
	return out, nil
}

// chatID derives an OpenAI-style id from the Anthropic message id.
func chatID(anthropicID string) string {
	if anthropicID == "" {
		return "chatcmpl-cerber"
	}
	return "chatcmpl-" + anthropicID
}
