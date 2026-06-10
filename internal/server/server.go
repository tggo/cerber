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
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tggo/cerber/internal/access"
	"github.com/tggo/cerber/internal/catalog"
	"github.com/tggo/cerber/internal/config"
	"github.com/tggo/cerber/internal/credential"
	"github.com/tggo/cerber/internal/metrics"
	"github.com/tggo/cerber/internal/provider"
	"github.com/tggo/cerber/internal/quota"
	"github.com/tggo/cerber/internal/translator"
	"github.com/tggo/cerber/internal/usage"

	"go.uber.org/zap"
)

//go:embed web/dashboard.html
var dashboardHTML []byte

//go:embed web/favicon.svg
var faviconSVG []byte

//go:embed web/chat.html
var chatHTML []byte

// Upstream issues Anthropic Messages requests. *anthropic.Client satisfies it;
// it is an interface so the server can be unit-tested against a mock.
type Upstream interface {
	Send(ctx context.Context, body []byte, stream bool, cred *credential.Credential, clientHeader http.Header) (*http.Response, error)
	CountTokens(ctx context.Context, body []byte, cred *credential.Credential, clientHeader http.Header) (*http.Response, error)
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
	quota          *quota.Tracker
	chatters       map[string]provider.Chatter
	provStores     map[string]*credential.Store // provider name -> its credential store (for the accounts view)
	provMu         sync.Mutex
	provModels     map[string][]string // provider name -> discovered model IDs (from ProbeAll)
	routes         []config.Route
	fallbacks      []config.Fallback // cross-provider/model fallback chains (OpenAI endpoint)
	catalog        *catalog.Catalog  // model alias -> canonical resolution
	persist        func(name string, tok credential.OAuthTokens)
	allowLocalhost bool
	mgmt           *access.Checker // if set, /admin/* requires this key
	keys           *access.Store   // dynamic, dashboard-managed client keys (consulted with static config keys)
	upstreamProxy  http.Handler
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
		quota:       quota.New(),
		chatters:    map[string]provider.Chatter{},
		provStores:  map[string]*credential.Store{},
		provModels:  map[string][]string{},
		catalog:     catalog.New(nil),
		cooldown:    defaultCooldown,
		refreshSkew: defaultRefreshSkew,
		now:         time.Now,
	}
}

// Usage returns the usage tracker (for metrics and the dashboard).
func (s *Server) Usage() *usage.Tracker { return s.usage }

// SetUsageTracker replaces the usage tracker (e.g. one loaded from disk with
// pricing). Call before Handler().
func (s *Server) SetUsageTracker(t *usage.Tracker) { s.usage = t }

// RegisterChatter adds an extra provider reachable from the OpenAI-compatible
// endpoint, keyed by its Name (e.g. "openai", "gemini").
func (s *Server) RegisterChatter(c provider.Chatter) { s.chatters[c.Name()] = c }

// RegisterProviderStore records a provider's credential store under a name so the
// accounts view and enable/disable cover every provider, not just Anthropic.
func (s *Server) RegisterProviderStore(name string, store *credential.Store) {
	if store != nil {
		s.provStores[name] = store
	}
}

// SetRoutes installs model-prefix routing overrides.
func (s *Server) SetRoutes(routes []config.Route) { s.routes = routes }

// SetModelAliases installs the model alias -> canonical map. Aliases are resolved
// before routing and before the request body reaches upstream. Call before
// Handler().
func (s *Server) SetModelAliases(aliases map[string]string) {
	s.catalog = catalog.New(aliases)
}

// SetFallbacks installs cross-provider/model fallback chains for the
// OpenAI-compatible endpoint. Call before Handler().
func (s *Server) SetFallbacks(fb []config.Fallback) { s.fallbacks = fb }

// SetTokenPersister installs a callback invoked with refreshed OAuth tokens so
// they can be persisted to disk (keyed by credential name).
func (s *Server) SetTokenPersister(f func(name string, tok credential.OAuthTokens)) { s.persist = f }

// SetAllowLocalhost lets loopback clients call cerber without a valid key.
func (s *Server) SetAllowLocalhost(v bool) { s.allowLocalhost = v }

// SetClientKeyStore installs the dynamic, dashboard-managed client-key store. Its
// keys are accepted in addition to the static config keys, and it backs the
// /admin/keys management endpoints. Call before Handler().
func (s *Server) SetClientKeyStore(st *access.Store) { s.keys = st }

// SetManagementKey requires a dedicated key for /admin/* (sent as Bearer,
// x-api-key, or X-Cerber-Management). Empty keeps /admin on the client-key check.
func (s *Server) SetManagementKey(key string) {
	if key != "" {
		s.mgmt = access.New([]string{key})
	}
}

// adminAuthorized gates /admin/* — by the management key if configured, else the
// normal client-key check.
func (s *Server) adminAuthorized(w http.ResponseWriter, r *http.Request) bool {
	if s.mgmt == nil {
		return s.authorized(w, r)
	}
	if s.mgmt.Allow(access.FromRequest(r)) || s.mgmt.Allow(r.Header.Get("X-Cerber-Management")) {
		return true
	}
	writeError(w, http.StatusUnauthorized, "invalid or missing management key")
	return false
}

// SetUpstreamProxy makes cerber a transparent reverse proxy for any path it does
// not specifically handle (e.g. /api/claude_code/*). Used by TLS impersonation so
// Claude Code's console/bootstrap calls reach the real upstream. transport should
// resolve the upstream (e.g. via DoH). If authToken is non-nil and returns a
// token, it replaces the client's Authorization (so console calls run on cerber's
// pooled credential, not whatever the client sent) — keeping cerber the sole
// token owner.
func (s *Server) SetUpstreamProxy(target *url.URL, transport http.RoundTripper, authToken func() string) {
	s.upstreamProxy = &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = target.Scheme
			r.URL.Host = target.Host
			r.Host = target.Host
			if authToken != nil {
				if tok := authToken(); tok != "" {
					r.Header.Del("x-api-key")
					r.Header.Set("Authorization", "Bearer "+tok)
				}
			}
		},
		Transport: transport,
	}
}

