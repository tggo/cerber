// Package server exposes cerber's HTTP API: a native Anthropic Messages
// passthrough (/v1/messages) and an OpenAI-compatible endpoint
// (/v1/chat/completions) that translates to Anthropic. It authenticates clients,
// rotates upstream credentials, and relays/translates responses (incl. streaming).
package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"cerber/internal/access"
	"cerber/internal/credential"
	"cerber/internal/metrics"
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
	access      *access.Checker
	creds       *credential.Store
	upstream    Upstream
	refresher   Refresher
	tr          *translator.Translator
	log         *zap.Logger
	usage       *usage.Tracker
	cooldown    time.Duration
	refreshSkew time.Duration
	now         func() time.Time
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
		cooldown:    defaultCooldown,
		refreshSkew: defaultRefreshSkew,
		now:         time.Now,
	}
}

// Usage returns the usage tracker (for metrics and the dashboard).
func (s *Server) Usage() *usage.Tracker { return s.usage }

// Handler returns the HTTP handler for the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
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
	resp, cred, err := s.dispatch(r.Context(), body, stream, r.Header)
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
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return
	}
	s.usage.Record(usage.Event{Credential: cred, Model: model})
	relay(w, resp)
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
	anthropicBody, stream, err := s.tr.OpenAIToAnthropic(body)
	if err != nil {
		s.usage.Record(usage.Event{Model: model, IsError: true})
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, cred, err := s.dispatch(r.Context(), anthropicBody, stream, r.Header)
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
func (s *Server) dispatch(ctx context.Context, body []byte, stream bool, clientHeader http.Header) (*http.Response, string, error) {
	var lastErr error
	var lastCred string
	for i, n := 0, s.creds.Len(); i < n; i++ {
		cred, err := s.creds.Next()
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
	if s.access.Allow(access.FromRequest(r)) {
		return true
	}
	writeError(w, http.StatusUnauthorized, "invalid or missing client API key")
	return false
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

// relay copies an upstream response (status, content-type, body) to the client,
// flushing as data arrives so streaming works.
func relay(w http.ResponseWriter, resp *http.Response) {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	flush := flusher(w)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
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

// relayError buffers a small upstream error response, logs it (status + body
// snippet, for diagnosing client/provider issues), and relays it to the client.
func (s *Server) relayError(w http.ResponseWriter, resp *http.Response) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	s.log.Warn("upstream error response",
		zap.Int("status", resp.StatusCode),
		zap.String("body", string(body)),
	)
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
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
