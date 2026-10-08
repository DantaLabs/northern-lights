package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/transfer"
)

func testBackupPrincipal() identity.Principal {
	return identity.Principal{TenantID: "tenant-a", ObjectID: "operator-1", Permissions: []identity.Permission{identity.PermissionTenantAdmin}}
}

type testBackupFence struct {
	high    assurance.BackupFenceHighWater
	err     error
	entered chan struct{}
	resume  <-chan struct{}
}

func (f testBackupFence) CaptureHighWater(ctx context.Context, _ string) (assurance.BackupFenceHighWater, error) {
	if f.entered != nil {
		close(f.entered)
		select {
		case <-ctx.Done():
			return assurance.BackupFenceHighWater{}, ctx.Err()
		case <-f.resume:
		}
	}
	return f.high, f.err
}

type testBackupBlob struct {
	err           error
	policyErr     error
	onValidate    func()
	calls         int
	validateCalls int
	uploadPath    string
	entered       chan struct{}
	resume        <-chan struct{}
}

type backupOperationHTTPFake struct {
	mu             sync.Mutex
	objects        map[string][]byte
	metadata       map[string]http.Header
	etags          map[string]string
	staged         map[string][]byte
	serviceDate    time.Time
	failPutSuffix  string
	failGetSuffix  string
	deleteRequests int
}

