package comfyui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tggo/cerber/internal/provider"
	"github.com/tggo/cerber/internal/provider/mocks"

	"github.com/stretchr/testify/mock"
)

// fakeComfy scripts a ComfyUI instance behind the mocked HTTPDoer.
type fakeComfy struct {
	mu          sync.Mutex
	submitCode  int
	submitBody  string
	submitErr   error
	pendingPoll int    // polls answered with {} before the entry appears
	history     string // the /history/<id> entry JSON (without the id wrapper)
	historyCode int
	objectInfo  string
	objectCode  int

	prompts []map[string]any // decoded /prompt payloads
	deletes map[string]int   // path -> delete calls
	polls   int
}

func (f *fakeComfy) doer(t *testing.T) *mocks.HTTPDoer {
	d := mocks.NewHTTPDoer(t)
	d.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		reply := func(code int, body string) (*http.Response, error) {
			if code == 0 {
				code = 200
			}
			return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/prompt":
			if f.submitErr != nil {
				return nil, f.submitErr
			}
			var p map[string]any
			_ = json.NewDecoder(r.Body).Decode(&p)
			f.prompts = append(f.prompts, p)
			body := f.submitBody
			if body == "" {
				body = `{"prompt_id":"p1","number":1,"node_errors":{}}`
			}
			return reply(f.submitCode, body)
		case r.Method == "GET" && r.URL.Path == "/history/p1":
			f.polls++
			if f.polls <= f.pendingPoll {
				return reply(200, `{}`)
			}
			if f.historyCode != 0 && f.historyCode != 200 {
				return reply(f.historyCode, "")
			}
			return reply(200, `{"p1":`+f.history+`}`)
		case r.Method == "POST" && (r.URL.Path == "/history" || r.URL.Path == "/queue"):
			if f.deletes == nil {
				f.deletes = map[string]int{}
			}
			f.deletes[r.URL.Path]++
			return reply(200, `{}`)
		case r.Method == "GET" && r.URL.Path == "/object_info/CerberLLMChat":
			return reply(f.objectCode, f.objectInfo)
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL)
		return reply(404, "")
	}).Maybe()
	return d
}

const okHistory = `{"status":{"status_str":"success","completed":true,"messages":[]},
 "outputs":{"1":{"text":["pong"],"reasoning":["thought"],"usage":["{\"prompt_tokens\": 12, \"completion_tokens\": 3, \"finish_reason\": \"length\"}"]}}}`

func newTestProvider(t *testing.T, f *fakeComfy, opt Options) *Provider {
	t.Helper()
	if opt.PollInterval == 0 {
		opt.PollInterval = time.Millisecond
	}
	p := New("http://gpu0:8188/", f.doer(t), opt)
	p.seed = func() uint32 { return 4242 }
	p.now = func() time.Time { return time.Unix(1700000000, 0) }
	return p
}

