// Command cerber is a trust-first, self-contained AI provider proxy.
// See CLAUDE.md for the design principles and AUDIT.md for the upstream audit.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tggo/cerber/internal/access"
	"github.com/tggo/cerber/internal/auth/claude"
	"github.com/tggo/cerber/internal/auth/login"
	"github.com/tggo/cerber/internal/auth/xai"
	"github.com/tggo/cerber/internal/config"
	"github.com/tggo/cerber/internal/credential"
	"github.com/tggo/cerber/internal/logging"
	"github.com/tggo/cerber/internal/provider/anthropic"
	"github.com/tggo/cerber/internal/provider/gemini"
	"github.com/tggo/cerber/internal/provider/openai"
	"github.com/tggo/cerber/internal/server"
	"github.com/tggo/cerber/internal/tlscert"
	"github.com/tggo/cerber/internal/tokenstore"
	"github.com/tggo/cerber/internal/upstreamdial"
	"github.com/tggo/cerber/internal/usage"
	"github.com/tggo/cerber/internal/version"

	"go.uber.org/zap"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	envPath := flag.String("env", ".env", "path to .env file (optional)")
	authDir := flag.String("auth-dir", "", "directory for OAuth tokens (default: config auth_dir or ./auths)")
	showVersion := flag.Bool("version", false, "print version and exit")
	claudeLogin := flag.Bool("claude-login", false, "run the Claude Code OAuth login and save tokens, then exit")
	xaiLogin := flag.Bool("xai-login", false, "run the xAI/Grok (SuperGrok/X Premium+) OAuth device login and save tokens, then exit")
	noBrowser := flag.Bool("no-browser", false, "with --claude-login/--xai-login: print the URL instead of opening a browser")
	loginPort := flag.Int("login-port", claude.DefaultCallbackPort, "with --claude-login: local OAuth callback port")
	genCert := flag.Bool("gen-cert", false, "generate a CA + cert for TLS impersonation (DOCKER ONLY), then exit")
	certDir := flag.String("cert-dir", "./certs", "with --gen-cert: output directory")
	impersonate := flag.String("impersonate", "api.anthropic.com", "with --gen-cert: comma-separated hostnames")
	seedCreds := flag.Bool("seed-claude-creds", false, "write ~/.claude/.credentials.json from auth_dir (impersonation), then exit")
	credsOut := flag.String("creds-out", "", "with --seed-claude-creds: output path (default ~/.claude/.credentials.json)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("cerber %s\n", version.String())
		return
	}

	if *genCert {
		runGenCert(*certDir, *impersonate)
		return
	}

	if *seedCreds {
		dir := *authDir
		if dir == "" {
			dir = "./auths"
		}
		runSeedClaudeCreds(dir, *credsOut)
		return
	}

	if *xaiLogin {
		runXAILogin(*authDir, *noBrowser)
		return
	}

	if *claudeLogin {
		runClaudeLogin(*authDir, *loginPort, *noBrowser)
		return
	}

	if err := config.LoadEnvFile(*envPath); err != nil {
		fatal("env: %v", err)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal("config: %v", err)
	}
	if *authDir != "" {
		cfg.AuthDir = *authDir
	}

	logger, closeLog, err := logging.New(cfg.Logging.Level, cfg.Logging.Dir, time.Now())
	if err != nil {
		fatal("logging: %v", err)
	}
	defer func() { _ = closeLog() }()

	// Merge OAuth tokens written by --claude-login with config credentials.
	diskCreds, err := tokenstore.Load(cfg.AuthDir)
	if err != nil {
		logger.Fatal("load tokens", zap.Error(err))
	}
	a := cfg.Providers.Anthropic
	if a == nil {
		// No anthropic block: allow OAuth-only operation if tokens exist on disk.
		if len(diskCreds) == 0 {
			logger.Fatal("no anthropic provider configured; add one to config or run: cerber --claude-login")
		}
		a = config.DefaultAnthropic()
	}
	merged := append(append([]config.Credential{}, a.Credentials...), diskCreds...)
	if len(merged) == 0 {
		logger.Fatal("no anthropic credentials configured; add one to config or run: cerber --claude-login")
	}
	store, err := credential.NewStore(merged, credential.WithFillFirst(cfg.Providers.Strategy == "fill-first"))
	if err != nil {
		logger.Fatal("credentials", zap.Error(err))
	}

	httpClient := &http.Client{Timeout: a.Timeout.Std()}
	if cfg.TLS.UseDoH {
		// Resolve the upstream via DoH so we bypass the /etc/hosts redirect that
		// points api.anthropic.com at cerber itself (TLS impersonation).
		res := upstreamdial.NewResolver()
		httpClient.Transport = &http.Transport{
			DialContext:       res.DialContext,
			ForceAttemptHTTP2: true,
			Proxy:             http.ProxyFromEnvironment,
		}
		logger.Info("upstream DoH resolution enabled")
	}
	client := anthropic.New(a.BaseURL, a.Version, httpClient)
	refresher := anthropic.NewRefresher(a.BaseURL, httpClient)

	// Dynamic, dashboard-managed client keys (persisted), accepted alongside the
	// static config keys.
	keyStore, kerr := access.LoadStore(cfg.Access.KeysFile)
	if kerr != nil {
		logger.Fatal("client keys", zap.Error(kerr))
	}
	dl := cfg.Access.DefaultKeyLimits
	keyStore.SetDefaultLimits(access.Limits{
		MaxCostUSD:   dl.MaxCostUSD,
		BudgetPeriod: dl.BudgetPeriod,
		MaxRequests:  dl.MaxRequests,
		MaxTokens:    dl.MaxTokens,
		RatePeriod:   dl.RatePeriod,
	})
	// Governance is only tamper-proof when a separate management_key gates /admin.
	// Without it, /admin falls back to the client-key check, so a capped key could
	// lift its own limits via POST /admin/keys/{name}/limits. Warn loudly.
	if cfg.Access.ManagementKey == "" && keyStoreHasLimits(keyStore, dl) {
		logger.Warn("per-key limits configured without access.management_key: " +
			"a managed client key can edit its own limits via /admin — set a management_key for multi-tenant use")
	}

	srv := server.New(access.New(cfg.Access.Keys), store, client, refresher, logger)
	srv.SetClientKeyStore(keyStore)
	srv.SetRoutes(cfg.Providers.Routing)
	srv.SetModelAliases(cfg.Providers.ModelAliases)
	srv.SetFallbacks(cfg.Providers.Fallbacks)
	srv.SetAllowLocalhost(cfg.Access.AllowLocalhost)
	srv.SetManagementKey(cfg.Access.ManagementKey)

	// Persistent usage + per-model pricing (cost). Loaded from disk, saved
	// periodically and on shutdown.
	tracker, terr := usage.Load(cfg.Usage.File, usage.WithRecentCap(cfg.Usage.RecentLog))
	if terr != nil {
		logger.Warn("load usage", zap.Error(terr))
		tracker = usage.New(usage.WithRecentCap(cfg.Usage.RecentLog))
	}
	if len(cfg.Usage.Pricing) > 0 {
		pricing := map[string]usage.Price{}
		for m, p := range cfg.Usage.Pricing {
			pricing[m] = usage.Price{Input: p.Input, Output: p.Output}
		}
		tracker.SetPricing(pricing)
	}
	srv.SetUsageTracker(tracker)

	// TLS impersonation: transparently proxy non-/v1 paths (Claude Code console/
	// bootstrap calls) to the real upstream, reusing the (DoH) transport.
	if cfg.TLS.Enabled {
		if target, perr := url.Parse(a.BaseURL); perr == nil {
			// Inject cerber's pooled OAuth credential into proxied console calls so
			// the client never needs valid auth (cerber is the sole token owner).
			authToken := func() string {
				c, cerr := store.NextOf(func(c *credential.Credential) bool { return c.Kind() == credential.KindOAuth })
				if cerr != nil {
					return ""
				}
				return c.AccessToken()
			}
			srv.SetUpstreamProxy(target, httpClient.Transport, authToken)
			logger.Info("upstream reverse-proxy enabled for unhandled paths", zap.String("target", a.BaseURL))
		}
	}
	srv.SetTokenPersister(func(name string, tok credential.OAuthTokens) {
		if _, perr := tokenstore.Save(cfg.AuthDir, name, tokenstore.Record{
			Name: name, AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, ExpiresAt: tok.ExpiresAt,
		}); perr != nil {
			logger.Warn("persist refreshed token", zap.String("credential", name), zap.Error(perr))
		}
	})

	// Anthropic's store is the primary one; expose it in the accounts view too.
	srv.RegisterProviderStore("anthropic", store)

	if o := cfg.Providers.OpenAI; o != nil {
		ostore, err := credential.NewStore(o.Credentials, credential.WithFillFirst(cfg.Providers.Strategy == "fill-first"))
		if err != nil {
			logger.Fatal("openai credentials", zap.Error(err))
		}
		srv.RegisterChatter(openai.New("openai", o.BaseURL, ostore, &http.Client{Timeout: o.Timeout.Std()}, openai.WithQueueMetrics(srv.Metrics())))
		srv.RegisterProviderStore("openai", ostore)
		logger.Info("openai provider enabled", zap.Int("credentials", ostore.Len()))
	}

	// ArliAI (https://www.arliai.com) is OpenAI-compatible: reuse the OpenAI
	// provider, named "arliai". Models are discovered via /v1/models and routed
	// by name (e.g. Qwen3.5-27B-Derestricted).
	if a := cfg.Providers.ArliAI; a != nil {
		astore, err := credential.NewStore(a.Credentials, credential.WithFillFirst(cfg.Providers.Strategy == "fill-first"))
		if err != nil {
			logger.Fatal("arliai credentials", zap.Error(err))
		}
		srv.RegisterChatter(openai.New("arliai", a.BaseURL, astore, &http.Client{Timeout: a.Timeout.Std()}, openai.WithConcurrency(a.Concurrency), openai.WithQueueMetrics(srv.Metrics())))
		srv.RegisterProviderStore("arliai", astore)
		logger.Info("arliai provider enabled", zap.Int("credentials", astore.Len()), zap.Int("concurrency", a.Concurrency))
	}

	// Grok = config API keys + xAI OAuth (Grok Build / SuperGrok subscription)
	// tokens written by --xai-login to auth_dir/xai. Enable the provider if either
	// is present.
	xaiDir := filepath.Join(cfg.AuthDir, "xai")
	xaiCreds, xerr := tokenstore.Load(xaiDir)
	if xerr != nil {
		logger.Fatal("load xai tokens", zap.Error(xerr))
	}
	if k := cfg.Providers.Grok; k != nil || len(xaiCreds) > 0 {
		if k == nil {
			k = config.DefaultGrok()
		}
		merged := append(append([]config.Credential{}, k.Credentials...), xaiCreds...)
		kstore, err := credential.NewStore(merged, credential.WithFillFirst(cfg.Providers.Strategy == "fill-first"))
		if err != nil {
			logger.Fatal("grok credentials", zap.Error(err))
		}
		// xAI/Grok is OpenAI-compatible: reuse the OpenAI provider, named "grok".
		grokHTTP := &http.Client{Timeout: k.Timeout.Std()}
		grokProv := openai.New("grok", k.BaseURL, kstore, grokHTTP, openai.WithQueueMetrics(srv.Metrics()))
		// Refresh subscription (OAuth) tokens before they expire and persist them.
		grokProv.SetOAuthRefresh(func(ctx context.Context, refreshToken string) (credential.OAuthTokens, error) {
			t, e := xai.Refresh(ctx, grokHTTP, refreshToken, time.Now)
			if e != nil {
				return credential.OAuthTokens{}, e
			}
			return credential.OAuthTokens{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, ExpiresAt: t.ExpiresAt}, nil
		}, func(name string, tok credential.OAuthTokens) {
			if _, perr := tokenstore.Save(xaiDir, name, tokenstore.Record{
				Name: name, AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, ExpiresAt: tok.ExpiresAt,
			}); perr != nil {
				logger.Warn("persist refreshed xai token", zap.String("credential", name), zap.Error(perr))
			}
		})
		srv.RegisterChatter(grokProv)
		srv.RegisterProviderStore("grok", kstore)
		logger.Info("grok provider enabled", zap.Int("credentials", kstore.Len()), zap.Int("oauth_subscription", len(xaiCreds)))
	}

	if o := cfg.Providers.Ollama; o != nil {
		creds := o.Credentials
		if len(creds) == 0 {
			// Local ollama/vLLM ignores auth; inject a dummy key so the rotating
			// store (which requires >=1 credential) has something to hand out.
			creds = []config.Credential{{Type: config.CredentialAPIKey, Name: "ollama", Key: "ollama"}}
		}
		ostore, err := credential.NewStore(creds, credential.WithFillFirst(cfg.Providers.Strategy == "fill-first"))
		if err != nil {
			logger.Fatal("ollama credentials", zap.Error(err))
		}
		// ollama/vLLM serve an OpenAI-compatible API: reuse the OpenAI provider.
		srv.RegisterChatter(openai.New("ollama", o.BaseURL, ostore, &http.Client{Timeout: o.Timeout.Std()}, openai.WithQueueMetrics(srv.Metrics())))
		srv.RegisterProviderStore("ollama", ostore)
		logger.Info("ollama provider enabled", zap.String("base_url", o.BaseURL),
			zap.Int("credentials", ostore.Len()))
	}

	if g := cfg.Providers.Gemini; g != nil {
		gstore, err := credential.NewStore(g.Credentials, credential.WithFillFirst(cfg.Providers.Strategy == "fill-first"))
		if err != nil {
			logger.Fatal("gemini credentials", zap.Error(err))
		}
		srv.RegisterChatter(gemini.New(g.BaseURL, gstore, &http.Client{Timeout: g.Timeout.Std()}))
		srv.RegisterProviderStore("gemini", gstore)
		logger.Info("gemini provider enabled", zap.Int("credentials", gstore.Len()))
	}

	// Periodically validate every credential and refresh each provider's model
	// list (drives key-health in the dashboard + discovery routing). Cadence comes
	// from providers.ollama.probe_interval when set, else 60s.
	probeInterval := 60 * time.Second
	if o := cfg.Providers.Ollama; o != nil && o.ProbeInterval.Std() > 0 {
		probeInterval = o.ProbeInterval.Std()
	}
	startHealthProbe(srv, probeInterval, logger)

	handler := srv.Handler()
	logger.Info("cerber starting",
		zap.String("version", version.String()),
		zap.String("addr", cfg.Server.Addr),
		zap.Int("credentials", store.Len()),
		zap.String("log_level", cfg.Logging.Level),
		zap.Bool("tls", cfg.TLS.Enabled),
	)

	// Optional HTTPS impersonation listener (Docker only).
	if cfg.TLS.Enabled {
		certFile := filepath.Join(cfg.TLS.CertDir, "cert.pem")
		keyFile := filepath.Join(cfg.TLS.CertDir, "key.pem")
		tlsSrv := &http.Server{Addr: cfg.TLS.Addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			logger.Info("cerber TLS listening", zap.String("addr", cfg.TLS.Addr), zap.Strings("hosts", cfg.TLS.Hosts))
			if err := tlsSrv.ListenAndServeTLS(certFile, keyFile); err != nil {
				logger.Fatal("tls server", zap.Error(err))
			}
		}()
	}

	// Persist usage periodically and on shutdown.
	saveUsage := func() {
		if err := tracker.Save(cfg.Usage.File); err != nil {
			logger.Warn("save usage", zap.Error(err))
		}
		// Persist lazily-stamped client-key last-used times.
		if err := keyStore.Save(); err != nil {
			logger.Warn("save keys", zap.Error(err))
		}
	}
	stopSave := make(chan struct{})
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				saveUsage()
			case <-stopSave:
				return
			}
		}
	}()

	httpSrv := &http.Server{Addr: cfg.Server.Addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server", zap.Error(err))
		}
	}()

	// Wait for SIGINT/SIGTERM, then persist usage and shut down gracefully.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	close(stopSave)
	saveUsage()
	logger.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
}

