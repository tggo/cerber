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
	Role      string                `json:"role,omitempty"`
	Content   string                `json:"content,omitempty"`
	ToolCalls []openaiToolCallDelta `json:"tool_calls,omitempty"`
}

// --- Anthropic streaming event (loose; only fields we read) ---

type anthropicStreamUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

func (u anthropicStreamUsage) totalInput() int64 {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Message *struct {
		ID    string               `json:"id"`
		Model string               `json:"model"`
		Usage anthropicStreamUsage `json:"usage"`
	} `json:"message"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		StopReason  string `json:"stop_reason"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
	// Index addresses the content block a start/delta event belongs to; a
	// tool_use block's fragments are keyed by it.
	Index        *int `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Usage *anthropicStreamUsage `json:"usage"` // message_delta carries usage at top level
}

// StreamAnthropicToOpenAI reads an Anthropic Messages SSE stream from r and
// writes an OpenAI chat.completion.chunk SSE stream to w, calling flush (if
// non-nil) after each chunk so bytes reach the client promptly. It always emits
// a terminating "data: [DONE]" line. It also returns the token usage parsed from
// the stream (input from message_start incl. cache, output from message_delta)
// so the caller can record cost for streamed responses.
func (t *Translator) StreamAnthropicToOpenAI(w io.Writer, r io.Reader, flush func()) (in, out int64, err error) {
	created := t.now().Unix()
	var id, model, finalReason string
	emittedRole := false
	done := false
	// toolIndex maps an Anthropic content-block index to the tool_calls[] index
	// the client accumulates against. OpenAI numbers tool calls consecutively
	// among themselves, while Anthropic numbers every block including text.
	toolIndex := map[int]int{}
	nextToolIndex := 0

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
				if t := ev.Message.Usage.totalInput(); t > 0 {
					in = t
				}
				if ev.Message.Usage.OutputTokens > 0 {
					out = ev.Message.Usage.OutputTokens
				}
			}
			if !emittedRole {
				emittedRole = true
				if err := emit(openaiStreamChoice{Index: 0, Delta: openaiDelta{Role: "assistant"}}); err != nil {
					return in, out, err
				}
			}
		case "content_block_start":
			if ev.ContentBlock == nil || ev.ContentBlock.Type != "tool_use" || ev.Index == nil {
				continue
			}
			ti := nextToolIndex
			nextToolIndex++
			toolIndex[*ev.Index] = ti
			delta := openaiDelta{ToolCalls: []openaiToolCallDelta{{
				Index:    ti,
				ID:       ev.ContentBlock.ID,
				Type:     "function",
				Function: openaiToolCallFuncDelta{Name: ev.ContentBlock.Name},
			}}}
			if err := emit(openaiStreamChoice{Index: 0, Delta: delta}); err != nil {
				return in, out, err
			}
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch {
			case ev.Delta.Type == "text_delta" && ev.Delta.Text != "":
				if err := emit(openaiStreamChoice{Index: 0, Delta: openaiDelta{Content: ev.Delta.Text}}); err != nil {
					return in, out, err
				}
			case ev.Delta.Type == "input_json_delta" && ev.Index != nil:
				ti, ok := toolIndex[*ev.Index]
				if !ok {
					continue // fragment for a block we never saw start
				}
				delta := openaiDelta{ToolCalls: []openaiToolCallDelta{{
					Index:    ti,
					Function: openaiToolCallFuncDelta{Arguments: ev.Delta.PartialJSON},
				}}}
				if err := emit(openaiStreamChoice{Index: 0, Delta: delta}); err != nil {
					return in, out, err
				}
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				finalReason = ev.Delta.StopReason
			}
			if ev.Usage != nil && ev.Usage.OutputTokens > 0 {
				out = ev.Usage.OutputTokens
			}
		case "message_stop":
			return in, out, finish()
		}
	}
	if err := sc.Err(); err != nil {
		return in, out, fmt.Errorf("translator: read anthropic stream: %w", err)
	}
	return in, out, finish()
}