// route returns the provider name a model should go to on the OpenAI endpoint.
// Order: configured prefixes, then discovered models, then built-in prefixes
// (gpt*/o*→openai, gemini*→gemini, grok*→grok, claude*→anthropic). An unknown
// model returns "" so the caller can reject it instead of silently using
// Anthropic.
func (s *Server) route(model string) string {
	for _, r := range s.routes {
		if strings.HasPrefix(model, r.Prefix) {
			return r.Provider
		}
	}
	// Discovery: route to whichever provider actually advertises this exact model
	// (from the latest probe), e.g. a local ollama serving "supergemma4-…" or
	// "hf.co/…" names that no prefix would match.
	if name := s.providerForModel(model); name != "" {
		return name
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
	case strings.HasPrefix(model, "claude"):
		return "anthropic"
	default:
		return "" // unknown model: no provider matched (see handleOpenAI)
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
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)
	mux.HandleFunc("GET /favicon.svg", s.handleFavicon)
	mux.HandleFunc("GET /llm.md", s.handleLLMDoc)
	mux.HandleFunc("GET /llms.txt", s.handleLLMDoc) // common convention alias
	mux.HandleFunc("POST /v1/messages", s.handleNative)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.handleCountTokens)
	mux.HandleFunc("POST /v1/chat/completions", s.handleOpenAI)
	mux.HandleFunc("POST /v1/images/generations", s.handleImages)
	mux.HandleFunc("POST /v1/embeddings", s.handleForward("/v1/embeddings"))
	mux.HandleFunc("POST /v1/completions", s.handleForward("/v1/completions"))
	mux.HandleFunc("POST /v1/responses", s.handleForward("/v1/responses"))
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /admin/stats", s.handleStats)
	mux.HandleFunc("GET /admin/usage.csv", s.handleUsageCSV)
	mux.HandleFunc("GET /admin/accounts", s.handleAccounts)
	mux.HandleFunc("GET /admin/providers", s.handleProviders)
	mux.HandleFunc("POST /admin/accounts/{name}/enable", s.handleSetAccount(true))
	mux.HandleFunc("POST /admin/accounts/{name}/disable", s.handleSetAccount(false))
	mux.HandleFunc("GET /admin/keys", s.handleKeysList)
	mux.HandleFunc("POST /admin/keys", s.handleKeyCreate)
	mux.HandleFunc("POST /admin/keys/{name}/enable", s.handleSetKey(true))
	mux.HandleFunc("POST /admin/keys/{name}/disable", s.handleSetKey(false))
	mux.HandleFunc("DELETE /admin/keys/{name}", s.handleKeyDelete)
	mux.HandleFunc("POST /admin/keys/{name}/delete", s.handleKeyDelete)
	mux.HandleFunc("POST /admin/keys/{name}/limits", s.handleKeyLimits)
	// /metrics is unauthenticated (standard for Prometheus scraping); it exposes
	// counts and credential names, never secrets.
	mux.Handle("GET /metrics", metrics.Handler(s.usage))
	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(dashboardHTML)
	})
	mux.HandleFunc("GET /chat", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(chatHTML)
	})
	// Catch-all: transparently proxy anything else to the real upstream (TLS
	// impersonation), or 404 when no upstream proxy is configured.
	mux.HandleFunc("/", s.handleCatchAll)
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

// handleCatchAll transparently proxies unhandled paths to the upstream (used by
// TLS impersonation for Claude Code's /api/* console calls), or 404s.
func (s *Server) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	if s.upstreamProxy != nil {
		s.upstreamProxy.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}

// handleStats returns the usage snapshot as JSON (requires a client key).
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthorized(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(s.usage.Snapshot())
}