// runGenCert generates a CA + leaf cert for TLS impersonation and prints setup.
func runGenCert(dir, impersonate string) {
	var hosts []string
	for _, h := range strings.Split(impersonate, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	f, err := tlscert.Generate(dir, hosts, time.Now())
	if err != nil {
		fatal("gen-cert: %v", err)
	}
	fmt.Printf("Generated TLS impersonation certs in %s:\n", dir)
	fmt.Printf("  CA   (trust this): %s\n", f.CA)
	fmt.Printf("  cert (server):     %s\n", f.Cert)
	fmt.Printf("  key  (server):     %s\n\n", f.Key)
	fmt.Printf("In the container: export NODE_EXTRA_CA_CERTS=%s and map %s -> 127.0.0.1.\n",
		f.CA, strings.Join(hosts, ", "))
}

// runSeedClaudeCreds writes a Claude Code credentials file from an OAuth token in
// auth_dir, so Claude Code in the impersonation container believes it has a normal
// Max login (no API key, no prompt). The token is given a far-future expiry so
// Claude Code never refreshes it — cerber is the sole token owner and injects its
// own pooled credential upstream.
func runSeedClaudeCreds(authDir, out string) {
	creds, err := tokenstore.Load(authDir)
	if err != nil {
		fatal("seed-claude-creds: %v", err)
	}
	var tok *config.Credential
	for i := range creds {
		if creds[i].Type == config.CredentialOAuth {
			tok = &creds[i]
			break
		}
	}
	if tok == nil {
		fatal("seed-claude-creds: no oauth credential in %s (run: cerber --claude-login)", authDir)
	}
	home, herr := os.UserHomeDir()
	if herr != nil {
		fatal("seed-claude-creds: home dir: %v", herr)
	}
	if out == "" {
		out = filepath.Join(home, ".claude", ".credentials.json")
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		fatal("seed-claude-creds: mkdir: %v", err)
	}

	// Credentials (.credentials.json): the OAuth token, with a far-future expiry so
	// Claude Code never refreshes it (cerber is the sole token owner).
	// No subscriptionType: Claude Code fetches the real account type from the API
	// (faking it here misreports Team accounts as Max).
	creds_payload := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  tok.AccessToken,
			"refreshToken": tok.RefreshToken,
			"expiresAt":    time.Now().AddDate(10, 0, 0).UnixMilli(),
			"scopes":       []string{"user:inference", "user:profile"},
		},
	}
	data, _ := json.Marshal(creds_payload)
	if err := os.WriteFile(out, data, 0o600); err != nil {
		fatal("seed-claude-creds: write credentials: %v", err)
	}

	// State (~/.claude.json): mark onboarding complete so interactive Claude Code
	// starts as a normal session instead of prompting login. Written only if
	// missing, so Claude Code's own enriched state (account, projects) persists.
	stateFile := filepath.Join(home, ".claude.json")
	if _, statErr := os.Stat(stateFile); os.IsNotExist(statErr) {
		state, _ := json.Marshal(map[string]any{
			"hasCompletedOnboarding": true,
			"numStartups":            1,
			"firstStartTime":         time.Now().UTC().Format(time.RFC3339),
		})
		if err := os.WriteFile(stateFile, state, 0o644); err != nil {
			fatal("seed-claude-creds: write state: %v", err)
		}
	}
	fmt.Printf("Seeded Claude Code credentials at %s and state at %s (from %s)\n", out, stateFile, tok.Name)
}

