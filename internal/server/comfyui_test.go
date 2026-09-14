package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tggo/cerber/internal/provider/comfyui"
	provmocks "github.com/tggo/cerber/internal/provider/mocks"
	"github.com/tggo/cerber/internal/usage"

	"github.com/stretchr/testify/mock"
)

// comfyServer wires a server with a comfyui provider whose ComfyUI answers
// every chat with "pong" (12 prompt / 3 completion tokens).
func comfyServer(t *testing.T) (*Server, *[]string) {
	t.Helper()
	var submitted []string
	doer := provmocks.NewHTTPDoer(t)
	doer.EXPECT().Do(mock.Anything).RunAndReturn(func(r *http.Request) (*http.Response, error) {
		body := `{}`
		switch {
		case r.URL.Path == "/prompt":
			b, _ := io.ReadAll(r.Body)
			submitted = append(submitted, string(b))
			body = `{"prompt_id":"p9"}`
		case r.URL.Path == "/history/p9":
			body = `{"p9":{"status":{"status_str":"success","completed":true},"outputs":{"1":{"text":["pong"],"usage":["{\"prompt_tokens\":12,\"completion_tokens\":3}"]}}}}`
		case r.URL.Path == "/object_info/CerberLLMChat":
			body = `{"CerberLLMChat":{"input":{"required":{"model":[["ollama/gemma3:12b"]]}}}}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}).Maybe()
	s, _ := newServer(t, newStore(t, 1))
	s.RegisterChatter(comfyui.New("http://gpu0:8188", doer, comfyui.Options{PollInterval: time.Millisecond}))
	return s, &submitted
}

func TestComfyUI_ChatThroughServer(t *testing.T) {
	for _, stream := range []bool{false, true} {
		s, submitted := comfyServer(t)
		body := `{"model":"comfyui-ollama/gemma3:12b","messages":[{"role":"user","content":"ping"}]}`
		if stream {
			body = `{"model":"comfyui-ollama/gemma3:12b","stream":true,"messages":[{"role":"user","content":"ping"}]}`
		}
		rec := do(t, s.Handler(), "POST", "/v1/chat/completions", body, clientKey)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"pong"`) {
			t.Fatalf("stream=%v: %d %s", stream, rec.Code, rec.Body.String())
		}
		if stream && strings.Contains(rec.Body.String(), `"choices":[]`) {
			t.Errorf("injected usage chunk leaked to the client: %s", rec.Body.String())
		}
		var p struct {
			Prompt map[string]struct {
				Inputs map[string]any `json:"inputs"`
			} `json:"prompt"`
		}
		if len(*submitted) != 1 || json.Unmarshal([]byte((*submitted)[0]), &p) != nil || p.Prompt["1"].Inputs["model"] != "ollama/gemma3:12b" {
			t.Errorf("stream=%v: submitted %v", stream, *submitted)
		}
		evs, _ := s.usage.RecentRequests(usage.RequestFilter{}, 0, 1)
		if len(evs) != 1 || evs[0].InputTokens != 12 || evs[0].OutputTokens != 3 || evs[0].Cost != 0 {
			t.Errorf("stream=%v: recorded %+v", stream, evs)
		}
	}
}

func TestComfyUI_ToolsRejectedAndModelsDiscovered(t *testing.T) {
	s, submitted := comfyServer(t)
	rec := do(t, s.Handler(), "POST", "/v1/chat/completions",
		`{"model":"comfyui-ollama/gemma3:12b","tools":[{"type":"function","function":{"name":"f"}}],"messages":[{"role":"user","content":"x"}]}`, clientKey)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "tool calling is not supported") || len(*submitted) != 0 {
		t.Errorf("tools = %d %s (submitted %d)", rec.Code, rec.Body.String(), len(*submitted))
	}
	store := newStore(t, 1)
	s.RegisterProviderStore("comfyui", store)
	s.ProbeAll(t.Context())
	if got := s.providerModels("comfyui"); len(got) != 1 || got[0] != "comfyui-ollama/gemma3:12b" {
		t.Errorf("discovered = %v", got)
	}
}
