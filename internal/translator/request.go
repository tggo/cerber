package translator

import (
	"encoding/json"
	"fmt"
	"strings"
)

// --- OpenAI request shapes (only the fields we translate) ---

type openaiRequest struct {
	Model             string          `json:"model"`
	Messages          []openaiMessage `json:"messages"`
	MaxTokens         *int            `json:"max_tokens,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	Stop              json.RawMessage `json:"stop,omitempty"` // string or []string
	Stream            bool            `json:"stream,omitempty"`
	Tools             []openaiTool    `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"` // string or object
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	ResponseFormat    *openaiResponseFormat `json:"response_format,omitempty"`
}

// openaiResponseFormat is OpenAI's {"type":"json_object"} / {"type":"json_schema", ...}.
// Anthropic has no equivalent field, so it is translated into a forced tool call — see
// JSONResponseToolName.
type openaiResponseFormat struct {
	Type       string `json:"type"`
	JSONSchema *struct {
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
		Strict *bool           `json:"strict,omitempty"`
	} `json:"json_schema,omitempty"`
}

type openaiMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []openaiPart
	// Tool-calling fields. ToolCalls appears on replayed assistant turns;
	// ToolCallID identifies which call a role=="tool" message answers.
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
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
	Model         string               `json:"model"`
	System        string               `json:"system,omitempty"`
	Messages      []anthropicMessage   `json:"messages"`
	MaxTokens     int                  `json:"max_tokens"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	Tools         []anthropicTool      `json:"tools,omitempty"`
	ToolChoice    *anthropicToolChoice `json:"tool_choice,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicToolChoice covers Anthropic's {auto|any|none|tool} selector.
// DisableParallelToolUse carries OpenAI's parallel_tool_calls:false.
type anthropicToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicBlock struct {
	Type   string           `json:"type"`
	Text   string           `json:"text,omitempty"`
	Source *anthropicSource `json:"source,omitempty"`
	// tool_use blocks
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result blocks
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
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
// Text and image content parts are supported, as is the full function-calling
// round trip: `tools` become Anthropic tools, an assistant turn's `tool_calls`
// become tool_use blocks, and role=="tool" messages become tool_result blocks on
// a user message (consecutive ones merge into a single message, which is how
// Anthropic expects parallel tool results).
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
	if ar.Tools, err = anthropicTools(in.Tools); err != nil {
		return nil, false, err
	}
	if ar.ToolChoice, err = anthropicChoice(in.ToolChoice, in.ParallelToolCalls); err != nil {
		return nil, false, err
	}

	var systemParts []string
	// mergingToolResults tracks whether the last appended message is the synthetic
	// user message holding tool_result blocks, so a run of role=="tool" messages
	// (parallel calls) lands in one message instead of several.
	mergingToolResults := false
	for i, m := range in.Messages {
		if m.Role == "tool" {
			block, err := toolResultBlock(m)
			if err != nil {
				return nil, false, fmt.Errorf("translator: message[%d]: %w", i, err)
			}
			if mergingToolResults {
				last := &ar.Messages[len(ar.Messages)-1]
				last.Content = append(last.Content, block)
			} else {
				ar.Messages = append(ar.Messages, anthropicMessage{Role: "user", Content: []anthropicBlock{block}})
				mergingToolResults = true
			}
			continue
		}
		mergingToolResults = false

		blocks, text, err := contentToBlocks(m.Content)
		if err != nil {
			return nil, false, fmt.Errorf("translator: message[%d]: %w", i, err)
		}
		switch m.Role {
		case "system":
			if text != "" {
				systemParts = append(systemParts, text)
			}
		case "user":
			ar.Messages = append(ar.Messages, anthropicMessage{Role: m.Role, Content: blocks})
		case "assistant":
			for _, tc := range m.ToolCalls {
				input, err := argumentsToObject(tc.Function.Arguments, tc.Function.Name)
				if err != nil {
					return nil, false, fmt.Errorf("translator: message[%d]: %w", i, err)
				}
				blocks = append(blocks, anthropicBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: input,
				})
			}
			ar.Messages = append(ar.Messages, anthropicMessage{Role: m.Role, Content: blocks})
		default:
			return nil, false, fmt.Errorf("translator: message[%d]: unsupported role %q", i, m.Role)
		}
	}
	ar.System = strings.Join(systemParts, "\n\n")
	// After the system prompt is assembled: the fallback path appends to it.
	applyJSONResponseFormat(&ar, in.ResponseFormat)

	if len(ar.Messages) == 0 {
		return nil, false, fmt.Errorf("translator: no user/assistant messages after conversion")
	}

	out, err = json.Marshal(ar)
	if err != nil {
		return nil, false, fmt.Errorf("translator: marshal anthropic request: %w", err)
	}
	return out, ar.Stream, nil
}


