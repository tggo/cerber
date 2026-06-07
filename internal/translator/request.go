package translator

import (
	"encoding/json"
	"fmt"
	"strings"
)

// --- OpenAI request shapes (only the fields we translate) ---

type openaiRequest struct {
	Model       string          `json:"model"`
	Messages    []openaiMessage `json:"messages"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stop        json.RawMessage `json:"stop,omitempty"` // string or []string
	Stream      bool            `json:"stream,omitempty"`
}

type openaiMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []openaiPart
}

type openaiPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

// --- Anthropic request shapes ---

type anthropicRequest struct {
	Model         string             `json:"model"`
	System        string             `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicBlock struct {
	Type   string           `json:"type"`
	Text   string           `json:"text,omitempty"`
	Source *anthropicSource `json:"source,omitempty"`
}

type anthropicSource struct {
	Type      string `json:"type"` // "url" or "base64"
	MediaType string `json:"media_type,omitempty"`
	URL       string `json:"url,omitempty"`
	Data      string `json:"data,omitempty"`
}

// OpenAIToAnthropic converts an OpenAI chat-completions request body into an
// Anthropic Messages request body. It also reports whether streaming was
// requested. System messages are merged into the Anthropic `system` field.
//
// Slice-1 scope: text and image content parts are supported; OpenAI `tools`
// / function-calling are not yet translated (use the native endpoint for those).
func (t *Translator) OpenAIToAnthropic(body []byte) (out []byte, stream bool, err error) {
	var in openaiRequest
	if err = json.Unmarshal(body, &in); err != nil {
		return nil, false, fmt.Errorf("translator: parse openai request: %w", err)
	}
	if in.Model == "" {
		return nil, false, fmt.Errorf("translator: openai request missing model")
	}
	if len(in.Messages) == 0 {
		return nil, false, fmt.Errorf("translator: openai request has no messages")
	}

	ar := anthropicRequest{
		Model:       in.Model,
		MaxTokens:   defaultMaxTokens,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		Stream:      in.Stream,
	}
	if in.MaxTokens != nil {
		ar.MaxTokens = *in.MaxTokens
	}
	if ar.StopSequences, err = parseStop(in.Stop); err != nil {
		return nil, false, err
	}

	var systemParts []string
	for i, m := range in.Messages {
		blocks, text, err := contentToBlocks(m.Content)
		if err != nil {
			return nil, false, fmt.Errorf("translator: message[%d]: %w", i, err)
		}
		switch m.Role {
		case "system":
			if text != "" {
				systemParts = append(systemParts, text)
			}
		case "user", "assistant":
			ar.Messages = append(ar.Messages, anthropicMessage{Role: m.Role, Content: blocks})
		default:
			return nil, false, fmt.Errorf("translator: message[%d]: unsupported role %q", i, m.Role)
		}
	}
	ar.System = strings.Join(systemParts, "\n\n")

	if len(ar.Messages) == 0 {
		return nil, false, fmt.Errorf("translator: no user/assistant messages after conversion")
	}

	out, err = json.Marshal(ar)
	if err != nil {
		return nil, false, fmt.Errorf("translator: marshal anthropic request: %w", err)
	}
	return out, ar.Stream, nil
}

// parseStop normalizes OpenAI `stop` (string | []string | null) to []string.
func parseStop(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []string{s}, nil
	}
	var ss []string
	if err := json.Unmarshal(raw, &ss); err == nil {
		return ss, nil
	}
	return nil, fmt.Errorf("translator: stop must be a string or array of strings")
}

// contentToBlocks converts OpenAI message content (string | []part) into
// Anthropic content blocks, and also returns the concatenated plain text (used
// for system messages).
func contentToBlocks(raw json.RawMessage) ([]anthropicBlock, string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, "", nil
	}
	// string content
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []anthropicBlock{{Type: "text", Text: s}}, s, nil
	}
	// array of parts
	var parts []openaiPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, "", fmt.Errorf("content must be a string or array of parts")
	}
	var blocks []anthropicBlock
	var texts []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			blocks = append(blocks, anthropicBlock{Type: "text", Text: p.Text})
			texts = append(texts, p.Text)
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				return nil, "", fmt.Errorf("image_url part missing url")
			}
			blocks = append(blocks, imageBlock(p.ImageURL.URL))
		default:
			return nil, "", fmt.Errorf("unsupported content part type %q", p.Type)
		}
	}
	return blocks, strings.Join(texts, ""), nil
}

// imageBlock maps an OpenAI image URL to an Anthropic image block. data: URIs
// become base64 sources; everything else is sent as a url source.
func imageBlock(url string) anthropicBlock {
	if media, data, ok := parseDataURI(url); ok {
		return anthropicBlock{Type: "image", Source: &anthropicSource{Type: "base64", MediaType: media, Data: data}}
	}
	return anthropicBlock{Type: "image", Source: &anthropicSource{Type: "url", URL: url}}
}

// parseDataURI splits a data:<media>;base64,<data> URI.
func parseDataURI(s string) (media, data string, ok bool) {
	rest, found := strings.CutPrefix(s, "data:")
	if !found {
		return "", "", false
	}
	meta, payload, found := strings.Cut(rest, ",")
	if !found {
		return "", "", false
	}
	media, isB64 := strings.CutSuffix(meta, ";base64")
	if !isB64 {
		return "", "", false
	}
	return media, payload, true
}
