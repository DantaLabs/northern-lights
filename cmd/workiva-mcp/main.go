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
	"io"
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
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

// version is set at build time via -ldflags.
var version = "dev"

// demoStartupMessage is printed when NL_DEMO_MODE is enabled.
const demoStartupMessage = "DEMO MODE: no Workiva credentials required. All data is synthetic."

func main() {
	if err := run(); err != nil {
		log.Fatalf("workiva-mcp: %v", err)
	}
}

func run() error {
	if isAuditInvocation(os.Args[1:]) {
		return runAudit(os.Args[2:], os.Stdout)
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		return runHealthcheck(os.Args[2:])
	}

	configPath := flag.String("config", "", "path to YAML config file (optional; NL_ env vars override)")
	mappingsPath := flag.String("mappings", "", "path to declarative mappings YAML (optional; defaults to configs/config.yaml if it exists)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("workiva-mcp %s\n", version)
		return nil
	}

	cfg, handler, cleanup, err := buildServer(*configPath, *mappingsPath)
	if err != nil {
		return err
	}
	defer cleanup()

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

func runHealthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	endpoint := fs.String("url", "http://127.0.0.1:8080/readyz", "readiness URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return checkHealth(ctx, &http.Client{Timeout: 5 * time.Second}, *endpoint)
}

func checkHealth(ctx context.Context, client *http.Client, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("healthcheck request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck status %d", resp.StatusCode)
	}
	return nil
}

// buildServer loads configuration, opens the store and audit log, builds
// the Workiva client, and returns the MCP HTTP handler. cleanup closes the
// store and audit log. It is extracted so integration tests can construct
// the same handler the binary serves without starting a listener.
func buildServer(configPath, mappingsPath string) (*config.Config, http.Handler, func(), error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load config: %w", err)
	}

	apiToken, err := mcpserver.EnvAPIToken()
	if err != nil {
		if !cfg.DemoMode {
			return nil, nil, nil, err
		}
		// In demo mode, generate a random token per run and print it so the
		// operator can connect. A hardcoded demo token would let anyone
		// reach a demo server exposed on a network.
		apiToken = randomToken()
		log.Printf("demo mode API token: %s", apiToken)
	}

	if cfg.DemoMode {
		log.Println(demoStartupMessage)
		if cfg.WorkivaClientID == "" {
			cfg.WorkivaClientID = "demo"
		}
		if cfg.WorkivaClientSecret == "" {
			cfg.WorkivaClientSecret = "demo"
		}
		cfg.RequireWriteConfirmation = true
	}

	if cfg.WorkivaClientID == "" || cfg.WorkivaClientSecret == "" {
		return nil, nil, nil, errors.New("NL_WORKIVA_CLIENT_ID and NL_WORKIVA_CLIENT_SECRET must be set (or enable NL_DEMO_MODE=true)")
	}

	store, err := mapping.Open(cfg.DBPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open mapping store: %w", err)
	}

	// Seed the store from a declarative mappings file when present, or
	// from the built-in demo fixture when running in demo mode.
	path := mappingsPath
	if cfg.DemoMode {
		if err := bootstrap.LoadDemoMappings(context.Background(), store); err != nil {
			if err := store.Close(); err != nil {
				log.Printf("close mapping store: %v", err)
			}
			return nil, nil, nil, fmt.Errorf("load demo mappings: %w", err)
		}
		log.Println("loaded demo mappings")
	} else {
		if path == "" {
			const defaultMappings = "configs/config.yaml"
			if _, err := os.Stat(defaultMappings); err == nil {
				path = defaultMappings
			}
		}
		if path != "" {
			if err := bootstrap.LoadMappings(context.Background(), path, store); err != nil {
				if err := store.Close(); err != nil {
					log.Printf("close mapping store: %v", err)
				}
				return nil, nil, nil, fmt.Errorf("load mappings: %w", err)
			}
			log.Printf("loaded mappings from %s", path)
		}
	}

	auditLog, err := audit.Open(cfg.DBPath)
	if err != nil {
		if err := store.Close(); err != nil {
			log.Printf("close mapping store: %v", err)
		}
		return nil, nil, nil, fmt.Errorf("open audit log: %w", err)
	}

	// Drop staged write confirmations left over from previous runs.
	if n, err := store.DeleteExpiredPendingWrites(context.Background(), 5*time.Minute); err != nil {
		log.Printf("pending write cleanup failed: %v", err)
	} else if n > 0 {
		log.Printf("cleaned up %d expired pending writes", n)
	}

	cleanup := func() {
		if err := auditLog.Close(); err != nil {
			log.Printf("close audit log: %v", err)
		}
		if err := store.Close(); err != nil {
			log.Printf("close mapping store: %v", err)
		}
	}

	if cfg.DemoMode {
		if _, err := auditLog.Append(context.Background(), audit.Entry{
			Actor:  "setup",
			Tool:   "system",
			Action: "init",
			Target: "demo",
		}); err != nil {
			cleanup()
			return nil, nil, nil, fmt.Errorf("seed demo audit entry: %w", err)
		}
	}

	baseURL, err := url.Parse(cfg.BaseURL())
	if err != nil {
		cleanup()
		return nil, nil, nil, fmt.Errorf("parse base URL: %w", err)
	}

	var client *workiva.Client
	if cfg.DemoMode {
		client = workiva.NewDemoClient(baseURL)
	} else {
		httpClient := &http.Client{Timeout: 60 * time.Second}
		tokens := workiva.NewTokenProvider(baseURL, cfg.WorkivaClientID, cfg.WorkivaClientSecret, "file:read file:write", httpClient)
		client = workiva.NewClient(baseURL, tokens, ratelimit.NewLimiter(), httpClient)
	}
	// Keep Northern Lights' proven REST implementation primary. The provider
	// router is the transport seam for selectively adopting Workiva's official
	// MCP capabilities later without changing our public tools or governance.
	workivaBackend := workivaprovider.NewRouter(client)

	registry := mcpserver.NewRegistry()
	for _, tool := range tools.All() {
		registry.Register(tool)
	}

	handler, err := mcpserver.New(mcpserver.Deps{
		Client: workivaBackend,
		Store:  store,
		Audit:  auditLog,
		Cfg:    cfg,
	}, registry, &mcpserver.Options{
		APIToken:                   apiToken,
		Version:                    version,
		DisableLocalhostProtection: cfg.DisableLocalhostProtection,
	})
	if err != nil {
		cleanup()
		return nil, nil, nil, err
	}

	return cfg, handler, cleanup, nil
}
