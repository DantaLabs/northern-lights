package transfer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

type recoveryBlobObject struct {
	body         []byte
	etag         string
	metadata     http.Header
	lastModified time.Time
}

type recoveryBlobFake struct {
	mu      sync.Mutex
	objects map[string]recoveryBlobObject
	staged  map[string][]byte
	service time.Time
}

func newRecoveryBlobFake() *recoveryBlobFake {
	return &recoveryBlobFake{
		objects: make(map[string]recoveryBlobObject),
		staged:  make(map[string][]byte),
		service: time.Date(2026, 10, 7, 4, 0, 0, 0, time.UTC),
	}
}

func (f *recoveryBlobFake) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("comp") == "list" {
		f.serveList(w, r)
		return
	}
	path := r.URL.Path
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		object, ok := f.objects[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		for key, values := range object.metadata {
			w.Header()[key] = append([]string(nil), values...)
		}
		w.Header().Set("ETag", object.etag)
		w.Header().Set("Last-Modified", object.lastModified.UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Length", strconv.Itoa(len(object.body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(object.body)
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("comp") == "block" {
			f.staged[path] = append([]byte(nil), body...)
			w.Header().Set("ETag", `"staged"`)
			w.WriteHeader(http.StatusCreated)
			return
		}
		if r.Header.Get("If-None-Match") != "*" {
			http.Error(w, "create-only required", http.StatusBadRequest)
			return
		}
		if _, exists := f.objects[path]; exists {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		if staged, ok := f.staged[path]; ok {
			body = staged
		}
		metadata := make(http.Header)
		for key, values := range r.Header {
			if strings.HasPrefix(strings.ToLower(key), "x-ms-meta-") {
				metadata[key] = append([]string(nil), values...)
			}
		}
		etag := `"` + strconv.Itoa(len(f.objects)+1) + `"`
		f.objects[path] = recoveryBlobObject{body: append([]byte(nil), body...), etag: etag, metadata: metadata, lastModified: f.service}
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *recoveryBlobFake) serveList(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := r.URL.Query().Get("prefix")
	type listedBlob struct {
		Name       string `xml:"Name"`
		Properties struct {
			ETag          string `xml:"Etag"`
			LastModified  string `xml:"Last-Modified"`
			ContentLength int64  `xml:"Content-Length"`
		} `xml:"Properties"`
	}
	response := struct {
		XMLName xml.Name `xml:"EnumerationResults"`
		Blobs   struct {
			Blob []*listedBlob `xml:"Blob"`
		} `xml:"Blobs"`
		Next string `xml:"NextMarker"`
	}{}
	for path, object := range f.objects {
		name := strings.TrimPrefix(path, "/private/")
		if strings.HasPrefix(name, prefix) {
			listed := &listedBlob{Name: name}
			listed.Properties.ETag = object.etag
			listed.Properties.LastModified = object.lastModified.UTC().Format(http.TimeFormat)
			listed.Properties.ContentLength = int64(len(object.body))
			response.Blobs.Blob = append(response.Blobs.Blob, listed)
		}
	}
	w.Header().Set("Date", f.service.Format(http.TimeFormat))
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(response)
}

func newRecoveryBackupTransport(t *testing.T, serverURL string) *assurance.AzureBackupTransport {
	t.Helper()
	client, err := azblob.NewClientWithNoCredential(serverURL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	transport, err := assurance.NewAzureBackupTransportWithClient(context.Background(), assurance.AzureBackupConfig{
		ServiceURL: serverURL, ContainerName: "private", TenantID: "tenant-a", EnvironmentDigest: "env-a",
		Timeout: 5 * time.Second, MaxDatabaseBytes: 32 << 20,
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	return transport
}

func TestBlobBackupDestroyRestoreAndFenceJoinAdmission(t *testing.T) {
	ctx := context.Background()
	store, _, auditLog, intent, claimTime, inventory, _, _ := newRestoreScanFixture(t, StateMachineVerified, true)
	operator := identity.ContextWithPrincipal(ctx, identity.Principal{TenantID: intent.TenantID, ObjectID: intent.ActorID})
	auditEntry, err := auditLog.Append(operator, audit.Entry{Actor: intent.ActorID, Tool: "startup_recovery_test", Action: "fixture", Target: intent.ID})
	if err != nil {
		t.Fatal(err)
	}
	checkpointPublic, checkpointPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	checkpointSignature := ed25519.Sign(checkpointPrivate, []byte(strings.Join([]string{intent.TenantID, strconv.FormatInt(auditEntry.Seq, 10), auditEntry.Hash, "audit-terminal"}, "\x00")))
	if _, err := store.db.Exec(`INSERT INTO assurance_checkpoints (tenant_id, checkpoint_id, checkpoint_kind, high_water_mark, checkpoint_hash, signature_hex, metadata_json, created_at) VALUES (?, 'audit-terminal', 'terminal', ?, ?, ?, '{}', CURRENT_TIMESTAMP)`, intent.TenantID, auditEntry.Seq, auditEntry.Hash, hex.EncodeToString(checkpointSignature)); err != nil {
		t.Fatal(err)
	}

	fixture := newRecoveryBlobFake()
	server := newScanHTTPServer(t, http.HandlerFunc(fixture.serve))
	defer server.Close()
	backup := newRecoveryBackupTransport(t, server.URL)
	fence := newScanFence(t, server.URL, 5*time.Second, 8, 32, 8<<20)
	for _, object := range inventory.objects {
		switch object.Kind {
		case "claim":
			if _, err := fence.CreateClaim(ctx, intent.ID, object.Body); err != nil {
				t.Fatalf("create external claim fence: %v", err)
			}
		case "terminal":
			if _, err := fence.CreateTerminal(ctx, intent.ID, object.Body); err != nil {
				t.Fatalf("create external terminal fence: %v", err)
			}
		}
	}
	highWater, err := fence.CaptureHighWater(ctx, intent.TenantID)
	if err != nil {
		t.Fatalf("capture external high-water: %v", err)
	}
	if highWaterTime(highWater).Format(http.TimeFormat) != fixture.service.Format(http.TimeFormat) {
		t.Fatalf("captured high-water %s does not come from fake Blob service Date %s", highWater.CapturedAt, fixture.service.Format(http.TimeFormat))
	}

	workDir := t.TempDir()
	sourcePath := filepath.Join(workDir, "source.db")
	if _, err := store.db.ExecContext(ctx, `VACUUM INTO ?`, sourcePath); err != nil {
		t.Fatalf("materialize disposable SQLite source: %v", err)
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	sourceDB, err := sqlitedb.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	backupKey := assurance.SigningKey{HMAC: []byte("combined-recovery-test-signing-key-32")}
	envelopeDir := filepath.Join(workDir, "envelope")
	envelope, err := assurance.CreateBackup(ctx, sourceDB, envelopeDir, assurance.BackupOptions{
		TenantID: intent.TenantID, SigningKey: backupKey, FenceHighWater: &highWater, CheckpointPublicKey: checkpointPublic,
	})
	if err != nil {
		_ = sourceDB.Close()
		t.Fatalf("create signed database envelope: %v", err)
	}
	if envelope.Manifest.Audit.LastSequence < 1 || envelope.Manifest.FenceHighWater == nil || *envelope.Manifest.FenceHighWater != highWater {
		_ = sourceDB.Close()
		t.Fatalf("backup omitted audit/fence high-water: %+v", envelope.Manifest)
	}
	restoreOpts := assurance.RestoreOptions{
		ExpectedTenantID: intent.TenantID, SigningKey: backupKey,
		ExpectedDatabaseSHA256: envelope.Manifest.DatabaseSHA256,
		MinimumAuditSequence:   envelope.Manifest.Audit.LastSequence,
		ExpectedSchemaMarkers:  envelope.Manifest.SchemaMarkers,
		RequireFenceHighWater:  true,
		CheckpointPublicKey:    checkpointPublic,
	}
	seal, err := backup.UploadEnvelope(ctx, envelopeDir, restoreOpts)
	if err != nil {
		_ = sourceDB.Close()
		t.Fatalf("upload immutable Blob envelope: %v", err)
	}
	if seal.DatabaseSHA256 != envelope.Manifest.DatabaseSHA256 || seal.ManifestETag == "" || seal.DatabaseETag == "" {
		_ = sourceDB.Close()
		t.Fatalf("Blob seal lacks verified hashes/ETags: %+v", seal)
	}
	if err := sourceDB.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{sourcePath, sourcePath + "-wal", sourcePath + "-shm", envelope.DatabasePath, envelope.DatabasePath + "-wal", envelope.DatabasePath + "-shm", envelope.ManifestPath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destroy exact disposable source artifact %q: %v", path, err)
		}
	}
	if err := os.Remove(envelopeDir); err != nil {
		t.Fatalf("remove disposable envelope directory: %v", err)
	}
	if _, err := os.Stat(sourcePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source DB still exists after destroy: %v", err)
	}
	if _, err := os.Stat(envelopeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local backup still exists after destroy: %v", err)
	}

	cleanPath := filepath.Join(workDir, "restored-clean.db")
	restored, scan, err := RestoreSelectedEnvelopeAndScan(ctx, backup, seal.EnvelopeID, cleanPath, restoreOpts, fence, "env-a")
	if err != nil || !restored.Ready || !scan.Ready || scan.Findings != 0 || scan.Quarantined != 0 {
		t.Fatalf("clean Blob restore/join failed: restore=%+v scan=%+v err=%v", restored, scan, err)
	}
	if restored.DatabaseSHA256 != envelope.Manifest.DatabaseSHA256 || restored.Manifest.Audit.LastSequence != envelope.Manifest.Audit.LastSequence || restored.Manifest.FenceHighWater == nil {
		t.Fatalf("restored backup evidence differs: %+v", restored.Manifest)
	}
	cleanDB, err := sqlitedb.Open(cleanPath)
	if err != nil {
		t.Fatal(err)
	}
	cleanStore, err := NewWithDB(cleanDB)
	if err != nil {
		_ = cleanDB.Close()
		t.Fatal(err)
	}
	cleanTransfer, err := cleanStore.Get(ctx, intent.TenantID, intent.ID)
	if err != nil || cleanTransfer.State != StateMachineVerified {
		_ = cleanDB.Close()
		t.Fatalf("old terminal transfer was not preserved: %+v err=%v", cleanTransfer, err)
	}
	if !time.Now().Before(cleanTransfer.Intent.ExpiresAt) {
		_ = cleanDB.Close()
		t.Fatal("test fixture's staged confirmation expired before the restore CAS check")
	}
	if ok, err := cleanStore.Claim(ctx, Claim{TenantID: intent.TenantID, ActorID: intent.ActorID, Permission: intent.Permission, TransferID: intent.ID, Token: "token", LeaseID: "post-restore", Now: claimTime, LeaseUntil: claimTime.Add(time.Minute)}); err != nil || ok {
		_ = cleanDB.Close()
		t.Fatalf("old terminal transfer became claimable after restore: claimed=%v err=%v", ok, err)
	}
	if err := cleanDB.Close(); err != nil {
		t.Fatal(err)
	}

	removedTerminal, missingTerminal := fixture.removeFence(intent.ID, "terminal")
	if !missingTerminal {
		t.Fatal("test fixture did not contain terminal fence")
	}
	quarantinePath := filepath.Join(workDir, "restored-quarantine.db")
	quarantinedRestore, quarantinedScan, err := RestoreSelectedEnvelopeAndScan(ctx, backup, seal.EnvelopeID, quarantinePath, restoreOpts, fence, "env-a")
	if err != nil || quarantinedRestore.Ready || quarantinedScan.Ready || quarantinedScan.Findings == 0 || quarantinedScan.Quarantined == 0 {
		t.Fatalf("missing terminal was not quarantined: restore=%+v scan=%+v err=%v", quarantinedRestore, quarantinedScan, err)
	}
	quarantineDB, err := sqlitedb.Open(quarantinePath)
	if err != nil {
		t.Fatal(err)
	}
	quarantineStore, err := NewWithDB(quarantineDB)
	if err != nil {
		_ = quarantineDB.Close()
		t.Fatal(err)
	}
	quarantinedTransfer, err := quarantineStore.Get(ctx, intent.TenantID, intent.ID)
	if err != nil || quarantinedTransfer.State != StateReconciliationRequired {
		_ = quarantineDB.Close()
		t.Fatalf("quarantine did not freeze transfer: %+v err=%v", quarantinedTransfer, err)
	}
	if ok, err := quarantineStore.Claim(ctx, Claim{TenantID: intent.TenantID, ActorID: intent.ActorID, Permission: intent.Permission, TransferID: intent.ID, Token: "token", LeaseID: "quarantined-retry", Now: claimTime, LeaseUntil: claimTime.Add(time.Minute)}); err != nil || ok {
		_ = quarantineDB.Close()
		t.Fatalf("quarantined old transfer became claimable: claimed=%v err=%v", ok, err)
	}
	var quarantineRows, auditRows int
	if err := quarantineDB.QueryRow(`SELECT count(*) FROM assurance_transfer_startup_quarantines WHERE tenant_id=? AND transfer_id=?`, intent.TenantID, intent.ID).Scan(&quarantineRows); err != nil {
		_ = quarantineDB.Close()
		t.Fatal(err)
	}
	if err := quarantineDB.QueryRow(`SELECT count(*) FROM audit_log WHERE tenant_id=? AND action='startup_quarantine'`, intent.TenantID).Scan(&auditRows); err != nil {
		_ = quarantineDB.Close()
		t.Fatal(err)
	}
	if quarantineRows != 1 || auditRows != 1 {
		_ = quarantineDB.Close()
		t.Fatalf("durable quarantine/audit counts=%d/%d", quarantineRows, auditRows)
	}
	if err := quarantineDB.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedQuarantine, err := sqlitedb.Open(quarantinePath)
	if err != nil {
		t.Fatalf("quarantine did not survive a database reopen: %v", err)
	}
	if err := reopenedQuarantine.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedQuarantine, err = sqlitedb.Open(quarantinePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopenedQuarantine.QueryRow(`SELECT count(*) FROM assurance_transfer_startup_quarantines WHERE tenant_id=? AND transfer_id=?`, intent.TenantID, intent.ID).Scan(&quarantineRows); err != nil {
		_ = reopenedQuarantine.Close()
		t.Fatal(err)
	}
	if err := reopenedQuarantine.QueryRow(`SELECT count(*) FROM audit_log WHERE tenant_id=? AND action='startup_quarantine'`, intent.TenantID).Scan(&auditRows); err != nil {
		_ = reopenedQuarantine.Close()
		t.Fatal(err)
	}
	if quarantineRows != 1 || auditRows != 1 {
		_ = reopenedQuarantine.Close()
		t.Fatalf("reopened quarantine/audit counts=%d/%d", quarantineRows, auditRows)
	}
	if err := reopenedQuarantine.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.restoreFence(fixture.objectPath(intent.ID, "terminal"), removedTerminal)

	terminalPath := fixture.objectPath(intent.ID, "terminal")
	fixture.mutateFence(terminalPath, func(object *recoveryBlobObject) {
		object.body = []byte("tampered fence bytes")
		object.etag = `"tampered"`
	})
	tamperPath := filepath.Join(workDir, "restored-tamper.db")
	tamperedRestore, _, tamperErr := RestoreSelectedEnvelopeAndScan(ctx, backup, seal.EnvelopeID, tamperPath, restoreOpts, fence, "env-a")
	if tamperErr == nil || !strings.Contains(tamperErr.Error(), "Azure fence scan") || tamperedRestore.Ready {
		t.Fatalf("tampered fence admitted: restore=%+v err=%v", tamperedRestore, tamperErr)
	}
	fixture.restoreFence(terminalPath, removedTerminal)
	fixture.mutateFence(terminalPath, func(object *recoveryBlobObject) {
		object.body = append([]byte(nil), inventory.objects[1].Body...)
		object.etag = `"restored-terminal"`
		object.lastModified = highWaterTime(highWater).Add(time.Second)
	})
	newerPath := filepath.Join(workDir, "restored-newer-fence.db")
	newerRestore, _, newerErr := RestoreSelectedEnvelopeAndScan(ctx, backup, seal.EnvelopeID, newerPath, restoreOpts, fence, "env-a")
	if newerErr == nil || !strings.Contains(newerErr.Error(), "newer than restore high-water") || newerRestore.Ready {
		t.Fatalf("fence newer than signed high-water admitted: restore=%+v err=%v", newerRestore, newerErr)
	}
}

func (f *recoveryBlobFake) objectPath(id, kind string) string {
	return "/private/" + restoreScanObjectPath("tenant-a", "env-a", id, kind)
}

func (f *recoveryBlobFake) removeFence(id, kind string) (recoveryBlobObject, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := f.objectPath(id, kind)
	object, ok := f.objects[path]
	delete(f.objects, path)
	return object, ok
}

func (f *recoveryBlobFake) restoreFence(path string, object recoveryBlobObject) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[path] = object
}

func (f *recoveryBlobFake) mutateFence(path string, mutate func(*recoveryBlobObject)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object := f.objects[path]
	mutate(&object)
	f.objects[path] = object
}

func highWaterTime(mark assurance.BackupFenceHighWater) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, mark.CapturedAt)
	return parsed
}
