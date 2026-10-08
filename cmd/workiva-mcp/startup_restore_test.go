package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/transfer"
)

func TestBuildServerOptInFailureNeverOpensApplicationDatabase(t *testing.T) {
	t.Setenv("NL_STARTUP_RESTORE_ADMISSION", "true")
	t.Setenv("NL_AUTH_MODE", "api_key")
	t.Setenv("NL_ASSURANCE_ENABLED", "false")
	path := filepath.Join(t.TempDir(), "must-remain-absent.db")
	t.Setenv("NL_DB_PATH", path)
	_, handler, cleanup, err := buildServer("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/readyz", 503}, {http.MethodPost, "/mcp", 503},
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(tc.method, tc.path, nil))
		if res.Code != tc.want {
			t.Fatalf("%s %s = %d", tc.method, tc.path, res.Code)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("application DB opened on failed admission: %v", err)
	}
}

func TestStartupRestoreAdmissionRequiresExplicitTrustAndKeepsPOSTClosed(t *testing.T) {
	cfg := &config.Config{AssuranceEnabled: true, AuthMode: config.AuthModeEntra, EntraTenantID: "tenant-a", DBPath: filepath.Join(t.TempDir(), "empty.db")}
	t.Setenv("NL_STARTUP_RESTORE_ADMISSION", "true")
	for _, field := range []string{"NL_RESTORE_BLOB_SERVICE_URL", "NL_RESTORE_BLOB_CONTAINER", "NL_RESTORE_ENVIRONMENT_DIGEST", "NL_RESTORE_ENVELOPE_ID", "NL_RESTORE_DATABASE_SHA256", "NL_RESTORE_MIN_AUDIT_SEQUENCE", "NL_RESTORE_ED25519_PUBLIC_KEY_HEX", "NL_RESTORE_CHECKPOINT_PUBLIC_KEY_HEX", "NL_RESTORE_CHECKPOINT_ID"} {
		t.Setenv(field, "")
	}
	if _, err := admitStartupRestore(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "SERVICE_URL") {
		t.Fatalf("missing URL: %v", err)
	}
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/healthz", 200}, {http.MethodGet, "/readyz", 503}, {http.MethodPost, "/mcp", 503},
	} {
		res := httptest.NewRecorder()
		blockedStartup().ServeHTTP(res, httptest.NewRequest(tc.method, tc.path, nil))
		if res.Code != tc.want {
			t.Fatalf("%s %s = %d", tc.method, tc.path, res.Code)
		}
	}
	cfg.AuthMode = config.AuthModeAPIKey
	if _, err := admitStartupRestore(context.Background(), cfg); err == nil {
		t.Fatal("API key must never admit restore")
	}
}

