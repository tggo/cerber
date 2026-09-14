// Package comfyui serves OpenAI chat completions from a GGUF model running
// inside ComfyUI, through the CerberLLMChat custom node
// (https://github.com/tggo/comfyui-cerber-llm).
//
// The point is sharing one GPU between image workflows and an occasional LLM
// call: ComfyUI runs one prompt at a time and the node borrows VRAM from
// ComfyUI's memory manager, so the chat request simply takes its turn in
// ComfyUI's queue. It is slow (the model loads per request) but never fights
// the image jobs for memory.
//
// Each chat request becomes a one-node prompt: POST /prompt, poll
// GET /history/<id>, translate the node's output into an OpenAI response.
// Models are listed from GET /object_info/<node> and exposed as
// "comfyui-<model>". It only contacts the configured ComfyUI base URL.
package comfyui

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tggo/cerber/internal/credential"
	"github.com/tggo/cerber/internal/provider"
)

// ModelPrefix marks a model served by this provider: "comfyui-<node model>".
const ModelPrefix = "comfyui-"

// DefaultNode is the custom node class the provider drives.
const DefaultNode = "CerberLLMChat"

// Defaults applied when the config leaves a field zero.
const (
	DefaultPollInterval = 500 * time.Millisecond
	DefaultMaxWait      = 30 * time.Minute
	DefaultNCtx         = 8192
	DefaultMaxTokens    = 1024
	defaultTemperature  = 0.8
	defaultTopP         = 0.95
	// keepAliveEvery is how often a stream that is still waiting on ComfyUI gets
	// an SSE comment, so proxies (Cloudflare cuts idle responses at 100s) keep the
	// connection open while the model loads and generates.
	keepAliveEvery = 15 * time.Second
	// credentialName labels usage from this keyless provider.
	credentialName = "comfyui"
)

// Options tune a Provider. Zero values mean the defaults above.
type Options struct {
	Node         string        // custom node class_type (default CerberLLMChat)
	PollInterval time.Duration // how often /history is polled
	MaxWait      time.Duration // give up on a prompt that hasn't finished after this long
	NCtx         int           // context window passed to the node
	MaxTokens    int           // output cap when the request sets none
	KeepLoaded   bool          // leave the model in VRAM between requests
}

// Provider talks to one ComfyUI instance.
type Provider struct {
	baseURL string
	http    provider.HTTPDoer
	opt     Options
	seed    func() uint32
	now     func() time.Time
}

// New builds a Provider for the ComfyUI at baseURL (e.g. http://gpu0:8188).
func New(baseURL string, doer provider.HTTPDoer, opt Options) *Provider {
	if opt.Node == "" {
		opt.Node = DefaultNode
	}
	if opt.PollInterval <= 0 {
		opt.PollInterval = DefaultPollInterval
	}
	if opt.MaxWait <= 0 {
		opt.MaxWait = DefaultMaxWait
	}
	if opt.NCtx <= 0 {
		opt.NCtx = DefaultNCtx
	}
	if opt.MaxTokens <= 0 {
		opt.MaxTokens = DefaultMaxTokens
	}
	return &Provider{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    doer,
		opt:     opt,
		seed:    randomSeed,
		now:     time.Now,
	}
}

func randomSeed() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.LittleEndian.Uint32(b[:])
}

// Name identifies this provider.
func (p *Provider) Name() string { return "comfyui" }

// BaseURL returns the ComfyUI base URL (safe to display).
func (p *Provider) BaseURL() string { return p.baseURL }

