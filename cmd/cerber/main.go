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

	store, err := credential.NewStore(cfg.Providers.Anthropic.Credentials)
	if err != nil {
		logger.Fatal("credentials", zap.Error(err))
	}

	httpClient := &http.Client{Timeout: cfg.Providers.Anthropic.Timeout.Std()}
	client := anthropic.New(cfg.Providers.Anthropic.BaseURL, cfg.Providers.Anthropic.Version, httpClient)
	refresher := anthropic.NewRefresher(cfg.Providers.Anthropic.BaseURL, httpClient)

	srv := server.New(access.New(cfg.Access.Keys), store, client, refresher, logger)

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
