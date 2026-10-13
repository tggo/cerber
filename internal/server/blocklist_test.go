package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tggo/cerber/internal/config"
	"github.com/tggo/cerber/internal/credential"
	"github.com/tggo/cerber/internal/modelblock"
	provmocks "github.com/tggo/cerber/internal/provider/mocks"
	"github.com/tggo/cerber/internal/provider/openai"
	"github.com/tggo/cerber/internal/usage"

	"github.com/stretchr/testify/mock"
)

// silentOpenAI is a real openai provider whose HTTPDoer has NO expectations: any
// upstream call fails the test (mockery mocks panic on unexpected calls).
func silentOpenAI(t *testing.T, name string) *openai.Provider {
	t.Helper()
	st, err := credential.NewStore([]config.Credential{{Type: config.CredentialAPIKey, Name: "a", Key: "sk"}})
	if err != nil {
		t.Fatal(err)
	}
	return openai.New(name, "https://api.openai.com", st, provmocks.NewHTTPDoer(t))
}

func blockedServer(t *testing.T, seed ...string) *Server {
	t.Helper()
	// The Upstream mock from newServer has no expectations either.
	s, _ := newServer(t, newStore(t, 1))
	s.RegisterChatter(silentOpenAI(t, "openai"))
	bl, err := modelblock.New(seed)
	if err != nil {
		t.Fatal(err)
	}
	s.SetModelBlocklist(bl)
	s.SetModelAliases(map[string]string{"haiku": "claude-haiku-4-5-20251001"})
	return s
}

func multipartBody(t *testing.T, model string) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "a.wav")
	_, _ = fw.Write([]byte("RIFF"))
	if model != "" {
		_ = mw.WriteField("model", model)
	}
	_ = mw.Close()
	return buf.String(), mw.FormDataContentType()
}

func TestBlocked_EveryEndpointDialect(t *testing.T) {
	chat := `{"model":"%s","messages":[{"role":"user","content":"hi"}]}`
	cases := []struct {
		name, path, model, body string
		anthropic               bool
	}{
		{"chat", "/v1/chat/completions", "claude-opus-4-8", strings.Replace(chat, "%s", "claude-opus-4-8", 1), false},
		{"chat via alias", "/v1/chat/completions", "haiku", strings.Replace(chat, "%s", "haiku", 1), false},
		{"messages", "/v1/messages", "claude-opus-5-5", `{"model":"claude-opus-5-5","max_tokens":1,"messages":[]}`, true},
		{"messages dated", "/v1/messages", "claude-haiku-4-5-20251001", `{"model":"claude-haiku-4-5-20251001","max_tokens":1}`, true},
		{"messages alias", "/v1/messages", "haiku", `{"model":"haiku","max_tokens":1}`, true},
		{"count_tokens", "/v1/messages/count_tokens", "claude-opus-4-8", `{"model":"claude-opus-4-8","messages":[]}`, true},
		{"responses", "/v1/responses", "gpt-image-*x", `{"model":"gpt-image-1x","input":"x"}`, false},
		{"completions", "/v1/completions", "gpt-image-1x", `{"model":"gpt-image-1x","prompt":"x"}`, false},
		{"embeddings", "/v1/embeddings", "gpt-image-1x", `{"model":"gpt-image-1x","input":"x"}`, false},
		{"images", "/v1/images/generations", "gpt-image-1x", `{"model":"gpt-image-1x","prompt":"x"}`, false},
		{"moderations", "/v1/moderations", "omni-moderation-latest", `{"input":"x"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := blockedServer(t, "claude-opus-*", "claude-haiku-4-5", "gpt-image-*x", "omni-moderation-latest")
			rec := do(t, s.Handler(), "POST", c.path, c.body, clientKey)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			e := got["error"].(map[string]any)
			msg := e["message"].(string)
			if !strings.HasSuffix(msg, `" is blocked on this cerber instance`) || !strings.HasPrefix(msg, `model "`) {
				t.Errorf("message=%q", msg)
			}
			if c.anthropic {
				if got["type"] != "error" || e["type"] != "permission_error" {
					t.Errorf("anthropic shape wrong: %s", rec.Body)
				}
			} else if _, has := got["type"]; has || e["type"] != "invalid_request_error" || e["code"] != "model_blocked" {
				t.Errorf("openai shape wrong: %s", rec.Body)
			}
			// Logged as an error, no cost, visible in the request log.
			evs, _ := s.Usage().RecentRequests(usage.RequestFilter{}, 0, 0)
			if len(evs) != 1 || !evs[0].Error || evs[0].Cost != 0 || evs[0].Status != http.StatusForbidden {
				t.Errorf("request log = %+v", evs)
			}
		})
	}
}

func TestBlocked_MessageNamesRequestedModel(t *testing.T) {
	s := blockedServer(t, "claude-haiku-4-5")
	rec := do(t, s.Handler(), "POST", "/v1/messages", `{"model":"haiku","max_tokens":1}`, clientKey)
	if !strings.Contains(rec.Body.String(), `model \"haiku\" is blocked`) {
		t.Errorf("body=%s", rec.Body)
	}
}

func TestBlocked_Transcriptions(t *testing.T) {
	s := blockedServer(t, "whisper-*")
	for _, model := range []string{"whisper-1", ""} { // explicit and default
		body, ct := multipartBody(t, model)
		r := httptest.NewRequest("POST", "/v1/audio/transcriptions", strings.NewReader(body))
		r.Header.Set("Content-Type", ct)
		r.Header.Set("Authorization", "Bearer "+clientKey)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Errorf("model %q: status=%d %s", model, rec.Code, rec.Body)
		}
	}
	if got := multipartModel([]byte("x"), "text/plain"); got != "" {
		t.Errorf("non-multipart model = %q", got)
	}
	body, ct := multipartBody(t, "")
	if got := multipartModel([]byte(body), ct); got != "" {
		t.Errorf("absent model = %q", got)
	}
}