// chatRequest is the subset of an OpenAI chat request this provider honours.
type chatRequest struct {
	Model               string            `json:"model"`
	Messages            []json.RawMessage `json:"messages"`
	MaxTokens           *int              `json:"max_tokens"`
	MaxCompletionTokens *int              `json:"max_completion_tokens"`
	Temperature         *float64          `json:"temperature"`
	TopP                *float64          `json:"top_p"`
	Seed                *int64            `json:"seed"`
	Stop                json.RawMessage   `json:"stop"`
	N                   *int              `json:"n"`
	Tools               []json.RawMessage `json:"tools"`
	ReasoningEffort     string            `json:"reasoning_effort"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

func badRequest(format string, a ...any) error {
	return &provider.BadRequestError{Err: fmt.Errorf("comfyui: "+format, a...)}
}

// validateMessages rejects what a text-only local model can't take, so the
// client gets a 400 instead of a failed ComfyUI run.
func validateMessages(msgs []json.RawMessage) error {
	if len(msgs) == 0 {
		return badRequest("messages must be a non-empty array")
	}
	for i, raw := range msgs {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return badRequest("messages[%d] is not an object", i)
		}
		switch m.Role {
		case "system", "developer", "user", "assistant":
		default:
			return badRequest("messages[%d].role %q is not supported (system, user, assistant; no tool calling)", i, m.Role)
		}
		c := bytes.TrimSpace(m.Content)
		if len(c) == 0 || c[0] == '"' || bytes.Equal(c, []byte("null")) {
			continue
		}
		var parts []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(c, &parts) != nil {
			return badRequest("messages[%d].content must be a string or text parts", i)
		}
		for _, part := range parts {
			if part.Type != "text" {
				return badRequest("messages[%d]: content part %q is not supported, text only", i, part.Type)
			}
		}
	}
	return nil
}

// stopJSON normalizes "stop" (string or array) into the node's JSON-array string.
func stopJSON(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		b, _ := json.Marshal([]string{one})
		return string(b), nil
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		return "", badRequest("stop must be a string or an array of strings")
	}
	b, _ := json.Marshal(many)
	return string(b), nil
}

// buildInputs maps an OpenAI request onto the node's inputs.
func (p *Provider) buildInputs(req *chatRequest) (map[string]any, error) {
	if !strings.HasPrefix(req.Model, ModelPrefix) || len(req.Model) == len(ModelPrefix) {
		return nil, badRequest("model %q must be %s<model>", req.Model, ModelPrefix)
	}
	if len(req.Tools) > 0 {
		return nil, badRequest("tool calling is not supported by local ComfyUI models")
	}
	if req.N != nil && *req.N > 1 {
		return nil, badRequest("n > 1 is not supported")
	}
	if err := validateMessages(req.Messages); err != nil {
		return nil, err
	}
	msgs, _ := json.Marshal(req.Messages)
	stop, err := stopJSON(req.Stop)
	if err != nil {
		return nil, err
	}
	maxTokens := p.opt.MaxTokens
	switch {
	case req.MaxCompletionTokens != nil && *req.MaxCompletionTokens > 0:
		maxTokens = *req.MaxCompletionTokens
	case req.MaxTokens != nil && *req.MaxTokens > 0:
		maxTokens = *req.MaxTokens
	}
	temperature, topP := defaultTemperature, defaultTopP
	if req.Temperature != nil {
		temperature = *req.Temperature
	}
	if req.TopP != nil {
		topP = *req.TopP
	}
	// No seed = a fresh draw per request. The node already refuses ComfyUI's
	// output cache; a random seed also keeps sampling honest.
	seed := int64(p.seed())
	if req.Seed != nil && *req.Seed >= 0 {
		seed = *req.Seed
	}
	effort := strings.ToLower(req.ReasoningEffort)
	return map[string]any{
		"model":        strings.TrimPrefix(req.Model, ModelPrefix),
		"messages":     string(msgs),
		"max_tokens":   maxTokens,
		"temperature":  temperature,
		"top_p":        topP,
		"seed":         seed,
		"n_ctx":        p.opt.NCtx,
		"n_gpu_layers": -1,
		"think":        effort != "" && effort != "none",
		"keep_loaded":  p.opt.KeepLoaded,
		"stop":         stop,
	}, nil
}

// nodeResult is the node's answer as exposed in /history outputs.
type nodeResult struct {
	Content          string
	Reasoning        string
	FinishReason     string
	PromptTokens     int64
	CompletionTokens int64
}

// Chat runs one chat request through ComfyUI and returns an OpenAI-format
// response. Validation failures (bad request, unknown model) come back as 4xx
// before anything is queued. A stream is answered immediately with keep-alive
// comments while ComfyUI works; a failure after that point is delivered as an
// SSE error event.
func (p *Provider) Chat(ctx context.Context, body []byte, stream bool, _ http.Header) (*provider.Response, error) {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, badRequest("invalid JSON body: %v", err)
	}
	inputs, err := p.buildInputs(&req)
	if err != nil {
		return nil, err
	}
	id, rejected, err := p.submit(ctx, inputs)
	if err != nil {
		return nil, err
	}
	if rejected != nil {
		return rejected, nil
	}
	created := p.now().Unix()
	if !stream {
		res, err := p.wait(ctx, id, nil)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(completion(id, req.Model, created, res))
		return jsonResponse(http.StatusOK, b), nil
	}

	includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
	pr, pw := io.Pipe()
	go func() {
		keepAlive := func() error {
			_, err := io.WriteString(pw, ": waiting for comfyui\n\n")
			return err
		}
		res, err := p.wait(ctx, id, keepAlive)
		if err != nil {
			msg, _ := json.Marshal(map[string]any{"error": map[string]string{"message": err.Error(), "type": "upstream_error"}})
			_, _ = fmt.Fprintf(pw, "data: %s\n\ndata: [DONE]\n\n", msg)
			_ = pw.Close()
			return
		}
		_, werr := pw.Write(sseChunks(id, req.Model, created, res, includeUsage))
		_ = pw.CloseWithError(werr)
	}()
	return &provider.Response{
		Status:     http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       pr,
		Credential: credentialName,
	}, nil
}

func jsonResponse(status int, b []byte) *provider.Response {
	return &provider.Response{
		Status:     status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(b)),
		Credential: credentialName,
	}
}

// openAIError renders an OpenAI-shaped error body.
func openAIError(status int, msg string) *provider.Response {
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"message": msg, "type": "invalid_request_error"}})
	return jsonResponse(status, b)
}

// submit queues the one-node prompt. A ComfyUI validation failure (e.g. a model
// not in the node's list) is returned as a ready 4xx response, not an error.
func (p *Provider) submit(ctx context.Context, inputs map[string]any) (id string, rejected *provider.Response, err error) {
	payload, _ := json.Marshal(map[string]any{
		"prompt":    map[string]any{"1": map[string]any{"class_type": p.opt.Node, "inputs": inputs}},
		"client_id": "cerber",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/prompt", bytes.NewReader(payload))
	if err != nil {
		return "", nil, fmt.Errorf("comfyui: build prompt request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("comfyui: submit prompt: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusBadRequest {
		return "", openAIError(http.StatusBadRequest, "comfyui rejected the request: "+validationMessage(raw)), nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("comfyui: submit prompt: status %d: %s", resp.StatusCode, snippet(raw))
	}
	var out struct {
		PromptID string `json:"prompt_id"`
	}
	if json.Unmarshal(raw, &out) != nil || out.PromptID == "" {
		return "", nil, fmt.Errorf("comfyui: submit prompt: no prompt_id in %s", snippet(raw))
	}
	return out.PromptID, nil, nil
}

// validationMessage pulls the human part out of a ComfyUI /prompt 400. The model
// list ComfyUI appends ("not in [...]") is cut: it's long and /v1/models has it.
func validationMessage(raw []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Details string `json:"details"`
		} `json:"error"`
		NodeErrors map[string]struct {
			Errors []struct {
				Details string `json:"details"`
			} `json:"errors"`
		} `json:"node_errors"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return snippet(raw)
	}
	msg := e.Error.Details
	for _, ne := range e.NodeErrors {
		for _, d := range ne.Errors {
			if d.Details != "" {
				msg = d.Details
			}
		}
	}
	if msg == "" {
		msg = e.Error.Message
	}
	if i := strings.Index(msg, " not in ["); i > 0 {
		msg = msg[:i] + " is not available (see GET /v1/models)"
	}
	return msg
}