func newBackupOperationHTTPFake(t *testing.T) (*backupOperationHTTPFake, *httptest.Server, *azblob.Client) {
	t.Helper()
	fake := &backupOperationHTTPFake{
		objects: make(map[string][]byte), metadata: make(map[string]http.Header),
		etags: make(map[string]string), staged: make(map[string][]byte),
		serviceDate: time.Date(2026, 10, 7, 4, 5, 6, 0, time.UTC),
	}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	client, err := azblob.NewClientWithNoCredential(server.URL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return fake, server, client
}

func (f *backupOperationHTTPFake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Query().Get("comp") == "list" {
		w.Header().Set("Date", f.serviceDate.Format(http.TimeFormat))
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `<EnumerationResults><Blobs></Blobs><NextMarker></NextMarker></EnumerationResults>`)
		return
	}
	if r.Method == http.MethodGet && r.URL.Query().Get("restype") == "container" {
		w.Header().Set("ETag", `"policy"`)
		w.Header().Set("Date", f.serviceDate.Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name := r.URL.Path
	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("comp") == "block" {
			f.staged[name] = append([]byte(nil), body...)
			w.Header().Set("ETag", `"staged"`)
			w.WriteHeader(http.StatusCreated)
			return
		}
		if f.failPutSuffix != "" && strings.HasSuffix(name, f.failPutSuffix) {
			http.Error(w, "injected upload failure", http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("If-None-Match") != "*" {
			http.Error(w, "create-only required", http.StatusBadRequest)
			return
		}
		if _, exists := f.objects[name]; exists {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		if staged, ok := f.staged[name]; ok {
			body = staged
		}
		f.objects[name] = append([]byte(nil), body...)
		metadata := make(http.Header)
		for key, values := range r.Header {
			if strings.HasPrefix(strings.ToLower(key), "x-ms-meta-") {
				metadata[key] = append([]string(nil), values...)
			}
		}
		f.metadata[name] = metadata
		etag := `"etag-` + strconv.Itoa(len(f.objects)) + `"`
		f.etags[name] = etag
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		if f.failGetSuffix != "" && strings.HasSuffix(name, f.failGetSuffix) {
			http.Error(w, "injected read-back failure", http.StatusServiceUnavailable)
			return
		}
		body, ok := f.objects[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		for key, values := range f.metadata[name] {
			w.Header()[key] = append([]string(nil), values...)
		}
		w.Header().Set("ETag", f.etags[name])
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	case http.MethodDelete:
		f.deleteRequests++
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (b *testBackupBlob) ValidatePrivateContainer(context.Context) error {
	b.validateCalls++
	if b.onValidate != nil {
		b.onValidate()
	}
	return b.policyErr
}
func (b *testBackupBlob) UploadEnvelope(ctx context.Context, directory string, _ assurance.RestoreOptions) (assurance.BlobBackupSeal, error) {
	b.calls++
	b.uploadPath = directory
	if b.entered != nil {
		close(b.entered)
		select {
		case <-ctx.Done():
			return assurance.BlobBackupSeal{}, ctx.Err()
		case <-b.resume:
		}
	}
	if b.err != nil {
		return assurance.BlobBackupSeal{}, b.err
	}
	return assurance.BlobBackupSeal{EnvelopeID: "0123456789abcdef0123456789abcdef", DatabaseSHA256: "db", ManifestSHA256: "manifest", DatabaseETag: "db-etag", ManifestETag: "manifest-etag", AuditHighWater: 1}, nil
}

func TestProductionBackupOperationHoldsSharedAdmissionAcrossFenceAndUpload(t *testing.T) {
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "op.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	if _, err := assurance.NewWithDB(db); err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := assurance.HashBytes([]byte("tenant-a"))
	high := assurance.BackupFenceHighWater{TenantDigest: hex.EncodeToString(digest[:]), EnvironmentDigest: "env-a", CapturedAt: time.Now().UTC().Format(time.RFC3339Nano), Authority: "azure_blob_last_modified"}
	fenceEntered, fenceResume := make(chan struct{}), make(chan struct{})
	uploadEntered, uploadResume := make(chan struct{}), make(chan struct{})
	blob := &testBackupBlob{entered: uploadEntered, resume: uploadResume}
	m := newMaintenanceCoordinator(db)
	m.operation = &productionBackupOperation{db: db, config: BackupOperationConfig{TenantID: "tenant-a", EnvironmentDigest: "env-a", SigningKey: assurance.SigningKey{Ed25519Private: key}, CheckpointKey: key, TempParent: t.TempDir(), Audit: log, Fence: testBackupFence{high: high, entered: fenceEntered, resume: fenceResume}, Blob: blob}}
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", ObjectID: "operator-1", Permissions: []identity.Permission{identity.PermissionTenantAdmin}})
	writerRelease, err := m.gate.EnterWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { _, e := m.CreateProductionBackup(ctx, testBackupPrincipal()); finished <- e }()
	deadline := time.After(time.Second)
	for !m.gate.Draining() {
		select {
		case <-deadline:
			t.Fatal("backup did not close admission")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if _, err := m.gate.EnterWrite(ctx); !errors.Is(err, assurance.ErrBackupDraining) {
		t.Fatalf("new writer err=%v", err)
	}
	select {
	case <-fenceEntered:
		t.Fatal("backup passed in-flight writer")
	default:
	}
	writerRelease()
	select {
	case <-fenceEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("fence callback timed out")
	}
	if _, err := m.gate.EnterWrite(ctx); !errors.Is(err, assurance.ErrBackupDraining) {
		t.Fatalf("writer entered during fence: %v", err)
	}
	close(fenceResume)
	select {
	case <-uploadEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("upload callback timed out")
	}
	if _, err := m.gate.EnterWrite(ctx); !errors.Is(err, assurance.ErrBackupDraining) {
		t.Fatalf("writer entered during upload: %v", err)
	}
	if _, err := m.CreateProductionBackup(ctx, testBackupPrincipal()); !errors.Is(err, assurance.ErrBackupDraining) {
		t.Fatalf("concurrent backup trigger error=%v want ErrBackupDraining", err)
	}
	if blob.calls != 1 {
		t.Fatalf("concurrent trigger changed upload count=%d want 1", blob.calls)
	}
	close(uploadResume)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backup operation timed out")
	}
	if m.gate.Draining() {
		t.Fatal("admission remained closed after verified upload")
	}
}

func TestProductionBackupKeepsGateClosedWhenEarlyCallbackMakesDBUnobservable(t *testing.T) {
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := assurance.NewWithDB(db); err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blob := &testBackupBlob{onValidate: func() { _ = db.Close() }}
	m := newMaintenanceCoordinator(db)
	m.operation = &productionBackupOperation{db: db, config: BackupOperationConfig{TenantID: "tenant-a", EnvironmentDigest: "env-a", SigningKey: assurance.SigningKey{Ed25519Private: key}, CheckpointKey: key, TempParent: t.TempDir(), Audit: log, Fence: testBackupFence{}, Blob: blob}}
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", ObjectID: "operator-1", Permissions: []identity.Permission{identity.PermissionTenantAdmin}})
	if _, err := m.CreateProductionBackup(ctx, testBackupPrincipal()); err == nil {
		t.Fatal("closed DB callback unexpectedly succeeded")
	}
	if !m.gate.Draining() {
		t.Fatal("gate reopened although shared SQLite readiness was unobservable")
	}
	if blob.calls != 0 {
		t.Fatalf("upload calls=%d", blob.calls)
	}
}

func TestProductionBackupOperationRealAzureUploadAndReceiptRestore(t *testing.T) {
	serviceDate := time.Date(2026, 10, 7, 4, 5, 6, 0, time.UTC)
	var listedPrefixes []string
	objects := map[string][]byte{}
	metadata := map[string]http.Header{}
	etags := map[string]string{}
	var mu sync.Mutex
	puts := 0
	objectGets := 0
	policyPublicAccess := ""
	policyStatus := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Query().Get("comp") == "list" {
			mu.Lock()
			listedPrefixes = append(listedPrefixes, r.URL.Query().Get("prefix"))
			mu.Unlock()
			w.Header().Set("Date", serviceDate.Format(http.TimeFormat))
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `<EnumerationResults><Blobs></Blobs><NextMarker></NextMarker></EnumerationResults>`)
			return
		}
		if r.Method == http.MethodGet && r.URL.Query().Get("restype") == "container" {
			mu.Lock()
			publicAccess, status := policyPublicAccess, policyStatus
			mu.Unlock()
			if status != 0 {
				http.Error(w, "injected policy read failure", status)
				return
			}
			if publicAccess != "" {
				w.Header().Set("x-ms-blob-public-access", publicAccess)
			}
			w.Header().Set("ETag", `"policy"`)
			w.Header().Set("Date", serviceDate.Format(http.TimeFormat))
			w.WriteHeader(200)
			return
		}
		name := r.URL.Path
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPut {
			raw, _ := io.ReadAll(r.Body)
			if r.URL.Query().Get("comp") == "block" {
				w.Header().Set("ETag", `"block"`)
				w.WriteHeader(201)
				return
			}
			puts++
			if r.Header.Get("If-None-Match") != "*" || objects[name] != nil {
				w.WriteHeader(412)
				return
			}
			objects[name] = raw
			metadata[name] = http.Header{}
			for k, v := range r.Header {
				if strings.HasPrefix(strings.ToLower(k), "x-ms-meta-") {
					metadata[name][k] = append([]string(nil), v...)
				}
			}
			etags[name] = `"etag-` + strconv.Itoa(puts) + `"`
			w.Header().Set("ETag", etags[name])
			w.WriteHeader(201)
			return
		}
		if r.Method == http.MethodGet {
			objectGets++
			body, ok := objects[name]
			if !ok {
				w.WriteHeader(404)
				return
			}
			for k, v := range metadata[name] {
				w.Header()[k] = v
			}
			w.Header().Set("ETag", etags[name])
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(200)
			_, _ = w.Write(body)
			return
		}
		w.WriteHeader(405)
	}))
	defer server.Close()
	client, err := azblob.NewClientWithNoCredential(server.URL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	db, err := sqlitedb.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = assurance.NewWithDB(db); err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tenant := "tenant-a"
	digest := assurance.HashBytes([]byte(tenant))
	fence, err := transfer.NewAzureBlobFenceWithClient(context.Background(), transfer.AzureFenceConfig{ServiceURL: server.URL, ContainerName: "private", TenantID: tenant, EnvironmentDigest: "env-a", Timeout: 5 * time.Second, MaxScanPages: 8, MaxScanObjects: 32, MaxScanBytes: 1 << 20}, client)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := assurance.NewAzureBackupTransportWithClient(context.Background(), assurance.AzureBackupConfig{ServiceURL: server.URL, ContainerName: "private", TenantID: tenant, EnvironmentDigest: "env-a", Timeout: 5 * time.Second, MaxDatabaseBytes: 32 << 20}, client)
	if err != nil {
		t.Fatal(err)
	}
	stagingParent := t.TempDir()
	m := newMaintenanceCoordinator(db)
	m.operation = &productionBackupOperation{db: db, config: BackupOperationConfig{TenantID: tenant, EnvironmentDigest: "env-a", SigningKey: assurance.SigningKey{Ed25519Private: key}, CheckpointKey: key, TempParent: stagingParent, Audit: log, Fence: fence, Blob: transport}}
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: tenant, ObjectID: "operator-1", Permissions: []identity.Permission{identity.PermissionTenantAdmin}})
	receipt, err := m.CreateProductionBackup(ctx, testBackupPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	if receipt.EnvelopeID == "" || receipt.DatabaseSHA256 == "" || receipt.ManifestSHA256 == "" || receipt.DatabaseETag == "" || receipt.ManifestETag == "" {
		t.Fatalf("incomplete receipt: %+v", receipt)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{sourcePath, sourcePath + "-wal", sourcePath + "-shm"} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatalf("destroy disposable source SQLite artifact %s: %v", filepath.Base(path), err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("source SQLite artifact still exists before restore (%s): %v", filepath.Base(path), err)
		}
	}
	staged, err := os.ReadDir(stagingParent)
	if err != nil || len(staged) != 0 {
		t.Fatalf("successful backup left staging entries=%v err=%v", staged, err)
	}
	pubBytes, err := hex.DecodeString(receipt.CheckpointPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	created, err := time.Parse(time.RFC3339Nano, receipt.MinimumCreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restored.db")
	result, scan, err := transfer.RestoreSelectedEnvelopeAndScan(context.Background(), transport, receipt.EnvelopeID, dest, assurance.RestoreOptions{ExpectedTenantID: tenant, SigningKey: assurance.SigningKey{Ed25519Private: key}, CheckpointPublicKey: ed25519.PublicKey(pubBytes), CheckpointID: receipt.CheckpointID, MinimumCreatedAt: created, MinimumAuditSequence: receipt.AuditHighWater, ExpectedDatabaseSHA256: receipt.DatabaseSHA256, RequireFenceHighWater: true}, fence, "env-a")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready || result.DatabaseSHA256 != receipt.DatabaseSHA256 || !scan.Ready || scan.Findings != 0 || scan.Quarantined != 0 {
		t.Fatalf("restore result=%+v scan=%+v", result, scan)
	}
	wantPrefix := "env/env-a/tenant/" + hex.EncodeToString(digest[:]) + "/fences/"
	mu.Lock()
	gotPrefixes := append([]string(nil), listedPrefixes...)
	mu.Unlock()
	if !reflect.DeepEqual(gotPrefixes, []string{wantPrefix, wantPrefix}) {
		t.Fatalf("real fence listed prefixes=%q want two bounded namespace reads %q", gotPrefixes, wantPrefix)
	}
	if result.Manifest.FenceHighWater == nil || result.Manifest.FenceHighWater.CapturedAt != serviceDate.Format(time.RFC3339Nano) {
		t.Fatalf("signed high-water=%+v want service Date %s", result.Manifest.FenceHighWater, serviceDate.Format(time.RFC3339Nano))
	}

	// Exercise the real startup selector with operator-retained receipt trust.
	// The startup constructor callbacks only inject these same local SDK clients;
	// selected restore and fence join remain the production implementations.
	oldBackup, oldFence, oldAdmission := startupBackupTransport, startupFenceInventory, startupRestoreAdmission
	t.Cleanup(func() {
		startupBackupTransport, startupFenceInventory, startupRestoreAdmission = oldBackup, oldFence, oldAdmission
	})
	startupBackupTransport = func(_ context.Context, cfg assurance.AzureBackupConfig) (*assurance.AzureBackupTransport, error) {
		if cfg.TenantID != tenant || cfg.EnvironmentDigest != "env-a" {
			t.Fatalf("startup backup adapter binding=%+v", cfg)
		}
		return transport, nil
	}
	startupFenceInventory = func(_ context.Context, cfg transfer.AzureFenceConfig) (*transfer.AzureBlobFence, error) {
		if cfg.TenantID != tenant || cfg.EnvironmentDigest != "env-a" {
			t.Fatalf("startup fence adapter binding=%+v", cfg)
		}
		return fence, nil
	}
	startupRestoreAdmission = transfer.RestoreSelectedEnvelopeAndScan
	t.Setenv("NL_STARTUP_RESTORE_ADMISSION", "true")
	t.Setenv("NL_RESTORE_BLOB_SERVICE_URL", "https://account.blob.core.windows.net")
	t.Setenv("NL_RESTORE_BLOB_CONTAINER", "private")
	t.Setenv("NL_RESTORE_ENVIRONMENT_DIGEST", "env-a")
	t.Setenv("NL_RESTORE_ENVELOPE_ID", receipt.EnvelopeID)
	t.Setenv("NL_RESTORE_DATABASE_SHA256", receipt.DatabaseSHA256)
	t.Setenv("NL_RESTORE_MIN_AUDIT_SEQUENCE", strconv.FormatInt(receipt.AuditHighWater, 10))
	t.Setenv("NL_RESTORE_ED25519_PUBLIC_KEY_HEX", receipt.CheckpointPublicKey)
	t.Setenv("NL_RESTORE_HMAC_KEY_HEX", "")
	t.Setenv("NL_RESTORE_CHECKPOINT_PUBLIC_KEY_HEX", receipt.CheckpointPublicKey)
	t.Setenv("NL_RESTORE_CHECKPOINT_ID", receipt.CheckpointID)
	startupPath := filepath.Join(t.TempDir(), "startup-restored.db")
	startupCfg := &config.Config{AuthMode: config.AuthModeEntra, AssuranceEnabled: true, EntraTenantID: tenant, DBPath: startupPath}
	if _, err := admitStartupRestore(context.Background(), startupCfg); err != nil {
		t.Fatalf("startup restore rejected retained production receipt trust: %v", err)
	}
	mu.Lock()
	baselineLists, baselineObjectGets := append([]string(nil), listedPrefixes...), objectGets
	mu.Unlock()
	for _, tc := range []struct {
		name         string
		publicAccess string
		status       int
	}{
		{name: "public container", publicAccess: "blob"},
		{name: "unobservable policy", status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			policyPublicAccess, policyStatus = tc.publicAccess, tc.status
			mu.Unlock()
			blockedPath := filepath.Join(t.TempDir(), "policy-blocked.db")
			blockedCfg := *startupCfg
			blockedCfg.DBPath = blockedPath
			if _, err := admitStartupRestore(context.Background(), &blockedCfg); err == nil || !strings.Contains(err.Error(), "private container validation failed") {
				t.Fatalf("startup accepted unsafe/unobservable container policy: %v", err)
			}
			if _, err := os.Stat(blockedPath); !os.IsNotExist(err) {
				t.Fatalf("unsafe container policy wrote restore destination: %v", err)
			}
			mu.Lock()
			gotLists, gotObjectGets := append([]string(nil), listedPrefixes...), objectGets
			mu.Unlock()
			if !reflect.DeepEqual(gotLists, baselineLists) || gotObjectGets != baselineObjectGets {
				t.Fatalf("policy rejection reached restore/scan: lists=%v->%v object GETs=%d->%d", baselineLists, gotLists, baselineObjectGets, gotObjectGets)
			}
		})
	}
}

func TestProductionBackupHTTPFailuresLeaveImmutableOrphansAndNoReceipt(t *testing.T) {
	for _, tc := range []struct {
		name            string
		failPutSuffix   string
		failGetSuffix   string
		wantObjectCount int
	}{
		{name: "manifest upload failure leaves database orphan", failPutSuffix: "manifest.json", wantObjectCount: 1},
		{name: "manifest read-back failure leaves create-only objects", failGetSuffix: "manifest.json", wantObjectCount: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake, server, client := newBackupOperationHTTPFake(t)
			fake.failPutSuffix, fake.failGetSuffix = tc.failPutSuffix, tc.failGetSuffix
			db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "failure.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Errorf("close test database: %v", err)
				}
			})
			if _, err := assurance.NewWithDB(db); err != nil {
				t.Fatal(err)
			}
			log, err := audit.NewWithDB(db)
			if err != nil {
				t.Fatal(err)
			}
			_, key, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			tenant := "tenant-a"
			cfg := assurance.AzureBackupConfig{ServiceURL: server.URL, ContainerName: "private", TenantID: tenant, EnvironmentDigest: "env-a", Timeout: 5 * time.Second, MaxDatabaseBytes: 32 << 20}
			blob, err := assurance.NewAzureBackupTransportWithClient(context.Background(), cfg, client)
			if err != nil {
				t.Fatal(err)
			}
			fence, err := transfer.NewAzureBlobFenceWithClient(context.Background(), transfer.AzureFenceConfig{ServiceURL: server.URL, ContainerName: "private", TenantID: tenant, EnvironmentDigest: "env-a", Timeout: 5 * time.Second, MaxScanPages: 8, MaxScanObjects: 32, MaxScanBytes: 1 << 20}, client)
			if err != nil {
				t.Fatal(err)
			}
			stagingParent := t.TempDir()
			m := newMaintenanceCoordinator(db)
			m.operation = &productionBackupOperation{db: db, config: BackupOperationConfig{TenantID: tenant, EnvironmentDigest: "env-a", SigningKey: assurance.SigningKey{Ed25519Private: key}, CheckpointKey: key, TempParent: stagingParent, Audit: log, Fence: fence, Blob: blob}}
			ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: tenant, ObjectID: "operator-1", Permissions: []identity.Permission{identity.PermissionTenantAdmin}})
			if receipt, err := m.CreateProductionBackup(ctx, testBackupPrincipal()); err == nil || receipt.EnvelopeID != "" {
				t.Fatalf("failed HTTP upload produced receipt=%+v err=%v", receipt, err)
			}
			if m.gate.Draining() {
				t.Fatal("healthy database admission remained closed after failed HTTP upload")
			}
			fake.mu.Lock()
			objectCount, deletes := len(fake.objects), fake.deleteRequests
			objects := make(map[string][]byte, len(fake.objects))
			for name, body := range fake.objects {
				objects[name] = append([]byte(nil), body...)
			}
			fake.mu.Unlock()
			if objectCount != tc.wantObjectCount || deletes != 0 {
				t.Fatalf("orphan inventory count=%d deletes=%d, want objects=%d deletes=0", objectCount, deletes, tc.wantObjectCount)
			}
			for name, body := range objects {
				if len(body) == 0 {
					t.Errorf("orphan object %q has empty bytes", name)
				}
			}
			if entries, err := os.ReadDir(stagingParent); err != nil || len(entries) != 1 {
				t.Fatalf("failed HTTP upload did not retain exactly one private staging dir: %v err=%v", entries, err)
			}
		})
	}
}

