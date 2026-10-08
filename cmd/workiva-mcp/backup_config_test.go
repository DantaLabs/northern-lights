package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/transfer"
)

type backupConfigVerifier struct {
	calls int
	err   error
}

func (v *backupConfigVerifier) Verify(context.Context, string) (identity.Principal, error) {
	v.calls++
	return identity.Principal{}, v.err
}

type backupConfigFence struct{}

func (backupConfigFence) CaptureHighWater(context.Context, string) (assurance.BackupFenceHighWater, error) {
	return assurance.BackupFenceHighWater{}, nil
}

type backupConfigBlob struct{}

func (backupConfigBlob) ValidatePrivateContainer(context.Context) error { return nil }
func (backupConfigBlob) UploadEnvelope(context.Context, string, assurance.RestoreOptions) (assurance.BlobBackupSeal, error) {
	return assurance.BlobBackupSeal{}, nil
}

func TestBackupMaintenanceDisabledNeedsNoConfigurationOrDependencies(t *testing.T) {
	unsetBackupConfigEnv(t, "NL_BACKUP_MAINTENANCE")
	handler, err := configureProductionBackupWithFactory(context.Background(), nil, mcpserver.Options{}, nil, nil, nil)
	if err != nil || handler != nil {
		t.Fatalf("disabled backup configuration handler=%v err=%v, want nil,nil", handler, err)
	}
	t.Setenv("NL_BACKUP_MAINTENANCE", "false")
	handler, err = configureProductionBackupWithFactory(context.Background(), nil, mcpserver.Options{}, nil, nil, nil)
	if err != nil || handler != nil {
		t.Fatalf("explicitly disabled backup configuration handler=%v err=%v", handler, err)
	}
}

func TestBackupMaintenanceOptInMustBeExact(t *testing.T) {
	for _, value := range []string{"", "TRUE", "True", "1", "yes", " true", "false "} {
		t.Run(value, func(t *testing.T) {
			if _, err := backupMaintenanceOptIn(value); err == nil {
				t.Fatalf("opt-in %q accepted, want strict validation", value)
			}
		})
	}
}

func TestBackupMaintenanceEnabledRejectsIneligibleServer(t *testing.T) {
	t.Setenv("NL_BACKUP_MAINTENANCE", "true")
	base := &config.Config{AuthMode: config.AuthModeEntra, AssuranceEnabled: true, EntraTenantID: "tenant-a"}
	verifier := &backupConfigVerifier{}
	for _, tc := range []struct {
		name string
		edit func(*config.Config, *mcpserver.Options)
	}{
		{"non-Entra", func(c *config.Config, _ *mcpserver.Options) { c.AuthMode = config.AuthModeAPIKey }},
		{"demo", func(c *config.Config, _ *mcpserver.Options) { c.DemoMode = true }},
		{"assurance-disabled", func(c *config.Config, _ *mcpserver.Options) { c.AssuranceEnabled = false }},
		{"missing-tenant", func(c *config.Config, _ *mcpserver.Options) { c.EntraTenantID = "" }},
		{"malformed-tenant-whitespace", func(c *config.Config, _ *mcpserver.Options) { c.EntraTenantID = " tenant-a " }},
		{"mismatched-auth-options", func(_ *config.Config, o *mcpserver.Options) { o.AuthMode = config.AuthModeAPIKey }},
		{"missing-verifier", func(_ *config.Config, o *mcpserver.Options) { o.TokenVerifier = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := *base
			opts := mcpserver.Options{AuthMode: config.AuthModeEntra, TokenVerifier: verifier}
			tc.edit(&cfg, &opts)
			called := false
			factory := func(context.Context, assurance.AzureBackupConfig, transfer.AzureFenceConfig) (backupFence, backupBlob, error) {
				called = true
				return backupConfigFence{}, backupConfigBlob{}, nil
			}
			if _, err := configureProductionBackupWithFactory(context.Background(), &cfg, opts, nil, nil, factory); err == nil {
				t.Fatal("ineligible server accepted backup maintenance")
			}
			if called {
				t.Fatal("storage dependency factory called before eligibility checks")
			}
		})
	}
}