// JSONResponseToolName is the synthetic Anthropic tool cerber uses to honour OpenAI's
// `response_format: {"type":"json_object"}` (and `json_schema`).
//
// Anthropic's Messages API has no response_format, and simply asking for JSON in the system
// prompt is not a contract: Haiku answers a "reply with JSON" instruction with a markdown-fenced
// object followed by a paragraph of prose, which every strict `json.Unmarshal` on the client side
// rejects. Forcing a tool call is Anthropic's own documented way to get a guaranteed-shaped
// object, and the argument block *is* the JSON — no fences, no commentary.
//
// The name is deliberately cerber-prefixed so a real client tool can never collide with it, which
// is what lets the response side recognise it by name alone and put the object back where an
// OpenAI client expects it (message.content). That keeps both translations stateless.
const JSONResponseToolName = "cerber_json_response"

// permissiveJSONSchema accepts any object: `json_object` asks for "valid JSON", not a shape.
var permissiveJSONSchema = json.RawMessage(`{"type":"object","additionalProperties":true}`)

// applyJSONResponseFormat turns response_format into a forced tool call.
//
// Skipped when the caller already sent tools of their own: forcing ours would suppress theirs, and
// silently dropping a client's tool calls is worse than returning prose they can still parse. Such
// a request instead gets an instruction appended to the system prompt — best-effort, and the only
// case where json_object is not a guarantee.
func applyJSONResponseFormat(ar *anthropicRequest, rf *openaiResponseFormat) {
	if rf == nil || (rf.Type != "json_object" && rf.Type != "json_schema") {
		return
	}

	if len(ar.Tools) > 0 {
		const instruction = "Reply with a single valid JSON object and nothing else: no prose, no explanation, no markdown code fences."
		if ar.System == "" {
			ar.System = instruction
		} else {
			ar.System += "\n\n" + instruction
		}
		return
	}

	schema := permissiveJSONSchema
	description := "Return the answer as a single JSON object."
	if rf.JSONSchema != nil && len(rf.JSONSchema.Schema) > 0 {
		schema = rf.JSONSchema.Schema
		if rf.JSONSchema.Name != "" {
			description = "Return the answer as a single JSON object named " + rf.JSONSchema.Name + "."
		}
	}

	ar.Tools = []anthropicTool{{
		Name:        JSONResponseToolName,
		Description: description,
		InputSchema: schema,
	}}
	ar.ToolChoice = &anthropicToolChoice{Type: "tool", Name: JSONResponseToolName}
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

// anthropicTools converts OpenAI tool declarations. A tool of a type other than
// "function" is an error rather than a silent drop: a model told it has no tools
// answers "I cannot do that", which is indistinguishable from a real refusal.
func anthropicTools(tools []openaiTool) ([]anthropicTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]anthropicTool, 0, len(tools))
	for i, t := range tools {
		if t.Type != "" && t.Type != "function" {
			return nil, fmt.Errorf("translator: tools[%d]: unsupported tool type %q", i, t.Type)
		}
		if t.Function.Name == "" {
			return nil, fmt.Errorf("translator: tools[%d]: missing function name", i)
		}
		out = append(out, anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: toolSchemaOrEmpty(t.Function.Parameters),
		})
	}
	return out, nil
}

// anthropicChoice maps OpenAI tool_choice + parallel_tool_calls onto Anthropic's
// single tool_choice object.
func anthropicChoice(raw json.RawMessage, parallel *bool) (*anthropicToolChoice, error) {
	kind, name, err := toolChoiceKind(raw)
	if err != nil {
		return nil, fmt.Errorf("translator: %w", err)
	}
	var disable *bool
	if parallel != nil && !*parallel {
		v := true
		disable = &v
	}
	if kind == "" {
		if disable == nil {
			return nil, nil
		}
		// parallel_tool_calls:false alone still needs a carrier object.
		return &anthropicToolChoice{Type: "auto", DisableParallelToolUse: disable}, nil
	}
	c := &anthropicToolChoice{DisableParallelToolUse: disable}
	switch kind {
	case "auto", "none":
		c.Type = kind
	case "required":
		c.Type = "any"
	case "tool":
		c.Type, c.Name = "tool", name
	}
	return c, nil
}

// toolResultBlock converts an OpenAI role=="tool" message into an Anthropic
// tool_result block. The result content is passed through as text, which is what
// Anthropic accepts for arbitrary tool output.
func toolResultBlock(m openaiMessage) (anthropicBlock, error) {
	if m.ToolCallID == "" {
		return anthropicBlock{}, fmt.Errorf("tool message missing tool_call_id")
	}
	_, text, err := contentToBlocks(m.Content)
	if err != nil {
		return anthropicBlock{}, err
	}
	content, err := json.Marshal([]anthropicBlock{{Type: "text", Text: text}})
	if err != nil {
		return anthropicBlock{}, err
	}
	return anthropicBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: content}, nil
}
