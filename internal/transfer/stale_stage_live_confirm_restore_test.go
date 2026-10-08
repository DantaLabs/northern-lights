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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

// A rollback backup contains a genuine staged row, while a later real Service
// confirmation has already created immutable external claim and terminal
// fences. Restore admission must join those facts before a fresh service can
// treat the staged reservation as replayable.
func TestStaleStageBackupJoinedWithLiveConfirmFences(t *testing.T) {
	runStaleStageRestoreScenario(t, false)
}

func TestPreConfirmHighWaterRejectsLaterLiveFences(t *testing.T) {
	runStaleStageRestoreScenario(t, true)
}

func runStaleStageRestoreScenario(t *testing.T, captureHighWaterBeforeConfirm bool) {
	t.Helper()
	ctx := context.Background()
	const tenant = "11111111-1111-1111-1111-111111111111"
	store, _ := testStore(t)
	fixture := &serviceFixture{value: `{"kind":"number","number":"1"}`, op: "https://provider.invalid/operations/restore-stale"}
	provider := &recoveryInstrumentedProvider{serviceFixture: fixture}
	router, err := newRecoveryProviderRouter(provider)
	if err != nil {
		t.Fatal(err)
	}
	assuranceStore, stageCtx, signedBundlePublicKey := activateTransferRouteWithTrust(t, store)
	fixture.ctx = stageCtx
	serverFixture := newRecoveryBlobFake()
	if captureHighWaterBeforeConfirm {
		serverFixture.service = time.Now().UTC().Truncate(time.Second)
	} else {
		serverFixture.service = time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	}
	fencePrefix := "env/env-a/tenant/" + digest(tenant) + "/fences/"
	fenceRequests := &recoveryFenceRequestCounts{prefix: fencePrefix}
	server := newScanHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fenceRequests.observe(r)
		serverFixture.serve(w, r)
	}))
	defer server.Close()
	fence := newProviderRecoveryFence(t, server.URL, tenant)
	svc, err := NewService(store, assuranceStore, router, fence)
	if err != nil {
		t.Fatal(err)
	}

	stageKey, stageRequest := "stale-stage-backup-key", "stale-stage-canonical-request"
	stageReq := StageRequest{RouteID: "energy-copy", IdempotencyDigest: stageKey, RequestDigest: stageRequest, Now: time.Now().UTC()}
	staged, err := svc.Stage(stageCtx, stageReq)
	if err != nil || staged.TransferID == "" || staged.Token == "" {
		t.Fatalf("actual Service.Stage: result=%+v err=%v", staged, err)
	}
	stageRow, err := store.Get(ctx, tenant, staged.TransferID)
	if err != nil || stageRow.State != StateStaged {
		t.Fatalf("stage backup source state=%s err=%v", stageRow.State, err)
	}
	// Make a signed audit checkpoint and snapshot the database while the transfer
	// is still genuinely staged. The subsequent confirmation is deliberately not
	// present in this SQLite image.
	checkpointPublic, checkpointPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	operator := identity.ContextWithPrincipal(ctx, identity.Principal{TenantID: tenant, ObjectID: "actor-1"})
	auditLog, err := audit.NewWithDB(store.db)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := auditLog.Append(operator, audit.Entry{Actor: "actor-1", Tool: "stale_restore_test", Action: "backup", Target: staged.TransferID})
	if err != nil {
		t.Fatal(err)
	}
	checkpointSig := ed25519.Sign(checkpointPrivate, []byte(strings.Join([]string{tenant, strconv.FormatInt(entry.Seq, 10), entry.Hash, "audit-terminal"}, "\x00")))
	if _, err := store.db.Exec(`INSERT INTO assurance_checkpoints (tenant_id, checkpoint_id, checkpoint_kind, high_water_mark, checkpoint_hash, signature_hex, metadata_json, created_at) VALUES (?, 'audit-terminal', 'terminal', ?, ?, ?, '{}', CURRENT_TIMESTAMP)`, tenant, entry.Seq, entry.Hash, hex.EncodeToString(checkpointSig)); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	stalePath := filepath.Join(workDir, "still-staged.db")
	if _, err := store.db.ExecContext(ctx, `VACUUM INTO ?`, stalePath); err != nil {
		t.Fatalf("snapshot staged source: %v", err)
	}

	backupKey := assurance.SigningKey{HMAC: []byte("stale-stage-recovery-test-signing-key")}
	envelopeDir := filepath.Join(workDir, "signed-envelope")
	backup := newProviderRecoveryBackup(t, server.URL, tenant)
	var highWater assurance.BackupFenceHighWater
	var envelope assurance.BackupEnvelope
	var restoreOpts assurance.RestoreOptions
	var seal assurance.BlobBackupSeal
	buildAndUpload := func(mark assurance.BackupFenceHighWater) {
		t.Helper()
		highWater = mark
		staleDB, openErr := sqlitedb.Open(stalePath)
		if openErr != nil {
			t.Fatal(openErr)
		}
		envelope, err = assurance.CreateBackup(ctx, staleDB, envelopeDir, assurance.BackupOptions{TenantID: tenant, SigningKey: backupKey, FenceHighWater: &highWater, CheckpointPublicKey: checkpointPublic})
		closeErr := staleDB.Close()
		if err != nil {
			t.Fatalf("create signed staged snapshot: %v", err)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		restoreOpts = assurance.RestoreOptions{
			ExpectedTenantID: tenant, SigningKey: backupKey, ExpectedDatabaseSHA256: envelope.Manifest.DatabaseSHA256,
			MinimumAuditSequence: envelope.Manifest.Audit.LastSequence, ExpectedSchemaMarkers: envelope.Manifest.SchemaMarkers,
			RequireFenceHighWater: true, CheckpointPublicKey: checkpointPublic,
		}
		seal, err = backup.UploadEnvelope(ctx, envelopeDir, restoreOpts)
		if err != nil {
			t.Fatalf("upload stale signed envelope: %v", err)
		}
		if envelope.Manifest.FenceHighWater == nil || envelope.Manifest.FenceHighWater.CapturedAt != highWater.CapturedAt {
			t.Fatalf("signed envelope did not preserve captured high-water: manifest=%+v expected=%+v", envelope.Manifest.FenceHighWater, highWater)
		}
	}
	var markTime time.Time
	if captureHighWaterBeforeConfirm {
		var captureErr error
		highWater, captureErr = fence.CaptureHighWater(ctx, tenant)
		if captureErr != nil {
			t.Fatalf("capture staged-state fence high-water: %v", captureErr)
		}
		markTime, err = time.Parse(time.RFC3339Nano, highWater.CapturedAt)
		if err != nil {
			t.Fatal(err)
		}
		serverFixture.service = markTime.Add(time.Second)
		buildAndUpload(highWater)
	}

	// This real confirmation occurs only after the signed rollback envelope was
	// created and uploaded. The fake's Last-Modified time is intentionally newer
	// than the captured high-water, so the restore inventory must not silently
	// treat these post-checkpoint objects as proof that the stale row is safe.
	confirmCtx := b1TrustedConfirmContext(tenant)
	confirmReq := ConfirmRequest{TransferID: staged.TransferID, Token: staged.Token, IdempotencyDigest: "post-backup-confirm-key", RequestDigest: "post-backup-confirm-request", Now: time.Now().UTC()}
	confirmed, err := svc.Confirm(confirmCtx, confirmReq)
	if err != nil || confirmed.State != StateMachineVerified {
		t.Fatalf("actual post-backup Service.Confirm: state=%s err=%v", confirmed.State, err)
	}
	preRestoreReads, preRestorePosts, preRestorePolls := provider.reads.Load(), provider.posts.Load(), provider.polls.Load()
	if preRestoreReads == 0 || preRestorePosts != 1 || preRestorePolls == 0 {
		t.Fatalf("positive provider control missing: reads=%d posts=%d polls=%d", preRestoreReads, preRestorePosts, preRestorePolls)
	}
	if got := serverFixture.fenceObjectCount(fencePrefix); got != 2 {
		t.Fatalf("real confirmation must create claim and terminal fences, got %d objects", got)
	}
	if !captureHighWaterBeforeConfirm {
		highWater, err = fence.CaptureHighWater(ctx, tenant)
		if err != nil {
			t.Fatalf("capture post-confirm fence high-water: %v", err)
		}
		markTime, err = time.Parse(time.RFC3339Nano, highWater.CapturedAt)
		if err != nil {
			t.Fatal(err)
		}
		buildAndUpload(highWater)
	}
	for name, object := range staleStageFenceSnapshot(serverFixture, fencePrefix) {
		if captureHighWaterBeforeConfirm && !object.LastModified.After(markTime) {
			t.Fatalf("post-backup fence %s is not strictly newer than signed mark: modified=%s mark=%s", name, object.LastModified, markTime)
		}
		if !captureHighWaterBeforeConfirm && object.LastModified.After(markTime) {
			t.Fatalf("post-confirm fence %s unexpectedly exceeds signed mark: modified=%s mark=%s", name, object.LastModified, markTime)
		}
	}
	preRestoreFenceWrites := fenceRequests.snapshot().puts
	preRestoreFenceObjects := staleStageFenceSnapshot(serverFixture, fencePrefix)
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{stalePath, stalePath + "-wal", stalePath + "-shm", envelope.DatabasePath, envelope.DatabasePath + "-wal", envelope.DatabasePath + "-shm", envelope.ManifestPath} {
		if err := removeRecoveryArtifact(path); err != nil {
			t.Fatalf("destroy disposable stale backup artifact %q: %v", path, err)
		}
	}
	if err := removeRecoveryEnvelopeDir(envelopeDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stalePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale disposable source still exists: %v", err)
	}
	if _, err := os.Stat(envelopeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local signed envelope still exists: %v", err)
	}

	// Restore the selected Azure envelope, then scan before constructing a fresh
	// service. A listed fence newer than the signed cutoff must fail admission,
	// not be ignored or treated as proof that the old staged row is safe.
	restoredPath := filepath.Join(workDir, "restored-stale.db")
	restored, err := backup.RestoreEnvelope(ctx, seal.EnvelopeID, restoredPath, restoreOpts)
	if err != nil || !restored.Ready {
		t.Fatalf("restore selected stale envelope: ready=%v err=%v", restored.Ready, err)
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
	restoredAssurance, err := assurance.NewWithDB(restoredDB)
	if err != nil {
		t.Fatal(err)
	}
	restoredAudit, err := audit.NewWithDB(restoredDB)
	if err != nil {
		t.Fatal(err)
	}
	restoredAssurance.SetAuditLog(restoredAudit)
	if err := restoredAssurance.Bootstrap(stageCtx, tenant, signedBundlePublicKey); err != nil {
		t.Fatalf("restore frozen trusted route config: %v", err)
	}
	var sourceRowVersion int64
	if err := restoredDB.QueryRow(`SELECT row_version FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&sourceRowVersion); err != nil {
		t.Fatal(err)
	}
	var restoredService *Service
	if !captureHighWaterBeforeConfirm {
		restoredService, err = NewService(restoredStore, restoredAssurance, router, fence)
		if err != nil {
			t.Fatalf("construct fresh service for signed post-confirm fence join: %v", err)
		}
	}
	scanCtx := identity.ContextWithPrincipal(ctx, identity.Principal{TenantID: tenant})
	scan, err := ScanAndJoinRestore(scanCtx, restoredStore, restoredAssurance, restoredAudit, fence, RestoreScanOptions{TenantID: tenant, EnvironmentDigest: "env-a", HighWater: *restored.Manifest.FenceHighWater})
	if captureHighWaterBeforeConfirm {
		if scan.Ready || err == nil || !strings.Contains(err.Error(), "newer than restore high-water") {
			t.Fatalf("restore must fail closed on post-cutoff live fences: scan=%+v err=%v", scan, err)
		}
		if err := restoredAssurance.Ready(); err == nil {
			t.Fatal("assurance readiness remained open after post-cutoff fence discovery")
		}
		if _, err := NewService(restoredStore, restoredAssurance, router, fence); err == nil {
			t.Fatal("fresh Service was constructed after post-cutoff fence caused restore admission failure")
		}
	} else {
		if err != nil || scan.Ready || scan.Findings != 1 || scan.Quarantined != 1 {
			t.Fatalf("stale staged row must join with covered post-confirm live fences: scan=%+v err=%v", scan, err)
		}
		var reason string
		if err := restoredDB.QueryRow(`SELECT recovery_reason FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&reason); err != nil || !strings.Contains(reason, "staged_row_has_restore_fence") {
			t.Fatalf("expected staged-row/live-fence quarantine, reason=%q err=%v", reason, err)
		}
		quarantined, err := restoredStore.Get(ctx, tenant, staged.TransferID)
		if err != nil || quarantined.State != StateReconciliationRequired {
			t.Fatalf("restore quarantine state=%s err=%v", quarantined.State, err)
		}
		fenceSnapshot := fenceRequests.snapshot()
		replay, err := restoredService.Stage(stageCtx, stageReq)
		if err != nil || replay.TransferID != staged.TransferID || replay.Token != "" || !replay.ReplayRequiresRestage {
			t.Fatalf("sealed token-free stage replay: result=%+v err=%v", replay, err)
		}
		if _, err := restoredService.Confirm(confirmCtx, ConfirmRequest{TransferID: staged.TransferID, Token: staged.Token, IdempotencyDigest: "new-key-after-restore", RequestDigest: "new-request-after-restore", Now: time.Now().UTC()}); err == nil {
			t.Fatal("new confirmation key unexpectedly accepted the pre-backup token after live-fence quarantine")
		}
		if after := fenceRequests.snapshot(); after != fenceSnapshot {
			t.Fatalf("replay/rejection accessed external fence namespace: before=%+v after=%+v", fenceSnapshot, after)
		}
		var afterRowVersion int64
		if err := restoredDB.QueryRow(`SELECT row_version FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&afterRowVersion); err != nil || afterRowVersion != sourceRowVersion+1 {
			t.Fatalf("replay/rejection changed quarantine row version unexpectedly: before_scan=%d after=%d err=%v", sourceRowVersion, afterRowVersion, err)
		}
	}

	providerSnapshot := [3]int32{provider.reads.Load(), provider.posts.Load(), provider.polls.Load()}
	if providerSnapshot != [3]int32{preRestoreReads, preRestorePosts, preRestorePolls} {
		t.Fatalf("restore/startup scan performed provider work for stale transfer: before=%v after=%v", [3]int32{preRestoreReads, preRestorePosts, preRestorePolls}, providerSnapshot)
	}
	fenceSnapshot := fenceRequests.snapshot()
	if fenceSnapshot.puts != preRestoreFenceWrites {
		t.Fatalf("restore admission rewrote external fences: writes %d->%d", preRestoreFenceWrites, fenceSnapshot.puts)
	}
	if after := staleStageFenceSnapshot(serverFixture, fencePrefix); !equalStaleStageFenceSnapshots(preRestoreFenceObjects, after) {
		t.Fatalf("restore/startup/replay changed immutable fence bytes or ETags: before=%v after=%v", preRestoreFenceObjects, after)
	}
	var finalRowVersion int64
	wantRowVersion := sourceRowVersion
	wantState := StateStaged
	if !captureHighWaterBeforeConfirm {
		wantRowVersion++
		wantState = StateReconciliationRequired
	}
	if err := restoredDB.QueryRow(`SELECT row_version FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&finalRowVersion); err != nil || finalRowVersion != wantRowVersion {
		t.Fatalf("restore/replay changed stale transfer row unexpectedly: before_scan=%d after=%d want=%d err=%v", sourceRowVersion, finalRowVersion, wantRowVersion, err)
	}
	if unchanged, err := restoredStore.Get(ctx, tenant, staged.TransferID); err != nil || unchanged.State != wantState {
		t.Fatalf("stale transfer final state=%s want=%s err=%v", unchanged.State, wantState, err)
	}
}

type staleStageFenceObject struct {
	ETag         string
	Body         []byte
	LastModified time.Time
}

func staleStageFenceSnapshot(fake *recoveryBlobFake, prefix string) map[string]staleStageFenceObject {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	out := make(map[string]staleStageFenceObject)
	for path, object := range fake.objects {
		key := strings.TrimPrefix(path, "/private/")
		if strings.HasPrefix(key, prefix) {
			out[key] = staleStageFenceObject{ETag: object.etag, Body: append([]byte(nil), object.body...), LastModified: object.lastModified}
		}
	}
	return out
}

func equalStaleStageFenceSnapshots(left, right map[string]staleStageFenceObject) bool {
	if len(left) != len(right) {
		return false
	}
	for name, l := range left {
		r, ok := right[name]
		if !ok || l.ETag != r.ETag || !l.LastModified.Equal(r.LastModified) || !bytes.Equal(l.Body, r.Body) {
			return false
		}
	}
	return true
}