func TestBlocked_AppliesToEveryCaller(t *testing.T) {
	s := blockedServer(t, "claude-opus-*")
	s.SetAllowLocalhost(true)
	// Localhost without a key.
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"claude-opus-4-8"}`))
	r.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("localhost status=%d", rec.Code)
	}
}

func TestBlocked_UnblockedModelPasses(t *testing.T) {
	s := blockedServer(t, "claude-opus-*")
	// Not blocked → proceeds to routing (unknown model → 400, not 403).
	rec := do(t, s.Handler(), "POST", "/v1/chat/completions", `{"model":"zzz-unknown"}`, clientKey)
	if rec.Code == http.StatusForbidden {
		t.Errorf("unblocked model got 403")
	}
}

func TestBlocked_FallbackSkipsBlockedTarget(t *testing.T) {
	s, up := newServer(t, newStore(t, 1))
	chatter := registerChatter(t, s, "openai")
	bl, _ := modelblock.New([]string{"gpt-4o"})
	s.SetModelBlocklist(bl)
	s.SetFallbacks([]config.Fallback{{Model: "claude", To: []string{"gpt-4o", "gpt-4.1"}}})

	up.EXPECT().Send(mock.Anything, mock.Anything, false, mock.Anything, mock.Anything).
		Return(resp(503, "application/json", `{"error":"down"}`), nil).Once()
	// Only gpt-4.1 may be called; the blocked gpt-4o must never reach the chatter.
	chatter.EXPECT().Chat(mock.Anything,
		mock.MatchedBy(func(b []byte) bool { return strings.Contains(string(b), "gpt-4.1") }),
		false, mock.Anything).
		Return(chatterResp(200, `{"id":"ok41"}`), nil).Once()

	rec := do(t, s.Handler(), "POST", "/v1/chat/completions", `{"model":"claude-x","stream":false,"messages":[{"role":"user","content":"hi"}]}`, clientKey)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok41") {
		t.Fatalf("fallback = %d %s", rec.Code, rec.Body)
	}
}

func TestBlocked_FallbackHeaderAndPrimary(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	bl, _ := modelblock.New([]string{"gpt-*"})
	s.SetModelBlocklist(bl)
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Cerber-Fallback", "gpt-4o, claude-b, GPT-5")
	if got := s.dropBlocked(s.openAITargets(r, "claude-a")); strings.Join(got, ",") != "claude-a,claude-b" {
		t.Errorf("targets=%v", got)
	}
	// Primary blocked → 403 right away, no silent fallback (no mock expectations set).
	r2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	r2.Header.Set("Authorization", "Bearer "+clientKey)
	r2.Header.Set("X-Cerber-Fallback", "claude-b")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r2)
	if rec.Code != http.StatusForbidden {
		t.Errorf("primary blocked status=%d", rec.Code)
	}
	if got := s.dropBlocked([]string{"only"}); len(got) != 1 {
		t.Errorf("single target = %v", got)
	}
}

func TestBlocked_ModelListsHideBlocked(t *testing.T) {
	s := blockedServer(t, "claude-opus-*", "gpt-*")
	s.setProviderModels("anthropic", []string{"claude-opus-4-8", "claude-sonnet-5"})
	s.setProviderModels("openai", []string{"gpt-4o"})

	rec := do(t, s.Handler(), "GET", "/v1/models", "", clientKey)
	body := rec.Body.String()
	if !strings.Contains(body, "claude-sonnet-5") || strings.Contains(body, "claude-opus-4-8") || strings.Contains(body, "gpt-4o") {
		t.Errorf("/v1/models = %s", body)
	}
	doc := do(t, s.Handler(), "GET", "/llm.md", "", clientKey).Body.String()
	if !strings.Contains(doc, "- `claude-sonnet-5`") || strings.Contains(doc, "- `claude-opus-4-8`") || strings.Contains(doc, "- `gpt-4o`") {
		t.Errorf("llm.md lists blocked models")
	}
	if !strings.Contains(doc, "## Blocked models") || !strings.Contains(doc, "`claude-opus-*`") {
		t.Errorf("llm.md missing blocked section")
	}
	// A provider whose models are ALL blocked is not shown as "use standard names".
	if strings.Contains(doc, "### openai\n\nUse this provider's standard") {
		t.Errorf("fully-blocked provider shown with generic hint")
	}
}

func adminDo(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, s.Handler(), method, path, body, "mgmt-secret")
}

func TestBlockedAdmin_AddListRemovePersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked_models.json")
	newS := func() *Server {
		s, _ := newServer(t, newStore(t, 1))
		s.SetManagementKey("mgmt-secret")
		bl, err := modelblock.Load(path, []string{"claude-fable-*"})
		if err != nil {
			t.Fatal(err)
		}
		s.SetModelBlocklist(bl)
		s.setProviderModels("anthropic", []string{"claude-haiku-4-5", "claude-haiku-4-5-20251001", "claude-fable-1", "claude-sonnet-5"})
		return s
	}
	type listResp struct {
		Blocked []blockedView `json:"blocked"`
	}
	decode := func(rec *httptest.ResponseRecorder) listResp {
		var l listResp
		if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
			t.Fatalf("decode %q: %v", rec.Body, err)
		}
		return l
	}

	s := newS()
	// Auth: client key is not enough with a management key.
	if rec := do(t, s.Handler(), "GET", "/admin/blocked-models", "", clientKey); rec.Code != http.StatusUnauthorized {
		t.Errorf("client key on admin = %d", rec.Code)
	}
	if rec := adminDo(t, s, "POST", "/admin/blocked-models", `{"mask":" Claude-Haiku-4-5 "}`); rec.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", rec.Code, rec.Body)
	}
	if rec := adminDo(t, s, "POST", "/admin/blocked-models", `{"mask":"claude-haiku-4-5"}`); rec.Code != http.StatusOK {
		t.Errorf("duplicate add = %d", rec.Code)
	}
	for _, bad := range []string{`{"mask":""}`, `{"mask":"a b"}`, `{}`, `not json`} {
		if rec := adminDo(t, s, "POST", "/admin/blocked-models", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("add %s = %d, want 400", bad, rec.Code)
		}
	}

	l := decode(adminDo(t, s, "GET", "/admin/blocked-models", ""))
	want := []blockedView{{"claude-fable-*", "config", 1}, {"claude-haiku-4-5", "operator", 2}}
	if len(l.Blocked) != 2 || l.Blocked[0] != want[0] || l.Blocked[1] != want[1] {
		t.Fatalf("list=%+v want %+v", l.Blocked, want)
	}

	// Takes effect immediately.
	if rec := do(t, s.Handler(), "POST", "/v1/messages", `{"model":"claude-haiku-4-5"}`, clientKey); rec.Code != http.StatusForbidden {
		t.Errorf("newly blocked model = %d", rec.Code)
	}

	// "Restart": a fresh server reading the same file still has the operator mask.
	s2 := newS()
	if l := decode(adminDo(t, s2, "GET", "/admin/blocked-models", "")); len(l.Blocked) != 2 {
		t.Fatalf("after restart: %+v", l.Blocked)
	}

	// Remove: config entry refused, unknown 404, bad 400, real one works (DELETE + POST forms).
	if rec := adminDo(t, s2, "DELETE", "/admin/blocked-models?mask=claude-fable-*", ""); rec.Code != http.StatusConflict {
		t.Errorf("remove config = %d", rec.Code)
	}
	if rec := adminDo(t, s2, "DELETE", "/admin/blocked-models?mask=nope", ""); rec.Code != http.StatusNotFound {
		t.Errorf("remove unknown = %d", rec.Code)
	}
	if rec := adminDo(t, s2, "POST", "/admin/blocked-models/delete", `{"mask":"a b"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("remove invalid = %d", rec.Code)
	}
	if rec := adminDo(t, s2, "POST", "/admin/blocked-models/delete", `{bad`); rec.Code != http.StatusBadRequest {
		t.Errorf("remove bad json = %d", rec.Code)
	}
	if rec := adminDo(t, s2, "DELETE", "/admin/blocked-models", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("remove no mask = %d", rec.Code)
	}
	if rec := adminDo(t, s2, "DELETE", "/admin/blocked-models?mask=CLAUDE-haiku-4-5", ""); rec.Code != http.StatusOK {
		t.Fatalf("remove = %d %s", rec.Code, rec.Body)
	}
	if l := decode(adminDo(t, newS(), "GET", "/admin/blocked-models", "")); len(l.Blocked) != 1 || l.Blocked[0].Mask != "claude-fable-*" {
		t.Errorf("after remove+restart: %+v", l.Blocked)
	}
	if _, still := s2.blocklist.Blocked("claude-haiku-4-5"); still {
		t.Errorf("unblocked model still blocked")
	}
	// POST-form removal of an existing mask.
	adminDo(t, s2, "POST", "/admin/blocked-models", `{"mask":"x-*"}`)
	if rec := adminDo(t, s2, "POST", "/admin/blocked-models/delete", `{"mask":"x-*"}`); rec.Code != http.StatusOK {
		t.Errorf("POST delete = %d", rec.Code)
	}
}

