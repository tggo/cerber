// Command cerber is a trust-first, self-contained AI provider proxy.
// See CLAUDE.md for the design principles and AUDIT.md for the upstream audit.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"cerber/internal/access"
	"cerber/internal/config"
	"cerber/internal/credential"
	"cerber/internal/provider/anthropic"
	"cerber/internal/server"
	"cerber/internal/version"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		log.Printf("cerber %s", version.String())
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	store, err := credential.NewStore(cfg.Providers.Anthropic.Credentials)
	if err != nil {
		log.Fatalf("credentials: %v", err)
	}

	httpClient := &http.Client{Timeout: cfg.Providers.Anthropic.Timeout.Std()}
	client := anthropic.New(cfg.Providers.Anthropic.BaseURL, cfg.Providers.Anthropic.Version, httpClient)
	refresher := anthropic.NewRefresher(cfg.Providers.Anthropic.BaseURL, httpClient)

	srv := server.New(access.New(cfg.Access.Keys), store, client, refresher)

	httpSrv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("cerber %s listening on %s (anthropic, %d credential(s))",
		version.String(), cfg.Server.Addr, store.Len())
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatalf("server: %v", err)
	}
}