func TestProductionBackupOperationRequiresTrustedPrincipalBeforeDrain(t *testing.T) {
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "op.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	if _, err := assurance.NewWithDB(db); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		caller identity.Principal
	}{
		{name: "missing trusted context", ctx: context.Background(), caller: identity.Principal{}},
		{name: "empty trusted object id", ctx: identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", Permissions: []identity.Permission{identity.PermissionTenantAdmin}}), caller: testBackupPrincipal()},
		{name: "forged caller differs from trusted context", ctx: identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", ObjectID: "operator-1", Permissions: []identity.Permission{identity.PermissionTenantAdmin}}), caller: identity.Principal{TenantID: "tenant-a", ObjectID: "forged", Permissions: []identity.Permission{identity.PermissionTenantAdmin}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMaintenanceCoordinator(db)
			m.operation = &productionBackupOperation{db: db, config: BackupOperationConfig{TenantID: "tenant-a"}}
			if _, err := m.CreateProductionBackup(tc.ctx, tc.caller); err == nil || !strings.Contains(err.Error(), "backup authorization failed") {
				t.Fatalf("unauthorized backup error=%v", err)
			}
			if m.gate.Draining() {
				t.Fatal("authorization failure left admission closed")
			}
		})
	}
}

func TestProductionBackupOperationCreatesSignedCheckpointAndSealedReceipt(t *testing.T) {
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "op.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	if _, err := assurance.NewWithDB(db); err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blob := &testBackupBlob{}
	hash := assurance.HashBytes([]byte("tenant-a"))
	hashHex := hex.EncodeToString(hash[:])
	cfg := BackupOperationConfig{TenantID: "tenant-a", EnvironmentDigest: "env-a", SigningKey: assurance.SigningKey{Ed25519Private: key}, CheckpointKey: key, TempParent: t.TempDir(), Audit: log, Fence: testBackupFence{high: assurance.BackupFenceHighWater{TenantDigest: hashHex, EnvironmentDigest: "env-a", CapturedAt: time.Now().UTC().Format(time.RFC3339Nano), Authority: "azure_blob_last_modified"}}, Blob: blob}
	m := newMaintenanceCoordinator(db)
	m.operation = &productionBackupOperation{db: db, config: cfg}
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", ObjectID: "operator-1", Permissions: []identity.Permission{identity.PermissionTenantAdmin}})
	receipt, err := m.CreateProductionBackup(ctx, testBackupPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	if receipt.EnvelopeID == "" || receipt.ManifestETag == "" || blob.calls != 1 {
		t.Fatalf("receipt=%+v uploads=%d", receipt, blob.calls)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_checkpoints WHERE tenant_id=?`, "tenant-a").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("checkpoints=%d", count)
	}
}

func TestProductionBackupOperationFailureHasNoReceiptAndReopensSafeDatabase(t *testing.T) {
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "op.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	if _, err := assurance.NewWithDB(db); err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blob := &testBackupBlob{err: errors.New("upload failed")}
	hash := assurance.HashBytes([]byte("tenant-a"))
	hashHex := hex.EncodeToString(hash[:])
	high := assurance.BackupFenceHighWater{TenantDigest: hashHex, EnvironmentDigest: "env-a", CapturedAt: time.Now().UTC().Format(time.RFC3339Nano), Authority: "azure_blob_last_modified"}
	stagingParent := t.TempDir()
	m := newMaintenanceCoordinator(db)
	m.operation = &productionBackupOperation{db: db, config: BackupOperationConfig{TenantID: "tenant-a", EnvironmentDigest: "env-a", SigningKey: assurance.SigningKey{Ed25519Private: key}, CheckpointKey: key, TempParent: stagingParent, Audit: log, Fence: testBackupFence{high: high}, Blob: blob}}
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", ObjectID: "operator-1", Permissions: []identity.Permission{identity.PermissionTenantAdmin}})
	if receipt, err := m.CreateProductionBackup(ctx, testBackupPrincipal()); err == nil || receipt.EnvelopeID != "" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if m.gate.Draining() {
		t.Fatal("safe writable database remained drained")
	}
	if blob.uploadPath == "" {
		t.Fatal("failed upload did not record its retained staging envelope")
	}
	if entries, err := os.ReadDir(blob.uploadPath); err != nil || len(entries) == 0 {
		t.Fatalf("failed upload staging is not retained: entries=%v err=%v", entries, err)
	}
	var writable int
	if err := db.QueryRow(`PRAGMA query_only`).Scan(&writable); err != nil || writable != 0 {
		t.Fatalf("query_only=%d err=%v", writable, err)
	}
}

func TestProductionBackupPolicyFailureStopsBeforeCheckpointUploadAndStaging(t *testing.T) {
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "policy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	if _, err := assurance.NewWithDB(db); err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hash := assurance.HashBytes([]byte("tenant-a"))
	blob := &testBackupBlob{policyErr: errors.New("container policy rejected")}
	stagingParent := t.TempDir()
	m := newMaintenanceCoordinator(db)
	m.operation = &productionBackupOperation{db: db, config: BackupOperationConfig{TenantID: "tenant-a", EnvironmentDigest: "env-a", SigningKey: assurance.SigningKey{Ed25519Private: key}, CheckpointKey: key, TempParent: stagingParent, Audit: log, Fence: testBackupFence{high: assurance.BackupFenceHighWater{TenantDigest: hex.EncodeToString(hash[:]), EnvironmentDigest: "env-a", CapturedAt: time.Now().UTC().Format(time.RFC3339Nano), Authority: "azure_blob_last_modified"}}, Blob: blob}}
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", ObjectID: "operator-1", Permissions: []identity.Permission{identity.PermissionTenantAdmin}})
	if receipt, err := m.CreateProductionBackup(ctx, testBackupPrincipal()); err == nil || receipt.EnvelopeID != "" {
		t.Fatalf("policy rejection receipt=%+v err=%v", receipt, err)
	}
	if blob.validateCalls != 1 || blob.calls != 0 || blob.uploadPath != "" {
		t.Fatalf("policy failure validation=%d upload=%d path=%q", blob.validateCalls, blob.calls, blob.uploadPath)
	}
	if entries, err := os.ReadDir(stagingParent); err != nil || len(entries) != 0 {
		t.Fatalf("policy failure created staging: entries=%v err=%v", entries, err)
	}
	var checkpoints int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_checkpoints WHERE tenant_id=?`, "tenant-a").Scan(&checkpoints); err != nil || checkpoints != 0 {
		t.Fatalf("policy failure checkpoint count=%d err=%v", checkpoints, err)
	}
	if m.gate.Draining() {
		t.Fatal("healthy database admission remained closed after policy rejection")
	}
}

func TestProductionBackupCancellationDuringUploadReopensHealthyAdmission(t *testing.T) {
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "cancel-upload.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	if _, err := assurance.NewWithDB(db); err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hash := assurance.HashBytes([]byte("tenant-a"))
	uploadEntered := make(chan struct{})
	blob := &testBackupBlob{entered: uploadEntered, resume: make(chan struct{})}
	m := newMaintenanceCoordinator(db)
	m.operation = &productionBackupOperation{db: db, config: BackupOperationConfig{TenantID: "tenant-a", EnvironmentDigest: "env-a", SigningKey: assurance.SigningKey{Ed25519Private: key}, CheckpointKey: key, TempParent: t.TempDir(), Audit: log, Fence: testBackupFence{high: assurance.BackupFenceHighWater{TenantDigest: hex.EncodeToString(hash[:]), EnvironmentDigest: "env-a", CapturedAt: time.Now().UTC().Format(time.RFC3339Nano), Authority: "azure_blob_last_modified"}}, Blob: blob}}
	ctx, cancel := context.WithCancel(identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", ObjectID: "operator-1", Permissions: []identity.Permission{identity.PermissionTenantAdmin}}))
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := m.CreateProductionBackup(ctx, testBackupPrincipal())
		result <- err
	}()
	select {
	case <-uploadEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("backup did not enter upload")
	}
	if !m.gate.Draining() {
		t.Fatal("admission opened before upload cancellation")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled upload returned a successful receipt")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled upload did not finish")
	}
	if m.gate.Draining() {
		t.Fatal("healthy admission remained closed after canceled upload")
	}
	var queryOnly int
	if err := db.QueryRow(`PRAGMA query_only`).Scan(&queryOnly); err != nil || queryOnly != 0 {
		t.Fatalf("healthy database not writable after cancellation: query_only=%d err=%v", queryOnly, err)
	}
}