func snippet(b []byte) string {
	const max = 300
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// wait polls /history until the prompt finishes, calling keepAlive (if set)
// about every keepAliveEvery. On cancellation the prompt is removed from the
// queue if it hasn't started (a running one finishes; ComfyUI can only
// interrupt whatever is running, which may be someone else's job).
func (p *Provider) wait(ctx context.Context, id string, keepAlive func() error) (*nodeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, p.opt.MaxWait)
	defer cancel()
	ticker := time.NewTicker(p.opt.PollInterval)
	defer ticker.Stop()
	lastAlive := p.now()
	for {
		res, done, err := p.poll(ctx, id)
		if err != nil && ctx.Err() == nil {
			return nil, err
		}
		if done {
			p.forget(id)
			return res, nil
		}
		if keepAlive != nil && p.now().Sub(lastAlive) >= keepAliveEvery {
			if kerr := keepAlive(); kerr != nil { // client went away
				p.dequeue(id)
				return nil, kerr
			}
			lastAlive = p.now()
		}
		select {
		case <-ctx.Done():
			p.dequeue(id)
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("comfyui: prompt %s did not finish within %s", id, p.opt.MaxWait)
			}
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// historyEntry is the part of GET /history/<id> this provider reads.
type historyEntry struct {
	Status struct {
		StatusStr string              `json:"status_str"`
		Completed bool                `json:"completed"`
		Messages  [][]json.RawMessage `json:"messages"`
	} `json:"status"`
	Outputs map[string]struct {
		Text      []string `json:"text"`
		Reasoning []string `json:"reasoning"`
		Usage     []string `json:"usage"`
	} `json:"outputs"`
}

// poll reads the prompt's history entry once. done=false while it is queued or
// running (no entry yet).
func (p *Provider) poll(ctx context.Context, id string) (*nodeResult, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/history/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, false, fmt.Errorf("comfyui: build history request: %w", err)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("comfyui: poll history: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("comfyui: poll history: status %d", resp.StatusCode)
	}
	var h map[string]historyEntry
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return nil, false, fmt.Errorf("comfyui: decode history: %w", err)
	}
	e, ok := h[id]
	if !ok {
		return nil, false, nil
	}
	if e.Status.StatusStr == "error" {
		return nil, false, fmt.Errorf("comfyui: %s failed: %s", p.opt.Node, executionError(e))
	}
	out, ok := e.Outputs["1"]
	if !ok || len(out.Text) == 0 {
		if !e.Status.Completed && e.Status.StatusStr == "" {
			return nil, false, nil // entry written before outputs; keep polling
		}
		return nil, false, fmt.Errorf("comfyui: %s produced no text output", p.opt.Node)
	}
	res := &nodeResult{Content: out.Text[0], FinishReason: "stop"}
	if len(out.Reasoning) > 0 {
		res.Reasoning = out.Reasoning[0]
	}
	if len(out.Usage) > 0 {
		var u struct {
			PromptTokens     int64  `json:"prompt_tokens"`
			CompletionTokens int64  `json:"completion_tokens"`
			FinishReason     string `json:"finish_reason"`
		}
		if json.Unmarshal([]byte(out.Usage[0]), &u) == nil {
			res.PromptTokens, res.CompletionTokens = u.PromptTokens, u.CompletionTokens
			if u.FinishReason != "" {
				res.FinishReason = u.FinishReason
			}
		}
	}
	return res, true, nil
}

// executionError finds the exception message in a failed prompt's status log.
func executionError(e historyEntry) string {
	for _, m := range e.Status.Messages {
		if len(m) < 2 {
			continue
		}
		var kind string
		if json.Unmarshal(m[0], &kind) != nil || kind != "execution_error" {
			continue
		}
		var detail struct {
			ExceptionMessage string `json:"exception_message"`
		}
		if json.Unmarshal(m[1], &detail) == nil && detail.ExceptionMessage != "" {
			return strings.TrimSpace(detail.ExceptionMessage)
		}
	}
	return "execution error"
}

// forget deletes a finished prompt's history entry (best effort) so a busy
// instance's history isn't flooded with chat calls.
func (p *Provider) forget(id string) { p.postBestEffort("/history", id) }

// dequeue removes a prompt that is still pending (best effort).
func (p *Provider) dequeue(id string) { p.postBestEffort("/queue", id) }

func (p *Provider) postBestEffort(path, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string][]string{"delete": {id}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := p.http.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

type message struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

func usageOf(r *nodeResult) usage {
	return usage{r.PromptTokens, r.CompletionTokens, r.PromptTokens + r.CompletionTokens}
}

func completion(id, model string, created int64, r *nodeResult) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-" + id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message{Role: "assistant", Content: r.Content, ReasoningContent: r.Reasoning},
			"finish_reason": r.FinishReason,
		}},
		"usage": usageOf(r),
	}
}