func TestStartupRestoreRequiresIndependentCheckpointTrustBeforeConstructingAdapters(t *testing.T) {
	oldBackup, oldFence := startupBackupTransport, startupFenceInventory
	t.Cleanup(func() { startupBackupTransport, startupFenceInventory = oldBackup, oldFence })
	cfg := &config.Config{AssuranceEnabled: true, AuthMode: config.AuthModeEntra, EntraTenantID: "tenant-a", DBPath: filepath.Join(t.TempDir(), "empty.db")}
	t.Setenv("NL_STARTUP_RESTORE_ADMISSION", "true")
	t.Setenv("NL_RESTORE_BLOB_SERVICE_URL", "https://example.blob.core.windows.net")
	t.Setenv("NL_RESTORE_BLOB_CONTAINER", "private")
	t.Setenv("NL_RESTORE_ENVIRONMENT_DIGEST", "env-a")
	t.Setenv("NL_RESTORE_ENVELOPE_ID", strings.Repeat("a", 32))
	t.Setenv("NL_RESTORE_DATABASE_SHA256", strings.Repeat("b", 64))
	t.Setenv("NL_RESTORE_MIN_AUDIT_SEQUENCE", "7")
	t.Setenv("NL_RESTORE_ED25519_PUBLIC_KEY_HEX", strings.Repeat("c", 64))
	t.Setenv("NL_RESTORE_HMAC_KEY_HEX", "")
	t.Setenv("NL_RESTORE_MIN_CREATED_AT", "")
	var constructions int
	startupBackupTransport = func(context.Context, assurance.AzureBackupConfig) (*assurance.AzureBackupTransport, error) {
		constructions++
		return &assurance.AzureBackupTransport{}, nil
	}
	startupFenceInventory = func(context.Context, transfer.AzureFenceConfig) (*transfer.AzureBlobFence, error) {
		constructions++
		return &transfer.AzureBlobFence{}, nil
	}
	for _, tc := range []struct{ name, key, id, minimumCreatedAt string }{
		{"missing-checkpoint-key", "", "audit-terminal", ""},
		{"malformed-checkpoint-key", "xyz", "audit-terminal", ""},
		{"uppercase-checkpoint-key", strings.ToUpper(strings.Repeat("c", 64)), "audit-terminal", ""},
		{"missing-checkpoint-id", strings.Repeat("c", 64), "", ""},
		{"checkpoint-id-path", strings.Repeat("c", 64), "../audit-terminal", ""},
		{"checkpoint-id-whitespace", strings.Repeat("c", 64), " audit-terminal", ""},
		{"checkpoint-id-invalid-uuid", strings.Repeat("c", 64), "not-a-checkpoint", ""},
		{"invalid-minimum-created-at", strings.Repeat("c", 64), "audit-terminal", "2026-10-07"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NL_RESTORE_CHECKPOINT_PUBLIC_KEY_HEX", tc.key)
			t.Setenv("NL_RESTORE_CHECKPOINT_ID", tc.id)
			t.Setenv("NL_RESTORE_MIN_CREATED_AT", tc.minimumCreatedAt)
			before := constructions
			if _, err := admitStartupRestore(context.Background(), cfg); err == nil {
				t.Fatal("missing or malformed independent checkpoint trust accepted")
			}
			if constructions != before {
				t.Fatalf("constructed storage adapters before checkpoint trust validation: before=%d after=%d", before, constructions)
			}
		})
	}
}

func TestStartupRestoreRejectsUnsafeStorageSelectionBeforeAdapters(t *testing.T) {
	oldBackup, oldFence := startupBackupTransport, startupFenceInventory
	t.Cleanup(func() { startupBackupTransport, startupFenceInventory = oldBackup, oldFence })
	cfg := &config.Config{AssuranceEnabled: true, AuthMode: config.AuthModeEntra, EntraTenantID: "tenant-a", DBPath: filepath.Join(t.TempDir(), "empty.db")}
	for key, value := range map[string]string{
		"NL_STARTUP_RESTORE_ADMISSION":         "true",
		"NL_RESTORE_BLOB_SERVICE_URL":          "https://account.blob.core.windows.net",
		"NL_RESTORE_BLOB_CONTAINER":            "private",
		"NL_RESTORE_ENVIRONMENT_DIGEST":        "env-a",
		"NL_RESTORE_ENVELOPE_ID":               strings.Repeat("a", 32),
		"NL_RESTORE_DATABASE_SHA256":           strings.Repeat("b", 64),
		"NL_RESTORE_MIN_AUDIT_SEQUENCE":        "1",
		"NL_RESTORE_ED25519_PUBLIC_KEY_HEX":    strings.Repeat("c", 64),
		"NL_RESTORE_HMAC_KEY_HEX":              "",
		"NL_RESTORE_CHECKPOINT_PUBLIC_KEY_HEX": strings.Repeat("c", 64),
		"NL_RESTORE_CHECKPOINT_ID":             "audit-terminal",
	} {
		t.Setenv(key, value)
	}
	constructions := 0
	startupBackupTransport = func(context.Context, assurance.AzureBackupConfig) (*assurance.AzureBackupTransport, error) {
		constructions++
		return &assurance.AzureBackupTransport{}, nil
	}
	startupFenceInventory = func(context.Context, transfer.AzureFenceConfig) (*transfer.AzureBlobFence, error) {
		constructions++
		return &transfer.AzureBlobFence{}, nil
	}
	for _, tc := range []struct{ name, env, value string }{
		{"http", "NL_RESTORE_BLOB_SERVICE_URL", "http://account.blob.core.windows.net"},
		{"path", "NL_RESTORE_BLOB_SERVICE_URL", "https://account.blob.core.windows.net/container"},
		{"credentials", "NL_RESTORE_BLOB_SERVICE_URL", "https://user:secret@account.blob.core.windows.net"},
		{"sas", "NL_RESTORE_BLOB_SERVICE_URL", "https://account.blob.core.windows.net?sig=secret"},
		{"empty-query-delimiter", "NL_RESTORE_BLOB_SERVICE_URL", "https://account.blob.core.windows.net?"},
		{"fragment", "NL_RESTORE_BLOB_SERVICE_URL", "https://account.blob.core.windows.net#fragment"},
		{"container-path-syntax", "NL_RESTORE_BLOB_CONTAINER", "private/other"},
		{"environment-path-syntax", "NL_RESTORE_ENVIRONMENT_DIGEST", "env/other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NL_RESTORE_BLOB_SERVICE_URL", "https://account.blob.core.windows.net")
			t.Setenv("NL_RESTORE_BLOB_CONTAINER", "private")
			t.Setenv("NL_RESTORE_ENVIRONMENT_DIGEST", "env-a")
			t.Setenv(tc.env, tc.value)
			before := constructions
			if _, err := admitStartupRestore(context.Background(), cfg); err == nil {
				t.Fatal("unsafe storage selection accepted")
			}
			if constructions != before {
				t.Fatalf("constructed storage adapters before selection validation: %d->%d", before, constructions)
			}
		})
	}
}