func readAll(t *testing.T, r *provider.Response) string {
	t.Helper()
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestChat_NonStream(t *testing.T) {
	f := &fakeComfy{pendingPoll: 2, history: okHistory}
	p := newTestProvider(t, f, Options{NCtx: 4096, KeepLoaded: true})
	body := `{"model":"comfyui-ollama/gemma3:12b","messages":[{"role":"system","content":"be brief"},{"role":"user","content":[{"type":"text","text":"ping"}]}],
		"max_tokens":10,"max_completion_tokens":20,"temperature":0,"stop":"END","reasoning_effort":"high"}`
	resp, err := p.Chat(context.Background(), []byte(body), false, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Status != 200 || resp.Credential != "comfyui" {
		t.Fatalf("resp = %+v", resp)
	}
	var out struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role, Content    string
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		RawUsage map[string]int64 `json:"usage"`
	}
	if err := json.Unmarshal([]byte(readAll(t, resp)), &out); err != nil {
		t.Fatal(err)
	}
	c := out.Choices[0]
	if out.ID != "chatcmpl-p1" || out.Model != "comfyui-ollama/gemma3:12b" || c.Message.Content != "pong" ||
		c.Message.ReasoningContent != "thought" || c.FinishReason != "length" {
		t.Errorf("completion = %+v", out)
	}
	if out.RawUsage["prompt_tokens"] != 12 || out.RawUsage["completion_tokens"] != 3 || out.RawUsage["total_tokens"] != 15 {
		t.Errorf("usage = %v", out.RawUsage)
	}

	node := f.prompts[0]["prompt"].(map[string]any)["1"].(map[string]any)
	if node["class_type"] != "CerberLLMChat" {
		t.Errorf("class_type = %v", node["class_type"])
	}
	in := node["inputs"].(map[string]any)
	want := map[string]any{
		"model": "ollama/gemma3:12b", "max_tokens": 20.0, "temperature": 0.0, "top_p": 0.95, "seed": 4242.0,
		"n_ctx": 4096.0, "n_gpu_layers": -1.0, "think": true, "keep_loaded": true, "stop": `["END"]`,
	}
	for k, v := range want {
		if in[k] != v {
			t.Errorf("input %s = %v, want %v", k, in[k], v)
		}
	}
	var msgs []map[string]any
	if err := json.Unmarshal([]byte(in["messages"].(string)), &msgs); err != nil || len(msgs) != 2 {
		t.Errorf("messages input = %v (%v)", in["messages"], err)
	}
	if f.deletes["/history"] != 1 {
		t.Errorf("finished prompt not removed from history: %v", f.deletes)
	}
}

func TestChat_DefaultsAndClientSeed(t *testing.T) {
	f := &fakeComfy{history: okHistory}
	p := newTestProvider(t, f, Options{})
	resp, err := p.Chat(context.Background(), []byte(`{"model":"comfyui-m","messages":[{"role":"user","content":"x"}],"seed":7,"stop":["a","b"]}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, resp)
	in := f.prompts[0]["prompt"].(map[string]any)["1"].(map[string]any)["inputs"].(map[string]any)
	if in["seed"] != 7.0 || in["max_tokens"] != float64(DefaultMaxTokens) || in["n_ctx"] != float64(DefaultNCtx) ||
		in["temperature"] != 0.8 || in["think"] != false || in["keep_loaded"] != false || in["stop"] != `["a","b"]` {
		t.Errorf("inputs = %v", in)
	}
}

func TestChat_Stream(t *testing.T) {
	for _, includeUsage := range []bool{false, true} {
		f := &fakeComfy{pendingPoll: 1, history: okHistory}
		p := newTestProvider(t, f, Options{})
		body := `{"model":"comfyui-m","stream":true,"messages":[{"role":"user","content":"x"}]}`
		if includeUsage {
			body = `{"model":"comfyui-m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"x"}]}`
		}
		resp, err := p.Chat(context.Background(), []byte(body), true, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Errorf("content-type = %q", resp.Header.Get("Content-Type"))
		}
		out := readAll(t, resp)
		for _, want := range []string{`"delta":{"role":"assistant","content":"pong","reasoning_content":"thought"}`, `"finish_reason":"length"`, "data: [DONE]\n\n"} {
			if !strings.Contains(out, want) {
				t.Errorf("include_usage=%v: stream missing %q:\n%s", includeUsage, want, out)
			}
		}
		if got := strings.Contains(out, `"choices":[],`); got != includeUsage {
			t.Errorf("include_usage=%v: usage chunk present = %v:\n%s", includeUsage, got, out)
		}
	}
}

func TestChat_StreamKeepAlive(t *testing.T) {
	f := &fakeComfy{pendingPoll: 3, history: okHistory}
	p := newTestProvider(t, f, Options{})
	var mu sync.Mutex
	clock := time.Unix(0, 0)
	p.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(10 * time.Second) // every read of the clock is 10s later
		return clock
	}
	resp, err := p.Chat(context.Background(), []byte(`{"model":"comfyui-m","messages":[{"role":"user","content":"x"}]}`), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out := readAll(t, resp); !strings.HasPrefix(out, ": waiting for comfyui\n\n") {
		t.Errorf("no keep-alive before the answer:\n%s", out)
	}
}

func TestChat_BadRequests(t *testing.T) {
	for name, body := range map[string]string{
		"invalid json":   `{`,
		"no prefix":      `{"model":"gemma3","messages":[{"role":"user","content":"x"}]}`,
		"bare prefix":    `{"model":"comfyui-","messages":[{"role":"user","content":"x"}]}`,
		"tools":          `{"model":"comfyui-m","tools":[{"type":"function"}],"messages":[{"role":"user","content":"x"}]}`,
		"n>1":            `{"model":"comfyui-m","n":2,"messages":[{"role":"user","content":"x"}]}`,
		"no messages":    `{"model":"comfyui-m","messages":[]}`,
		"message array":  `{"model":"comfyui-m","messages":[1]}`,
		"tool role":      `{"model":"comfyui-m","messages":[{"role":"tool","content":"x"}]}`,
		"image part":     `{"model":"comfyui-m","messages":[{"role":"user","content":[{"type":"image_url"}]}]}`,
		"content number": `{"model":"comfyui-m","messages":[{"role":"user","content":5}]}`,
		"stop number":    `{"model":"comfyui-m","stop":5,"messages":[{"role":"user","content":"x"}]}`,
	} {
		f := &fakeComfy{}
		p := newTestProvider(t, f, Options{})
		_, err := p.Chat(context.Background(), []byte(body), false, nil)
		var bad *provider.BadRequestError
		if !errors.As(err, &bad) {
			t.Errorf("%s: err = %v, want BadRequestError", name, err)
		}
		if len(f.prompts) != 0 {
			t.Errorf("%s: submitted to ComfyUI despite a bad request", name)
		}
	}
}

func TestChat_ValidationRejected(t *testing.T) {
	f := &fakeComfy{submitCode: 400, submitBody: `{"error":{"type":"prompt_outputs_failed_validation","message":"Prompt outputs failed validation","details":""},
		"node_errors":{"1":{"errors":[{"details":"Value not in list: model: 'gone' not in ['a','b']"}]}}}`}
	p := newTestProvider(t, f, Options{})
	resp, err := p.Chat(context.Background(), []byte(`{"model":"comfyui-gone","messages":[{"role":"user","content":"x"}]}`), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := readAll(t, resp)
	if resp.Status != 400 || !strings.Contains(out, "model: 'gone' is not available (see GET /v1/models)") || strings.Contains(out, "['a','b']") {
		t.Errorf("status %d body %s", resp.Status, out)
	}
	if got := validationMessage([]byte("not json")); got != "not json" {
		t.Errorf("validationMessage(non-JSON) = %q", got)
	}
	if got := validationMessage([]byte(`{"error":{"message":"Prompt has no outputs"}}`)); got != "Prompt has no outputs" {
		t.Errorf("validationMessage(message only) = %q", got)
	}
}

func TestChat_UpstreamFailures(t *testing.T) {
	msgs := `{"model":"comfyui-m","messages":[{"role":"user","content":"x"}]}`
	for name, tc := range map[string]struct {
		f    *fakeComfy
		want string
	}{
		"submit transport": {&fakeComfy{submitErr: errors.New("connection refused")}, "connection refused"},
		"submit 500":       {&fakeComfy{submitCode: 500, submitBody: "boom"}, "status 500: boom"},
		"no prompt_id":     {&fakeComfy{submitBody: `{}`}, "no prompt_id"},
		"history 500":      {&fakeComfy{historyCode: 500}, "poll history: status 500"},
		"history garbage":  {&fakeComfy{history: `nope`}, "decode history"},
		"execution error": {&fakeComfy{history: `{"status":{"status_str":"error","messages":[["execution_start",{}],
			["execution_error",{"exception_message":"Failed to load model from file: x\n"}]]},"outputs":{}}`}, "CerberLLMChat failed: Failed to load model from file: x"},
		"error without message": {&fakeComfy{history: `{"status":{"status_str":"error","messages":[["x"]]},"outputs":{}}`}, "execution error"},
		"no text output":        {&fakeComfy{history: `{"status":{"status_str":"success","completed":true},"outputs":{}}`}, "produced no text output"},
	} {
		p := newTestProvider(t, tc.f, Options{})
		_, err := p.Chat(context.Background(), []byte(msgs), false, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestChat_StreamErrorEvent(t *testing.T) {
	f := &fakeComfy{history: `{"status":{"status_str":"error","messages":[["execution_error",{"exception_message":"CUDA out of memory"}]]},"outputs":{}}`}
	p := newTestProvider(t, f, Options{})
	resp, err := p.Chat(context.Background(), []byte(`{"model":"comfyui-m","messages":[{"role":"user","content":"x"}]}`), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := readAll(t, resp)
	if !strings.Contains(out, `"error":{"message":"comfyui: CerberLLMChat failed: CUDA out of memory"`) || !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("stream = %s", out)
	}
}

func TestChat_CancelAndTimeoutDequeue(t *testing.T) {
	msgs := []byte(`{"model":"comfyui-m","messages":[{"role":"user","content":"x"}]}`)

	f := &fakeComfy{pendingPoll: 1 << 30}
	p := newTestProvider(t, f, Options{MaxWait: 20 * time.Millisecond})
	_, err := p.Chat(context.Background(), msgs, false, nil)
	if err == nil || !strings.Contains(err.Error(), "did not finish within 20ms") {
		t.Errorf("timeout err = %v", err)
	}
	if f.deletes["/queue"] != 1 {
		t.Errorf("timed-out prompt not dequeued: %v", f.deletes)
	}

	f = &fakeComfy{pendingPoll: 1 << 30}
	p = newTestProvider(t, f, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	go func() { time.Sleep(5 * time.Millisecond); cancel() }()
	if _, err = p.Chat(ctx, msgs, false, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancel err = %v", err)
	}
	if f.deletes["/queue"] != 1 {
		t.Errorf("canceled prompt not dequeued: %v", f.deletes)
	}
}

func TestProbeCredential(t *testing.T) {
	f := &fakeComfy{objectInfo: `{"CerberLLMChat":{"input":{"required":{"model":[["ollama/gemma3:12b","qwen/Q.gguf"]]}}}}`}
	p := newTestProvider(t, f, Options{})
	models, err := p.ProbeCredential(context.Background(), nil)
	if err != nil || strings.Join(models, ",") != "comfyui-ollama/gemma3:12b,comfyui-qwen/Q.gguf" {
		t.Errorf("models = %v err = %v", models, err)
	}
	if p.Name() != "comfyui" || p.BaseURL() != "http://gpu0:8188" {
		t.Errorf("name/base = %s %s", p.Name(), p.BaseURL())
	}

	for name, tc := range map[string]struct {
		f    *fakeComfy
		want string
	}{
		"placeholder only": {&fakeComfy{objectInfo: `{"CerberLLMChat":{"input":{"required":{"model":[["(no GGUF models found — see the README)"]]}}}}`}, ""},
		"node missing":     {&fakeComfy{objectInfo: `{}`}, "is not installed"},
		"status":           {&fakeComfy{objectCode: 500}, "status 500"},
		"garbage":          {&fakeComfy{objectInfo: `x`}, "decode object_info"},
	} {
		models, err := newTestProvider(t, tc.f, Options{}).ProbeCredential(context.Background(), nil)
		if tc.want == "" {
			if err != nil || len(models) != 0 {
				t.Errorf("%s: models = %v err = %v", name, models, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