func TestBlockedAdmin_PersistFailureIs500(t *testing.T) {
	ro := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	if f, err := os.Create(filepath.Join(ro, "probe")); err == nil { // running as root: perms don't bind
		f.Close()
		t.Skip("cannot make a directory unwritable here")
	}
	s, _ := newServer(t, newStore(t, 1))
	s.SetManagementKey("mgmt-secret")
	bl, err := modelblock.Load(filepath.Join(ro, "blocked.json"), []string{"seed"})
	if err != nil {
		t.Fatal(err)
	}
	s.SetModelBlocklist(bl)
	if rec := adminDo(t, s, "POST", "/admin/blocked-models", `{"mask":"b"}`); rec.Code != http.StatusInternalServerError {
		t.Errorf("add persist failure = %d, want 500", rec.Code)
	}
	if l := s.blocklist.Entries(); len(l) != 1 {
		t.Errorf("failed add changed list: %v", l)
	}
	// Removal persist failure: put an operator entry in memory-only via a writable list first.
	rw := filepath.Join(t.TempDir(), "b.json")
	bl2, _ := modelblock.Load(rw, nil)
	_, _, _ = bl2.Add("keep")
	s.SetModelBlocklist(bl2)
	if err := os.Remove(rw); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(rw, 0o700); err != nil { // path is now a directory: rename onto it fails
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rw, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := adminDo(t, s, "DELETE", "/admin/blocked-models?mask=keep", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("remove persist failure = %d, want 500", rec.Code)
	}
}

func TestSetModelBlocklistNilKeepsCurrent(t *testing.T) {
	s, _ := newServer(t, newStore(t, 1))
	bl, _ := modelblock.New([]string{"x"})
	s.SetModelBlocklist(bl)
	s.SetModelBlocklist(nil)
	if _, ok := s.blocklist.Blocked("x"); !ok {
		t.Error("nil must not replace the list")
	}
}
