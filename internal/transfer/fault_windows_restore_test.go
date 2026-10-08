package transfer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

type faultFenceObjectSnapshot struct {
	body []byte
	etag string
}

func snapshotFaultFenceObjects(fixture *recoveryBlobFake, prefix string) map[string]faultFenceObjectSnapshot {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	result := make(map[string]faultFenceObjectSnapshot)
	for path, object := range fixture.objects {
		if strings.HasPrefix(strings.TrimPrefix(path, "/private/"), prefix) {
			result[path] = faultFenceObjectSnapshot{body: append([]byte(nil), object.body...), etag: object.etag}
		}
	}
	return result
}

func assertFaultFenceObjectsUnchanged(t *testing.T, fixture *recoveryBlobFake, prefix string, before map[string]faultFenceObjectSnapshot) {
	t.Helper()
	after := snapshotFaultFenceObjects(fixture, prefix)
	if !reflect.DeepEqual(faultFenceObjectNames(before), faultFenceObjectNames(after)) {
		t.Fatalf("immutable fence object set changed: before=%v after=%v", faultFenceObjectNames(before), faultFenceObjectNames(after))
	}
	for path, expected := range before {
		got := after[path]
		if expected.etag != got.etag || !bytes.Equal(expected.body, got.body) {
			t.Fatalf("immutable fence object %q changed: ETag %q->%q, body equal=%t", path, expected.etag, got.etag, bytes.Equal(expected.body, got.body))
		}
	}
}

func faultFenceObjectNames(objects map[string]faultFenceObjectSnapshot) []string {
	names := make([]string, 0, len(objects))
	for name := range objects {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func assertFaultEnvelopeAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local envelope remains before restore at %q: %v", path, err)
	}
}

// snapshotAtProvider202 captures a real signed SQLite backup after the fake
// Workiva call has returned an operation reference, but before Service can
// persist that reference or poll the operation.
type snapshotAtProvider202 struct {
	*recoveryInstrumentedProvider
	capture func(context.Context) error
}

func (p *snapshotAtProvider202) UpdateSheetWithRetryAfter(ctx context.Context, spreadsheetID, sheetID string, update workiva.SheetUpdate) (string, time.Duration, error) {
	operation, retryAfter, err := p.recoveryInstrumentedProvider.UpdateSheetWithRetryAfter(ctx, spreadsheetID, sheetID, update)
	if err != nil {
		return operation, retryAfter, err
	}
	if p.capture != nil {
		if captureErr := p.capture(ctx); captureErr != nil {
			return operation, retryAfter, captureErr
		}
	}
	return operation, 0, errors.New("injected process stop after provider 202")
}

