package transfer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/workiva"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

// This exercises the real transfer service around a durable database restore:
// the provider performs real test-transport reads, one POST, polling, and readback
// before backup, and the same counters must remain unchanged through admission
// and both confirmation replay cases after restore.
func TestServiceConfirmationReplaySurvivesAzureBackupDestroyRestore(t *testing.T) {
	ctx := context.Background()
	const tenant = "11111111-1111-1111-1111-111111111111"
	store, _ := testStore(t)
	fixture := &serviceFixture{value: `{"kind":"number","number":"1"}`, op: "https://provider.invalid/operations/1"}
	initialRouter, err := workivaprovider.NewRouterChecked(fixture)
	if err != nil {
		t.Fatal(err)
	}
	assuranceStore, stageCtx, signedBundlePublicKey := activateTransferRouteWithTrust(t, store)
	fixture.ctx = stageCtx
	svc, err := NewTestService(store, assuranceStore, initialRouter, &FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	stageKey, stageRequest := "distinct-stage-ingress-key", "canonical-stage-request"
	staged, err := svc.Stage(fixture.ctx, StageRequest{RouteID: "energy-copy", IdempotencyDigest: stageKey, RequestDigest: stageRequest, Now: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	provider := &recoveryInstrumentedProvider{serviceFixture: fixture}
	router, err := newRecoveryProviderRouter(provider)
	if err != nil {
		t.Fatal(err)
	}
	svc.provider = router
	serverFixture := newRecoveryBlobFake()
	serverFixture.service = time.Now().UTC().Add(2 * time.Second).Truncate(time.Second)
	fencePrefix := "env/env-a/tenant/" + digest(tenant) + "/fences/"
	fenceRequests := &recoveryFenceRequestCounts{prefix: fencePrefix}
	server := newScanHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fenceRequests.observe(r)
		serverFixture.serve(w, r)
	}))
	defer server.Close()
	fence := newProviderRecoveryFence(t, server.URL, tenant)
	svc.fence = fence

	confirmCtx := b1TrustedConfirmContext(tenant)
	req := ConfirmRequest{TransferID: staged.TransferID, Token: staged.Token, IdempotencyDigest: "distinct-confirm-ingress-key", RequestDigest: "canonical-confirm-request", Now: time.Now().UTC()}
	confirmed, err := svc.Confirm(confirmCtx, req)
	if err != nil || confirmed.State != StateMachineVerified {
		t.Fatalf("confirm staged transfer: state=%s err=%v", confirmed.State, err)
	}
	preBackupReads, preBackupPosts, preBackupPolls := provider.reads.Load(), provider.posts.Load(), provider.polls.Load()
	if preBackupReads == 0 || preBackupPosts != 1 || preBackupPolls == 0 {
		t.Fatalf("positive-control provider activity missing: reads=%d posts=%d polls=%d", preBackupReads, preBackupPosts, preBackupPolls)
	}
	preBackupFenceObjects := serverFixture.fenceObjectCount(fencePrefix)

	highWater, err := fence.CaptureHighWater(ctx, tenant)
	if err != nil {
		t.Fatalf("capture Blob fence high-water: %v", err)
	}
	checkpointPublic, checkpointPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	operator := identity.ContextWithPrincipal(ctx, identity.Principal{TenantID: tenant, ObjectID: "actor-1"})
	auditLog, err := audit.NewWithDB(store.db)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := auditLog.Append(operator, audit.Entry{Actor: "actor-1", Tool: "provider_recovery_test", Action: "backup", Target: staged.TransferID})
	if err != nil {
		t.Fatal(err)
	}
	checkpointSig := ed25519.Sign(checkpointPrivate, []byte(strings.Join([]string{tenant, strconv.FormatInt(entry.Seq, 10), entry.Hash, "audit-terminal"}, "\x00")))
	if _, err := store.db.Exec(`INSERT INTO assurance_checkpoints (tenant_id, checkpoint_id, checkpoint_kind, high_water_mark, checkpoint_hash, signature_hex, metadata_json, created_at) VALUES (?, 'audit-terminal', 'terminal', ?, ?, ?, '{}', CURRENT_TIMESTAMP)`, tenant, entry.Seq, entry.Hash, hex.EncodeToString(checkpointSig)); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	sourcePath := filepath.Join(workDir, "source.db")
	if _, err := store.db.ExecContext(ctx, `VACUUM INTO ?`, sourcePath); err != nil {
		t.Fatalf("materialize disposable source DB: %v", err)
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	sourceDB, err := sqlitedb.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	backupKey := assurance.SigningKey{HMAC: []byte("provider-recovery-test-signing-key-32")}
	envelopeDir := filepath.Join(workDir, "envelope")
	envelope, err := assurance.CreateBackup(ctx, sourceDB, envelopeDir, assurance.BackupOptions{
		TenantID: tenant, SigningKey: backupKey, FenceHighWater: &highWater, CheckpointPublicKey: checkpointPublic,
	})
	if err != nil {
		_ = sourceDB.Close()
		t.Fatal(err)
	}
	restoreOpts := assurance.RestoreOptions{
		ExpectedTenantID: tenant, SigningKey: backupKey, ExpectedDatabaseSHA256: envelope.Manifest.DatabaseSHA256,
		MinimumAuditSequence: envelope.Manifest.Audit.LastSequence, ExpectedSchemaMarkers: envelope.Manifest.SchemaMarkers,
		RequireFenceHighWater: true, CheckpointPublicKey: checkpointPublic,
	}
	backupTransport := newProviderRecoveryBackup(t, server.URL, tenant)
	seal, err := backupTransport.UploadEnvelope(ctx, envelopeDir, restoreOpts)
	if err != nil {
		_ = sourceDB.Close()
		t.Fatalf("upload immutable signed envelope: %v", err)
	}
	if seal.DatabaseSHA256 != envelope.Manifest.DatabaseSHA256 || seal.ManifestETag == "" || seal.DatabaseETag == "" {
		t.Fatalf("Blob seal lacks verified hashes/ETags: %+v", seal)
	}
	if err := sourceDB.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{sourcePath, sourcePath + "-wal", sourcePath + "-shm", envelope.DatabasePath, envelope.DatabasePath + "-wal", envelope.DatabasePath + "-shm", envelope.ManifestPath} {
		if err := removeRecoveryArtifact(path); err != nil {
			t.Fatalf("destroy disposable source artifact %q: %v", path, err)
		}
	}
	if err := removeRecoveryEnvelopeDir(envelopeDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sourcePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disposable source database still exists after destruction: %v", err)
	}
	if _, err := os.Stat(envelopeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local envelope still exists after destruction: %v", err)
	}
	restoredPath := filepath.Join(workDir, "restored.db")
	restoredEnvelope, scan, err := RestoreSelectedEnvelopeAndScan(ctx, backupTransport, seal.EnvelopeID, restoredPath, restoreOpts, fence, "env-a")
	if err != nil || !restoredEnvelope.Ready || !scan.Ready || scan.Findings != 0 {
		reason := ""
		if db, openErr := sqlitedb.Open(restoredPath); openErr == nil {
			_ = db.QueryRow(`SELECT recovery_reason FROM transfer_intents WHERE transfer_id=?`, staged.TransferID).Scan(&reason)
			_ = db.Close()
		}
		t.Fatalf("restore selected Azure envelope and scan fences: restore_ready=%v scan=%+v reason=%q err=%v", restoredEnvelope.Ready, scan, reason, err)
	}

	restoredDB, err := sqlitedb.Open(restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	restoredStore, err := NewWithDB(restoredDB)
	if err != nil {
		_ = restoredDB.Close()
		t.Fatal(err)
	}
	// Construct a fresh service over the restored SQLite state and same usable
	// provider transport, then prove startup admission and sealed replay are local.
	restoredAssurance, err := assurance.NewWithDB(restoredDB)
	if err != nil {
		_ = restoredDB.Close()
		t.Fatal(err)
	}
	restoredAudit, err := audit.NewWithDB(restoredDB)
	if err != nil {
		_ = restoredDB.Close()
		t.Fatal(err)
	}
	restoredAssurance.SetAuditLog(restoredAudit)
	if err := restoredAssurance.Bootstrap(stageCtx, tenant, signedBundlePublicKey); err != nil {
		_ = restoredDB.Close()
		t.Fatalf("restore trusted signed route configuration: %v", err)
	}
	if err := restoredAssurance.Ready(); err != nil {
		_ = restoredDB.Close()
		t.Fatalf("restored assurance store is not ready before rejection control: %v", err)
	}
	restoredRouter, err := newRecoveryProviderRouter(provider)
	if err != nil {
		_ = restoredDB.Close()
		t.Fatal(err)
	}
	restoredService, err := NewService(restoredStore, restoredAssurance, restoredRouter, fence)
	if err != nil {
		_ = restoredDB.Close()
		t.Fatal(err)
	}
	fenceReadsBeforeReplay := fenceRequests.snapshot()
	readsAtRestore, postsAtRestore, pollsAtRestore := preBackupReads, preBackupPosts, preBackupPolls
	replay, err := restoredService.Confirm(confirmCtx, ConfirmRequest{TransferID: staged.TransferID, IdempotencyDigest: req.IdempotencyDigest, RequestDigest: req.RequestDigest})
	if err != nil {
		_ = restoredDB.Close()
		t.Fatalf("token-free same-key replay after restore: %v", err)
	}
	if !reflect.DeepEqual(replay, confirmed) {
		_ = restoredDB.Close()
		t.Fatalf("restored sealed replay changed result: first=%+v replay=%+v", confirmed, replay)
	}
	if _, err := restoredService.Confirm(confirmCtx, ConfirmRequest{TransferID: staged.TransferID, Token: staged.Token, IdempotencyDigest: "different-confirm-key", RequestDigest: "different-confirm-request", Now: time.Now().UTC()}); err == nil || !strings.Contains(err.Error(), "confirmation claim unavailable") {
		_ = restoredDB.Close()
		t.Fatalf("new-key old-token rejection was not the staged-claim guard: %v", err)
	}
	if reads, posts, polls := provider.reads.Load(), provider.posts.Load(), provider.polls.Load(); reads != readsAtRestore || posts != postsAtRestore || polls != pollsAtRestore {
		_ = restoredDB.Close()
		t.Fatalf("restore/startup/replay performed provider work: reads %d->%d posts %d->%d polls %d->%d", readsAtRestore, reads, postsAtRestore, posts, pollsAtRestore, polls)
	}
	postReplayFenceObjects := serverFixture.fenceObjectCount(fencePrefix)
	if postReplayFenceObjects != preBackupFenceObjects {
		_ = restoredDB.Close()
		t.Fatalf("restore/startup/replay changed immutable Blob fence object count: %d->%d", preBackupFenceObjects, postReplayFenceObjects)
	}
	if after := fenceRequests.snapshot(); after != fenceReadsBeforeReplay {
		_ = restoredDB.Close()
		t.Fatalf("confirmation replays touched Azure fence namespace: before=%+v after=%+v", fenceReadsBeforeReplay, after)
	}
	if confirmed.State != StateMachineVerified || replay.State != StateMachineVerified || replay.OperationReference != confirmed.OperationReference {
		_ = restoredDB.Close()
		t.Fatalf("restored terminal state/evidence changed: confirmed=%+v replay=%+v", confirmed, replay)
	}
	if err := restoredDB.Close(); err != nil {
		t.Fatal(err)
	}
}

func removeRecoveryArtifact(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func removeRecoveryEnvelopeDir(path string) error { return os.Remove(path) }

func newRecoveryProviderRouter(f interface {
	ListSpreadsheets(context.Context) ([]workiva.Spreadsheet, error)
	ListSheets(context.Context, string) ([]workiva.Sheet, error)
	GetSheetData(context.Context, string, string, string, []string) (*workiva.SheetData, error)
	ReadUncached(context.Context, assurance.SourceRequest) (assurance.ProviderRead, error)
	UpdateSheetWithRetryAfter(context.Context, string, string, workiva.SheetUpdate) (string, time.Duration, error)
	WaitOperationWithInitialRetryAfter(context.Context, string, time.Duration) (string, error)
}) (*workivaprovider.Router, error) {
	return workivaprovider.NewRouterChecked(f)
}

type recoveryInstrumentedProvider struct {
	*serviceFixture
	polls atomic.Int32
}

type recoveryFenceRequestCounts struct {
	mu     sync.Mutex
	prefix string
	puts   int
	gets   int
	lists  int
}

type recoveryFenceRequestSnapshot struct{ puts, gets, lists int }

func (c *recoveryFenceRequestCounts) observe(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/private/")
	if r.URL.Query().Get("comp") == "list" {
		if strings.HasPrefix(r.URL.Query().Get("prefix"), c.prefix) {
			c.lists++
		}
		return
	}
	if !strings.HasPrefix(path, c.prefix) {
		return
	}
	switch r.Method {
	case http.MethodPut:
		c.puts++
	case http.MethodGet:
		c.gets++
	}
}

func (c *recoveryFenceRequestCounts) snapshot() recoveryFenceRequestSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return recoveryFenceRequestSnapshot{puts: c.puts, gets: c.gets, lists: c.lists}
}

func (f *recoveryBlobFake) fenceObjectCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for path := range f.objects {
		if strings.HasPrefix(strings.TrimPrefix(path, "/private/"), prefix) {
			n++
		}
	}
	return n
}

func (p *recoveryInstrumentedProvider) WaitOperationWithInitialRetryAfter(ctx context.Context, operation string, delay time.Duration) (string, error) {
	p.polls.Add(1)
	return p.serviceFixture.WaitOperationWithInitialRetryAfter(ctx, operation, delay)
}

func newProviderRecoveryFence(t *testing.T, serverURL, tenant string) *AzureBlobFence {
	t.Helper()
	client, err := azblob.NewClientWithNoCredential(serverURL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: 0}}})
	if err != nil {
		t.Fatal(err)
	}
	fence, err := newAzureBlobFence(client, AzureFenceConfig{ContainerName: "private", TenantID: tenant, EnvironmentDigest: "env-a", Timeout: 5 * time.Second, MaxScanPages: 8, MaxScanObjects: 32, MaxScanBytes: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return fence
}

func newProviderRecoveryBackup(t *testing.T, serverURL, tenant string) *assurance.AzureBackupTransport {
	t.Helper()
	client, err := azblob.NewClientWithNoCredential(serverURL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	transport, err := assurance.NewAzureBackupTransportWithClient(context.Background(), assurance.AzureBackupConfig{ServiceURL: serverURL, ContainerName: "private", TenantID: tenant, EnvironmentDigest: "env-a", Timeout: 5 * time.Second, MaxDatabaseBytes: 32 << 20}, client)
	if err != nil {
		t.Fatal(err)
	}
	return transport
}
