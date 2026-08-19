package translator

import (
	"encoding/json"
	"fmt"
	"strings"
)

// This file carries the tool/function-calling shapes shared by the OpenAI
// request and response translations. Tool calling is the one part of the OpenAI
// dialect where every upstream disagrees on naming (OpenAI `tools[].function`,
// Anthropic `tools[].input_schema`, Gemini `functionDeclarations`), so the
// conversion lives in one place rather than being duplicated per provider.

// --- OpenAI tool shapes ---

// openaiTool is one entry of the OpenAI `tools` array. Only type=="function" is
// meaningful for chat completions; other types (e.g. hosted tools) are rejected
// rather than silently dropped, because dropping them is exactly the failure
// this package exists to prevent.
type openaiTool struct {
	Type     string           `json:"type"`
	Function openaiToolSchema `json:"function"`
}

type openaiToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// openaiToolCall is an assistant-emitted call, both on the request side (history
// replayed by the client) and the response side (what we produce).
type openaiToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openaiToolCallFunc `json:"function"`
}

type openaiToolCallFunc struct {
	Name string `json:"name"`
	// Arguments is a JSON-encoded *string*, not an object — an OpenAI quirk that
	// both Anthropic (`input`) and Gemini (`args`) get right, so every conversion
	// through here marshals or unmarshals across that boundary.
	Arguments string `json:"arguments"`
}

// openaiToolCallDelta is the streaming form: an index-addressed fragment that the
// client accumulates. Arguments is never omitted — clients concatenate it blindly
// and a missing key breaks naive accumulators.
type openaiToolCallDelta struct {
	Index    int                     `json:"index"`
	ID       string                  `json:"id,omitempty"`
	Type     string                  `json:"type,omitempty"`
	Function openaiToolCallFuncDelta `json:"function"`
}

type openaiToolCallFuncDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// emptyObjectSchema is the input schema used when a caller declares a tool with
// no parameters. Anthropic and Gemini both require the field to be present.
const emptyObjectSchema = `{"type":"object","properties":{}}`

// toolArguments normalizes a tool call's arguments to the JSON-encoded string
// OpenAI clients expect. Anthropic `input` / Gemini `args` are objects; an absent
// or null one becomes "{}".
func toolArguments(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return "{}"
	}
	return s
}

// argumentsToObject converts an OpenAI arguments string back into a JSON object
// for upstreams that take structured input. An empty string becomes {}; anything
// that is not valid JSON is an error, since forwarding it would make the upstream
// fail with a far less obvious message.
func argumentsToObject(args string, toolName string) (json.RawMessage, error) {
	s := strings.TrimSpace(args)
	if s == "" {
		return json.RawMessage(`{}`), nil
	}
	if !json.Valid([]byte(s)) {
		return nil, fmt.Errorf("tool call %q: arguments is not valid JSON", toolName)
	}
	return json.RawMessage(s), nil
}

// toolSchemaOrEmpty returns the caller's JSON Schema, or an empty object schema
// when none was supplied.
func toolSchemaOrEmpty(raw json.RawMessage) json.RawMessage {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return json.RawMessage(emptyObjectSchema)
	}
	return raw
}

// toolChoiceKind classifies an OpenAI `tool_choice` value into a provider-neutral
// kind plus, for a pinned tool, its name. Kinds: "auto", "none", "required",
// "tool", or "" when the caller did not set tool_choice.
func toolChoiceKind(raw json.RawMessage) (kind, name string, err error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return "", "", nil
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		switch str {
		case "auto", "none", "required":
			return str, "", nil
		default:
			return "", "", fmt.Errorf("unsupported tool_choice %q", str)
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", "", fmt.Errorf("tool_choice must be a string or object")
	}
	switch obj.Type {
	case "function", "tool":
		n := obj.Function.Name
		if n == "" {
			n = obj.Name
		}
		if n == "" {
			return "", "", fmt.Errorf("tool_choice of type %q missing function name", obj.Type)
		}
		return "tool", n, nil
	case "auto", "none", "required", "any":
		if obj.Type == "any" {
			return "required", "", nil
		}
		return obj.Type, "", nil
	default:
		return "", "", fmt.Errorf("unsupported tool_choice type %q", obj.Type)
	}
}
