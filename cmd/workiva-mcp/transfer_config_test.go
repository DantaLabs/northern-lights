package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/ratelimit"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/transfer"
	"github.com/dantalabs/northern-lights/internal/workiva"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

func TestTransferProductionOptIn(t *testing.T) {
	tests := []struct {
		name, value string
		present     bool
		wantEnabled bool
		wantErr     bool
	}{
		{name: "unset disables"},
		{name: "false disables", value: "false", present: true},
		{name: "true enables", value: "true", present: true, wantEnabled: true},
		{name: "empty rejected", value: "", present: true, wantErr: true},
		{name: "uppercase rejected", value: "TRUE", present: true, wantErr: true},
		{name: "other rejected", value: "yes", present: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := transferProductionOptIn(tt.value, tt.present)
			if (err != nil) != tt.wantErr || got != tt.wantEnabled {
				t.Fatalf("transferProductionOptIn() = (%v, %v), want (%v, err=%v)", got, err, tt.wantEnabled, tt.wantErr)
			}
		})
	}
}

func TestConfigureProductionTransferRefusesEveryMissingAdmissionBeforeProviderUse(t *testing.T) {
	f := newProductionTransferFixture(t)
	base := f.config()
	tests := []struct {
		name string
		edit func(*productionTransferConfig)
	}{
		{"auth", func(c *productionTransferConfig) { c.authMode = config.AuthModeAPIKey }},
		{"demo", func(c *productionTransferConfig) { c.demoMode = true }},
		{"assurance disabled", func(c *productionTransferConfig) { c.assuranceEnabled = false }},
		{"no admitted restore fence", func(c *productionTransferConfig) { c.startupFence = nil }},
		{"no backup maintenance", func(c *productionTransferConfig) { c.backupConfigured = false }},
		{"no evidence delivery", func(c *productionTransferConfig) { c.evidenceConfigured = false }},
		{"no shared audit", func(c *productionTransferConfig) { c.audit = nil }},
		{"no shared database", func(c *productionTransferConfig) { c.db = nil }},
		{"assurance unready", func(c *productionTransferConfig) { c.assuranceStore = f.unreadyAssurance }},
		{"no provider router", func(c *productionTransferConfig) { c.provider = nil }},
		{"no maintenance gate", func(c *productionTransferConfig) { c.gate = nil }},
		{"restore-backup namespace mismatch", func(c *productionTransferConfig) { c.backupNamespace.container = "different" }},
		{"evidence environment mismatch", func(c *productionTransferConfig) { c.evidenceNamespace.environment = "other" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			tt.edit(&cfg)
			service, stop, err := configureProductionTransfer(context.Background(), cfg)
			if err == nil || service != nil || stop != nil {
				t.Fatalf("refusal returned service=%v stop=%v err=%v", service, stop != nil, err)
			}
			if got := f.providerCalls.Load(); got != 0 {
				t.Fatalf("startup refusal called Workiva provider %d times", got)
			}
		})
	}
}

func TestConfigureProductionTransferDisabledStartsNothing(t *testing.T) {
	service, stop, err := configureProductionTransfer(context.Background(), productionTransferConfig{})
	if err != nil || service != nil || stop != nil {
		t.Fatalf("disabled transfer got service=%v stop=%v err=%v", service, stop != nil, err)
	}
}

