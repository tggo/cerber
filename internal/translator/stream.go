package translator

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// --- OpenAI streaming chunk shapes ---

type openaiStreamChunk struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []openaiStreamChoice `json:"choices"`
}

type openaiStreamChoice struct {
	Index        int         `json:"index"`
	Delta        openaiDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type openaiDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// --- Anthropic streaming event (loose; only fields we read) ---

type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Message *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
	} `json:"message"`
	Delta *struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
}

// StreamAnthropicToOpenAI reads an Anthropic Messages SSE stream from r and
// writes an OpenAI chat.completion.chunk SSE stream to w, calling flush (if
// non-nil) after each chunk so bytes reach the client promptly. It always emits
// a terminating "data: [DONE]" line.
func (t *Translator) StreamAnthropicToOpenAI(w io.Writer, r io.Reader, flush func()) error {
	created := t.now().Unix()
	var id, model, finalReason string
	emittedRole := false
	done := false

	emit := func(choice openaiStreamChoice) error {
		chunk := openaiStreamChunk{
			ID:      chatID(id),
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []openaiStreamChoice{choice},
		}
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
		fr := finishReason(finalReason)
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
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue // skip "event:" lines, comments, blanks
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		var ev anthropicStreamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue // tolerate non-JSON keepalives
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				id, model = ev.Message.ID, ev.Message.Model
			}
			if !emittedRole {
				emittedRole = true
				if err := emit(openaiStreamChoice{Index: 0, Delta: openaiDelta{Role: "assistant"}}); err != nil {
					return err
				}
			}
		case "content_block_delta":
			if ev.Delta != nil && ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				if err := emit(openaiStreamChoice{Index: 0, Delta: openaiDelta{Content: ev.Delta.Text}}); err != nil {
					return err
				}
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				finalReason = ev.Delta.StopReason
			}
		case "message_stop":
			return finish()
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("translator: read anthropic stream: %w", err)
	}
	return finish()
}