func TestProductionBackupCancellationWhileWaitingForDrainReopensHealthyAdmission(t *testing.T) {
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "cancel-drain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	if _, err := assurance.NewWithDB(db); err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hash := assurance.HashBytes([]byte("tenant-a"))
	blob := &testBackupBlob{}
	m := newMaintenanceCoordinator(db)
	m.operation = &productionBackupOperation{db: db, config: BackupOperationConfig{TenantID: "tenant-a", EnvironmentDigest: "env-a", SigningKey: assurance.SigningKey{Ed25519Private: key}, CheckpointKey: key, TempParent: t.TempDir(), Audit: log, Fence: testBackupFence{high: assurance.BackupFenceHighWater{TenantDigest: hex.EncodeToString(hash[:]), EnvironmentDigest: "env-a", CapturedAt: time.Now().UTC().Format(time.RFC3339Nano), Authority: "azure_blob_last_modified"}}, Blob: blob}}
	ctx, cancel := context.WithCancel(identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: "tenant-a", ObjectID: "operator-1", Permissions: []identity.Permission{identity.PermissionTenantAdmin}}))
	defer cancel()
	releaseWriter, err := m.gate.EnterWrite(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := m.CreateProductionBackup(ctx, testBackupPrincipal())
		result <- err
	}()
	deadline := time.After(3 * time.Second)
	for !m.gate.Draining() {
		select {
		case <-deadline:
			releaseWriter()
			t.Fatal("backup did not enter drain while writer was active")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled drain error=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled drain did not finish")
	}
	releaseWriter()
	if m.gate.Draining() || blob.validateCalls != 0 || blob.calls != 0 {
		t.Fatalf("healthy canceled drain state draining=%t validations=%d uploads=%d", m.gate.Draining(), blob.validateCalls, blob.calls)
	}
}