func TestConfigureProductionTransferBuildsAzureBackedServiceAndGatesJanitor(t *testing.T) {
	f := newProductionTransferFixture(t)
	service, stop, err := configureProductionTransfer(context.Background(), f.config())
	if err != nil || service == nil || stop == nil {
		t.Fatalf("enabled service=%v stop=%v err=%v", service, stop != nil, err)
	}
	if !f.fence.Durable() || f.azureRequests.Load() == 0 {
		t.Fatal("service was not assembled with a exercised durable Azure fence")
	}
	transferStore, err := transfer.NewWithDB(f.db)
	if err != nil {
		t.Fatal(err)
	}
	release, err := f.gate.BeginDrain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	intent := transfer.Intent{ID: "janitor-expired", TenantID: f.tenant, ActorID: "actor", Permission: "workiva.write.preview", RouteID: "route", RouteRevision: 1, MappingID: "mapping", Source: transfer.Endpoint{ResourceID: "source", SheetID: "s", Locator: "A1", Fingerprint: strings.Repeat("a", 64)}, Target: transfer.Endpoint{ResourceID: "target", SheetID: "t", Locator: "B2", Fingerprint: strings.Repeat("b", 64)}, Before: `{"kind":"number","value":1}`, Intended: `{"kind":"number","value":2}`, PolicyID: "policy", PolicyRevision: 1, PolicyHash: strings.Repeat("c", 64), MappingHash: strings.Repeat("d", 64), ExpiresAt: now.Add(time.Minute)}
	if _, err := transferStore.Stage(context.Background(), intent, "token", "key", "request", now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE transfer_intents SET state='claimed',lease_id='expired',lease_expires_at=? WHERE tenant_id=? AND transfer_id=?`, now.Add(-time.Second).Format(time.RFC3339Nano), intent.TenantID, intent.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	var state string
	if err := f.db.QueryRow(`SELECT state FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, intent.TenantID, intent.ID).Scan(&state); err != nil || state != string(transfer.StateClaimed) {
		t.Fatalf("janitor bypassed maintenance gate: state=%q err=%v", state, err)
	}
	release()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := f.db.QueryRow(`SELECT state FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, intent.TenantID, intent.ID).Scan(&state); err == nil && state == string(transfer.StateReconciliationRequired) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if state != string(transfer.StateReconciliationRequired) {
		t.Fatalf("expired intent not quarantined after gate opened: %q", state)
	}
	if f.providerCalls.Load() != 0 {
		t.Fatalf("startup/janitor called provider %d times", f.providerCalls.Load())
	}
	stop()
	if err := f.db.Close(); err != nil {
		t.Fatalf("close DB after janitor joined: %v", err)
	}
}

type productionTransferFixture struct {
	db                               *sql.DB
	tenant                           string
	assuranceStore, unreadyAssurance *assurance.Store
	audit                            *audit.Log
	provider                         *workivaprovider.Router
	gate                             *assurance.DrainGate
	fence                            *transfer.AzureBlobFence
	azureRequests, providerCalls     *atomic.Int64
}

func newProductionTransferFixture(t *testing.T) *productionTransferFixture {
	t.Helper()
	tenant := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "transfer-startup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := assurance.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	auditLog, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	store.SetAuditLog(auditLog)
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bundle := assurance.Bundle{SchemaVersion: 1, BundleID: "transfer-startup-fixture", BundleVersion: 1, TenantID: tenant, Reports: []assurance.ReportRevision{{ReportID: "transfer-report", Revision: 1, Name: "Transfer report", Owner: "fixture", Status: "active", RetentionClass: "standard", ResourcePolicyHash: strings.Repeat("a", 64), Periods: []assurance.Period{{Key: "2026-Q3", Label: "Q3", Start: "2026-07-01", End: "2026-09-30"}}, Fields: []assurance.FieldDefinition{{FieldID: "transfer-field", ResourceID: "transfer-resource", ExternalResourceID: "transfer-external", SubresourceID: "transfer-sheet", Locator: "B2", Kind: assurance.ValueNumber, Required: true, Order: 1}}}}}
	raw, err := assurance.CanonicalJSON(bundle)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := assurance.ValidateBundle(raw, ed25519.Sign(private, raw), tenant, pub)
	if err != nil {
		t.Fatal(err)
	}
	operator := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: tenant, ObjectID: "operator", Permissions: identity.AllPermissions()})
	if err := store.StageBundle(operator, validated); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestActivation(operator, bundle.BundleID, bundle.BundleVersion); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: tenant}), tenant, pub); err != nil {
		t.Fatal(err)
	}
	unready, err := assurance.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	azureRequests := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		azureRequests.Add(1)
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><EnumerationResults><Blobs></Blobs><NextMarker></NextMarker></EnumerationResults>`))
	}))
	t.Cleanup(server.Close)
	azureClient, err := azblob.NewClientWithNoCredential(server.URL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	fence, err := transfer.NewAzureBlobFenceWithClient(context.Background(), transfer.AzureFenceConfig{ServiceURL: "https://account.blob.core.windows.net", ContainerName: "private-data", TenantID: tenant, EnvironmentDigest: "env-a", Timeout: time.Second, MaxScanPages: 4, MaxScanObjects: 32, MaxScanBytes: 4096}, azureClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fence.CaptureHighWater(context.Background(), tenant); err != nil {
		t.Fatalf("exercise local Azure fence: %v", err)
	}
	providerCalls := &atomic.Int64{}
	providerHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		http.Error(w, "provider must not be used during startup", http.StatusInternalServerError)
	}))
	t.Cleanup(providerHTTP.Close)
	baseURL, _ := url.Parse(providerHTTP.URL)
	httpClient := providerHTTP.Client()
	tokens := workiva.NewTokenProvider(baseURL, "test-client", "test-secret", "file:read file:write", httpClient)
	workivaClient := workiva.NewClient(baseURL, tokens, ratelimit.NewLimiter(), httpClient)
	provider := workivaprovider.NewRouter(workivaClient)
	return &productionTransferFixture{db: db, tenant: tenant, assuranceStore: store, unreadyAssurance: unready, audit: auditLog, provider: provider, gate: assurance.NewDrainGate(), fence: fence, azureRequests: azureRequests, providerCalls: providerCalls}
}