func TestStartupRestoreAdmissionDoesNotConstructServiceBeforeJoin(t *testing.T) {
	oldBackup, oldFence, oldAdmission, oldPrivateContainer := startupBackupTransport, startupFenceInventory, startupRestoreAdmission, startupPrivateContainerAdmission
	t.Cleanup(func() {
		startupBackupTransport, startupFenceInventory, startupRestoreAdmission, startupPrivateContainerAdmission = oldBackup, oldFence, oldAdmission, oldPrivateContainer
	})
	cfg := &config.Config{AssuranceEnabled: true, AuthMode: config.AuthModeEntra, EntraTenantID: "tenant-a", DBPath: filepath.Join(t.TempDir(), "empty.db")}
	t.Setenv("NL_STARTUP_RESTORE_ADMISSION", "true")
	t.Setenv("NL_RESTORE_BLOB_SERVICE_URL", "https://example.blob.core.windows.net")
	t.Setenv("NL_RESTORE_BLOB_CONTAINER", "private")
	t.Setenv("NL_RESTORE_ENVIRONMENT_DIGEST", "env-a")
	t.Setenv("NL_RESTORE_ENVELOPE_ID", strings.Repeat("a", 32))
	t.Setenv("NL_RESTORE_DATABASE_SHA256", strings.Repeat("b", 64))
	t.Setenv("NL_RESTORE_MIN_AUDIT_SEQUENCE", "7")
	t.Setenv("NL_RESTORE_ED25519_PUBLIC_KEY_HEX", strings.Repeat("c", 64))
	t.Setenv("NL_RESTORE_HMAC_KEY_HEX", "")
	t.Setenv("NL_RESTORE_CHECKPOINT_PUBLIC_KEY_HEX", strings.Repeat("c", 64))
	t.Setenv("NL_RESTORE_CHECKPOINT_ID", "audit-terminal")
	t.Setenv("NL_RESTORE_MIN_CREATED_AT", "2026-10-07T04:05:06Z")
	startupBackupTransport = func(_ context.Context, c assurance.AzureBackupConfig) (*assurance.AzureBackupTransport, error) {
		if c.TenantID != cfg.EntraTenantID || c.EnvironmentDigest != "env-a" {
			t.Fatal("backup binding")
		}
		return &assurance.AzureBackupTransport{}, nil
	}
	startupFenceInventory = func(_ context.Context, c transfer.AzureFenceConfig) (*transfer.AzureBlobFence, error) {
		if c.TenantID != cfg.EntraTenantID || c.EnvironmentDigest != "env-a" {
			t.Fatal("fence binding")
		}
		return &transfer.AzureBlobFence{}, nil
	}
	startupPrivateContainerAdmission = func(context.Context, *assurance.AzureBackupTransport) error { return nil }
	calls := 0
	startupRestoreAdmission = func(_ context.Context, _ *assurance.AzureBackupTransport, id, dest string, o assurance.RestoreOptions, _ transfer.FenceInventory, env string) (assurance.RestoreResult, transfer.RestoreScanResult, error) {
		calls++
		if id != strings.Repeat("a", 32) || dest != cfg.DBPath || env != "env-a" || o.ExpectedTenantID != cfg.EntraTenantID || !o.RequireFenceHighWater || o.MinimumAuditSequence != 7 || o.ExpectedDatabaseSHA256 != strings.Repeat("b", 64) || len(o.SigningKey.Ed25519Public) != 32 || hex.EncodeToString(o.CheckpointPublicKey) != strings.Repeat("c", 64) || o.CheckpointID != "audit-terminal" || !o.MinimumCreatedAt.Equal(time.Date(2026, 10, 7, 4, 5, 6, 0, time.UTC)) {
			t.Fatal("selection not bound")
		}
		return assurance.RestoreResult{Ready: true}, transfer.RestoreScanResult{Ready: false, Findings: 1, Quarantined: 1}, nil
	}
	if fence, err := admitStartupRestore(context.Background(), cfg); err == nil || fence != nil {
		t.Fatalf("contradiction admitted: fence=%v err=%v", fence, err)
	}
	if calls != 1 {
		t.Fatalf("scan calls=%d", calls)
	}
	t.Setenv("NL_RESTORE_DATABASE_SHA256", strings.Repeat("d", 64))
	t.Setenv("NL_RESTORE_MIN_AUDIT_SEQUENCE", "0")
	if _, err := admitStartupRestore(context.Background(), cfg); err == nil {
		t.Fatal("stale selection admitted")
	}
	if calls != 1 {
		t.Fatal("stale selection reached Blob")
	}
	t.Setenv("NL_RESTORE_MIN_AUDIT_SEQUENCE", "7")
	startupPrivateContainerAdmission = nil
	if _, err := admitStartupRestore(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "private container admission unavailable") {
		t.Fatalf("nil private-container verifier did not fail closed: %v", err)
	}
	if calls != 1 {
		t.Fatal("nil private-container verifier reached restore admission")
	}
	startupPrivateContainerAdmission = func(context.Context, *assurance.AzureBackupTransport) error { return nil }
	startupRestoreAdmission = func(_ context.Context, _ *assurance.AzureBackupTransport, _, _ string, _ assurance.RestoreOptions, _ transfer.FenceInventory, _ string) (assurance.RestoreResult, transfer.RestoreScanResult, error) {
		return assurance.RestoreResult{Ready: true}, transfer.RestoreScanResult{Ready: true}, nil
	}
	fence, err := admitStartupRestore(context.Background(), cfg)
	if err != nil || fence == nil {
		t.Fatalf("verified join: %v", err)
	}
}