func TestBackupKeyProvisioningAdmissionRejectsIneligibleConfigBeforeSecretRead(t *testing.T) {
	for _, cfg := range []*config.Config{
		{AuthMode: config.AuthModeAPIKey, AssuranceEnabled: true, EntraTenantID: "tenant-a"},
		{AuthMode: config.AuthModeEntra, AssuranceEnabled: false, EntraTenantID: "tenant-a"},
		{AuthMode: config.AuthModeEntra, AssuranceEnabled: true, DemoMode: true, EntraTenantID: "tenant-a"},
		{AuthMode: config.AuthModeEntra, AssuranceEnabled: true},
	} {
		called := false
		if _, err := provisionRuntimeBackupSigningKey(context.Background(), cfg, testBackupSecretID, func(context.Context, string) ([]byte, error) {
			called = true
			return nil, nil
		}, t.TempDir()); err == nil {
			t.Fatal("ineligible startup configuration admitted secret provisioning")
		}
		if called {
			t.Fatal("Key Vault reader called before startup eligibility check")
		}
	}
}

func TestProvisionedTempParentOverrideIsValidated(t *testing.T) {
	root := t.TempDir()
	keyPath := makeBackupConfigKey(t, filepath.Join(root, "key"), 0o600)
	validTemp := filepath.Join(root, "private")
	if err := os.Mkdir(validTemp, 0o700); err != nil {
		t.Fatal(err)
	}
	setValidBackupConfigEnv(t, keyPath, validTemp)
	t.Setenv("NL_BACKUP_MAINTENANCE", "true")
	unsetBackupConfigEnv(t, backupSigningKeySecretIDEnv)
	link := filepath.Join(root, "link")
	if err := os.Symlink(validTemp, link); err != nil {
		t.Fatal(err)
	}
	db := dummyConfigDB(t)
	t.Cleanup(func() { _ = db.Close() })
	auditLog, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	public := filepath.Join(root, "public")
	if err := os.Mkdir(public, 0o755); err != nil {
		t.Fatal(err)
	}
	maintenance := newMaintenanceCoordinator(db)
	cases := []string{filepath.Join(root, "missing"), link, public}
	configure := func(candidate string) error {
		_, err := configureProductionBackupWithKeyPaths(context.Background(), &config.Config{AuthMode: config.AuthModeEntra, AssuranceEnabled: true, EntraTenantID: "tenant-a"}, mcpserver.Options{AuthMode: config.AuthModeEntra, TokenVerifier: &backupConfigVerifier{}}, maintenance, auditLog, func(context.Context, assurance.AzureBackupConfig, transfer.AzureFenceConfig) (backupFence, backupBlob, error) {
			return backupConfigFence{}, backupConfigBlob{}, nil
		}, keyPath, candidate)
		return err
	}
	if err := configure(validTemp); err != nil {
		t.Fatalf("valid provisioned temp parent rejected: %v", err)
	}
	for _, state := range []struct {
		name    string
		present bool
	}{
		{name: "unset", present: false},
		{name: "present-empty", present: true},
	} {
		t.Run("environment-preserved-"+state.name, func(t *testing.T) {
			setState := func(name string) {
				if state.present {
					t.Setenv(name, "")
				} else {
					unsetBackupConfigEnv(t, name)
				}
			}
			setState("NL_BACKUP_SIGNING_KEY_FILE")
			setState("NL_BACKUP_TEMP_PARENT")
			type envState struct {
				value   string
				present bool
			}
			beforeKey, keyPresent := os.LookupEnv("NL_BACKUP_SIGNING_KEY_FILE")
			beforeParent, parentPresent := os.LookupEnv("NL_BACKUP_TEMP_PARENT")
			before := [2]envState{{value: beforeKey, present: keyPresent}, {value: beforeParent, present: parentPresent}}
			if err := configure(validTemp); err != nil {
				t.Fatalf("valid provisioned paths rejected: %v", err)
			}
			afterKey, afterKeyPresent := os.LookupEnv("NL_BACKUP_SIGNING_KEY_FILE")
			afterParent, afterParentPresent := os.LookupEnv("NL_BACKUP_TEMP_PARENT")
			after := [2]envState{{value: afterKey, present: afterKeyPresent}, {value: afterParent, present: afterParentPresent}}
			if before != after {
				t.Fatalf("key-path environment mutated: before=%+v after=%+v", before, after)
			}
		})
	}
	for _, candidate := range cases {
		if err := configure(candidate); err == nil {
			t.Fatalf("provisioned temp parent %q accepted", candidate)
		}
	}
}