func TestProvider202BeforeOperationPersistenceRestoresToQuarantineWithoutReplay(t *testing.T) {
	const tenant = "11111111-1111-1111-1111-111111111111"
	ctx := context.Background()
	store, _ := testStore(t)
	baseProvider := &serviceFixture{value: `{"kind":"number","number":"1"}`, op: "https://provider.invalid/operations/1"}
	activeAssurance, stageCtx, originalBundlePublic := activateTransferRouteWithTrust(t, store)
	baseProvider.ctx = stageCtx
	confirmedCtx := b1TrustedConfirmContext(tenant)

	blobFixture := newRecoveryBlobFake()
	blobFixture.service = time.Now().UTC().Add(2 * time.Second).Truncate(time.Second)
	blobServer := newScanHTTPServer(t, http.HandlerFunc(blobFixture.serve))
	t.Cleanup(blobServer.Close)
	fence := newProviderRecoveryFence(t, blobServer.URL, tenant)
	backup := newProviderRecoveryBackup(t, blobServer.URL, tenant)

	baseProvider.op = "provider-operation-202"
	instrumented := &recoveryInstrumentedProvider{serviceFixture: baseProvider}
	var envelopeDir string
	var envelopeArtifacts []string
	var seal assurance.BlobBackupSeal
	var restoreOpts assurance.RestoreOptions
	barrier := &snapshotAtProvider202{
		recoveryInstrumentedProvider: instrumented,
		capture: func(callCtx context.Context) error {
			var state, operationReference string
			if err := store.db.QueryRowContext(callCtx, `SELECT state,operation_reference FROM transfer_intents WHERE tenant_id=?`, tenant).Scan(&state, &operationReference); err != nil {
				return err
			}
			if state != string(State("submitting")) || operationReference != "" {
				return errors.New("provider-202 barrier did not observe submitting row with empty operation reference")
			}
			highWater, err := fence.CaptureHighWater(callCtx, tenant)
			if err != nil {
				return err
			}
			checkpointPublic, checkpointPrivate, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				return err
			}
			operator := identity.ContextWithPrincipal(callCtx, identity.Principal{TenantID: tenant, ObjectID: "actor-1"})
			log, err := audit.NewWithDB(store.db)
			if err != nil {
				return err
			}
			entry, err := log.Append(operator, audit.Entry{Actor: "actor-1", Tool: "fault_windows_test", Action: "provider_202_barrier", Target: "pending"})
			if err != nil {
				return err
			}
			message := strings.Join([]string{tenant, strconv.FormatInt(entry.Seq, 10), entry.Hash, "audit-terminal"}, "\x00")
			checkpointSignature := ed25519.Sign(checkpointPrivate, []byte(message))
			if _, err := store.db.ExecContext(callCtx, `INSERT INTO assurance_checkpoints (tenant_id, checkpoint_id, checkpoint_kind, high_water_mark, checkpoint_hash, signature_hex, metadata_json, created_at) VALUES (?, 'audit-terminal', 'terminal', ?, ?, ?, '{}', CURRENT_TIMESTAMP)`, tenant, entry.Seq, entry.Hash, hex.EncodeToString(checkpointSignature)); err != nil {
				return err
			}
			backupKey := assurance.SigningKey{HMAC: []byte("fault-recovery-test-signing-key-32")}
			envelopeDir = filepath.Join(t.TempDir(), "captured-envelope")
			envelope, err := assurance.CreateBackup(callCtx, store.db, envelopeDir, assurance.BackupOptions{TenantID: tenant, SigningKey: backupKey, FenceHighWater: &highWater, CheckpointPublicKey: checkpointPublic})
			if err != nil {
				return err
			}
			envelopeArtifacts = []string{envelope.DatabasePath, envelope.DatabasePath + "-wal", envelope.DatabasePath + "-shm", envelope.ManifestPath}
			restoreOpts = assurance.RestoreOptions{
				ExpectedTenantID: tenant, SigningKey: backupKey,
				ExpectedDatabaseSHA256: envelope.Manifest.DatabaseSHA256,
				MinimumAuditSequence:   envelope.Manifest.Audit.LastSequence,
				ExpectedSchemaMarkers:  envelope.Manifest.SchemaMarkers,
				RequireFenceHighWater:  true, CheckpointPublicKey: checkpointPublic,
			}
			seal, err = backup.UploadEnvelope(callCtx, envelopeDir, restoreOpts)
			if err != nil {
				return err
			}
			return nil
		},
	}
	router, err := newRecoveryProviderRouter(barrier)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewTestService(store, activeAssurance, router, &FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	svc.fence = fence

	stageKey, stageRequest := "fault-window-stage-key", "fault-window-stage-request"
	staged, err := svc.Stage(stageCtx, StageRequest{RouteID: "energy-copy", IdempotencyDigest: stageKey, RequestDigest: stageRequest, Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("stage fixture transfer: %v", err)
	}
	confirmed, err := svc.Confirm(confirmedCtx, ConfirmRequest{TransferID: staged.TransferID, Token: staged.Token, IdempotencyDigest: "fault-window-confirm-key", RequestDigest: "fault-window-confirm-request", Now: time.Now().UTC()})
	if err == nil {
		t.Fatalf("confirm returned cleanly after injected 202 barrier: %+v", confirmed)
	}
	if seal.EnvelopeID == "" {
		t.Fatal("provider 202 barrier did not seal a backup")
	}
	if got := baseProvider.posts.Load(); got != 1 {
		t.Fatalf("initial provider POSTs=%d, want exactly one", got)
	}
	if got := instrumented.polls.Load(); got != 0 {
		t.Fatalf("original provider polls=%d, want zero after injected pre-persist stop", got)
	}
	originalRow, err := store.Get(ctx, tenant, staged.TransferID)
	if err != nil || originalRow.OperationReference != baseProvider.op {
		t.Fatalf("original unknown disposition did not preserve known operation reference %q: row=%+v err=%v", baseProvider.op, originalRow, err)
	}

	// Destroy the captured local copy before restore: recovery must come only
	// from the signed Blob envelope, not a surviving source database or files.
	if err := store.db.Close(); err != nil {
		t.Fatalf("close source database before restore: %v", err)
	}
	for _, path := range envelopeArtifacts {
		if err := removeRecoveryArtifact(path); err != nil {
			t.Fatalf("destroy captured local envelope artifact %q: %v", path, err)
		}
	}
	if err := removeRecoveryEnvelopeDir(envelopeDir); err != nil {
		t.Fatalf("remove empty captured envelope directory: %v", err)
	}
	if _, err := os.Stat(envelopeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local envelope directory remains before restore: %v", err)
	}
	restoredPath := filepath.Join(t.TempDir(), "restored-after-202.db")
	readsBeforeRestore, postsBeforeRestore, pollsBeforeRestore := baseProvider.reads.Load(), baseProvider.posts.Load(), instrumented.polls.Load()
	fencePrefix := "env/env-a/tenant/" + digest(tenant) + "/fences/"
	fenceObjectsBeforeRestore := snapshotFaultFenceObjects(blobFixture, fencePrefix)
	assertFaultEnvelopeAbsent(t, envelopeDir)
	restored, scan, err := RestoreSelectedEnvelopeAndScan(ctx, backup, seal.EnvelopeID, restoredPath, restoreOpts, fence, "env-a")
	if err != nil || restored.Ready || scan.Ready || scan.Findings != 1 || scan.Quarantined != 1 {
		t.Fatalf("pre-op-persist snapshot was not quarantined: restore=%+v scan=%+v err=%v", restored, scan, err)
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
	if err := restoredAssurance.Bootstrap(stageCtx, tenant, originalBundlePublic); err != nil {
		_ = restoredDB.Close()
		t.Fatalf("reopen restored assurance state: %v", err)
	}
	var quarantineReason string
	if err := restoredDB.QueryRowContext(ctx, `SELECT reason FROM assurance_transfer_startup_quarantines WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&quarantineReason); err != nil || !strings.Contains(quarantineReason, "provider_outcome_unknown") {
		_ = restoredDB.Close()
		t.Fatalf("durable transfer quarantine reason=%q err=%v, want provider_outcome_unknown finding", quarantineReason, err)
	}
	transferRow, err := restoredStore.Get(ctx, tenant, staged.TransferID)
	if err != nil || transferRow.State != StateReconciliationRequired || transferRow.OperationReference != "" {
		_ = restoredDB.Close()
		t.Fatalf("restored uncertainty was not frozen without an unpersisted operation ref: %+v err=%v", transferRow, err)
	}
	if got := baseProvider.reads.Load(); got != readsBeforeRestore {
		_ = restoredDB.Close()
		t.Fatalf("provider reads during restore=%d, want unchanged %d", got-readsBeforeRestore, 0)
	}
	if got := baseProvider.posts.Load(); got != postsBeforeRestore {
		_ = restoredDB.Close()
		t.Fatalf("provider POSTs after restore=%d, want unchanged %d", got-postsBeforeRestore, 0)
	}
	if got := instrumented.polls.Load(); got != pollsBeforeRestore {
		_ = restoredDB.Close()
		t.Fatalf("provider polls after restore=%d, want unchanged %d", got-pollsBeforeRestore, 0)
	}
	// Rebind the test service to the restored, quarantined stores and attempt
	// both token-free same-key replay and a fresh-key old-token confirmation.
	// This deliberately exercises the service guard in-process; it does not
	// claim that production startup constructs a service while readiness fails.
	svc.store = restoredStore
	svc.assuranceStore = restoredAssurance
	sameKeyReplay, sameKeyErr := svc.Confirm(confirmedCtx, ConfirmRequest{
		TransferID: staged.TransferID, IdempotencyDigest: "fault-window-confirm-key",
		RequestDigest: "fault-window-confirm-request", Now: time.Now().UTC(),
	})
	if sameKeyErr == nil || sameKeyErr.Error() != "idempotency_in_progress" {
		_ = restoredDB.Close()
		t.Fatalf("token-free same-key replay error=%v, want idempotency_in_progress while the durable reservation remains in progress", sameKeyErr)
	}
	if sameKeyReplay.State != "" {
		_ = restoredDB.Close()
		t.Fatalf("in-progress replay returned a transfer projection: %+v", sameKeyReplay)
	}
	_, freshKeyErr := svc.Confirm(confirmedCtx, ConfirmRequest{
		TransferID: staged.TransferID, Token: staged.Token, IdempotencyDigest: "fresh-after-restore-key",
		RequestDigest: "fresh-after-restore-request", Now: time.Now().UTC(),
	})
	if freshKeyErr == nil {
		_ = restoredDB.Close()
		t.Fatal("old confirmation token with a fresh key was accepted after restore")
	}
	if baseProvider.reads.Load() != readsBeforeRestore || baseProvider.posts.Load() != postsBeforeRestore || instrumented.polls.Load() != pollsBeforeRestore {
		_ = restoredDB.Close()
		t.Fatalf("provider work occurred after restored confirmations: reads=%d posts=%d polls=%d", baseProvider.reads.Load()-readsBeforeRestore, baseProvider.posts.Load()-postsBeforeRestore, instrumented.polls.Load()-pollsBeforeRestore)
	}
	assertFaultFenceObjectsUnchanged(t, blobFixture, fencePrefix, fenceObjectsBeforeRestore)
	if err := restoredDB.Close(); err != nil {
		t.Fatal(err)
	}
}

type snapshotBeforeClaimFence struct {
	Fence
	capture func(context.Context) error
}

func (f *snapshotBeforeClaimFence) CreateClaim(ctx context.Context, id string, body []byte) (string, error) {
	if err := f.capture(ctx); err != nil {
		return "", err
	}
	return "", errors.New("injected process stop before claim-fence creation")
}

type snapshotAfterClaimVerification struct {
	Fence
	capture func(context.Context, string) error
}

func (f *snapshotAfterClaimVerification) VerifyClaim(ctx context.Context, id, claimDigest string, expected ...[]byte) error {
	if err := f.Fence.VerifyClaim(ctx, id, claimDigest, expected...); err != nil {
		return err
	}
	if err := f.capture(ctx, id); err != nil {
		return err
	}
	return errors.New("injected process stop after verified claim, before provider POST")
}

func captureFaultWindowBackup(t *testing.T, ctx context.Context, store *Store, tenant, action string, fence *AzureBlobFence, backup *assurance.AzureBackupTransport) (string, []string, assurance.BlobBackupSeal, assurance.RestoreOptions) {
	t.Helper()
	highWater, err := fence.CaptureHighWater(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	operator := identity.ContextWithPrincipal(ctx, identity.Principal{TenantID: tenant, ObjectID: "actor-1"})
	log, err := audit.NewWithDB(store.db)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := log.Append(operator, audit.Entry{Actor: "actor-1", Tool: "fault_windows_test", Action: action, Target: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	checkpointPublic, checkpointPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	message := strings.Join([]string{tenant, strconv.FormatInt(entry.Seq, 10), entry.Hash, "audit-terminal"}, "\x00")
	checkpointSignature := ed25519.Sign(checkpointPrivate, []byte(message))
	if _, err := store.db.ExecContext(ctx, `INSERT INTO assurance_checkpoints (tenant_id, checkpoint_id, checkpoint_kind, high_water_mark, checkpoint_hash, signature_hex, metadata_json, created_at) VALUES (?, 'audit-terminal', 'terminal', ?, ?, ?, '{}', CURRENT_TIMESTAMP)`, tenant, entry.Seq, entry.Hash, hex.EncodeToString(checkpointSignature)); err != nil {
		t.Fatal(err)
	}
	backupKey := assurance.SigningKey{HMAC: []byte("fault-recovery-test-signing-key-32")}
	envelopeDir := filepath.Join(t.TempDir(), "captured-fault-envelope")
	envelope, err := assurance.CreateBackup(ctx, store.db, envelopeDir, assurance.BackupOptions{TenantID: tenant, SigningKey: backupKey, FenceHighWater: &highWater, CheckpointPublicKey: checkpointPublic})
	if err != nil {
		t.Fatal(err)
	}
	artifacts := []string{envelope.DatabasePath, envelope.DatabasePath + "-wal", envelope.DatabasePath + "-shm", envelope.ManifestPath}
	opts := assurance.RestoreOptions{ExpectedTenantID: tenant, SigningKey: backupKey, ExpectedDatabaseSHA256: envelope.Manifest.DatabaseSHA256, MinimumAuditSequence: envelope.Manifest.Audit.LastSequence, ExpectedSchemaMarkers: envelope.Manifest.SchemaMarkers, RequireFenceHighWater: true, CheckpointPublicKey: checkpointPublic}
	seal, err := backup.UploadEnvelope(ctx, envelopeDir, opts)
	if err != nil {
		t.Fatal(err)
	}
	return envelopeDir, artifacts, seal, opts
}

func destroyFaultWindowLocalEnvelope(t *testing.T, path string, artifacts []string) {
	t.Helper()
	for _, artifact := range artifacts {
		if err := removeRecoveryArtifact(artifact); err != nil {
			t.Fatalf("destroy captured local envelope artifact %q: %v", artifact, err)
		}
	}
	if err := removeRecoveryEnvelopeDir(path); err != nil {
		t.Fatalf("remove empty captured envelope directory: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local envelope directory remains before restore: %v", err)
	}
}

func TestVerifiedClaimBeforeProviderPOSTRestoresToQuarantineWithoutReplay(t *testing.T) {
	const tenant = "11111111-1111-1111-1111-111111111111"
	ctx := context.Background()
	store, _ := testStore(t)
	baseProvider := &serviceFixture{value: `{"kind":"number","number":"1"}`, op: "https://provider.invalid/operations/1"}
	activeAssurance, stageCtx, bundlePublic := activateTransferRouteWithTrust(t, store)
	baseProvider.ctx = stageCtx
	confirmedCtx := b1TrustedConfirmContext(tenant)
	blobFixture := newRecoveryBlobFake()
	blobFixture.service = time.Now().UTC().Add(2 * time.Second).Truncate(time.Second)
	blobServer := newScanHTTPServer(t, http.HandlerFunc(blobFixture.serve))
	t.Cleanup(blobServer.Close)
	realFence := newProviderRecoveryFence(t, blobServer.URL, tenant)
	backup := newProviderRecoveryBackup(t, blobServer.URL, tenant)
	var transferID, envelopeDir string
	var artifacts []string
	var seal assurance.BlobBackupSeal
	var restoreOpts assurance.RestoreOptions
	fence := &snapshotAfterClaimVerification{Fence: realFence, capture: func(callCtx context.Context, id string) error {
		transferID = id
		var state, op string
		if err := store.db.QueryRowContext(callCtx, `SELECT state,operation_reference FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, id).Scan(&state, &op); err != nil {
			return err
		}
		if state != string(StateClaimed) || op != "" {
			return errors.New("verified-claim barrier did not observe claimed row with no operation reference")
		}
		prefix := "env/env-a/tenant/" + digest(tenant) + "/fences/" + id + "/"
		if blobFixture.fenceObjectCount(prefix+"claim.json") != 1 || blobFixture.fenceObjectCount(prefix+"terminal.json") != 0 {
			return errors.New("verified-claim barrier did not observe exactly one claim object and no terminal object")
		}
		envelopeDir, artifacts, seal, restoreOpts = captureFaultWindowBackup(t, callCtx, store, tenant, "verified_claim_before_post", realFence, backup)
		return nil
	}}
	instrumented := &recoveryInstrumentedProvider{serviceFixture: baseProvider}
	router, err := newRecoveryProviderRouter(instrumented)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewTestService(store, activeAssurance, router, &FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	svc.fence = fence
	staged, err := svc.Stage(stageCtx, StageRequest{RouteID: "energy-copy", IdempotencyDigest: "fault-window-verified-stage", RequestDigest: "fault-window-verified-stage-request", Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("stage fixture transfer: %v", err)
	}
	_, confirmErr := svc.Confirm(confirmedCtx, ConfirmRequest{TransferID: staged.TransferID, Token: staged.Token, IdempotencyDigest: "fault-window-confirm-key", RequestDigest: "fault-window-confirm-request", Now: time.Now().UTC()})
	if confirmErr == nil || !strings.Contains(confirmErr.Error(), "claim_fence_unverified") {
		t.Fatalf("confirm error=%v, want fail-closed verified-claim barrier error", confirmErr)
	}
	if transferID != staged.TransferID || seal.EnvelopeID == "" {
		t.Fatal("verified-claim barrier did not capture the intended transfer backup")
	}
	if baseProvider.posts.Load() != 0 || instrumented.polls.Load() != 0 {
		t.Fatalf("provider mutation/poll before POST barrier: posts=%d polls=%d", baseProvider.posts.Load(), instrumented.polls.Load())
	}
	if err := store.db.Close(); err != nil {
		t.Fatalf("close source database before restore: %v", err)
	}
	destroyFaultWindowLocalEnvelope(t, envelopeDir, artifacts)
	restoredPath := filepath.Join(t.TempDir(), "restored-after-verified-claim.db")
	readsBeforeRestore, postsBeforeRestore, pollsBeforeRestore := baseProvider.reads.Load(), baseProvider.posts.Load(), instrumented.polls.Load()
	fencePrefix := "env/env-a/tenant/" + digest(tenant) + "/fences/"
	fenceObjectsBeforeRestore := snapshotFaultFenceObjects(blobFixture, fencePrefix)
	assertFaultEnvelopeAbsent(t, envelopeDir)
	restored, scan, err := RestoreSelectedEnvelopeAndScan(ctx, backup, seal.EnvelopeID, restoredPath, restoreOpts, realFence, "env-a")
	if err != nil || restored.Ready || scan.Ready || scan.Findings != 1 || scan.Quarantined != 1 {
		t.Fatalf("verified-claim snapshot was not quarantined: restore=%+v scan=%+v err=%v", restored, scan, err)
	}
	restoredDB, err := sqlitedb.Open(restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restoredDB.Close() }()
	restoredStore, err := NewWithDB(restoredDB)
	if err != nil {
		t.Fatal(err)
	}
	row, err := restoredStore.Get(ctx, tenant, staged.TransferID)
	if err != nil || row.State != StateReconciliationRequired || row.OperationReference != "" {
		t.Fatalf("restored verified-claim row disposition=%+v err=%v", row, err)
	}
	var reason string
	if err := restoredDB.QueryRowContext(ctx, `SELECT reason FROM assurance_transfer_startup_quarantines WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&reason); err != nil || (!strings.Contains(reason, "provider_outcome_unknown") && !strings.Contains(reason, "missing_or_mismatched_claim_fence")) {
		t.Fatalf("durable verified-claim quarantine reason=%q err=%v", reason, err)
	}
	prefix := "env/env-a/tenant/" + digest(tenant) + "/fences/" + staged.TransferID + "/"
	if blobFixture.fenceObjectCount(prefix+"claim.json") != 1 || blobFixture.fenceObjectCount(prefix+"terminal.json") != 0 {
		t.Fatalf("restore changed immutable fence inventory: claim=%d terminal=%d", blobFixture.fenceObjectCount(prefix+"claim.json"), blobFixture.fenceObjectCount(prefix+"terminal.json"))
	}
	restoredAssurance, err := assurance.NewWithDB(restoredDB)
	if err != nil {
		t.Fatal(err)
	}
	restoredAudit, err := audit.NewWithDB(restoredDB)
	if err != nil {
		t.Fatal(err)
	}
	restoredAssurance.SetAuditLog(restoredAudit)
	if err := restoredAssurance.Bootstrap(stageCtx, tenant, bundlePublic); err != nil {
		t.Fatalf("bootstrap restored assurance: %v", err)
	}
	svc.store, svc.assuranceStore = restoredStore, restoredAssurance
	replay, replayErr := svc.Confirm(confirmedCtx, ConfirmRequest{TransferID: staged.TransferID, IdempotencyDigest: "fault-window-confirm-key", RequestDigest: "fault-window-confirm-request", Now: time.Now().UTC()})
	if replayErr == nil || replayErr.Error() != "idempotency_in_progress" || replay.State != "" {
		t.Fatalf("token-free same-key replay=%+v err=%v, want exact idempotency_in_progress", replay, replayErr)
	}
	if _, err := svc.Confirm(confirmedCtx, ConfirmRequest{TransferID: staged.TransferID, Token: staged.Token, IdempotencyDigest: "fresh-after-restore-key", RequestDigest: "fresh-after-restore-request", Now: time.Now().UTC()}); err == nil {
		t.Fatal("old confirmation token with fresh key accepted after restore")
	}
	if baseProvider.reads.Load() != readsBeforeRestore || baseProvider.posts.Load() != postsBeforeRestore || instrumented.polls.Load() != pollsBeforeRestore {
		t.Fatalf("provider work during restore/replay: reads=%d posts=%d polls=%d", baseProvider.reads.Load()-readsBeforeRestore, baseProvider.posts.Load()-postsBeforeRestore, instrumented.polls.Load()-pollsBeforeRestore)
	}
	assertFaultFenceObjectsUnchanged(t, blobFixture, fencePrefix, fenceObjectsBeforeRestore)
}

func TestLocalClaimBeforeFenceCreationRestoresToQuarantineWithoutReplay(t *testing.T) {
	const tenant = "11111111-1111-1111-1111-111111111111"
	ctx := context.Background()
	store, _ := testStore(t)
	baseProvider := &serviceFixture{value: `{"kind":"number","number":"1"}`, op: "https://provider.invalid/operations/1"}
	activeAssurance, stageCtx, bundlePublic := activateTransferRouteWithTrust(t, store)
	baseProvider.ctx = stageCtx
	confirmedCtx := b1TrustedConfirmContext(tenant)
	blobFixture := newRecoveryBlobFake()
	blobFixture.service = time.Now().UTC().Add(2 * time.Second).Truncate(time.Second)
	blobServer := newScanHTTPServer(t, http.HandlerFunc(blobFixture.serve))
	t.Cleanup(blobServer.Close)
	realFence := newProviderRecoveryFence(t, blobServer.URL, tenant)
	backup := newProviderRecoveryBackup(t, blobServer.URL, tenant)
	var envelopeDir string
	var envelopeArtifacts []string
	var transferID string
	var seal assurance.BlobBackupSeal
	var restoreOpts assurance.RestoreOptions
	fence := &snapshotBeforeClaimFence{Fence: realFence, capture: func(callCtx context.Context) error {
		var state string
		if err := store.db.QueryRowContext(callCtx, `SELECT state FROM transfer_intents WHERE tenant_id=?`, tenant).Scan(&state); err != nil {
			return err
		}
		if state != string(StateClaimed) {
			return errors.New("claim barrier did not observe committed claimed state")
		}
		if transferID == "" || blobFixture.fenceObjectCount("env/env-a/tenant/"+digest(tenant)+"/fences/"+transferID+"/") != 0 {
			return errors.New("claim or terminal fence exists at the local-claim barrier")
		}
		operator := identity.ContextWithPrincipal(callCtx, identity.Principal{TenantID: tenant, ObjectID: "actor-1"})
		log, err := audit.NewWithDB(store.db)
		if err != nil {
			return err
		}
		entry, err := log.Append(operator, audit.Entry{Actor: "actor-1", Tool: "fault_windows_test", Action: "claim_before_fence", Target: "pending"})
		if err != nil {
			return err
		}
		checkpointPublic, checkpointPrivate, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		message := strings.Join([]string{tenant, strconv.FormatInt(entry.Seq, 10), entry.Hash, "audit-terminal"}, "\x00")
		checkpointSignature := ed25519.Sign(checkpointPrivate, []byte(message))
		if _, err := store.db.ExecContext(callCtx, `INSERT INTO assurance_checkpoints (tenant_id, checkpoint_id, checkpoint_kind, high_water_mark, checkpoint_hash, signature_hex, metadata_json, created_at) VALUES (?, 'audit-terminal', 'terminal', ?, ?, ?, '{}', CURRENT_TIMESTAMP)`, tenant, entry.Seq, entry.Hash, hex.EncodeToString(checkpointSignature)); err != nil {
			return err
		}
		highWater, err := realFence.CaptureHighWater(callCtx, tenant)
		if err != nil {
			return err
		}
		backupKey := assurance.SigningKey{HMAC: []byte("fault-recovery-test-signing-key-32")}
		envelopeDir = filepath.Join(t.TempDir(), "captured-claim-envelope")
		envelope, err := assurance.CreateBackup(callCtx, store.db, envelopeDir, assurance.BackupOptions{TenantID: tenant, SigningKey: backupKey, FenceHighWater: &highWater, CheckpointPublicKey: checkpointPublic})
		if err != nil {
			return err
		}
		envelopeArtifacts = []string{envelope.DatabasePath, envelope.DatabasePath + "-wal", envelope.DatabasePath + "-shm", envelope.ManifestPath}
		restoreOpts = assurance.RestoreOptions{ExpectedTenantID: tenant, SigningKey: backupKey, ExpectedDatabaseSHA256: envelope.Manifest.DatabaseSHA256, MinimumAuditSequence: envelope.Manifest.Audit.LastSequence, ExpectedSchemaMarkers: envelope.Manifest.SchemaMarkers, RequireFenceHighWater: true, CheckpointPublicKey: checkpointPublic}
		seal, err = backup.UploadEnvelope(callCtx, envelopeDir, restoreOpts)
		return err
	}}
	instrumented := &recoveryInstrumentedProvider{serviceFixture: baseProvider}
	router, err := newRecoveryProviderRouter(instrumented)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewTestService(store, activeAssurance, router, &FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	svc.fence = fence
	stageKey, stageRequest := "fault-window-claim-stage-key", "fault-window-claim-stage-request"
	staged, err := svc.Stage(stageCtx, StageRequest{RouteID: "energy-copy", IdempotencyDigest: stageKey, RequestDigest: stageRequest, Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("stage fixture transfer: %v", err)
	}
	transferID = staged.TransferID
	_, confirmErr := svc.Confirm(confirmedCtx, ConfirmRequest{TransferID: staged.TransferID, Token: staged.Token, IdempotencyDigest: "fault-window-confirm-key", RequestDigest: "fault-window-confirm-request", Now: time.Now().UTC()})
	if confirmErr == nil || !strings.Contains(confirmErr.Error(), "claim_fence_unavailable") {
		t.Fatalf("confirm error=%v, want fail-closed claim-fence failure after snapshot barrier", confirmErr)
	}
	if seal.EnvelopeID == "" {
		t.Fatal("local-claim barrier did not upload signed backup")
	}
	var state, claimDigest string
	if err := store.db.QueryRowContext(ctx, `SELECT state,claim_fence_digest FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&state, &claimDigest); err != nil || state != string(StateReconciliationRequired) || claimDigest != "" {
		t.Fatalf("post-failure local claim disposition state=%q digest=%q err=%v", state, claimDigest, err)
	}
	if baseProvider.posts.Load() != 0 || instrumented.polls.Load() != 0 {
		t.Fatalf("provider mutation/poll before claim-fence creation: posts=%d polls=%d", baseProvider.posts.Load(), instrumented.polls.Load())
	}
	if err := store.db.Close(); err != nil {
		t.Fatalf("close source database before restore: %v", err)
	}
	destroyFaultWindowLocalEnvelope(t, envelopeDir, envelopeArtifacts)
	restoredPath := filepath.Join(t.TempDir(), "restored-after-claim.db")
	readsBeforeRestore, postsBeforeRestore, pollsBeforeRestore := baseProvider.reads.Load(), baseProvider.posts.Load(), instrumented.polls.Load()
	fencePrefix := "env/env-a/tenant/" + digest(tenant) + "/fences/"
	fenceObjectsBeforeRestore := snapshotFaultFenceObjects(blobFixture, fencePrefix)
	assertFaultEnvelopeAbsent(t, envelopeDir)
	restored, scan, err := RestoreSelectedEnvelopeAndScan(ctx, backup, seal.EnvelopeID, restoredPath, restoreOpts, realFence, "env-a")
	if err != nil || restored.Ready || scan.Ready || scan.Findings != 1 || scan.Quarantined != 1 {
		t.Fatalf("pre-claim-fence snapshot was not quarantined: restore=%+v scan=%+v err=%v", restored, scan, err)
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
	restoredRow, err := restoredStore.Get(ctx, tenant, staged.TransferID)
	if err != nil || restoredRow.State != StateReconciliationRequired || restoredRow.ClaimFenceDigest != "" {
		_ = restoredDB.Close()
		t.Fatalf("restored claimed row disposition=%+v err=%v", restoredRow, err)
	}
	var reason string
	if err := restoredDB.QueryRowContext(ctx, `SELECT reason FROM assurance_transfer_startup_quarantines WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&reason); err != nil || !strings.Contains(reason, "missing_or_mismatched_claim_fence") {
		_ = restoredDB.Close()
		t.Fatalf("durable missing-fence quarantine reason=%q err=%v", reason, err)
	}
	prefix := "env/env-a/tenant/" + digest(tenant) + "/fences/" + staged.TransferID + "/"
	if got := blobFixture.fenceObjectCount(prefix); got != 0 {
		t.Fatalf("claim/terminal fences unexpectedly exist at/after restore: count=%d", got)
	}
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
	if err := restoredAssurance.Bootstrap(stageCtx, tenant, bundlePublic); err != nil {
		_ = restoredDB.Close()
		t.Fatalf("bootstrap restored assurance: %v", err)
	}
	svc.store = restoredStore
	svc.assuranceStore = restoredAssurance
	sameKeyReplay, sameKeyErr := svc.Confirm(confirmedCtx, ConfirmRequest{TransferID: staged.TransferID, IdempotencyDigest: "fault-window-confirm-key", RequestDigest: "fault-window-confirm-request", Now: time.Now().UTC()})
	if sameKeyErr == nil || sameKeyErr.Error() != "idempotency_in_progress" || sameKeyReplay.State != "" {
		_ = restoredDB.Close()
		t.Fatalf("same-key replay result=%+v err=%v, want exact idempotency_in_progress", sameKeyReplay, sameKeyErr)
	}
	_, freshKeyErr := svc.Confirm(confirmedCtx, ConfirmRequest{TransferID: staged.TransferID, Token: staged.Token, IdempotencyDigest: "fresh-after-restore-key", RequestDigest: "fresh-after-restore-request", Now: time.Now().UTC()})
	if freshKeyErr == nil {
		_ = restoredDB.Close()
		t.Fatal("old confirmation token with fresh key accepted after restore")
	}
	if baseProvider.reads.Load() != readsBeforeRestore || baseProvider.posts.Load() != postsBeforeRestore || instrumented.polls.Load() != pollsBeforeRestore {
		_ = restoredDB.Close()
		t.Fatalf("provider work after restore/replay: reads=%d posts=%d polls=%d", baseProvider.reads.Load()-readsBeforeRestore, baseProvider.posts.Load()-postsBeforeRestore, instrumented.polls.Load()-pollsBeforeRestore)
	}
	assertFaultFenceObjectsUnchanged(t, blobFixture, fencePrefix, fenceObjectsBeforeRestore)
	if err := restoredDB.Close(); err != nil {
		t.Fatal(err)
	}
}

type snapshotBeforeOperationPoll struct {
	*recoveryInstrumentedProvider
	capture func(context.Context, string) error
}

func (p *snapshotBeforeOperationPoll) WaitOperationWithInitialRetryAfter(ctx context.Context, operation string, delay time.Duration) (string, error) {
	if err := p.capture(ctx, operation); err != nil {
		return "", err
	}
	return "", errors.New("injected process stop after operation persistence, before polling")
}

type snapshotBeforeLocalTerminalCommit struct {
	Fence
	capture func(context.Context, string) error
}

func (f *snapshotBeforeLocalTerminalCommit) VerifyTerminal(ctx context.Context, id, terminalDigest string, expected ...[]byte) error {
	if err := f.Fence.VerifyTerminal(ctx, id, terminalDigest, expected...); err != nil {
		return err
	}
	if err := f.capture(ctx, id); err != nil {
		return err
	}
	return errors.New("injected process stop after terminal fence verification, before local final transaction")
}

func TestOperationReferenceBeforePollRestoresToQuarantineWithoutReplay(t *testing.T) {
	const tenant = "11111111-1111-1111-1111-111111111111"
	ctx := context.Background()
	store, _ := testStore(t)
	baseProvider := &serviceFixture{value: `{"kind":"number","number":"1"}`, op: "provider-operation-persisted"}
	activeAssurance, stageCtx, bundlePublic := activateTransferRouteWithTrust(t, store)
	baseProvider.ctx = stageCtx
	confirmedCtx := b1TrustedConfirmContext(tenant)
	blobFixture := newRecoveryBlobFake()
	blobFixture.service = time.Now().UTC().Add(2 * time.Second).Truncate(time.Second)
	blobServer := newScanHTTPServer(t, http.HandlerFunc(blobFixture.serve))
	t.Cleanup(blobServer.Close)
	realFence := newProviderRecoveryFence(t, blobServer.URL, tenant)
	backup := newProviderRecoveryBackup(t, blobServer.URL, tenant)
	var transferID, envelopeDir string
	var artifacts []string
	var seal assurance.BlobBackupSeal
	var restoreOpts assurance.RestoreOptions
	instrumented := &recoveryInstrumentedProvider{serviceFixture: baseProvider}
	provider := &snapshotBeforeOperationPoll{recoveryInstrumentedProvider: instrumented, capture: func(callCtx context.Context, operation string) error {
		var state, gotOperation, claimDigest, terminalDigest string
		if err := store.db.QueryRowContext(callCtx, `SELECT state,operation_reference,claim_fence_digest,terminal_fence_digest FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, transferID).Scan(&state, &gotOperation, &claimDigest, &terminalDigest); err != nil {
			return err
		}
		if state != string(State("submitting")) || gotOperation != operation || claimDigest == "" || terminalDigest != "" || operation != baseProvider.op {
			return errors.New("operation-before-poll barrier observed unexpected persisted row bindings")
		}
		prefix := "env/env-a/tenant/" + digest(tenant) + "/fences/" + transferID + "/"
		if blobFixture.fenceObjectCount(prefix+"claim.json") != 1 || blobFixture.fenceObjectCount(prefix+"terminal.json") != 0 {
			return errors.New("operation-before-poll barrier fence inventory mismatch")
		}
		envelopeDir, artifacts, seal, restoreOpts = captureFaultWindowBackup(t, callCtx, store, tenant, "operation_reference_before_poll", realFence, backup)
		return nil
	}}
	router, err := newRecoveryProviderRouter(provider)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewTestService(store, activeAssurance, router, &FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	svc.fence = realFence
	staged, err := svc.Stage(stageCtx, StageRequest{RouteID: "energy-copy", IdempotencyDigest: "fault-window-poll-stage", RequestDigest: "fault-window-poll-stage-request", Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("stage fixture transfer: %v", err)
	}
	transferID = staged.TransferID
	_, confirmErr := svc.Confirm(confirmedCtx, ConfirmRequest{TransferID: transferID, Token: staged.Token, IdempotencyDigest: "fault-window-confirm-key", RequestDigest: "fault-window-confirm-request", Now: time.Now().UTC()})
	if confirmErr == nil || !strings.Contains(confirmErr.Error(), "poll_outcome_unknown") {
		t.Fatalf("confirm error=%v, want failure at persisted-operation pre-poll barrier", confirmErr)
	}
	if seal.EnvelopeID == "" || baseProvider.posts.Load() != 1 || instrumented.polls.Load() != 0 {
		t.Fatalf("pre-poll barrier outcome: envelope=%q posts=%d polls=%d", seal.EnvelopeID, baseProvider.posts.Load(), instrumented.polls.Load())
	}
	if err := store.db.Close(); err != nil {
		t.Fatalf("close source database before restore: %v", err)
	}
	destroyFaultWindowLocalEnvelope(t, envelopeDir, artifacts)
	restoredPath := filepath.Join(t.TempDir(), "restored-after-op-persist.db")
	readsBeforeRestore, postsBeforeRestore, pollsBeforeRestore := baseProvider.reads.Load(), baseProvider.posts.Load(), instrumented.polls.Load()
	fencePrefix := "env/env-a/tenant/" + digest(tenant) + "/fences/"
	fenceObjectsBeforeRestore := snapshotFaultFenceObjects(blobFixture, fencePrefix)
	assertFaultEnvelopeAbsent(t, envelopeDir)
	restored, scan, err := RestoreSelectedEnvelopeAndScan(ctx, backup, seal.EnvelopeID, restoredPath, restoreOpts, realFence, "env-a")
	if err != nil || restored.Ready || scan.Ready || scan.Findings != 1 || scan.Quarantined != 1 {
		t.Fatalf("operation-before-poll snapshot was not quarantined: restore=%+v scan=%+v err=%v", restored, scan, err)
	}
	restoredDB, err := sqlitedb.Open(restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restoredDB.Close() }()
	restoredStore, err := NewWithDB(restoredDB)
	if err != nil {
		t.Fatal(err)
	}
	row, err := restoredStore.Get(ctx, tenant, transferID)
	if err != nil || row.State != StateReconciliationRequired || row.OperationReference != baseProvider.op || row.ClaimFenceDigest == "" || row.TerminalFenceDigest != "" {
		t.Fatalf("restored operation evidence not frozen: row=%+v err=%v", row, err)
	}
	var reason string
	if err := restoredDB.QueryRowContext(ctx, `SELECT reason FROM assurance_transfer_startup_quarantines WHERE tenant_id=? AND transfer_id=?`, tenant, transferID).Scan(&reason); err != nil || !strings.Contains(reason, "provider_outcome_unknown") {
		t.Fatalf("operation-before-poll quarantine reason=%q err=%v", reason, err)
	}
	restoredAssurance, err := assurance.NewWithDB(restoredDB)
	if err != nil {
		t.Fatal(err)
	}
	restoredAudit, err := audit.NewWithDB(restoredDB)
	if err != nil {
		t.Fatal(err)
	}
	restoredAssurance.SetAuditLog(restoredAudit)
	if err := restoredAssurance.Bootstrap(stageCtx, tenant, bundlePublic); err != nil {
		t.Fatalf("bootstrap restored assurance: %v", err)
	}
	svc.store, svc.assuranceStore = restoredStore, restoredAssurance
	replay, replayErr := svc.Confirm(confirmedCtx, ConfirmRequest{TransferID: transferID, IdempotencyDigest: "fault-window-confirm-key", RequestDigest: "fault-window-confirm-request", Now: time.Now().UTC()})
	if replayErr == nil || replayErr.Error() != "idempotency_in_progress" || replay.State != "" {
		t.Fatalf("same-key replay=%+v err=%v, want exact idempotency_in_progress", replay, replayErr)
	}
	if _, err := svc.Confirm(confirmedCtx, ConfirmRequest{TransferID: transferID, Token: staged.Token, IdempotencyDigest: "fresh-after-restore-key", RequestDigest: "fresh-after-restore-request", Now: time.Now().UTC()}); err == nil {
		t.Fatal("old confirmation token with fresh key accepted after restore")
	}
	if baseProvider.reads.Load() != readsBeforeRestore || baseProvider.posts.Load() != postsBeforeRestore || instrumented.polls.Load() != pollsBeforeRestore {
		t.Fatalf("provider work during restore/replay: reads=%d posts=%d polls=%d", baseProvider.reads.Load()-readsBeforeRestore, baseProvider.posts.Load()-postsBeforeRestore, instrumented.polls.Load()-pollsBeforeRestore)
	}
	assertFaultFenceObjectsUnchanged(t, blobFixture, fencePrefix, fenceObjectsBeforeRestore)
}

func TestTerminalFenceBeforeLocalFinalTransactionRestoresToQuarantineWithoutReplay(t *testing.T) {
	const tenant = "11111111-1111-1111-1111-111111111111"
	ctx := context.Background()
	store, _ := testStore(t)
	baseProvider := &serviceFixture{value: `{"kind":"number","number":"1"}`, op: "provider-operation-terminal"}
	activeAssurance, stageCtx, bundlePublic := activateTransferRouteWithTrust(t, store)
	baseProvider.ctx = stageCtx
	confirmedCtx := b1TrustedConfirmContext(tenant)
	blobFixture := newRecoveryBlobFake()
	blobFixture.service = time.Now().UTC().Add(2 * time.Second).Truncate(time.Second)
	blobServer := newScanHTTPServer(t, http.HandlerFunc(blobFixture.serve))
	t.Cleanup(blobServer.Close)
	realFence := newProviderRecoveryFence(t, blobServer.URL, tenant)
	backup := newProviderRecoveryBackup(t, blobServer.URL, tenant)
	var transferID, envelopeDir string
	var artifacts []string
	var seal assurance.BlobBackupSeal
	var restoreOpts assurance.RestoreOptions
	fence := &snapshotBeforeLocalTerminalCommit{Fence: realFence, capture: func(callCtx context.Context, id string) error {
		transferID = id
		var state, operation, terminalDigest string
		var readbacks int
		if err := store.db.QueryRowContext(callCtx, `SELECT state,operation_reference,terminal_fence_digest,(SELECT count(*) FROM assurance_transfer_readbacks WHERE tenant_id=? AND transfer_id=?) FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, id, tenant, id).Scan(&state, &operation, &terminalDigest, &readbacks); err != nil {
			return err
		}
		if state != string(State("submitting")) || operation != baseProvider.op || terminalDigest != "" || readbacks != 0 {
			return errors.New("terminal-fence barrier observed unexpected pre-final-transaction local evidence")
		}
		prefix := "env/env-a/tenant/" + digest(tenant) + "/fences/" + id + "/"
		if blobFixture.fenceObjectCount(prefix+"claim.json") != 1 || blobFixture.fenceObjectCount(prefix+"terminal.json") != 1 {
			return errors.New("terminal-fence barrier did not observe one claim and one terminal object")
		}
		envelopeDir, artifacts, seal, restoreOpts = captureFaultWindowBackup(t, callCtx, store, tenant, "terminal_fence_before_local_commit", realFence, backup)
		return nil
	}}
	instrumented := &recoveryInstrumentedProvider{serviceFixture: baseProvider}
	router, err := newRecoveryProviderRouter(instrumented)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewTestService(store, activeAssurance, router, &FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	svc.fence = fence
	staged, err := svc.Stage(stageCtx, StageRequest{RouteID: "energy-copy", IdempotencyDigest: "fault-window-terminal-stage", RequestDigest: "fault-window-terminal-stage-request", Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("stage fixture transfer: %v", err)
	}
	transferID = staged.TransferID
	_, confirmErr := svc.Confirm(confirmedCtx, ConfirmRequest{TransferID: transferID, Token: staged.Token, IdempotencyDigest: "fault-window-confirm-key", RequestDigest: "fault-window-confirm-request", Now: time.Now().UTC()})
	if confirmErr == nil || !strings.Contains(confirmErr.Error(), "terminal_fence_unverified") {
		t.Fatalf("confirm error=%v, want failure after verified terminal fence", confirmErr)
	}
	if seal.EnvelopeID == "" || baseProvider.posts.Load() != 1 || instrumented.polls.Load() < 1 {
		t.Fatalf("terminal barrier activity: envelope=%q posts=%d polls=%d", seal.EnvelopeID, baseProvider.posts.Load(), instrumented.polls.Load())
	}
	if err := store.db.Close(); err != nil {
		t.Fatalf("close source database before restore: %v", err)
	}
	destroyFaultWindowLocalEnvelope(t, envelopeDir, artifacts)
	restoredPath := filepath.Join(t.TempDir(), "restored-after-terminal-fence.db")
	readsBeforeRestore, postsBeforeRestore, pollsBeforeRestore := baseProvider.reads.Load(), baseProvider.posts.Load(), instrumented.polls.Load()
	fencePrefix := "env/env-a/tenant/" + digest(tenant) + "/fences/"
	fenceObjectsBeforeRestore := snapshotFaultFenceObjects(blobFixture, fencePrefix)
	assertFaultEnvelopeAbsent(t, envelopeDir)
	restored, scan, err := RestoreSelectedEnvelopeAndScan(ctx, backup, seal.EnvelopeID, restoredPath, restoreOpts, realFence, "env-a")
	if err != nil || restored.Ready || scan.Ready || scan.Findings != 1 || scan.Quarantined != 1 {
		t.Fatalf("terminal-fence snapshot did not remain quarantined absent complete local evidence: restore=%+v scan=%+v err=%v", restored, scan, err)
	}
	restoredDB, err := sqlitedb.Open(restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restoredDB.Close() }()
	restoredStore, err := NewWithDB(restoredDB)
	if err != nil {
		t.Fatal(err)
	}
	row, err := restoredStore.Get(ctx, tenant, transferID)
	if err != nil || row.State != StateReconciliationRequired || row.OperationReference != baseProvider.op || row.TerminalFenceDigest != "" {
		t.Fatalf("restored terminal-fence row disposition=%+v err=%v", row, err)
	}
	var reason string
	if err := restoredDB.QueryRowContext(ctx, `SELECT reason FROM assurance_transfer_startup_quarantines WHERE tenant_id=? AND transfer_id=?`, tenant, transferID).Scan(&reason); err != nil || (!strings.Contains(reason, "terminal_state_contradiction") && !strings.Contains(reason, "provider_outcome_unknown")) {
		t.Fatalf("durable terminal-fence quarantine reason=%q err=%v", reason, err)
	}
	prefix := "env/env-a/tenant/" + digest(tenant) + "/fences/" + transferID + "/"
	if blobFixture.fenceObjectCount(prefix+"claim.json") != 1 || blobFixture.fenceObjectCount(prefix+"terminal.json") != 1 {
		t.Fatalf("restore mutated immutable claim/terminal objects: claim=%d terminal=%d", blobFixture.fenceObjectCount(prefix+"claim.json"), blobFixture.fenceObjectCount(prefix+"terminal.json"))
	}
	restoredAssurance, err := assurance.NewWithDB(restoredDB)
	if err != nil {
		t.Fatal(err)
	}
	restoredAudit, err := audit.NewWithDB(restoredDB)
	if err != nil {
		t.Fatal(err)
	}
	restoredAssurance.SetAuditLog(restoredAudit)
	if err := restoredAssurance.Bootstrap(stageCtx, tenant, bundlePublic); err != nil {
		t.Fatalf("bootstrap restored assurance: %v", err)
	}
	svc.store, svc.assuranceStore = restoredStore, restoredAssurance
	replay, replayErr := svc.Confirm(confirmedCtx, ConfirmRequest{TransferID: transferID, IdempotencyDigest: "fault-window-confirm-key", RequestDigest: "fault-window-confirm-request", Now: time.Now().UTC()})
	if replayErr == nil || replayErr.Error() != "idempotency_in_progress" || replay.State != "" {
		t.Fatalf("same-key replay=%+v err=%v, want exact idempotency_in_progress", replay, replayErr)
	}
	if _, err := svc.Confirm(confirmedCtx, ConfirmRequest{TransferID: transferID, Token: staged.Token, IdempotencyDigest: "fresh-after-restore-key", RequestDigest: "fresh-after-restore-request", Now: time.Now().UTC()}); err == nil {
		t.Fatal("old confirmation token with fresh key accepted after restore")
	}
	if baseProvider.reads.Load() != readsBeforeRestore || baseProvider.posts.Load() != postsBeforeRestore || instrumented.polls.Load() != pollsBeforeRestore {
		t.Fatalf("provider work during restore/replay: reads=%d posts=%d polls=%d", baseProvider.reads.Load()-readsBeforeRestore, baseProvider.posts.Load()-postsBeforeRestore, instrumented.polls.Load()-pollsBeforeRestore)
	}
	assertFaultFenceObjectsUnchanged(t, blobFixture, fencePrefix, fenceObjectsBeforeRestore)
}