// handleUsageCSV exports usage for spreadsheets/analysis: the hourly time-series
// plus per-credential and per-model breakdowns, as one CSV (section-tagged rows).
func (s *Server) handleUsageCSV(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthorized(w, r) {
		return
	}
	rep := s.usage.Snapshot()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="cerber-usage.csv"`)
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"section", "key", "requests", "errors", "input_tokens", "output_tokens", "cost"})
	row := func(section, key string, st usage.Stat, cost float64) {
		_ = cw.Write([]string{
			section, key,
			strconv.FormatInt(st.Requests, 10), strconv.FormatInt(st.Errors, 10),
			strconv.FormatInt(st.InputTokens, 10), strconv.FormatInt(st.OutputTokens, 10),
			strconv.FormatFloat(cost, 'f', -1, 64),
		})
	}
	row("total", "all", rep.Totals, rep.TotalCost)
	for _, e := range rep.ByCredential {
		row("credential", e.Name, e.Stat, e.Cost)
	}
	for _, e := range rep.ByModel {
		row("model", e.Name, e.Stat, e.Cost)
	}
	for _, b := range rep.Series {
		row("hour", time.Unix(b.Unix, 0).UTC().Format(time.RFC3339), b.Stat, 0)
	}
	cw.Flush()
}

// accountView is one credential's state plus its usage and quota, for orchestration.
type accountView struct {
	credential.Info
	Provider     string          `json:"provider"`
	Requests     int64           `json:"requests"`
	Errors       int64           `json:"errors"`
	InputTokens  int64           `json:"input_tokens"`
	OutputTokens int64           `json:"output_tokens"`
	Quota        *quota.Snapshot `json:"quota,omitempty"`
}

// accountStores returns every provider credential store keyed by provider name.
// The Anthropic store (s.creds) is always included under "anthropic"; additional
// providers are whatever was registered via RegisterProviderStore.
func (s *Server) accountStores() map[string]*credential.Store {
	stores := map[string]*credential.Store{"anthropic": s.creds}
	for name, st := range s.provStores {
		stores[name] = st
	}
	return stores
}

// setProviderModels stores the discovered model IDs for a provider.
func (s *Server) setProviderModels(name string, models []string) {
	s.provMu.Lock()
	defer s.provMu.Unlock()
	s.provModels[name] = models
}

// providerModels returns a copy of a provider's discovered model IDs.
func (s *Server) providerModels(name string) []string {
	s.provMu.Lock()
	defer s.provMu.Unlock()
	m := s.provModels[name]
	out := make([]string, len(m))
	copy(out, m)
	return out
}

// providerForModel returns the provider whose discovered models include the exact
// model, or "" if none.
func (s *Server) providerForModel(model string) string {
	s.provMu.Lock()
	defer s.provMu.Unlock()
	for name, models := range s.provModels {
		for _, m := range models {
			if m == model {
				return name
			}
		}
	}
	return ""
}

// providersForModel returns every provider whose discovered models include the
// exact model, sorted for determinism. Used to build fallback chains (a model
// served by more than one provider can fail over between them).
func (s *Server) providersForModel(model string) []string {
	s.provMu.Lock()
	defer s.provMu.Unlock()
	var out []string
	for name, models := range s.provModels {
		for _, m := range models {
			if m == model {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// proberFor returns the Prober for a provider name: the Anthropic upstream for
// "anthropic", otherwise the registered chatter. Returns nil if it can't probe.
func (s *Server) proberFor(name string) provider.Prober {
	if name == "anthropic" {
		if pr, ok := s.upstream.(provider.Prober); ok {
			return pr
		}
		return nil
	}
	if pr, ok := s.chatters[name].(provider.Prober); ok {
		return pr
	}
	return nil
}

// ProbeAll validates every credential of every provider against its upstream and
// refreshes the discovered model set per provider. Per-credential validity is
// recorded on the store (visible in /admin/accounts); the model union per
// provider drives discovery routing and /admin/providers. Credentials whose
// probe is unsupported or errors keep their last health; invalid ones are flagged.
func (s *Server) ProbeAll(ctx context.Context) {
	for name, store := range s.accountStores() {
		prober := s.proberFor(name)
		if prober == nil {
			continue
		}
		modelSet := map[string]struct{}{}
		for _, c := range store.All() {
			models, err := prober.ProbeCredential(ctx, c)
			switch {
			case errors.Is(err, provider.ErrInvalidCredential):
				store.SetHealth(c, false, "credential rejected by upstream")
			case err != nil:
				store.SetHealth(c, false, err.Error())
			default:
				store.SetHealth(c, true, "")
				for _, m := range models {
					modelSet[m] = struct{}{}
				}
			}
		}
		models := make([]string, 0, len(modelSet))
		for m := range modelSet {
			models = append(models, m)
		}
		sort.Strings(models)
		s.setProviderModels(name, models)
		s.log.Debug("provider probed", zap.String("provider", name),
			zap.Int("credentials", store.Len()), zap.Int("models", len(models)))
	}
}

// handleAccounts lists every provider's credentials with state and usage.
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthorized(w, r) {
		return
	}
	use := map[string]usage.Stat{}
	for _, e := range s.usage.Snapshot().ByCredential {
		use[e.Name] = e.Stat
	}
	// Stable order: by provider name, then credential name within a provider.
	stores := s.accountStores()
	names := make([]string, 0, len(stores))
	for name := range stores {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []accountView
	for _, prov := range names {
		for _, info := range stores[prov].List() {
			st := use[info.Name]
			av := accountView{
				Info: info, Provider: prov, Requests: st.Requests, Errors: st.Errors,
				InputTokens: st.InputTokens, OutputTokens: st.OutputTokens,
			}
			if q, ok := s.quota.Get(info.Name); ok {
				av.Quota = &q
			}
			out = append(out, av)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"accounts": out})
}

// providerView is a provider's credential health + discovered models for the UI.
type providerView struct {
	Name         string   `json:"name"`
	BaseURL      string   `json:"base_url,omitempty"`
	Credentials  int      `json:"credentials"`
	HealthyCreds int      `json:"healthy_credentials"`
	Models       []string `json:"models,omitempty"`
}

// baseURLOf returns a provider's upstream base URL if it exposes one.
func (s *Server) baseURLOf(name string) string {
	var p any = s.chatters[name]
	if name == "anthropic" {
		p = s.upstream
	}
	if b, ok := p.(provider.BaseURLer); ok {
		return b.BaseURL()
	}
	return ""
}

// handleProviders lists each configured provider: credential count, how many
// credentials are currently healthy, and the models discovered for it.
func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthorized(w, r) {
		return
	}
	stores := s.accountStores()
	names := make([]string, 0, len(stores))
	for name := range stores {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]providerView, 0, len(names))
	for _, name := range names {
		healthy := 0
		for _, info := range stores[name].List() {
			if info.HealthChecked && info.Healthy {
				healthy++
			}
		}
		out = append(out, providerView{
			Name:         name,
			BaseURL:      s.baseURLOf(name),
			Credentials:  stores[name].Len(),
			HealthyCreds: healthy,
			Models:       s.providerModels(name),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"providers": out})
}

// handleFavicon serves the embedded SVG favicon (public, cached) so the dashboard
// tab is identifiable. Browsers request /favicon.ico by default; we serve the SVG
// for both paths (modern browsers accept it).
func (s *Server) handleFavicon(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(faviconSVG)
}

// handleLLMDoc serves a live, self-describing usage guide (markdown) so an agent
// or browser given only the base URL can learn how to connect: endpoints, auth,
// model routing, and the actual models each provider serves. Public (no key) by
// design — it exposes no secrets, just how to use the API (which still needs a
// key from the public side). Being unauthenticated also lets a plain browser GET
// it (browsers can't attach an Authorization header to a navigation).
func (s *Server) handleLLMDoc(w http.ResponseWriter, r *http.Request) {
	scheme := "https"
	if xf := r.Header.Get("X-Forwarded-Proto"); xf != "" {
		scheme = xf
	} else if r.TLS == nil {
		scheme = "http"
	}
	base := scheme + "://" + r.Host

	var b strings.Builder
	fmt.Fprintf(&b, "# cerber — LLM proxy usage\n\n")
	fmt.Fprintf(&b, "cerber is a multi-provider proxy that speaks both the OpenAI and the\n")
	fmt.Fprintf(&b, "Anthropic API dialects and routes each request to the right upstream.\n\n")

	fmt.Fprintf(&b, "## Base URL\n\n    %s\n\n", base)

	fmt.Fprintf(&b, "## Authentication\n\n")
	fmt.Fprintf(&b, "- From the local network: no key required.\n")
	fmt.Fprintf(&b, "- From outside: send `Authorization: Bearer <YOUR_KEY>` (or `x-api-key: <YOUR_KEY>`).\n")
	fmt.Fprintf(&b, "  Ask the operator for a key; keys are managed in the dashboard.\n\n")

	fmt.Fprintf(&b, "## Endpoints\n\n")
	fmt.Fprintf(&b, "- `POST /v1/chat/completions` — OpenAI-compatible chat (all providers, incl. Claude). Point any OpenAI SDK at base_url `%s/v1`.\n", base)
	fmt.Fprintf(&b, "- `POST /v1/messages` — Anthropic-native messages. Point the Anthropic SDK at base_url `%s`.\n", base)
	fmt.Fprintf(&b, "- `POST /v1/messages/count_tokens` — Anthropic token counting.\n")
	fmt.Fprintf(&b, "- `POST /v1/images/generations` — image generation (OpenAI Images shape; e.g. `grok-imagine-image`).\n")
	fmt.Fprintf(&b, "- `GET /v1/models` — list available model ids (use these exact strings as `model`).\n")
	fmt.Fprintf(&b, "- `GET /llm.md` — this document.\n\n")

	fmt.Fprintf(&b, "## Recommended models\n\n")
	fmt.Fprintf(&b, "- Default: `claude-sonnet-4-6` (strong; works on `/v1/chat/completions` and `/v1/messages`).\n")
	fmt.Fprintf(&b, "- Cheap/fast: `claude-haiku-4-5-20251001`.\n")
	fmt.Fprintf(&b, "- Local/free: an ollama model from the list below.\n")
	fmt.Fprintf(&b, "Always set `model` to an exact id (see `GET /v1/models` or the list below). Streaming: add `\"stream\": true`.\n\n")

	fmt.Fprintf(&b, "## Picking a specific account / subscription\n\n")
	fmt.Fprintf(&b, "Send header `X-Cerber-Cred: <value>` to pin which credential serves the request: `oauth` (a subscription token), `key` (an API key), or an exact account name from `GET /admin/accounts`. Omit it to let cerber rotate.\n\n")

	fmt.Fprintf(&b, "## Model routing\n\n")
	fmt.Fprintf(&b, "Just set `model`; cerber routes by name:\n\n")
	fmt.Fprintf(&b, "- `gpt*`, `o1*`, `o3*`, `o4*`, `chatgpt*` → OpenAI\n")
	fmt.Fprintf(&b, "- `gemini*` → Gemini\n")
	fmt.Fprintf(&b, "- `grok*` → xAI (Grok)\n")
	fmt.Fprintf(&b, "- `claude*` → Anthropic (Claude)\n")
	fmt.Fprintf(&b, "- the local models listed below → ollama\n")
	fmt.Fprintf(&b, "- anything else → 400 (unknown model)\n\n")

	fmt.Fprintf(&b, "## Available models\n\n")
	nameSet := map[string]struct{}{}
	for name := range s.accountStores() {
		nameSet[name] = struct{}{}
	}
	for name := range s.chatters {
		nameSet[name] = struct{}{}
	}
	names := make([]string, 0, len(nameSet))
	for name := range nameSet {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if models := s.providerModels(name); len(models) > 0 {
			fmt.Fprintf(&b, "### %s (%d)\n\n", name, len(models))
			for _, m := range models {
				fmt.Fprintf(&b, "- `%s`\n", m)
			}
			b.WriteString("\n")
			continue
		}
		// No discovered list (e.g. Claude via OAuth): give the routing hint.
		fmt.Fprintf(&b, "### %s\n\nUse this provider's standard model names (routed by the prefixes above).\n\n", name)
	}

	fmt.Fprintf(&b, "## Examples\n\n")
	fmt.Fprintf(&b, "OpenAI dialect (curl):\n\n")
	fmt.Fprintf(&b, "```sh\ncurl %s/v1/chat/completions \\\n  -H 'Authorization: Bearer $KEY' -H 'Content-Type: application/json' \\\n  -d '{\"model\":\"claude-sonnet-4-6\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'\n```\n\n", base)
	fmt.Fprintf(&b, "Anthropic dialect (curl):\n\n")
	fmt.Fprintf(&b, "```sh\ncurl %s/v1/messages \\\n  -H 'Authorization: Bearer $KEY' -H 'anthropic-version: 2023-06-01' -H 'Content-Type: application/json' \\\n  -d '{\"model\":\"claude-sonnet-4-6\",\"max_tokens\":256,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'\n```\n\n", base)
	fmt.Fprintf(&b, "OpenAI Python SDK:\n\n")
	fmt.Fprintf(&b, "```python\nfrom openai import OpenAI\nclient = OpenAI(base_url=\"%s/v1\", api_key=\"$KEY\")\nclient.chat.completions.create(model=\"claude-sonnet-4-6\", messages=[{\"role\":\"user\",\"content\":\"hi\"}])\n```\n\n", base)
	fmt.Fprintf(&b, "Streaming is supported on both endpoints (`\"stream\": true`).\n")

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, b.String())
}