type startupAdmissionVerifier struct{ tenant string }

func (v startupAdmissionVerifier) Verify(_ context.Context, token string) (identity.Principal, error) {
	if token != "verified-startup-token" {
		return identity.Principal{}, errors.New("invalid test token")
	}
	return identity.Principal{TenantID: v.tenant, ObjectID: "operator-1", Permissions: []identity.Permission{identity.PermissionWorkivaWritePreview}}, nil
}

func TestBuildServerCleansProvisionedKeyWhenStartupRestoreBlocks(t *testing.T) {
	const tenant = "11111111-1111-1111-1111-111111111111"
	oldBackup, oldReader := startupBackupTransport, productionBackupKeySecretReader
	var startupOrder []string
	t.Cleanup(func() {
		startupBackupTransport = oldBackup
		productionBackupKeySecretReader = oldReader
	})
	startupBackupTransport = func(context.Context, assurance.AzureBackupConfig) (*assurance.AzureBackupTransport, error) {
		startupOrder = append(startupOrder, "restore")
		return nil, errors.New("injected restore transport failure")
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	productionBackupKeySecretReader = func(context.Context, string) ([]byte, error) {
		startupOrder = append(startupOrder, "key")
		return []byte(hex.EncodeToString(private)), nil
	}
	entriesBefore, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := make(map[string]bool)
	for _, entry := range entriesBefore {
		before[entry.Name()] = true
	}
	bundlePublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"NL_STARTUP_RESTORE_ADMISSION":         "true",
		"NL_AUTH_MODE":                         "entra",
		"NL_ENTRA_TENANT_ID":                   tenant,
		"NL_ENTRA_AUTHORITY":                   "https://login.microsoftonline.com/" + tenant + "/v2.0",
		"NL_ENTRA_AUDIENCE":                    "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"NL_ENTRA_SCOPE_PERMISSIONS":           `{"transfer.preview":["workiva.write.preview"]}`,
		"NL_ASSURANCE_ENABLED":                 "true",
		"NL_ASSURANCE_BUNDLE_PUBLIC_KEY":       hex.EncodeToString(bundlePublic),
		"NL_DB_PATH":                           filepath.Join(t.TempDir(), "not-opened.db"),
		"NL_WORKIVA_CLIENT_ID":                 "test-client",
		"NL_WORKIVA_CLIENT_SECRET":             "test-secret",
		"NL_RESTORE_BLOB_SERVICE_URL":          "https://example.blob.core.windows.net",
		"NL_RESTORE_BLOB_CONTAINER":            "private",
		"NL_RESTORE_ENVIRONMENT_DIGEST":        "env-a",
		"NL_RESTORE_ENVELOPE_ID":               strings.Repeat("a", 32),
		"NL_RESTORE_DATABASE_SHA256":           strings.Repeat("b", 64),
		"NL_RESTORE_MIN_AUDIT_SEQUENCE":        "1",
		"NL_RESTORE_ED25519_PUBLIC_KEY_HEX":    strings.Repeat("c", 64),
		"NL_RESTORE_HMAC_KEY_HEX":              "",
		"NL_RESTORE_CHECKPOINT_PUBLIC_KEY_HEX": hex.EncodeToString(bundlePublic),
		"NL_RESTORE_CHECKPOINT_ID":             "audit-terminal",
		"NL_BACKUP_MAINTENANCE":                "true",
		backupSigningKeySecretIDEnv:            testBackupSecretID,
	} {
		t.Setenv(key, value)
	}
	unsetBackupConfigEnv(t, "NL_BACKUP_SIGNING_KEY_FILE")
	unsetBackupConfigEnv(t, "NL_BACKUP_TEMP_PARENT")
	cfg, handler, cleanup, err := buildServer("", "")
	if err != nil || cfg == nil || handler == nil || cleanup == nil {
		t.Fatalf("blocked restore server cfg=%v handler=%v cleanup=%v err=%v", cfg != nil, handler != nil, cleanup != nil, err)
	}
	if len(startupOrder) != 2 || startupOrder[0] != "key" || startupOrder[1] != "restore" {
		t.Fatalf("startup sequence=%v, want key provisioning before restore admission", startupOrder)
	}
	entriesAfter, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entriesAfter {
		if strings.HasPrefix(entry.Name(), "northern-lights-key-") && !before[entry.Name()] {
			t.Errorf("provisioned key directory survived blocked startup: %s", entry.Name())
		}
	}
}

