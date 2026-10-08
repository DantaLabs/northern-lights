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

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/bootstrap"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/mcpserver/tools"
	"github.com/dantalabs/northern-lights/internal/ratelimit"
	"github.com/dantalabs/northern-lights/internal/relationships"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/transfer"
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
	transferEnabled, err := transferProductionEnabled()
	if err != nil {
		return nil, nil, nil, err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load config: %w", err)
	}
	var provisionedKey *provisionedBackupKey
	backupEnabled := os.Getenv("NL_BACKUP_MAINTENANCE") == "true"
	if rawOptIn, present := os.LookupEnv("NL_BACKUP_MAINTENANCE"); present {
		enabled, optErr := backupMaintenanceOptIn(rawOptIn)
		if optErr != nil {
			return nil, nil, nil, optErr
		}
		backupEnabled = enabled
	}
	if backupEnabled {
		secretID, hasSecretID := os.LookupEnv(backupSigningKeySecretIDEnv)
		_, hasFile := os.LookupEnv("NL_BACKUP_SIGNING_KEY_FILE")
		_, hasParent := os.LookupEnv("NL_BACKUP_TEMP_PARENT")
		if hasSecretID && (hasFile || hasParent) {
			return nil, nil, nil, errors.New("backup signing key secret ID cannot be combined with legacy key file or temporary parent")
		}
		if hasSecretID {
			parent := os.TempDir()
			if parent != "/tmp" {
				return nil, nil, nil, errors.New("backup signing key requires /tmp ephemeral storage")
			}
			var provisionErr error
			provisionedKey, provisionErr = provisionRuntimeBackupSigningKey(context.Background(), cfg, secretID, productionBackupKeySecretReader, parent)
			if provisionErr != nil {
				return nil, nil, nil, provisionErr
			}
		}
	}
	provisionCleanupOnFailure := true
	defer func() {
		if provisionCleanupOnFailure && provisionedKey != nil {
			provisionedKey.cleanup()
		}
	}()
	storageCtx := storageContextForConfig(cfg)
	var startupFence *transfer.AzureBlobFence
	if transferEnabled && os.Getenv("NL_STARTUP_RESTORE_ADMISSION") != "true" {
		return nil, nil, nil, errors.New("transfer production requires NL_STARTUP_RESTORE_ADMISSION=true")
	}
	if restoreOptIn() {
		startupFence, err = admitStartupRestore(storageCtx, cfg)
		if err != nil {
			if transferEnabled {
				return nil, nil, nil, fmt.Errorf("transfer production startup restore admission: %w", err)
			}
			log.Printf("startup restore blocked: %v", err)
			if provisionedKey != nil {
				provisionedKey.cleanup()
				provisionCleanupOnFailure = false
			}
			return cfg, blockedStartup(), func() {}, nil
		}
	}

	apiToken := ""
	if cfg.AuthMode == config.AuthModeAPIKey {
		apiToken, err = mcpserver.EnvAPIToken()
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
	}

	authOptions, err := buildAuthOptions(context.Background(), cfg, apiToken, startupEntraVerifierFactory)
	if err != nil {
		return nil, nil, nil, err
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

	db, err := sqlitedb.Open(cfg.DBPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open shared database: %w", err)
	}
	maintenance := newMaintenanceCoordinator(db)
	closeDB := func() {
		if err := db.Close(); err != nil {
			log.Printf("close shared database: %v", err)
		}
	}
	store, err := mapping.NewWithDB(db)
	if err != nil {
		closeDB()
		return nil, nil, nil, fmt.Errorf("bootstrap mapping store: %w", err)
	}
	auditLog, err := audit.NewWithDB(db)
	if err != nil {
		closeDB()
		return nil, nil, nil, fmt.Errorf("bootstrap audit log: %w", err)
	}
	assuranceStore, err := assurance.NewWithDB(db)
	if err != nil {
		closeDB()
		return nil, nil, nil, fmt.Errorf("bootstrap assurance store: %w", err)
	}
	assuranceStore.SetAuditLog(auditLog)
	if cfg.AssuranceEnabled {
		publicKey, err := assurance.ParsePublicKey(cfg.AssuranceBundlePublicKey)
		if err != nil {
			closeDB()
			return nil, nil, nil, fmt.Errorf("load assurance bundle public key: %w", err)
		}
		if err := assuranceStore.Bootstrap(storageCtx, identity.StorageTenant(storageCtx), publicKey); err != nil {
			closeDB()
			return nil, nil, nil, fmt.Errorf("bootstrap assurance definitions: %w", err)
		}
	}
	if err := configureProductionEvidenceStorage(storageCtx, cfg, assuranceStore); err != nil {
		closeDB()
		return nil, nil, nil, fmt.Errorf("configure production evidence storage: %w", err)
	}
	backupKeyPath, backupTempParent := "", ""
	if provisionedKey != nil {
		backupKeyPath, backupTempParent = provisionedKey.keyPath, provisionedKey.stage
	}
	backupMaintenance, err := configureProductionBackupWithKeyPaths(storageCtx, cfg, authOptions, maintenance, auditLog, defaultBackupDependencyFactory, backupKeyPath, backupTempParent)
	if err != nil {
		closeDB()
		return nil, nil, nil, fmt.Errorf("configure production backup maintenance: %w", err)
	}
	janitorCtx, cancelJanitor := context.WithCancel(storageCtx)
	janitorDone := make(chan struct{})
	var stopTransferJanitor func()
	go func() {
		defer close(janitorDone)
		assuranceStore.RunJanitorGated(janitorCtx, 10*time.Second, maintenance.gate)
	}()
	cleanup := func() {
		cancelJanitor()
		<-janitorDone
		if stopTransferJanitor != nil {
			stopTransferJanitor()
		}
		if provisionedKey != nil {
			provisionedKey.cleanup()
		}
		closeDB()
	}
	provisionCleanupOnFailure = false

	// from the built-in demo fixture when running in demo mode.
	path := mappingsPath
	if cfg.DemoMode {
		if err := bootstrap.LoadDemoMappings(storageCtx, store); err != nil {
			cleanup()
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
			if err := bootstrap.LoadMappings(storageCtx, path, store); err != nil {
				cleanup()
				return nil, nil, nil, fmt.Errorf("load mappings: %w", err)
			}
			log.Printf("loaded mappings from %s", path)
		}
	}

	// Drop staged write confirmations left over from previous runs. The
	// janitor runs before any tenant context exists, so it sweeps every
	// tenant's expired rows.
	if n, err := store.DeleteExpiredPendingWritesGlobal(storageCtx, 5*time.Minute); err != nil {
		log.Printf("pending write cleanup failed: %v", err)
	} else if n > 0 {
		log.Printf("cleaned up %d expired pending writes", n)
	}

	// cleanup also stops the bounded assurance janitor before closing SQLite.

	if cfg.DemoMode {
		if _, err := auditLog.Append(storageCtx, audit.Entry{
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
	restoreNS := blobNamespace{accountURL: os.Getenv("NL_RESTORE_BLOB_SERVICE_URL"), container: os.Getenv("NL_RESTORE_BLOB_CONTAINER"), environment: os.Getenv("NL_RESTORE_ENVIRONMENT_DIGEST")}
	backupNS := blobNamespace{accountURL: os.Getenv("NL_BACKUP_BLOB_SERVICE_URL"), container: os.Getenv("NL_BACKUP_BLOB_CONTAINER"), environment: os.Getenv("NL_BACKUP_ENVIRONMENT_DIGEST")}
	evidenceNS := blobNamespace{accountURL: os.Getenv("NL_EVIDENCE_BLOB_SERVICE_URL"), container: os.Getenv("NL_EVIDENCE_BLOB_CONTAINER"), environment: os.Getenv("NL_EVIDENCE_ENVIRONMENT_DIGEST")}
	transferService, transferStop, err := configureProductionTransfer(storageCtx, productionTransferConfig{
		enabled: transferEnabled, restoreNamespace: restoreNS, backupNamespace: backupNS, evidenceNamespace: evidenceNS,
		authMode: cfg.AuthMode, demoMode: cfg.DemoMode, assuranceEnabled: cfg.AssuranceEnabled, startupFence: startupFence,
		backupConfigured:   backupMaintenance != nil && os.Getenv("NL_BACKUP_MAINTENANCE") == "true",
		evidenceConfigured: os.Getenv(evidenceStorageOptInEnv) == "true",
		assuranceStore:     assuranceStore, audit: auditLog, db: db, provider: workivaBackend, gate: maintenance.gate,
	})
	if err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	stopTransferJanitor = transferStop

	registry := mcpserver.NewRegistry()
	for _, tool := range tools.All() {
		registry.Register(tool)
	}

	graphStore, err := relationships.NewStore(assuranceStore.DB(), assuranceStore.Ready)
	if err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	// Production transfer remains opt-in; enabling it is not release acceptance.
	authOptions.Version = version
	authOptions.DisableLocalhostProtection = cfg.DisableLocalhostProtection
	handler, err := mcpserver.New(mcpserver.Deps{
		Client:        workivaBackend,
		Store:         store,
		Audit:         auditLog,
		Assurance:     assuranceStore,
		Relationships: graphStore,
		Transfer:      transferService,
		DrainGate:     maintenance.gate,
		Cfg:           cfg,
	}, registry, &authOptions)
	if err != nil {
		cleanup()
		return nil, nil, nil, err
	}

	return cfg, &productionHandler{Handler: handler, maintenance: maintenance, backupMaintenance: backupMaintenance}, cleanup, nil
}

// storageContextForConfig supplies startup/bootstrap work with the same
// trusted tenant boundary used by requests. API-key mode deliberately has no
// principal and therefore resolves to the explicit legacy tenant.
func storageContextForConfig(cfg *config.Config) context.Context {
	ctx := context.Background()
	if cfg != nil && cfg.AuthMode == config.AuthModeEntra {
		return identity.ContextWithPrincipal(ctx, identity.Principal{TenantID: cfg.EntraTenantID})
	}
	return ctx
}

type entraVerifierFactory func(context.Context, identity.EntraVerifierConfig, *http.Client) (identity.TokenVerifier, error)

var startupEntraVerifierFactory entraVerifierFactory = func(ctx context.Context, cfg identity.EntraVerifierConfig, client *http.Client) (identity.TokenVerifier, error) {
	return identity.NewDiscoveredEntraVerifier(ctx, cfg, client)
}

func buildAuthOptions(ctx context.Context, cfg *config.Config, apiToken string, factory entraVerifierFactory) (mcpserver.Options, error) {
	mode := cfg.AuthMode
	if mode == "" {
		mode = config.AuthModeAPIKey
	}
	switch mode {
	case config.AuthModeAPIKey:
		return mcpserver.Options{AuthMode: mode, APIToken: apiToken}, nil
	case config.AuthModeEntra:
		if factory == nil {
			return mcpserver.Options{}, errors.New("initialize Entra verifier: verifier factory is unavailable")
		}
		client := &http.Client{Timeout: identity.DiscoveryHTTPTimeout}
		verifier, err := factory(ctx, cfg.EntraVerifierConfig(), client)
		if err != nil {
			return mcpserver.Options{}, fmt.Errorf("initialize Entra verifier: %w", err)
		}
		return mcpserver.Options{AuthMode: mode, TokenVerifier: verifier}, nil
	default:
		return mcpserver.Options{}, fmt.Errorf("unsupported auth mode %q", mode)
	}
}