// handleSetAccount enables/disables a credential at runtime.
func (s *Server) handleSetAccount(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.adminAuthorized(w, r) {
			return
		}
		name := r.PathValue("name")
		found := false
		for _, st := range s.accountStores() {
			if st.SetEnabled(name, enabled) {
				found = true
				break
			}
		}
		if !found {
			writeError(w, http.StatusNotFound, "no credential named "+name)
			return
		}
		s.log.Info("account state changed", zap.String("credential", name), zap.Bool("enabled", enabled))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "enabled": enabled})
	}
}

// handleKeysList returns the managed client keys (redacted — no secrets).
func (s *Server) handleKeysList(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthorized(w, r) {
		return
	}
	if s.keys == nil {
		writeError(w, http.StatusServiceUnavailable, "client-key store not configured")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": s.keys.List()})
}

// handleKeyCreate mints a new client key. The full secret is returned exactly
// once in the response; thereafter only its last 4 chars are ever shown.
func (s *Server) handleKeyCreate(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthorized(w, r) {
		return
	}
	if s.keys == nil {
		writeError(w, http.StatusServiceUnavailable, "client-key store not configured")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	key, info, err := s.keys.Add(req.Name)
	if err != nil {
		switch {
		case errors.Is(err, access.ErrNameTaken):
			writeError(w, http.StatusConflict, err.Error())
		case strings.Contains(err.Error(), "required"):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			s.log.Warn("create client key", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "could not create key")
		}
		return
	}
	s.log.Info("client key created", zap.String("name", info.Name))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"name": info.Name, "key": key, "created_at": info.CreatedAt})
}

