// Command cerber is a trust-first, self-contained AI provider proxy.
// See CLAUDE.md for the design principles and AUDIT.md for the upstream audit.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
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
	"cerber/internal/tokenstore"
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
	flag.Parse()

	if *showVersion {
		fmt.Printf("cerber %s\n", version.String())
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
	client := anthropic.New(a.BaseURL, a.Version, httpClient)
	refresher := anthropic.NewRefresher(a.BaseURL, httpClient)

	srv := server.New(access.New(cfg.Access.Keys), store, client, refresher, logger)
	srv.SetRoutes(cfg.Providers.Routing)
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

	httpSrv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	logger.Info("cerber starting",
		zap.String("version", version.String()),
		zap.String("addr", cfg.Server.Addr),
		zap.Int("credentials", store.Len()),
		zap.String("log_level", cfg.Logging.Level),
	)
	if err := httpSrv.ListenAndServe(); err != nil {
		logger.Fatal("server", zap.Error(err))
	}
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