func TestBackupMaintenanceEnabledRejectsInvalidStorageAndKeyConfiguration(t *testing.T) {
	t.Setenv("NL_BACKUP_MAINTENANCE", "true")
	root := t.TempDir()
	key := makeBackupConfigKey(t, filepath.Join(root, "key"), 0o600)
	privateDir := filepath.Join(root, "private")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	setValidBackupConfigEnv(t, key, privateDir)
	cfg := &config.Config{AuthMode: config.AuthModeEntra, AssuranceEnabled: true, EntraTenantID: "tenant-a"}
	verifier := &backupConfigVerifier{}
	for _, tc := range []struct{ name, env, value string }{
		{"service-url-missing", "NL_BACKUP_BLOB_SERVICE_URL", ""},
		{"service-url-path", "NL_BACKUP_BLOB_SERVICE_URL", "https://acct.blob.core.windows.net/container"},
		{"service-url-sas", "NL_BACKUP_BLOB_SERVICE_URL", "https://acct.blob.core.windows.net?sig=secret"},
		{"service-url-empty-query", "NL_BACKUP_BLOB_SERVICE_URL", "https://acct.blob.core.windows.net?"},
		{"service-url-http", "NL_BACKUP_BLOB_SERVICE_URL", "http://acct.blob.core.windows.net"},
		{"service-url-non-azure", "NL_BACKUP_BLOB_SERVICE_URL", "https://storage.example.com"},
		{"service-url-invalid-account", "NL_BACKUP_BLOB_SERVICE_URL", "https://ab.blob.core.windows.net"},
		{"service-url-invalid-account-character", "NL_BACKUP_BLOB_SERVICE_URL", "https://bad_name.blob.core.windows.net"},
		{"container-invalid", "NL_BACKUP_BLOB_CONTAINER", "Bad--Name"},
		{"container-missing", "NL_BACKUP_BLOB_CONTAINER", ""},
		{"environment-invalid", "NL_BACKUP_ENVIRONMENT_DIGEST", "env/a"},
		{"environment-missing", "NL_BACKUP_ENVIRONMENT_DIGEST", ""},
		{"key-path-relative", "NL_BACKUP_SIGNING_KEY_FILE", "key"},
		{"key-path-missing", "NL_BACKUP_SIGNING_KEY_FILE", ""},
		{"temp-parent-missing", "NL_BACKUP_TEMP_PARENT", filepath.Join(root, "absent")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setValidBackupConfigEnv(t, key, privateDir)
			t.Setenv(tc.env, tc.value)
			called := false
			factory := func(context.Context, assurance.AzureBackupConfig, transfer.AzureFenceConfig) (backupFence, backupBlob, error) {
				called = true
				return backupConfigFence{}, backupConfigBlob{}, nil
			}
			db := dummyConfigDB(t)
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Errorf("close test database: %v", err)
				}
			})
			auditLog, err := audit.NewWithDB(db)
			if err != nil {
				t.Fatal(err)
			}
			maintenance := newMaintenanceCoordinator(db)
			_, err = configureProductionBackupWithFactory(context.Background(), cfg, mcpserver.Options{TokenVerifier: verifier}, maintenance, auditLog, factory)
			if err == nil {
				t.Fatal("invalid backup configuration accepted")
			}
			if called {
				t.Fatal("dependency factory called with invalid backup configuration")
			}
		})
	}
}