// runXAILogin performs the xAI/Grok OAuth device flow and saves the token under
// auth_dir/xai, where it is picked up as a Grok OAuth credential at startup.
func runXAILogin(authDir string, noBrowser bool) {
	if authDir == "" {
		authDir = "./auths"
	}
	tok, err := login.Grok(context.Background(), login.Options{NoBrowser: noBrowser, Out: os.Stdout})
	if err != nil {
		fatal("xai login: %v", err)
	}
	dir := filepath.Join(authDir, "xai")
	path, err := tokenstore.Save(dir, "xai", tokenstore.Record{
		Name: "xai", AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, ExpiresAt: tok.ExpiresAt,
	})
	if err != nil {
		fatal("save xai tokens: %v", err)
	}
	fmt.Printf("\nGrok login successful. Token saved to %s\n", path)
}

// runClaudeLogin performs the interactive OAuth flow and saves the tokens.
func runClaudeLogin(authDir string, port int, noBrowser bool) {
	if authDir == "" {
		authDir = "./auths"
	}
	tok, err := login.Claude(context.Background(), login.Options{
		Port: port, NoBrowser: noBrowser, Out: os.Stdout,
	})
	if err != nil {
		fatal("claude login: %v", err)
	}
	// Name uniquely per account so multiple orgs on the same email don't collide.
	name := tok.Email
	if tok.OrgName != "" {
		if name != "" {
			name += "-" + tok.OrgName
		} else {
			name = tok.OrgName
		}
	}
	if name == "" {
		name = "claude"
	}
	path, err := tokenstore.Save(authDir, name, tokenstore.Record{
		Name: name, AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken,
		Email: tok.Email, ExpiresAt: tok.ExpiresAt,
	})
	if err != nil {
		fatal("save tokens: %v", err)
	}
	fmt.Printf("\nClaude login successful (%s). Tokens saved to %s\n", name, path)
}

// startHealthProbe runs an initial credential/model probe across all providers
// and then repeats it on the given interval. A non-positive interval disables
// the repeat (the one-shot still runs).
func startHealthProbe(srv *server.Server, interval time.Duration, logger *zap.Logger) {
	probe := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.ProbeAll(ctx)
	}
	go probe()
	if interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			probe()
		}
	}()
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// keyStoreHasLimits reports whether per-key governance is in effect: either a
// non-zero default applied to new keys, or any existing managed key carrying a
// budget/rate limit.
func keyStoreHasLimits(st *access.Store, def config.KeyLimits) bool {
	if def.MaxCostUSD > 0 || def.MaxRequests > 0 || def.MaxTokens > 0 {
		return true
	}
	for _, k := range st.List() {
		if k.Limits.MaxCostUSD > 0 || k.Limits.MaxRequests > 0 || k.Limits.MaxTokens > 0 {
			return true
		}
	}
	return false
}
