// Command cerber is a trust-first, self-contained AI provider proxy.
// See CLAUDE.md for the design principles and AUDIT.md for the upstream audit.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"cerber/internal/access"
	"cerber/internal/config"
	"cerber/internal/credential"
	"cerber/internal/logging"
	"cerber/internal/provider/anthropic"
	"cerber/internal/provider/gemini"
	"cerber/internal/provider/openai"
	"cerber/internal/server"
	"cerber/internal/version"

	"go.uber.org/zap"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	envPath := flag.String("env", ".env", "path to .env file (optional)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("cerber %s\n", version.String())
		return
	}

	if err := config.LoadEnvFile(*envPath); err != nil {
		fatal("env: %v", err)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal("config: %v", err)
	}

	logger, closeLog, err := logging.New(cfg.Logging.Level, cfg.Logging.Dir, time.Now())
	if err != nil {
		fatal("logging: %v", err)
	}
	defer func() { _ = closeLog() }()

	a := cfg.Providers.Anthropic
	if a == nil {
		logger.Fatal("anthropic provider is currently required (it backs /v1/messages and the default route)")
	}
	store, err := credential.NewStore(a.Credentials)
	if err != nil {
		logger.Fatal("credentials", zap.Error(err))
	}

	httpClient := &http.Client{Timeout: a.Timeout.Std()}
	client := anthropic.New(a.BaseURL, a.Version, httpClient)
	refresher := anthropic.NewRefresher(a.BaseURL, httpClient)

	srv := server.New(access.New(cfg.Access.Keys), store, client, refresher, logger)
	srv.SetRoutes(cfg.Providers.Routing)

	if o := cfg.Providers.OpenAI; o != nil {
		ostore, err := credential.NewStore(o.Credentials)
		if err != nil {
			logger.Fatal("openai credentials", zap.Error(err))
		}
		srv.RegisterChatter(openai.New(o.BaseURL, ostore, &http.Client{Timeout: o.Timeout.Std()}))
		logger.Info("openai provider enabled", zap.Int("credentials", ostore.Len()))
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

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
