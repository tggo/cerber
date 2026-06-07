// Package server exposes cerber's HTTP API: a native Anthropic Messages
// passthrough (/v1/messages) and an OpenAI-compatible endpoint
// (/v1/chat/completions) that translates to Anthropic. It authenticates clients,
// rotates upstream credentials, and relays/translates responses (incl. streaming).
package server

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"cerber/internal/access"
	"cerber/internal/config"
	"cerber/internal/credential"
	"cerber/internal/metrics"
	"cerber/internal/provider"
	"cerber/internal/translator"
	"cerber/internal/usage"

	"go.uber.org/zap"
)

//go:embed web/dashboard.html
var dashboardHTML []byte

// Upstream issues Anthropic Messages requests. *anthropic.Client satisfies it;
// it is an interface so the server can be unit-tested against a mock.
type Upstream interface {
	Send(ctx context.Context, body []byte, stream bool, cred *credential.Credential, clientHeader http.Header) (*http.Response, error)
}

// Refresher renews an OAuth credential's access token. *anthropic.Refresher
// satisfies it; it is an interface so the server can be tested against a mock.
type Refresher interface {
	Refresh(ctx context.Context, refreshToken string) (credential.OAuthTokens, error)
}

// Server holds the wired dependencies for the HTTP API.
type Server struct {
	access         *access.Checker
	creds          *credential.Store
	upstream       Upstream
	refresher      Refresher
	tr             *translator.Translator
	log            *zap.Logger
	usage          *usage.Tracker
	chatters       map[string]provider.Chatter
	routes         []config.Route
	persist        func(name string, tok credential.OAuthTokens)
	allowLocalhost bool
	cooldown       time.Duration
	refreshSkew    time.Duration
	now            func() time.Time
}

// defaultCooldown sidelines a credential after an auth/rate-limit failure.
const defaultCooldown = 60 * time.Second

// defaultRefreshSkew refreshes an OAuth token this long before it expires.
const defaultRefreshSkew = 60 * time.Second

// New wires a Server. refresher may be nil to disable OAuth refresh (e.g.
// api-key-only deployments). A nil logger is replaced with a no-op logger. The
// translator is created with the default clock.
func New(checker *access.Checker, creds *credential.Store, up Upstream, refresher Refresher, logger *zap.Logger) *Server {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Server{
		access:      checker,
		creds:       creds,
		upstream:    up,
		refresher:   refresher,
		tr:          translator.New(),
		log:         logger,
		usage:       usage.New(),
		chatters:    map[string]provider.Chatter{},
		cooldown:    defaultCooldown,
		refreshSkew: defaultRefreshSkew,
		now:         time.Now,
	}
}

// Usage returns the usage tracker (for metrics and the dashboard).
func (s *Server) Usage() *usage.Tracker { return s.usage }

// RegisterChatter adds an extra provider reachable from the OpenAI-compatible
// endpoint, keyed by its Name (e.g. "openai", "gemini").
func (s *Server) RegisterChatter(c provider.Chatter) { s.chatters[c.Name()] = c }

// SetRoutes installs model-prefix routing overrides.
func (s *Server) SetRoutes(routes []config.Route) { s.routes = routes }

// SetTokenPersister installs a callback invoked with refreshed OAuth tokens so
// they can be persisted to disk (keyed by credential name).
func (s *Server) SetTokenPersister(f func(name string, tok credential.OAuthTokens)) { s.persist = f }

// SetAllowLocalhost lets loopback clients call cerber without a valid key.
func (s *Server) SetAllowLocalhost(v bool) { s.allowLocalhost = v }

// route returns the provider name a model should go to on the OpenAI endpoint.
// Configured prefixes win; otherwise built-in defaults; default is "anthropic".
func (s *Server) route(model string) string {
	for _, r := range s.routes {
		if strings.HasPrefix(model, r.Prefix) {
			return r.Provider
		}
	}
	switch {
	case strings.HasPrefix(model, "gpt"), strings.HasPrefix(model, "o1"),
		strings.HasPrefix(model, "o3"), strings.HasPrefix(model, "o4"),
		strings.HasPrefix(model, "chatgpt"):
		return "openai"
	case strings.HasPrefix(model, "gemini"):
		return "gemini"
	case strings.HasPrefix(model, "grok"):
		return "grok"
	default:
		return "anthropic"
	}
}