// handleSetKey enables/disables a managed client key.
func (s *Server) handleSetKey(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.adminAuthorized(w, r) {
			return
		}
		if s.keys == nil {
			writeError(w, http.StatusServiceUnavailable, "client-key store not configured")
			return
		}
		name := r.PathValue("name")
		if err := s.keys.SetEnabled(name, enabled); err != nil {
			writeError(w, http.StatusNotFound, "no client key named "+name)
			return
		}
		s.log.Info("client key state changed", zap.String("name", name), zap.Bool("enabled", enabled))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "enabled": enabled})
	}
}

// handleKeyDelete removes a managed client key.
func (s *Server) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthorized(w, r) {
		return
	}
	if s.keys == nil {
		writeError(w, http.StatusServiceUnavailable, "client-key store not configured")
		return
	}
	name := r.PathValue("name")
	if err := s.keys.Delete(name); err != nil {
		writeError(w, http.StatusNotFound, "no client key named "+name)
		return
	}
	s.log.Info("client key deleted", zap.String("name", name))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "deleted": true})
}

// handleKeyLimits sets the budget/rate-limit governance config for a managed
// client key. The body is an access.Limits JSON object; an empty object clears
// all limits (makes the key unlimited).
func (s *Server) handleKeyLimits(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthorized(w, r) {
		return
	}
	if s.keys == nil {
		writeError(w, http.StatusServiceUnavailable, "client-key store not configured")
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var lim access.Limits
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &lim); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	name := r.PathValue("name")
	if err := s.keys.SetLimits(name, lim); err != nil {
		switch {
		case errors.Is(err, access.ErrNotFound):
			writeError(w, http.StatusNotFound, "no client key named "+name)
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	s.log.Info("client key limits changed", zap.String("name", name))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "limits": lim})
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
	body, model := s.canonicalModel(body)
	stream := wantsStream(body)
	s.log.Debug("native request", append(debugRequestFields(body, stream),
		zap.String("anthropic_beta", r.Header.Get("anthropic-beta")))...)
	dumpRequest(body) // diagnostic: writes raw body if CERBER_DUMP_DIR is set
	resp, cred, err := s.dispatch(r.Context(), credFilter(r), func(c *credential.Credential) (*http.Response, error) {
		return s.upstream.Send(r.Context(), body, stream, c, r.Header)
	})
	if err != nil {
		s.record(r.Context(), usage.Event{Credential: cred, Model: model, IsError: true})
		writeUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		s.record(r.Context(), usage.Event{Credential: cred, Model: model, IsError: true})
		s.relayError(w, resp)
		return
	}
	// Non-streaming bodies are small: buffer to extract token usage, then write.
	if !stream {
		body, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			s.record(r.Context(), usage.Event{Credential: cred, Model: model, IsError: true})
			writeError(w, http.StatusBadGateway, "read upstream response")
			return
		}
		in, out := anthropicUsage(body)
		s.record(r.Context(), usage.Event{Credential: cred, Model: model, InputTokens: in, OutputTokens: out})
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
	s.log.Debug("native usage", zap.String("credential", cred), zap.Int64("input", in), zap.Int64("output", out), zap.Bool("stream", true))
	s.record(r.Context(), usage.Event{Credential: cred, Model: model, InputTokens: in, OutputTokens: out})
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

// parseAnthropicStreamUsage pulls token counts from one SSE data payload. Input
// (from message_start) includes cache tokens; output comes from message_delta.
func parseAnthropicStreamUsage(data []byte) (in, out int64) {
	var ev struct {
		Message struct {
			Usage anthropicUsageFields `json:"usage"`
		} `json:"message"`
		Usage anthropicUsageFields `json:"usage"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return 0, 0
	}
	out = ev.Usage.OutputTokens
	if out == 0 {
		out = ev.Message.Usage.OutputTokens
	}
	return ev.Message.Usage.totalInput(), out
}

// handleModels serves an OpenAI-shaped model list aggregated from every
// provider's discovered models (from ProbeAll). Authenticated like the API.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	s.provMu.Lock()
	names := make([]string, 0, len(s.provModels))
	for name := range s.provModels {
		names = append(names, name)
	}
	sort.Strings(names)
	var data []model
	for _, prov := range names {
		for _, id := range s.provModels[prov] {
			data = append(data, model{ID: id, Object: "model", OwnedBy: prov})
		}
	}
	s.provMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// handleImages proxies an OpenAI-format image-generation request to the provider
// that serves the model (e.g. grok-imagine-image → grok). Anthropic has no image
// generation. Token cost isn't applicable; only request/error counts are recorded.
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	body, model := s.canonicalModel(body)
	target := s.route(model)
	if target == "" || target == "anthropic" {
		s.record(r.Context(), usage.Event{Model: model, IsError: true})
		writeError(w, http.StatusBadRequest, fmt.Sprintf("no image-generation provider for model %q", model))
		return
	}
	gen, ok := s.chatters[target].(provider.ImageGenerator)
	if !ok {
		s.record(r.Context(), usage.Event{Model: model, IsError: true})
		writeError(w, http.StatusNotImplemented, "provider "+target+" does not support image generation")
		return
	}
	resp, err := gen.Images(r.Context(), body, r.Header)
	if err != nil {
		s.record(r.Context(), usage.Event{Model: model, IsError: true})
		writeUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()
	if resp.Status >= 400 {
		s.record(r.Context(), usage.Event{Credential: resp.Credential, Model: model, IsError: true})
		copyUpstreamHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.Status)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	s.record(r.Context(), usage.Event{Credential: resp.Credential, Model: model})
	copyUpstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.Status)
	_, _ = io.Copy(w, resp.Body)
}

// handleForward returns a handler that passes an OpenAI-compatible request
// through to a fixed sub-path (e.g. /v1/embeddings, /v1/completions,
// /v1/responses) on the provider that serves the model, with credential
// rotation. The response (OpenAI-format, possibly streaming) is relayed
// unchanged via relayChatter, recording token usage. Anthropic does not serve
// these endpoints; an unrouted/unsupported model → 400/501.
func (s *Server) handleForward(subpath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(w, r) {
			return
		}
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		body, model := s.canonicalModel(body)
		stream := wantsStream(body)
		target := s.route(model)
		if target == "" || target == "anthropic" {
			s.record(r.Context(), usage.Event{Model: model, IsError: true})
			writeError(w, http.StatusBadRequest, fmt.Sprintf("no provider for model %q on %s", model, subpath))
			return
		}
		fwd, ok := s.chatters[target].(provider.Forwarder)
		if !ok {
			s.record(r.Context(), usage.Event{Model: model, IsError: true})
			writeError(w, http.StatusNotImplemented, "provider "+target+" does not support "+subpath)
			return
		}
		resp, err := fwd.Forward(r.Context(), subpath, body, stream, r.Header)
		if err != nil {
			s.record(r.Context(), usage.Event{Model: model, IsError: true})
			writeUpstreamError(w, err)
			return
		}
		s.relayChatter(w, r, resp, target, model, stream)
	}
}

// handleCountTokens proxies Anthropic's /v1/messages/count_tokens through the
// pooled credentials (Anthropic-only, like /v1/messages).
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	body, model := s.canonicalModel(body)
	resp, cred, err := s.dispatch(r.Context(), credFilter(r), func(c *credential.Credential) (*http.Response, error) {
		return s.upstream.CountTokens(r.Context(), body, c, r.Header)
	})
	if err != nil {
		s.record(r.Context(), usage.Event{Credential: cred, Model: model, IsError: true})
		writeUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		s.relayError(w, resp)
		return
	}
	copyUpstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
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
	body, model := s.canonicalModel(body)
	stream := wantsStream(body)

	// Try the requested model, then any fallback targets, until one succeeds or is
	// a non-retryable (client) error. Fallback only kicks in before any bytes are
	// written, so a started stream is never re-attempted.
	targets := s.openAITargets(r, model)
	for i, tgt := range targets {
		if s.tryOpenAITarget(w, r, body, tgt, stream, i == len(targets)-1) {
			return
		}
		s.log.Info("openai fallback: target failed, trying next",
			zap.String("failed_model", tgt))
	}
}

// openAITargets returns the ordered models to try for an OpenAI request: the
// requested model first, then fallbacks. A request header X-Cerber-Fallback
// (comma-separated model names) overrides the configured fallback chains.
func (s *Server) openAITargets(r *http.Request, model string) []string {
	targets := []string{model}
	if h := strings.TrimSpace(r.Header.Get("X-Cerber-Fallback")); h != "" {
		for _, t := range strings.Split(h, ",") {
			if t = strings.TrimSpace(t); t != "" {
				targets = append(targets, t)
			}
		}
		return targets
	}
	for _, f := range s.fallbacks {
		if model == f.Model || strings.HasPrefix(model, f.Model) {
			for _, t := range f.To {
				if t = strings.TrimSpace(t); t != "" {
					targets = append(targets, t)
				}
			}
			break
		}
	}
	return targets
}

// tryOpenAITarget attempts to satisfy an OpenAI-format request for one target
// model. It returns true when the exchange is finished — success relayed, or a
// non-retryable/final error written — and false when a retryable failure
// occurred with no bytes written, so the caller should try the next target.
// A retryable failure is: no credential available / transport error, or an
// upstream 5xx. Client (4xx) errors and successes are terminal. last marks the
// final target, after which even retryable failures are surfaced to the client.
func (s *Server) tryOpenAITarget(w http.ResponseWriter, r *http.Request, body []byte, tgtModel string, stream, last bool) bool {
	target := s.route(tgtModel)
	if target == "" {
		if last {
			s.record(r.Context(), usage.Event{Model: tgtModel, IsError: true})
			writeError(w, http.StatusBadRequest, fmt.Sprintf("no provider configured for model %q", tgtModel))
			return true
		}
		return false
	}
	// Point the request body at this target's model.
	tbody := body
	if extractModel(body) != tgtModel {
		if rb, ok := setModelField(body, tgtModel); ok {
			tbody = rb
		}
	}

	if target != "anthropic" {
		chatter, ok := s.chatters[target]
		if !ok {
			if last {
				s.record(r.Context(), usage.Event{Model: tgtModel, IsError: true})
				writeError(w, http.StatusNotImplemented, "provider "+target+" is not configured")
				return true
			}
			return false
		}
		resp, err := chatter.Chat(r.Context(), tbody, stream, r.Header)
		if err != nil {
			var bad *provider.BadRequestError
			if errors.As(err, &bad) {
				// Deterministic client error: surface it, don't burn fallbacks.
				s.record(r.Context(), usage.Event{Model: tgtModel, IsError: true})
				writeError(w, http.StatusBadRequest, bad.Error())
				return true
			}
			if !last {
				return false
			}
			s.record(r.Context(), usage.Event{Model: tgtModel, IsError: true})
			writeUpstreamError(w, err)
			return true
		}
		if resp.Status >= 500 && !last {
			_ = resp.Body.Close()
			return false
		}
		s.relayChatter(w, r, resp, chatter.Name(), tgtModel, stream)
		return true
	}

	// Anthropic target: translate the request and dispatch through pooled creds.
	anthropicBody, streamA, err := s.tr.OpenAIToAnthropic(tbody)
	if err != nil {
		if !last {
			return false
		}
		s.record(r.Context(), usage.Event{Model: tgtModel, IsError: true})
		writeError(w, http.StatusBadRequest, err.Error())
		return true
	}
	resp, cred, derr := s.dispatch(r.Context(), credFilter(r), func(c *credential.Credential) (*http.Response, error) {
		return s.upstream.Send(r.Context(), anthropicBody, streamA, c, r.Header)
	})
	if derr != nil {
		if !last {
			return false
		}
		s.record(r.Context(), usage.Event{Credential: cred, Model: tgtModel, IsError: true})
		writeUpstreamError(w, derr)
		return true
	}
	if resp.StatusCode >= 500 && !last {
		_ = resp.Body.Close()
		return false
	}
	if resp.StatusCode != http.StatusOK {
		s.record(r.Context(), usage.Event{Credential: cred, Model: tgtModel, IsError: true})
		s.relayError(w, resp)
		_ = resp.Body.Close()
		return true
	}
	s.relayAnthropicAsOpenAI(w, r, resp, cred, tgtModel, streamA)
	return true
}

// relayAnthropicAsOpenAI relays a successful (2xx) Anthropic Messages response to
// an OpenAI-format client: streaming via the Anthropic->OpenAI SSE translator, or
// non-streaming by buffering, translating, and recording token usage. It closes
// resp.Body.
func (s *Server) relayAnthropicAsOpenAI(w http.ResponseWriter, r *http.Request, resp *http.Response, cred, model string, stream bool) {
	defer resp.Body.Close()
	if stream {
		s.record(r.Context(), usage.Event{Credential: cred, Model: model})
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
		s.record(r.Context(), usage.Event{Credential: cred, Model: model, IsError: true})
		writeError(w, http.StatusBadGateway, "read upstream response")
		return
	}
	in, out := anthropicUsage(upstreamBody)
	s.record(r.Context(), usage.Event{Credential: cred, Model: model, InputTokens: in, OutputTokens: out})
	translated, err := s.tr.AnthropicToOpenAI(upstreamBody)
	if err != nil {
		s.log.Warn("translate upstream response failed", zap.Error(err),
			zap.String("content_type", resp.Header.Get("Content-Type")),
			zap.String("content_encoding", resp.Header.Get("Content-Encoding")),
			zap.Int("body_len", len(upstreamBody)))
		writeError(w, http.StatusBadGateway, "translate upstream response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(translated)
}

// dispatch sends a request upstream, rotating credentials and sidelining any
// that fail with auth/rate-limit errors. send performs the actual call with the
// chosen credential (so the same rotation/refresh serves /v1/messages and
// /v1/messages/count_tokens). It returns the response, the name of the credential
// used (or last tried), and an error. The response Body must be closed.
// refreshCred refreshes an OAuth credential via the store's singleflight
// EnsureFresh (so concurrent requests spend the rotating refresh token once).
// force=true refreshes regardless of expiry (to recover from a 401). It persists
// + logs on success. Returns whether a refresh actually ran.
func (s *Server) refreshCred(ctx context.Context, cred *credential.Credential, force bool) (bool, error) {
	if s.refresher == nil || cred.Kind() != credential.KindOAuth {
		return false, nil
	}
	tok, did, err := s.creds.EnsureFresh(cred, force, s.now(), s.refreshSkew, func() (credential.OAuthTokens, error) {
		return s.refresher.Refresh(ctx, cred.RefreshToken())
	})
	if err != nil {
		s.log.Warn("oauth refresh failed", zap.String("credential", cred.Name()),
			zap.Bool("forced", force), zap.Error(err))
		return false, err
	}
	if did {
		s.log.Info("oauth token refreshed", zap.String("credential", cred.Name()),
			zap.Bool("forced", force), zap.Time("expires_at", tok.ExpiresAt))
		if s.persist != nil {
			s.persist(cred.Name(), tok)
		}
	}
	return did, nil
}

func (s *Server) dispatch(ctx context.Context, match func(*credential.Credential) bool, send func(*credential.Credential) (*http.Response, error)) (*http.Response, string, error) {
	var lastErr error
	var lastCred string
	for i, n := 0, s.creds.Len(); i < n; i++ {
		cred, err := s.creds.NextOf(match)
		if err != nil {
			return nil, lastCred, err // ErrNoneAvailable
		}
		lastCred = cred.Name()
		// Proactive refresh if the OAuth token is near expiry (singleflight).
		if cred.NeedsRefresh(s.now(), s.refreshSkew) {
			s.log.Info("oauth token near expiry, refreshing",
				zap.String("credential", cred.Name()), zap.Time("expires_at", cred.ExpiresAt()))
			if _, rerr := s.refreshCred(ctx, cred, false); rerr != nil {
				s.creds.Penalize(cred, s.cooldown)
				lastErr = fmt.Errorf("refresh %s: %w", cred, rerr)
				continue
			}
		}
		s.log.Debug("dispatch", zap.String("credential", cred.Name()), zap.Int("attempt", i+1))
		resp, err := send(cred)
		if err != nil {
			// Client cancellation/timeout is not the credential's fault — don't
			// sideline it (one canceled request would otherwise cooldown the only
			// account and cascade 503s).
			if ctx.Err() != nil {
				s.log.Debug("request canceled by client", zap.String("credential", cred.Name()))
				return nil, cred.Name(), ctx.Err()
			}
			s.log.Warn("upstream send failed", zap.String("credential", cred.Name()), zap.Error(err))
			lastErr = err
			s.creds.Penalize(cred, s.cooldown)
			continue
		}
		if isCredFailure(resp.StatusCode) {
			status := resp.StatusCode
			_ = resp.Body.Close()
			// An OAuth 401 usually means the access token was invalidated (e.g. a
			// rotation elsewhere), not that the account is dead — force one refresh
			// and retry the SAME credential before sidelining it. (Only 401: a 403 is
			// a permission/policy/rate decision that refreshing can't fix, and
			// rotating the token on every 403 just churns it.)
			if status == http.StatusUnauthorized && cred.Kind() == credential.KindOAuth && s.refresher != nil {
				if did, rerr := s.refreshCred(ctx, cred, true); rerr == nil && did {
					s.log.Info("retrying after forced oauth refresh", zap.String("credential", cred.Name()))
					resp2, err2 := send(cred)
					if err2 == nil && !isCredFailure(resp2.StatusCode) {
						s.quota.Record(cred.Name(), resp2.Header)
						s.creds.MarkSuccess(cred)
						return resp2, cred.Name(), nil
					}
					if resp2 != nil {
						_ = resp2.Body.Close()
					}
				}
			}
			s.log.Warn("upstream credential failure, sidelining",
				zap.String("credential", cred.Name()), zap.Int("status", status))
			s.creds.Penalize(cred, s.cooldown)
			lastErr = fmt.Errorf("upstream auth/rate-limit status %d", status)
			continue
		}
		s.quota.Record(cred.Name(), resp.Header) // passive Anthropic quota capture
		s.creds.MarkSuccess(cred)
		return resp, cred.Name(), nil
	}
	if lastErr == nil {
		lastErr = errors.New("no credentials available")
	}
	return nil, lastCred, lastErr
}

// credFilter returns a credential matcher from the X-Cerber-Cred header:
// "oauth" -> OAuth credentials only, "key"/"api_key" -> API-key credentials only,
// "" -> any; anything else selects a credential by exact name (for picking a
// specific account when several are pooled).
func credFilter(r *http.Request) func(*credential.Credential) bool {
	return credential.MatchHeader(r.Header.Get("X-Cerber-Cred"))
}

// debugRequestFields inspects an Anthropic request body for debug logging,
// without dumping message content (which may be large/sensitive).
func debugRequestFields(body []byte, stream bool) []zap.Field {
	var probe struct {
		Model       string            `json:"model"`
		MaxTokens   int               `json:"max_tokens"`
		System      json.RawMessage   `json:"system"`
		Messages    []json.RawMessage `json:"messages"`
		Tools       []json.RawMessage `json:"tools"`
		ContextMgmt json.RawMessage   `json:"context_management"`
		Thinking    json.RawMessage   `json:"thinking"`
	}
	_ = json.Unmarshal(body, &probe)
	return []zap.Field{
		zap.String("model", probe.Model),
		zap.Bool("stream", stream),
		zap.Int("body_bytes", len(body)),
		zap.Int("max_tokens", probe.MaxTokens),
		zap.Int("messages", len(probe.Messages)),
		zap.Int("tools", len(probe.Tools)),
		zap.Int("system_bytes", len(probe.System)),
		zap.Bool("context_management", len(probe.ContextMgmt) > 0),
		zap.Bool("thinking", len(probe.Thinking) > 0),
	}
}

// dumpRequest writes the raw request body to CERBER_DUMP_DIR/native-request.json
// when that env var is set (diagnostics only; no-op otherwise).
func dumpRequest(body []byte) {
	dir := os.Getenv("CERBER_DUMP_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "native-request.json"), body, 0o600)
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

// canonicalModel resolves the request's model through the alias catalog and, when
// an alias actually applies, rewrites the body's "model" field so the canonical
// name reaches both routing and the upstream provider. When no alias applies the
// body is returned untouched (zero risk for the common case). It returns the
// (possibly rewritten) body and the canonical model name.
func (s *Server) canonicalModel(body []byte) ([]byte, string) {
	model := extractModel(body)
	canon := s.catalog.Canonical(model)
	if canon == model {
		return body, model
	}
	if rewritten, ok := setModelField(body, canon); ok {
		return rewritten, canon
	}
	return body, canon
}

// setModelField returns body with its top-level "model" field set to model,
// preserving all other fields (nested values are kept verbatim). ok is false if
// the body is not a JSON object. Field order is not guaranteed.
func setModelField(body []byte, model string) ([]byte, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return body, false
	}
	quoted, err := json.Marshal(model)
	if err != nil {
		return body, false
	}
	obj["model"] = quoted
	out, err := json.Marshal(obj)
	if err != nil {
		return body, false
	}
	return out, true
}

// anthropicUsage extracts input/output token counts from an Anthropic Messages
// response body. Input includes cache creation/read tokens (Claude Code caches
// its large tool/system prompt, so plain input_tokens alone is misleadingly low).
func anthropicUsage(body []byte) (in, out int64) {
	var probe struct {
		Usage anthropicUsageFields `json:"usage"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Usage.totalInput(), probe.Usage.OutputTokens
}

// anthropicUsageFields are the token fields in an Anthropic usage object.
type anthropicUsageFields struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// totalInput sums fresh input with cache creation and cache read tokens.
func (u anthropicUsageFields) totalInput() int64 {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
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
	presented := access.FromRequest(r)
	// Static config keys are the operator's own and bypass per-key governance.
	if s.access.Allow(presented) {
		return true
	}
	// Managed (dashboard) keys carry optional budgets/rate-limits: identify the
	// key, enforce its limits, and stash its name on the context so the response
	// path can charge cost/tokens back to it (see record).
	if name, ok := s.keys.Identify(presented); ok {
		switch s.keys.Admit(name) {
		case access.DeniedBudget:
			s.log.Warn("client key budget exceeded", zap.String("key", name))
			writeError(w, http.StatusPaymentRequired, "client key budget exceeded")
			return false
		case access.DeniedRate:
			s.log.Warn("client key rate limit exceeded", zap.String("key", name))
			writeError(w, http.StatusTooManyRequests, "client key rate limit exceeded")
			return false
		}
		*r = *r.WithContext(withClientKey(r.Context(), name))
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

// clientKeyCtxKey is the context key under which the matched managed client-key
// name is stored, so the response path can charge usage back to it.
type clientKeyCtxKey struct{}

// withClientKey returns a context carrying the managed client-key name.
func withClientKey(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, clientKeyCtxKey{}, name)
}

// clientKeyFrom extracts the managed client-key name from the context, if any.
func clientKeyFrom(ctx context.Context) (string, bool) {
	name, ok := ctx.Value(clientKeyCtxKey{}).(string)
	return name, ok && name != ""
}

// record forwards an event to the usage tracker and, when the request was made
// with a managed client key, charges its cost (from configured pricing) and
// tokens against that key's budget/rate window. It is the single point through
// which all handlers record usage.
func (s *Server) record(ctx context.Context, e usage.Event) {
	s.usage.Record(e)
	if name, ok := clientKeyFrom(ctx); ok {
		s.keys.Charge(name, s.usage.Cost(e.Model, e.InputTokens, e.OutputTokens), e.InputTokens+e.OutputTokens)
	}
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
		s.record(r.Context(), usage.Event{Model: model, IsError: true})
		var bad *provider.BadRequestError
		if errors.As(err, &bad) {
			writeError(w, http.StatusBadRequest, bad.Error())
			return
		}
		writeUpstreamError(w, err)
		return
	}
	s.relayChatter(w, r, resp, c.Name(), model, stream)
}

// relayChatter writes an already-obtained provider.Response (OpenAI-format) to
// the client: error status relayed as-is, success streamed or buffered (with
// token usage recorded). It closes resp.Body.
func (s *Server) relayChatter(w http.ResponseWriter, r *http.Request, resp *provider.Response, providerName, model string, stream bool) {
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if resp.Status != http.StatusOK {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		s.record(r.Context(), usage.Event{Credential: resp.Credential, Model: model, IsError: true})
		s.log.Warn("provider error response", zap.String("provider", providerName),
			zap.Int("status", resp.Status), zap.String("body", string(buf)))
		if ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.Status)
		_, _ = w.Write(buf)
		return
	}

	if stream {
		s.record(r.Context(), usage.Event{Credential: resp.Credential, Model: model})
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
		s.record(r.Context(), usage.Event{Credential: resp.Credential, Model: model, IsError: true})
		writeError(w, http.StatusBadGateway, "read provider response")
		return
	}
	in, out := openaiUsage(buf)
	s.record(r.Context(), usage.Event{Credential: resp.Credential, Model: model, InputTokens: in, OutputTokens: out})
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
