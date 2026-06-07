// Package server exposes cerber's HTTP API: a native Anthropic Messages
// passthrough (/v1/messages) and an OpenAI-compatible endpoint
// (/v1/chat/completions) that translates to Anthropic. It authenticates clients,
// rotates upstream credentials, and relays/translates responses (incl. streaming).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"cerber/internal/access"
	"cerber/internal/credential"
	"cerber/internal/translator"
)

// Upstream issues Anthropic Messages requests. *anthropic.Client satisfies it;
// it is an interface so the server can be unit-tested against a mock.
type Upstream interface {
	Send(ctx context.Context, body []byte, stream bool, cred *credential.Credential) (*http.Response, error)
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
	cooldown    time.Duration
	refreshSkew time.Duration
	now         func() time.Time
}

// defaultCooldown sidelines a credential after an auth/rate-limit failure.
const defaultCooldown = 60 * time.Second

// defaultRefreshSkew refreshes an OAuth token this long before it expires.
const defaultRefreshSkew = 60 * time.Second

// New wires a Server. refresher may be nil to disable OAuth refresh (e.g.
// api-key-only deployments). The translator is created with the default clock.
func New(checker *access.Checker, creds *credential.Store, up Upstream, refresher Refresher) *Server {
	return &Server{
		access:      checker,
		creds:       creds,
		upstream:    up,
		refresher:   refresher,
		tr:          translator.New(),
		cooldown:    defaultCooldown,
		refreshSkew: defaultRefreshSkew,
		now:         time.Now,
	}
}

// Handler returns the HTTP handler for the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("POST /v1/messages", s.handleNative)
	mux.HandleFunc("POST /v1/chat/completions", s.handleOpenAI)
	return mux
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
	resp, err := s.dispatch(r.Context(), body, wantsStream(body))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()
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
	anthropicBody, stream, err := s.tr.OpenAIToAnthropic(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := s.dispatch(r.Context(), anthropicBody, stream)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()

	// Upstream errors are relayed as-is (already JSON).
	if resp.StatusCode != http.StatusOK {
		relay(w, resp)
		return
	}

	if stream {
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
		writeError(w, http.StatusBadGateway, "read upstream response")
		return
	}
	out, err := s.tr.AnthropicToOpenAI(upstreamBody)
	if err != nil {
		writeError(w, http.StatusBadGateway, "translate upstream response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// dispatch sends body upstream, rotating credentials and sidelining any that
// fail with auth/rate-limit errors. The returned response's Body must be closed.
func (s *Server) dispatch(ctx context.Context, body []byte, stream bool) (*http.Response, error) {
	var lastErr error
	for i, n := 0, s.creds.Len(); i < n; i++ {
		cred, err := s.creds.Next()
		if err != nil {
			return nil, err // ErrNoneAvailable
		}
		if s.refresher != nil && cred.NeedsRefresh(s.now(), s.refreshSkew) {
			tok, rerr := s.refresher.Refresh(ctx, cred.RefreshToken())
			if rerr != nil {
				s.creds.Cooldown(cred, s.cooldown)
				lastErr = fmt.Errorf("refresh %s: %w", cred, rerr)
				continue
			}
			s.creds.UpdateOAuth(cred, tok)
		}
		resp, err := s.upstream.Send(ctx, body, stream, cred)
		if err != nil {
			lastErr = err
			s.creds.Cooldown(cred, s.cooldown)
			continue
		}
		if isCredFailure(resp.StatusCode) {
			_ = resp.Body.Close()
			s.creds.Cooldown(cred, s.cooldown)
			lastErr = fmt.Errorf("upstream auth/rate-limit status %d", resp.StatusCode)
			continue
		}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no credentials available")
	}
	return nil, lastErr
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