func TestBackupSigningKeyRequiresPrivateFileAndValidEd25519Bytes(t *testing.T) {
	dir := t.TempDir()
	valid := makeBackupConfigKey(t, filepath.Join(dir, "valid"), 0o400)
	if parsed, err := loadBackupSigningKey(valid); err != nil || len(parsed) != ed25519.PrivateKeySize {
		t.Fatalf("valid private key load len=%d err=%v", len(parsed), err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	inconsistent := append([]byte(nil), private...)
	inconsistent[ed25519.SeedSize] ^= 1
	badBytes := filepath.Join(dir, "bad-bytes")
	if err := os.WriteFile(badBytes, []byte(strings.ToLower(strings.TrimSpace(hexEncode(inconsistent)))), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBackupSigningKey(badBytes); err == nil {
		t.Fatal("inconsistent Ed25519 private bytes accepted")
	}
	badMode := filepath.Join(dir, "bad-mode")
	if err := os.WriteFile(badMode, []byte(hexEncode(private)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBackupSigningKey(badMode); err == nil {
		t.Fatal("world-readable key accepted")
	}
	symlink := filepath.Join(dir, "key-link")
	if err := os.Symlink(valid, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBackupSigningKey(symlink); err == nil {
		t.Fatal("symlinked key accepted")
	}
	malformed := filepath.Join(dir, "malformed")
	if err := os.WriteFile(malformed, []byte(strings.Repeat("z", ed25519.PrivateKeySize*2)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBackupSigningKey(malformed); err == nil {
		t.Fatal("malformed hex key accepted")
	}
	oversized := filepath.Join(dir, "oversized")
	if err := os.WriteFile(oversized, []byte(strings.Repeat("0", backupMaxKeyFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBackupSigningKey(oversized); err == nil {
		t.Fatal("oversized key file accepted")
	}
	parentLink := filepath.Join(dir, "linked-parent")
	if err := os.Symlink(dir, parentLink); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBackupSigningKey(filepath.Join(parentLink, "valid")); err == nil {
		t.Fatal("key below a symlinked parent accepted")
	}
	if !equalBytes(public, private.Public().(ed25519.PublicKey)) {
		t.Fatal("test keypair malformed")
	}
}

func TestBackupTempParentRejectsSymlinkAndNonPrivateDirectories(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateBackupTempParent(private); err != nil {
		t.Fatalf("private parent rejected: %v", err)
	}
	public := filepath.Join(root, "public")
	if err := os.Mkdir(public, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(public, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateBackupTempParent(public); err == nil {
		t.Fatal("group/world-accessible temporary parent accepted")
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateBackupTempParent(link); err == nil {
		t.Fatal("symlinked temporary parent accepted")
	}
}

func TestBackupMaintenanceEnabledInstallsEndpointWithTrustedDependencies(t *testing.T) {
	root := t.TempDir()
	keyPath := makeBackupConfigKey(t, filepath.Join(root, "backup.key"), 0o600)
	tempParent := filepath.Join(root, "staging")
	if err := os.Mkdir(tempParent, 0o700); err != nil {
		t.Fatal(err)
	}
	setValidBackupConfigEnv(t, keyPath, tempParent)
	t.Setenv("NL_BACKUP_MAINTENANCE", "true")
	db := dummyConfigDB(t)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	auditLog, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	verifier := &backupConfigVerifier{err: errors.New("test token rejected")}
	cfg := &config.Config{AuthMode: config.AuthModeEntra, AssuranceEnabled: true, EntraTenantID: "tenant-a"}
	maintenance := newMaintenanceCoordinator(db)
	var gotBlob assurance.AzureBackupConfig
	var gotFence transfer.AzureFenceConfig
	factory := func(_ context.Context, blob assurance.AzureBackupConfig, fence transfer.AzureFenceConfig) (backupFence, backupBlob, error) {
		gotBlob, gotFence = blob, fence
		return backupConfigFence{}, backupConfigBlob{}, nil
	}
	handler, err := configureProductionBackupWithFactory(context.Background(), cfg, mcpserver.Options{AuthMode: config.AuthModeEntra, TokenVerifier: verifier}, maintenance, auditLog, factory)
	if err != nil || handler == nil {
		t.Fatalf("configure enabled backup handler=%v err=%v", handler, err)
	}
	operation := maintenance.operation
	if operation == nil || operation.db != db || operation.config.TenantID != cfg.EntraTenantID || operation.config.EnvironmentDigest != "prod-a" || operation.config.TempParent != tempParent || operation.config.Audit != auditLog || operation.config.Fence == nil || operation.config.Blob == nil {
		t.Fatalf("production backup operation dependencies/config not installed: %+v", operation)
	}
	if len(operation.config.SigningKey.Ed25519Private) != ed25519.PrivateKeySize || !equalBytes(operation.config.SigningKey.Ed25519Private, operation.config.CheckpointKey) {
		t.Fatal("backup and checkpoint signing keys were not loaded consistently")
	}
	if gotBlob.ServiceURL != "https://acct.blob.core.windows.net" || gotBlob.ContainerName != "private-backups" || gotBlob.TenantID != "tenant-a" || gotBlob.EnvironmentDigest != "prod-a" || gotFence.ServiceURL != gotBlob.ServiceURL || gotFence.ContainerName != gotBlob.ContainerName || gotFence.TenantID != gotBlob.TenantID || gotFence.EnvironmentDigest != gotBlob.EnvironmentDigest {
		t.Fatalf("storage adapters received unexpected tenant/environment binding: blob=%+v fence=%+v", gotBlob, gotFence)
	}
	production := &productionHandler{Handler: http.NotFoundHandler(), maintenance: maintenance, backupMaintenance: handler}
	req := httptest.NewRequest(http.MethodPost, "/maintenance/backup", nil)
	req.Header.Set("Authorization", "Bearer rejected")
	rec := httptest.NewRecorder()
	production.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || verifier.calls != 1 {
		t.Fatalf("installed endpoint status=%d verifier calls=%d", rec.Code, verifier.calls)
	}
	if got := identity.StorageTenant(storageContextForConfig(cfg)); got != cfg.EntraTenantID {
		t.Fatalf("backup startup tenant=%q, want trusted configured tenant %q", got, cfg.EntraTenantID)
	}
}

func TestBuildServerBackupMaintenanceDisabledAndInvalidOptIn(t *testing.T) {
	oldReader := productionBackupKeySecretReader
	readerCalls := 0
	productionBackupKeySecretReader = func(context.Context, string) ([]byte, error) {
		readerCalls++
		return nil, errors.New("unexpected Key Vault access")
	}
	t.Cleanup(func() { productionBackupKeySecretReader = oldReader })
	setupDemoEnvironment := func(t *testing.T, dbPath string) {
		t.Helper()
		t.Setenv("NL_DEMO_MODE", "true")
		t.Setenv("NL_AUTH_MODE", string(config.AuthModeAPIKey))
		t.Setenv("NL_API_KEY", "test-only-demo-key")
		t.Setenv("NL_DB_PATH", dbPath)
		t.Setenv("NL_LISTEN_ADDR", "127.0.0.1:0")
		t.Setenv("NL_STARTUP_RESTORE_ADMISSION", "")
	}
	t.Run("unset disables endpoint", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "disabled.db")
		setupDemoEnvironment(t, dbPath)
		t.Setenv(backupSigningKeySecretIDEnv, testBackupSecretID)
		unsetBackupConfigEnv(t, "NL_BACKUP_MAINTENANCE")
		_, handler, cleanup, err := buildServer("", "")
		if err != nil {
			t.Fatalf("buildServer with opt-in unset: %v", err)
		}
		defer cleanup()
		production, ok := handler.(*productionHandler)
		if !ok || production.backupMaintenance != nil {
			t.Fatalf("disabled server handler=%T backup endpoint=%v", handler, ok && production.backupMaintenance != nil)
		}
		if readerCalls != 0 {
			t.Fatalf("disabled server made %d Key Vault reads", readerCalls)
		}
	})
	t.Run("explicit false disables secret provisioning", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "disabled-false.db")
		setupDemoEnvironment(t, dbPath)
		t.Setenv(backupSigningKeySecretIDEnv, testBackupSecretID)
		t.Setenv("NL_BACKUP_MAINTENANCE", "false")
		_, _, cleanup, err := buildServer("", "")
		if err != nil {
			t.Fatalf("buildServer with opt-in false: %v", err)
		}
		cleanup()
		if readerCalls != 0 {
			t.Fatalf("explicitly disabled server made %d Key Vault reads", readerCalls)
		}
	})
	t.Run("empty legacy variable conflicts before secret provisioning", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "conflict-empty.db")
		setupDemoEnvironment(t, dbPath)
		t.Setenv("NL_BACKUP_MAINTENANCE", "true")
		t.Setenv(backupSigningKeySecretIDEnv, "")
		t.Setenv("NL_BACKUP_SIGNING_KEY_FILE", "")
		_, handler, cleanup, err := buildServer("", "")
		if err == nil || handler != nil || cleanup != nil || !strings.Contains(err.Error(), "cannot be combined") {
			t.Fatalf("empty legacy conflict handler=%v cleanup=%v err=%v", handler, cleanup != nil, err)
		}
		if readerCalls != 0 {
			t.Fatalf("conflicting config made %d Key Vault reads", readerCalls)
		}
	})
	t.Run("ineligible config rejected before secret provisioning", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "ineligible.db")
		setupDemoEnvironment(t, dbPath)
		t.Setenv("NL_BACKUP_MAINTENANCE", "true")
		t.Setenv(backupSigningKeySecretIDEnv, testBackupSecretID)
		_, handler, cleanup, err := buildServer("", "")
		if err == nil || handler != nil || cleanup != nil || !strings.Contains(err.Error(), "non-demo Entra assurance") {
			t.Fatalf("ineligible server handler=%v cleanup=%v err=%v", handler, cleanup != nil, err)
		}
		if readerCalls != 0 {
			t.Fatalf("ineligible server made %d Key Vault reads", readerCalls)
		}
	})
	t.Run("invalid opt-in fails closed", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "invalid-opt-in.db")
		setupDemoEnvironment(t, dbPath)
		t.Setenv("NL_BACKUP_MAINTENANCE", "enabled")
		_, handler, cleanup, err := buildServer("", "")
		if err == nil || handler != nil || cleanup != nil || !strings.Contains(err.Error(), "NL_BACKUP_MAINTENANCE") {
			t.Fatalf("buildServer invalid opt-in handler=%v cleanup=%v err=%v", handler, cleanup != nil, err)
		}
	})
}

func setValidBackupConfigEnv(t *testing.T, keyPath, tempParent string) {
	t.Helper()
	t.Setenv("NL_BACKUP_BLOB_SERVICE_URL", "https://acct.blob.core.windows.net")
	t.Setenv("NL_BACKUP_BLOB_CONTAINER", "private-backups")
	t.Setenv("NL_BACKUP_ENVIRONMENT_DIGEST", "prod-a")
	t.Setenv("NL_BACKUP_SIGNING_KEY_FILE", keyPath)
	t.Setenv("NL_BACKUP_TEMP_PARENT", tempParent)
}

func unsetBackupConfigEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "temporary-value")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

func makeBackupConfigKey(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(hexEncode(key)), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func hexEncode(data []byte) string {
	const digits = "0123456789abcdef"
	encoded := make([]byte, len(data)*2)
	for i, b := range data {
		encoded[i*2], encoded[i*2+1] = digits[b>>4], digits[b&15]
	}
	return string(encoded)
}

func dummyConfigDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	return db
}