// sseChunks renders a finished answer as an OpenAI chat stream: the whole text
// in one delta, the finish reason, an optional usage chunk, then [DONE].
func sseChunks(id, model string, created int64, r *nodeResult, includeUsage bool) []byte {
	var buf bytes.Buffer
	chunk := func(choices any, extra map[string]any) {
		c := map[string]any{"id": "chatcmpl-" + id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": choices}
		for k, v := range extra {
			c[k] = v
		}
		b, _ := json.Marshal(c)
		buf.WriteString("data: ")
		buf.Write(b)
		buf.WriteString("\n\n")
	}
	chunk([]map[string]any{{"index": 0, "delta": message{Role: "assistant", Content: r.Content, ReasoningContent: r.Reasoning}, "finish_reason": nil}}, nil)
	chunk([]map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": r.FinishReason}}, nil)
	if includeUsage {
		chunk([]map[string]any{}, map[string]any{"usage": usageOf(r)})
	}
	buf.WriteString("data: [DONE]\n\n")
	return buf.Bytes()
}

// ProbeCredential checks that ComfyUI is up and the node is installed, and lists
// its models as "comfyui-<model>". The credential is ignored (ComfyUI's API has
// no auth); it exists only because the probe loop is per credential.
func (p *Provider) ProbeCredential(ctx context.Context, _ *credential.Credential) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/object_info/"+url.PathEscape(p.opt.Node), nil)
	if err != nil {
		return nil, fmt.Errorf("comfyui: build object_info request: %w", err)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("comfyui: object_info status %d", resp.StatusCode)
	}
	var info map[string]struct {
		Input struct {
			Required struct {
				Model []json.RawMessage `json:"model"`
			} `json:"required"`
		} `json:"input"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("comfyui: decode object_info: %w", err)
	}
	node, ok := info[p.opt.Node]
	if !ok {
		return nil, fmt.Errorf("comfyui: node %s is not installed (see github.com/tggo/comfyui-cerber-llm)", p.opt.Node)
	}
	var names []string
	if len(node.Input.Required.Model) > 0 {
		_ = json.Unmarshal(node.Input.Required.Model[0], &names)
	}
	models := make([]string, 0, len(names))
	for _, n := range names {
		if n != "" && !strings.HasPrefix(n, "(") { // "(no GGUF models found …)" placeholder
			models = append(models, ModelPrefix+n)
		}
	}
	return models, nil
}
