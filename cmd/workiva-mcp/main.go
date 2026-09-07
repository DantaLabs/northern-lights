// Command workiva-mcp is the northern-lights Workiva MCP server
// entrypoint. It loads configuration, opens the mapping store and audit
// log, builds the Workiva client, and serves the MCP streamable HTTP
// endpoint with bearer auth.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/bootstrap"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/mcpserver/tools"
	"github.com/dantalabs/northern-lights/internal/ratelimit"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	if err := run(); err != nil {
		log.Fatalf("workiva-mcp: %v", err)
	}
}

func run() error {
	configPath := flag.String("config", "", "path to YAML config file (optional; NL_ env vars override)")
	mappingsPath := flag.String("mappings", "", "path to declarative mappings YAML (optional; defaults to configs/config.yaml if it exists)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("workiva-mcp %s\n", version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.WorkivaClientID == "" || cfg.WorkivaClientSecret == "" {
		return errors.New("NL_WORKIVA_CLIENT_ID and NL_WORKIVA_CLIENT_SECRET must be set")
	}
	apiToken, err := mcpserver.EnvAPIToken()
	if err != nil {
		return err
	}

	// The mapping store and audit log open separate handles on the same
	// SQLite file. Each handle pins MaxOpenConns(1), so writes serialize
	// within each package and brief SQLITE_BUSY contention between them is
	// possible under heavy load but self-correcting; keeping the handles
	// independent lets the audit log be verified and exported standalone.
	store, err := mapping.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open mapping store: %w", err)
	}
	defer store.Close()

	// Seed the store from a declarative mappings file when present.
	path := *mappingsPath
	if path == "" {
		const defaultMappings = "configs/config.yaml"
		if _, err := os.Stat(defaultMappings); err == nil {
			path = defaultMappings
		}
	}
	if path != "" {
		if err := bootstrap.LoadMappings(context.Background(), path, store); err != nil {
			return fmt.Errorf("load mappings: %w", err)
		}
		log.Printf("loaded mappings from %s", path)
	}

	auditLog, err := audit.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	defer auditLog.Close()

	baseURL, err := url.Parse(cfg.BaseURL())
	if err != nil {
		return fmt.Errorf("parse base URL: %w", err)
	}
	httpClient := &http.Client{Timeout: 60 * time.Second}
	tokens := workiva.NewTokenProvider(baseURL, cfg.WorkivaClientID, cfg.WorkivaClientSecret, "file:read file:write", httpClient)
	client := workiva.NewClient(baseURL, tokens, ratelimit.NewLimiter(), httpClient)

	registry := mcpserver.NewRegistry()
	for _, tool := range []mcpserver.Tool{
		tools.ListSpreadsheets(),
		tools.ReadRange(),
		tools.SearchFields(),
		tools.GetField(),
		tools.UpdateField(),
		tools.SyncMapping(),
		tools.AuditTrail(),
	} {
		registry.Register(tool)
	}

	handler, err := mcpserver.New(mcpserver.Deps{
		Client: client,
		Store:  store,
		Audit:  auditLog,
		Cfg:    cfg,
	}, registry, &mcpserver.Options{APIToken: apiToken, Version: version})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("workiva-mcp %s listening on %s (region %s)", version, cfg.ListenAddr, cfg.Region)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