// Handler returns the HTTP handler for the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Root responds 200 to clients (e.g. Claude Code) that probe the base URL
	// with GET/HEAD "/" for connectivity. {$} matches only the exact path, so
	// unknown paths still 404. A GET handler also serves HEAD.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "cerber\n")
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("POST /v1/messages", s.handleNative)
	mux.HandleFunc("POST /v1/chat/completions", s.handleOpenAI)
	mux.HandleFunc("GET /admin/stats", s.handleStats)
	// /metrics is unauthenticated (standard for Prometheus scraping); it exposes
	// counts and credential names, never secrets.
	mux.Handle("GET /metrics", metrics.Handler(s.usage))
	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(dashboardHTML)
	})
	return s.logRequests(mux)
}

// logRequests logs one line per request (method, path, status, latency).
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("request",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", rec.status),
			zap.Duration("latency", s.now().Sub(start)),
		)
	})
}

// recorder captures the response status while preserving streaming (Flush).
type recorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.written = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.written {
		r.written = true
	}
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// handleStats returns the usage snapshot as JSON (requires a client key).
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(s.usage.Snapshot())
}

// handleNative passes an Anthropic Messages request straight through, injecting a
// credential and relaying the (possibly streaming) response unchanged.
func (s *Server) handleNative(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	model := extractModel(body)
	stream := wantsStream(body)
	resp, cred, err := s.dispatch(r.Context(), body, stream, r.Header, credFilter(r))
	if err != nil {
		s.usage.Record(usage.Event{Credential: cred, Model: model, IsError: true})
		writeUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		s.usage.Record(usage.Event{Credential: cred, Model: model, IsError: true})
		s.relayError(w, resp)
		return
	}
	// Non-streaming bodies are small: buffer to extract token usage, then write.
	if !stream {
		body, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			s.usage.Record(usage.Event{Credential: cred, Model: model, IsError: true})
			writeError(w, http.StatusBadGateway, "read upstream response")
			return
		}
		in, out := anthropicUsage(body)
		s.usage.Record(usage.Event{Credential: cred, Model: model, InputTokens: in, OutputTokens: out})
		copyUpstreamHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return
	}
	// Streaming: relay the SSE through while parsing Anthropic usage events so
	// token counts are recorded (Claude Code streams).
	copyUpstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	in, out := streamRelayAnthropicUsage(w, resp.Body)
	s.usage.Record(usage.Event{Credential: cred, Model: model, InputTokens: in, OutputTokens: out})
}

// streamRelayAnthropicUsage copies an Anthropic SSE stream to the client
// (flushing per line) while extracting token usage: input from message_start,
// output from message_delta.
func streamRelayAnthropicUsage(w http.ResponseWriter, body io.Reader) (in, out int64) {
	flush := flusher(w)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if _, err := w.Write(append(line, '\n')); err != nil {
			return in, out
		}
		flush()
		if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			i, o := parseAnthropicStreamUsage(bytes.TrimSpace(data))
			if i > 0 {
				in = i
			}
			if o > 0 {
				out = o
			}
		}
	}
	return in, out
}