func TestBuildServerCleanRestoreAdmissionKeepsTransferUnavailableOverRawMCP(t *testing.T) {
	const tenant = "11111111-1111-1111-1111-111111111111"
	oldBackup, oldFence, oldAdmission, oldVerifier, oldPrivateContainer := startupBackupTransport, startupFenceInventory, startupRestoreAdmission, startupEntraVerifierFactory, startupPrivateContainerAdmission
	t.Cleanup(func() {
		startupBackupTransport, startupFenceInventory, startupRestoreAdmission, startupEntraVerifierFactory, startupPrivateContainerAdmission = oldBackup, oldFence, oldAdmission, oldVerifier, oldPrivateContainer
	})
	startupBackupTransport = func(context.Context, assurance.AzureBackupConfig) (*assurance.AzureBackupTransport, error) {
		return &assurance.AzureBackupTransport{}, nil
	}
	startupFenceInventory = func(context.Context, transfer.AzureFenceConfig) (*transfer.AzureBlobFence, error) {
		return &transfer.AzureBlobFence{}, nil
	}
	startupPrivateContainerAdmission = func(context.Context, *assurance.AzureBackupTransport) error { return nil }
	startupEntraVerifierFactory = func(_ context.Context, cfg identity.EntraVerifierConfig, _ *http.Client) (identity.TokenVerifier, error) {
		return startupAdmissionVerifier{tenant: cfg.TenantID}, nil
	}

	dbPath := filepath.Join(t.TempDir(), "admitted.db")
	sourcePath := filepath.Join(t.TempDir(), "restore-source.db")
	bundlePublic, bundlePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sourceDB, err := sqlitedb.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	assuranceStore, err := assurance.NewWithDB(sourceDB)
	if err != nil {
		_ = sourceDB.Close()
		t.Fatal(err)
	}
	bundle := assurance.Bundle{SchemaVersion: 1, BundleID: "startup-admission-fixture", BundleVersion: 1, TenantID: tenant,
		Reports: []assurance.ReportRevision{{ReportID: "startup-report", Revision: 1, Name: "Startup report", Owner: "fixture", Status: "active", RetentionClass: "standard", ResourcePolicyHash: strings.Repeat("a", 64), Periods: []assurance.Period{{Key: "2026-Q3", Label: "Q3", Start: "2026-07-01", End: "2026-09-30"}}, Fields: []assurance.FieldDefinition{{FieldID: "startup-field", ResourceID: "startup-resource", ExternalResourceID: "startup-external", SubresourceID: "startup-sheet", Locator: "Sheet 1!B2", Kind: assurance.ValueNumber, Required: true, Order: 1}}}},
	}
	rawBundle, err := assurance.CanonicalJSON(bundle)
	if err != nil {
		_ = sourceDB.Close()
		t.Fatal(err)
	}
	validatedBundle, err := assurance.ValidateBundle(rawBundle, ed25519.Sign(bundlePrivate, rawBundle), tenant, bundlePublic)
	if err != nil {
		_ = sourceDB.Close()
		t.Fatal(err)
	}
	operatorCtx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: tenant, ObjectID: "fixture-operator", Permissions: identity.AllPermissions()})
	if err := assuranceStore.StageBundle(operatorCtx, validatedBundle); err != nil {
		_ = sourceDB.Close()
		t.Fatal(err)
	}
	if err := assuranceStore.RequestActivation(operatorCtx, bundle.BundleID, bundle.BundleVersion); err != nil {
		_ = sourceDB.Close()
		t.Fatal(err)
	}
	if err := sourceDB.Close(); err != nil {
		t.Fatal(err)
	}
	startupRestoreAdmission = func(_ context.Context, _ *assurance.AzureBackupTransport, _, destination string, _ assurance.RestoreOptions, _ transfer.FenceInventory, _ string) (assurance.RestoreResult, transfer.RestoreScanResult, error) {
		if _, err := os.Stat(destination); !os.IsNotExist(err) {
			return assurance.RestoreResult{}, transfer.RestoreScanResult{}, errors.New("restore destination was not empty")
		}
		image, err := os.ReadFile(sourcePath)
		if err != nil {
			return assurance.RestoreResult{}, transfer.RestoreScanResult{}, err
		}
		if err := os.WriteFile(destination, image, 0o600); err != nil {
			return assurance.RestoreResult{}, transfer.RestoreScanResult{}, err
		}
		return assurance.RestoreResult{Ready: true}, transfer.RestoreScanResult{Ready: true}, nil
	}
	for key, value := range map[string]string{
		"NL_STARTUP_RESTORE_ADMISSION":         "true",
		"NL_AUTH_MODE":                         "entra",
		"NL_ENTRA_TENANT_ID":                   tenant,
		"NL_ENTRA_AUTHORITY":                   "https://login.microsoftonline.com/" + tenant + "/v2.0",
		"NL_ENTRA_AUDIENCE":                    "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"NL_ENTRA_SCOPE_PERMISSIONS":           `{"transfer.preview":["workiva.write.preview"]}`,
		"NL_ASSURANCE_ENABLED":                 "true",
		"NL_ASSURANCE_BUNDLE_PUBLIC_KEY":       hex.EncodeToString(bundlePublic),
		"NL_DB_PATH":                           dbPath,
		"NL_WORKIVA_CLIENT_ID":                 "test-client",
		"NL_WORKIVA_CLIENT_SECRET":             "test-secret",
		"NL_RESTORE_BLOB_SERVICE_URL":          "https://example.blob.core.windows.net",
		"NL_RESTORE_BLOB_CONTAINER":            "private",
		"NL_RESTORE_ENVIRONMENT_DIGEST":        "env-a",
		"NL_RESTORE_ENVELOPE_ID":               strings.Repeat("a", 32),
		"NL_RESTORE_DATABASE_SHA256":           strings.Repeat("b", 64),
		"NL_RESTORE_MIN_AUDIT_SEQUENCE":        "1",
		"NL_RESTORE_ED25519_PUBLIC_KEY_HEX":    strings.Repeat("c", 64),
		"NL_RESTORE_HMAC_KEY_HEX":              "",
		"NL_RESTORE_CHECKPOINT_PUBLIC_KEY_HEX": hex.EncodeToString(bundlePublic),
		"NL_RESTORE_CHECKPOINT_ID":             "audit-terminal",
	} {
		t.Setenv(key, value)
	}

	_, handler, cleanup, err := buildServer("", "")
	if err != nil {
		t.Fatalf("buildServer after clean restore admission: %v", err)
	}
	defer cleanup()
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("clean admission did not proceed to application DB open: %v", err)
	}

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	post := func(id int, method string, params any) (int, []byte) {
		t.Helper()
		message := map[string]any{"jsonrpc": "2.0", "method": method}
		if id > 0 {
			message["id"] = id
		}
		if params != nil {
			message["params"] = params
		}
		payload, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, server.URL+"/mcp", strings.NewReader(string(payload)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer verified-startup-token")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, body
	}
	if status, body := post(1, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "startup-admission-test", "version": "test"}}); status != http.StatusOK {
		t.Fatalf("raw MCP initialize status=%d: %s", status, body)
	}
	if status, body := post(0, "notifications/initialized", nil); status != http.StatusAccepted {
		t.Fatalf("raw MCP initialized notification status=%d: %s", status, body)
	}
	status, body := post(2, "tools/call", map[string]any{"name": "workiva_transfer_value", "arguments": map[string]any{
		"phase": "stage", "idempotency_key": "startup-admission-test",
		"source": map[string]any{"mapping_id": "mapping-test"},
		"target": map[string]any{"resource_id": "resource-test", "locator": "Sheet 1!B2"},
	}})
	if status != http.StatusOK {
		t.Fatalf("raw MCP transfer call status=%d: %s", status, body)
	}
	var envelope struct {
		Result struct {
			IsError           bool           `json:"isError"`
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode raw MCP transfer response: %v: %s", err, body)
	}
	errorInfo, _ := envelope.Result.StructuredContent["error"].(map[string]any)
	if envelope.Result.IsError || envelope.Result.StructuredContent["status"] != "unavailable" || errorInfo["code"] != "transfer_service_unavailable" || envelope.Result.StructuredContent["no_mutation_submitted"] != true {
		t.Fatalf("clean startup admission exposed transfer service: %#v; raw=%s", envelope.Result, body)
	}
}
