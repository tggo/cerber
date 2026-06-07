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
	"path/filepath"
	"strings"
	"time"

	"cerber/internal/access"
	"cerber/internal/auth/claude"
	"cerber/internal/auth/login"
	"cerber/internal/config"
	"cerber/internal/credential"
	"cerber/internal/logging"
	"cerber/internal/provider/anthropic"
	"cerber/internal/provider/gemini"
	"cerber/internal/provider/openai"
	"cerber/internal/server"
	"cerber/internal/tlscert"
	"cerber/internal/tokenstore"
	"cerber/internal/upstreamdial"
	"cerber/internal/version"

	"go.uber.org/zap"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	envPath := flag.String("env", ".env", "path to .env file (optional)")
	authDir := flag.String("auth-dir", "", "directory for OAuth tokens (default: config auth_dir or ./auths)")
	showVersion := flag.Bool("version", false, "print version and exit")
	claudeLogin := flag.Bool("claude-login", false, "run the Claude Code OAuth login and save tokens, then exit")
	noBrowser := flag.Bool("no-browser", false, "with --claude-login: print the URL instead of opening a browser")
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
	store, err := credential.NewStore(merged)
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

	srv := server.New(access.New(cfg.Access.Keys), store, client, refresher, logger)
	srv.SetRoutes(cfg.Providers.Routing)
	srv.SetAllowLocalhost(cfg.Access.AllowLocalhost)

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

	if o := cfg.Providers.OpenAI; o != nil {
		ostore, err := credential.NewStore(o.Credentials)
		if err != nil {
			logger.Fatal("openai credentials", zap.Error(err))
		}
		srv.RegisterChatter(openai.New("openai", o.BaseURL, ostore, &http.Client{Timeout: o.Timeout.Std()}))
		logger.Info("openai provider enabled", zap.Int("credentials", ostore.Len()))
	}

	if k := cfg.Providers.Grok; k != nil {
		kstore, err := credential.NewStore(k.Credentials)
		if err != nil {
			logger.Fatal("grok credentials", zap.Error(err))
		}
		// xAI/Grok is OpenAI-compatible: reuse the OpenAI provider, named "grok".
		srv.RegisterChatter(openai.New("grok", k.BaseURL, kstore, &http.Client{Timeout: k.Timeout.Std()}))
		logger.Info("grok provider enabled", zap.Int("credentials", kstore.Len()))
	}

	if g := cfg.Providers.Gemini; g != nil {
		gstore, err := credential.NewStore(g.Credentials)
		if err != nil {
			logger.Fatal("gemini credentials", zap.Error(err))
		}
		srv.RegisterChatter(gemini.New(g.BaseURL, gstore, &http.Client{Timeout: g.Timeout.Std()}))
		logger.Info("gemini provider enabled", zap.Int("credentials", gstore.Len()))
	}

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

	httpSrv := &http.Server{Addr: cfg.Server.Addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	if err := httpSrv.ListenAndServe(); err != nil {
		logger.Fatal("server", zap.Error(err))
	}
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
	creds_payload := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":      tok.AccessToken,
			"refreshToken":     tok.RefreshToken,
			"expiresAt":        time.Now().AddDate(10, 0, 0).UnixMilli(),
			"scopes":           []string{"user:inference", "user:profile"},
			"subscriptionType": "max",
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
	name := tok.Email
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

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