// parseAnthropicStreamUsage pulls token counts from one SSE data payload.
func parseAnthropicStreamUsage(data []byte) (in, out int64) {
	var ev struct {
		Message struct {
			Usage struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage struct {
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return 0, 0
	}
	out = ev.Usage.OutputTokens
	if out == 0 {
		out = ev.Message.Usage.OutputTokens
	}
	return ev.Message.Usage.InputTokens, out
}

// handleOpenAI accepts an OpenAI chat-completions request, translates it to
// Anthropic, and translates the response back.
func (s *Server) handleOpenAI(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	model := extractModel(body)
	stream := wantsStream(body)

	// Route non-Anthropic models to their provider (OpenAI-format passthrough/translation).
	if target := s.route(model); target != "anthropic" {
		chatter, ok := s.chatters[target]
		if !ok {
			s.usage.Record(usage.Event{Model: model, IsError: true})
			writeError(w, http.StatusNotImplemented, "provider "+target+" is not configured")
			return
		}
		s.serveChatter(w, r, chatter, body, model, stream)
		return
	}

	anthropicBody, streamA, err := s.tr.OpenAIToAnthropic(body)
	stream = streamA
	if err != nil {
		s.usage.Record(usage.Event{Model: model, IsError: true})
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, cred, err := s.dispatch(r.Context(), anthropicBody, stream, r.Header, credFilter(r))
	if err != nil {
		s.usage.Record(usage.Event{Credential: cred, Model: model, IsError: true})
		writeUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()

	// Upstream errors are relayed as-is (already JSON).
	if resp.StatusCode != http.StatusOK {
		s.usage.Record(usage.Event{Credential: cred, Model: model, IsError: true})
		s.relayError(w, resp)
		return
	}

	if stream {
		s.usage.Record(usage.Event{Credential: cred, Model: model})
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flush := flusher(w)
		if err := s.tr.StreamAnthropicToOpenAI(w, resp.Body, flush); err != nil {
			return // client likely went away; nothing more we can do
		}
		return
	}

	upstreamBody, err := io.ReadAll(resp.Body)
	if err != nil {
		s.usage.Record(usage.Event{Credential: cred, Model: model, IsError: true})
		writeError(w, http.StatusBadGateway, "read upstream response")
		return
	}
	in, out := anthropicUsage(upstreamBody)
	s.usage.Record(usage.Event{Credential: cred, Model: model, InputTokens: in, OutputTokens: out})
	translated, err := s.tr.AnthropicToOpenAI(upstreamBody)
	if err != nil {
		writeError(w, http.StatusBadGateway, "translate upstream response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(translated)
}

// dispatch sends body upstream, rotating credentials and sidelining any that
// fail with auth/rate-limit errors. It returns the response, the name of the
// credential used (or last tried), and an error. The response Body must be closed.
func (s *Server) dispatch(ctx context.Context, body []byte, stream bool, clientHeader http.Header, match func(*credential.Credential) bool) (*http.Response, string, error) {
	var lastErr error
	var lastCred string
	for i, n := 0, s.creds.Len(); i < n; i++ {
		cred, err := s.creds.NextOf(match)
		if err != nil {
			return nil, lastCred, err // ErrNoneAvailable
		}
		lastCred = cred.Name()
		if s.refresher != nil && cred.NeedsRefresh(s.now(), s.refreshSkew) {
			s.log.Debug("refreshing oauth credential", zap.String("credential", cred.Name()))
			tok, rerr := s.refresher.Refresh(ctx, cred.RefreshToken())
			if rerr != nil {
				s.log.Warn("oauth refresh failed", zap.String("credential", cred.Name()), zap.Error(rerr))
				s.creds.Cooldown(cred, s.cooldown)
				lastErr = fmt.Errorf("refresh %s: %w", cred, rerr)
				continue
			}
			s.creds.UpdateOAuth(cred, tok)
			if s.persist != nil {
				s.persist(cred.Name(), tok)
			}
		}
		s.log.Debug("dispatch", zap.String("credential", cred.Name()), zap.Bool("stream", stream), zap.Int("attempt", i+1))
		resp, err := s.upstream.Send(ctx, body, stream, cred, clientHeader)
		if err != nil {
			s.log.Warn("upstream send failed", zap.String("credential", cred.Name()), zap.Error(err))
			lastErr = err
			s.creds.Cooldown(cred, s.cooldown)
			continue
		}
		if isCredFailure(resp.StatusCode) {
			s.log.Warn("upstream credential failure, rotating",
				zap.String("credential", cred.Name()), zap.Int("status", resp.StatusCode))
			_ = resp.Body.Close()
			s.creds.Cooldown(cred, s.cooldown)
			lastErr = fmt.Errorf("upstream auth/rate-limit status %d", resp.StatusCode)
			continue
		}
		return resp, cred.Name(), nil
	}
	if lastErr == nil {
		lastErr = errors.New("no credentials available")
	}
	return nil, lastCred, lastErr
}

// credFilter returns a credential matcher from the X-Cerber-Cred header:
// "oauth" -> OAuth credentials only, "key"/"api_key" -> API-key credentials only,
// anything else (or absent) -> any credential.
func credFilter(r *http.Request) func(*credential.Credential) bool {
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("X-Cerber-Cred"))) {
	case "oauth":
		return func(c *credential.Credential) bool { return c.Kind() == credential.KindOAuth }
	case "key", "api_key", "apikey":
		return func(c *credential.Credential) bool { return c.Kind() == credential.KindAPIKey }
	default:
		return nil
	}
}

// extractModel reads the top-level "model" field from a request body (present in
// both OpenAI and Anthropic request shapes).
func extractModel(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Model
}

// anthropicUsage extracts input/output token counts from an Anthropic Messages
// response body. Returns zeros if absent.
func anthropicUsage(body []byte) (in, out int64) {
	var probe struct {
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Usage.InputTokens, probe.Usage.OutputTokens
}

// isCredFailure reports statuses that indicate the credential, not the request,
// is the problem — so we should rotate to another account.
func isCredFailure(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if s.allowLocalhost && isLoopback(r.RemoteAddr) {
		return true
	}
	if s.access.Allow(access.FromRequest(r)) {
		return true
	}
	// Diagnostic without leaking secrets: which auth the client sent and how long
	// the presented key is (helps tell an OAuth bearer from a short gateway key).
	auth := r.Header.Get("Authorization")
	scheme := ""
	if i := strings.IndexByte(auth, ' '); i > 0 {
		scheme = auth[:i]
	}
	s.log.Warn("unauthorized client",
		zap.Bool("authorization", auth != ""),
		zap.String("auth_scheme", scheme),
		zap.Bool("x_api_key", r.Header.Get("x-api-key") != ""),
		zap.Int("presented_len", len(access.FromRequest(r))),
	)
	writeError(w, http.StatusUnauthorized, "invalid or missing client API key")
	return false
}

// isLoopback reports whether a "host:port" remote address is a loopback IP.
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read request body")
		return nil, false
	}
	return body, true
}

// wantsStream reports whether a raw Anthropic/JSON body requests streaming.
func wantsStream(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Stream
}

// hopByHop headers are connection-specific and must not be forwarded.
var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailer": true,
	"transfer-encoding": true, "upgrade": true, "content-length": true,
}

// copyUpstreamHeaders forwards an upstream response's headers to the client,
// dropping hop-by-hop ones, so faithful clients (e.g. Claude Code) see the
// provider's rate-limit and metadata headers.
func copyUpstreamHeaders(dst http.Header, src http.Header) {
	for k, vs := range src {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}

// relay copies an upstream response (status, headers, body) to the client,
// flushing as data arrives so streaming works.
func relay(w http.ResponseWriter, resp *http.Response) {
	copyUpstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	streamCopy(w, resp.Body)
}

// streamCopy copies body to w, flushing after each chunk so streaming works.
func streamCopy(w http.ResponseWriter, body io.Reader) {
	flush := flusher(w)
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flush()
		}
		if err != nil {
			return
		}
	}
}

// serveChatter delegates an OpenAI-format request to a non-Anthropic provider and
// relays/records the OpenAI-format response.
func (s *Server) serveChatter(w http.ResponseWriter, r *http.Request, c provider.Chatter, body []byte, model string, stream bool) {
	resp, err := c.Chat(r.Context(), body, stream, r.Header)
	if err != nil {
		s.usage.Record(usage.Event{Model: model, IsError: true})
		var bad *provider.BadRequestError
		if errors.As(err, &bad) {
			writeError(w, http.StatusBadRequest, bad.Error())
			return
		}
		writeUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if resp.Status != http.StatusOK {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		s.usage.Record(usage.Event{Credential: resp.Credential, Model: model, IsError: true})
		s.log.Warn("provider error response", zap.String("provider", c.Name()),
			zap.Int("status", resp.Status), zap.String("body", string(buf)))
		if ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.Status)
		_, _ = w.Write(buf)
		return
	}

	if stream {
		s.usage.Record(usage.Event{Credential: resp.Credential, Model: model})
		if ct == "" {
			ct = "text/event-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(http.StatusOK)
		streamCopy(w, resp.Body)
		return
	}

	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		s.usage.Record(usage.Event{Credential: resp.Credential, Model: model, IsError: true})
		writeError(w, http.StatusBadGateway, "read provider response")
		return
	}
	in, out := openaiUsage(buf)
	s.usage.Record(usage.Event{Credential: resp.Credential, Model: model, InputTokens: in, OutputTokens: out})
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
}

// openaiUsage extracts token counts from an OpenAI chat-completion response.
func openaiUsage(body []byte) (in, out int64) {
	var probe struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Usage.PromptTokens, probe.Usage.CompletionTokens
}

// relayError buffers a small upstream error response, logs it (status + body
// snippet, for diagnosing client/provider issues), and relays it to the client.
func (s *Server) relayError(w http.ResponseWriter, resp *http.Response) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	s.log.Warn("upstream error response",
		zap.Int("status", resp.StatusCode),
		zap.String("body", string(body)),
	)
	copyUpstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// flusher returns a flush function, or a no-op if w cannot flush.
func flusher(w http.ResponseWriter) func() {
	if f, ok := w.(http.Flusher); ok {
		return f.Flush
	}
	return func() {}
}

func writeUpstreamError(w http.ResponseWriter, err error) {
	if errors.Is(err, credential.ErrNoneAvailable) {
		writeError(w, http.StatusServiceUnavailable, "all upstream credentials are unavailable")
		return
	}
	writeError(w, http.StatusBadGateway, "upstream error: "+err.Error())
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": http.StatusText(status)},
	})
}