func (f *productionTransferFixture) config() productionTransferConfig {
	ns := blobNamespace{accountURL: "https://account.blob.core.windows.net", container: "private-data", environment: "env-a"}
	return productionTransferConfig{enabled: true, restoreNamespace: ns, backupNamespace: ns, evidenceNamespace: blobNamespace{accountURL: ns.accountURL, container: "evidence-private", environment: ns.environment}, authMode: config.AuthModeEntra, assuranceEnabled: true, startupFence: f.fence, backupConfigured: true, evidenceConfigured: true, assuranceStore: f.assuranceStore, audit: f.audit, db: f.db, provider: f.provider, gate: f.gate, quarantineInterval: 10 * time.Millisecond}
}

func TestTransferNamespaceRequiresConfiguredMatchingRecoveryAndDelivery(t *testing.T) {
	base := blobNamespace{accountURL: "https://account.blob.core.windows.net", container: "private-data", environment: "sandbox-a"}
	tests := []struct {
		name                      string
		restore, backup, evidence blobNamespace
	}{
		{name: "missing restore"},
		{name: "missing backup", restore: base, evidence: base},
		{name: "missing evidence", restore: base, backup: base},
		{name: "account mismatch", restore: base, backup: blobNamespace{accountURL: "https://other.blob.core.windows.net", container: base.container, environment: base.environment}, evidence: base},
		{name: "container mismatch", restore: base, backup: blobNamespace{accountURL: base.accountURL, container: "other-data", environment: base.environment}, evidence: base},
		{name: "backup environment mismatch", restore: base, backup: blobNamespace{accountURL: base.accountURL, container: base.container, environment: "sandbox-b"}, evidence: base},
		{name: "evidence environment mismatch", restore: base, backup: base, evidence: blobNamespace{accountURL: base.accountURL, container: base.container, environment: "sandbox-b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := transferNamespace(tt.restore, tt.backup, tt.evidence); err == nil {
				t.Fatal("transferNamespace() accepted missing or mismatched production prerequisite")
			}
		})
	}
	evidence := blobNamespace{accountURL: base.accountURL, container: "evidence-private", environment: base.environment}
	if err := transferNamespace(base, base, evidence); err != nil {
		t.Fatalf("matching namespaces rejected: %v", err)
	}
}

func TestTransferNamespaceRejectsEvidenceContainerSharedWithRecoveryStorage(t *testing.T) {
	backup := blobNamespace{accountURL: "https://account.blob.core.windows.net", container: "private-backups", environment: "env-a"}
	restore := backup
	tests := []struct {
		name     string
		evidence blobNamespace
	}{
		{name: "same backup pair", evidence: backup},
		{name: "same restore pair", evidence: restore},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := transferNamespace(restore, backup, tc.evidence); err == nil {
				t.Fatal("transfer namespace admitted evidence delivery into the recovery container")
			}
		})
	}
	if err := transferNamespace(restore, backup, blobNamespace{accountURL: "https://other.blob.core.windows.net", container: backup.container, environment: backup.environment}); err != nil {
		t.Fatalf("same container name on distinct account was treated as same physical namespace: %v", err)
	}
	if err := transferNamespace(restore, backup, blobNamespace{accountURL: backup.accountURL, container: "evidence-private", environment: backup.environment}); err != nil {
		t.Fatalf("separate evidence container rejected: %v", err)
	}
}

func TestConfigureProductionTransferRejectsSharedEvidenceContainerBeforeAzureOrProviderWork(t *testing.T) {
	f := newProductionTransferFixture(t)
	cfg := f.config()
	cfg.evidenceNamespace = cfg.backupNamespace
	azureBefore := f.azureRequests.Load()
	service, stop, err := configureProductionTransfer(context.Background(), cfg)
	if err == nil || service != nil || stop != nil {
		t.Fatalf("shared evidence/recovery container returned service=%v stop=%v err=%v", service, stop != nil, err)
	}
	if got := f.azureRequests.Load(); got != azureBefore {
		t.Fatalf("transfer config attempted Azure fence work after namespace collision: before=%d after=%d", azureBefore, got)
	}
	if got := f.providerCalls.Load(); got != 0 {
		t.Fatalf("transfer config called provider after namespace collision: %d", got)
	}
}
