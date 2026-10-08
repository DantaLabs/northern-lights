package transfer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestRealBlobRecoveryMatrixQuarantinesMissingOrphanAndContradictoryFences(t *testing.T) {
	for _, scenario := range []string{"missing_claim", "orphan_claim", "canonical_contradictory_terminal"} {
		t.Run(scenario, func(t *testing.T) {
			matrix := newRecoveryMatrixBackup(t, scenario)
			defer matrix.server.Close()
			restoredPath := filepath.Join(matrix.workDir, "restored.db")
			restored, scan, err := RestoreSelectedEnvelopeAndScan(context.Background(), matrix.backup, matrix.seal.EnvelopeID, restoredPath, matrix.restoreOpts, matrix.fence, "env-a")
			if err != nil || restored.Ready || scan.Ready || scan.Findings != 1 || scan.Quarantined != 1 {
				t.Fatalf("%s was not quarantined by real Blob restore: restore_ready=%v scan=%+v err=%v", scenario, restored.Ready, scan, err)
			}
			db, err := sqlitedb.Open(restoredPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			quarantinedID := matrix.intent.ID
			if scenario == "orphan_claim" {
				quarantinedID = "orphan-transfer"
			}
			var reason string
			if err := db.QueryRow(`SELECT reason FROM assurance_transfer_startup_quarantines WHERE tenant_id=? AND transfer_id=?`, matrix.intent.TenantID, quarantinedID).Scan(&reason); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing_claim":
				if !strings.Contains(reason, "missing_or_mismatched_claim_fence") {
					t.Fatalf("missing-claim reason=%q", reason)
				}
			case "orphan_claim":
				var count int
				if err := db.QueryRow(`SELECT count(*) FROM assurance_transfer_startup_quarantines WHERE tenant_id=? AND transfer_id='orphan-transfer' AND reason='fence_orphan'`, matrix.intent.TenantID).Scan(&count); err != nil || count != 1 {
					t.Fatalf("orphan quarantine rows=%d err=%v", count, err)
				}
			case "canonical_contradictory_terminal":
				if !strings.Contains(reason, "terminal_state_contradiction") {
					t.Fatalf("contradictory-terminal reason=%q", reason)
				}
			}
			var audits, quarantines int
			if err := db.QueryRow(`SELECT count(*) FROM audit_log WHERE tenant_id=? AND action='startup_quarantine'`, matrix.intent.TenantID).Scan(&audits); err != nil || audits == 0 {
				t.Fatalf("quarantine audit rows=%d err=%v", audits, err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM assurance_transfer_startup_quarantines WHERE tenant_id=?`, matrix.intent.TenantID).Scan(&quarantines); err != nil || quarantines != 1 || audits != 1 {
				t.Fatalf("quarantine/audit counts=%d/%d err=%v", quarantines, audits, err)
			}
			if scenario != "orphan_claim" {
				var state string
				if err := db.QueryRow(`SELECT state FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, matrix.intent.TenantID, matrix.intent.ID).Scan(&state); err != nil || state != string(StateReconciliationRequired) {
					t.Fatalf("quarantined transfer state=%q err=%v", state, err)
				}
			}
			if scenario == "orphan_claim" {
				var state string
				if err := db.QueryRow(`SELECT state FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, matrix.intent.TenantID, matrix.intent.ID).Scan(&state); err != nil || state != string(StateMachineVerified) {
					t.Fatalf("orphan fence changed unrelated transfer state=%q err=%v", state, err)
				}
				var localOrphan int
				if err := db.QueryRow(`SELECT count(*) FROM transfer_intents WHERE tenant_id=? AND transfer_id='orphan-transfer'`, matrix.intent.TenantID).Scan(&localOrphan); err != nil || localOrphan != 0 {
					t.Fatalf("orphan fence created local transfer rows=%d err=%v", localOrphan, err)
				}
			}
			freshIntent := matrix.intent
			freshIntent.ID = quarantinedID
			quarantineStore, err := NewWithDB(db)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := quarantineStore.Stage(context.Background(), freshIntent, "replacement-token", "new-key", "new-request", time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "external fence quarantine blocks transfer ID reuse") {
				t.Fatalf("quarantined ID reuse error=%v", err)
			}
			if scenario == "canonical_contradictory_terminal" && !matrix.contradictoryTerminalWasCanonical {
				t.Fatal("contradictory terminal fixture was not canonical")
			}
			if scenario == "orphan_claim" && reason != "fence_orphan" {
				t.Fatalf("orphan quarantine reason=%q", reason)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := sqlitedb.Open(restoredPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := reopened.QueryRow(`SELECT count(*) FROM assurance_transfer_startup_quarantines WHERE tenant_id=? AND transfer_id=?`, matrix.intent.TenantID, quarantinedID).Scan(&quarantines); err != nil || quarantines != 1 {
				_ = reopened.Close()
				t.Fatalf("reopened quarantine count=%d err=%v", quarantines, err)
			}
			if err := reopened.QueryRow(`SELECT count(*) FROM audit_log WHERE tenant_id=? AND action='startup_quarantine'`, matrix.intent.TenantID).Scan(&audits); err != nil || audits != 1 {
				_ = reopened.Close()
				t.Fatalf("reopened audit count=%d err=%v", audits, err)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRealBlobRecoveryMatrixPreservesPendingStageAndRejectsRollback(t *testing.T) {
	t.Run("pending_stage_is_preserved_without_recovering_confirmation_token", func(t *testing.T) {
		matrix := newRecoveryMatrixBackup(t, "pending_staged")
		defer matrix.server.Close()
		destination := filepath.Join(matrix.workDir, "pending-restored.db")
		restored, scan, err := RestoreSelectedEnvelopeAndScan(context.Background(), matrix.backup, matrix.seal.EnvelopeID, destination, matrix.restoreOpts, matrix.fence, "env-a")
		if err != nil || !restored.Ready || !scan.Ready || scan.Findings != 0 {
			t.Fatalf("staged transfer restore: restore=%+v scan=%+v err=%v", restored, scan, err)
		}
		db, err := sqlitedb.Open(destination)
		if err != nil {
			t.Fatal(err)
		}
		store, err := NewWithDB(db)
		if err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		got, err := store.Get(context.Background(), matrix.intent.TenantID, matrix.intent.ID)
		if err != nil || got.State != StateStaged {
			_ = db.Close()
			t.Fatalf("pending transfer state=%q err=%v", got.State, err)
		}
		replay, err := store.Stage(context.Background(), matrix.intent, "original-token", "idem", "request", time.Now().UTC())
		if err != nil || replay.TransferID != matrix.intent.ID || !replay.ReplayRequiresRestage || replay.Token != "" {
			_ = db.Close()
			t.Fatalf("staged replay projection=%+v err=%v", replay, err)
		}
		afterReplay, err := store.Get(context.Background(), matrix.intent.TenantID, matrix.intent.ID)
		if err != nil || afterReplay.State != StateStaged || !afterReplay.Intent.ExpiresAt.Equal(got.Intent.ExpiresAt) {
			_ = db.Close()
			t.Fatalf("staged replay changed frozen pending intent: before=%+v after=%+v err=%v", got.Intent, afterReplay.Intent, err)
		}
		var frozen, tokenDigest string
		var rowVersion int64
		var transferRows int
		if err := db.QueryRow(`SELECT intent_json,token_digest,row_version FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, matrix.intent.TenantID, matrix.intent.ID).Scan(&frozen, &tokenDigest, &rowVersion); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT count(*) FROM transfer_intents WHERE tenant_id=?`, matrix.intent.TenantID).Scan(&transferRows); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		if tokenDigest != confirmationDigest("token", []byte(frozen)) || strings.Contains(frozen, `"token"`) || rowVersion != matrix.sourceRowVersion || transferRows != 1 {
			_ = db.Close()
			t.Fatalf("staged token/row changed across replay: digest_valid=%v frozen_contains_token=%v row_version=%d want=%d transfer_rows=%d", tokenDigest == confirmationDigest("token", []byte(frozen)), strings.Contains(frozen, `"token"`), rowVersion, matrix.sourceRowVersion, transferRows)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("audit_trigger_rolls_back_quarantine_transaction", func(t *testing.T) {
		matrix := newRecoveryMatrixBackup(t, "audit_trigger")
		defer matrix.server.Close()
		destination := filepath.Join(matrix.workDir, "audit-failure-restored.db")
		restored, _, err := RestoreSelectedEnvelopeAndScan(context.Background(), matrix.backup, matrix.seal.EnvelopeID, destination, matrix.restoreOpts, matrix.fence, "env-a")
		if err == nil || !strings.Contains(err.Error(), "audit unavailable") || restored.Ready {
			t.Fatalf("audit append failure err=%v restore_ready=%v", err, restored.Ready)
		}
		db, err := sqlitedb.Open(destination)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		var state string
		if err := db.QueryRow(`SELECT state FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, matrix.intent.TenantID, matrix.intent.ID).Scan(&state); err != nil || state != string(StateMachineVerified) {
			t.Fatalf("audit failure did not roll back transfer state: state=%q err=%v", state, err)
		}
		var quarantines, audits int
		if err := db.QueryRow(`SELECT count(*) FROM assurance_transfer_startup_quarantines WHERE tenant_id=?`, matrix.intent.TenantID).Scan(&quarantines); err != nil || quarantines != 0 {
			t.Fatalf("audit failure left quarantine rows=%d err=%v", quarantines, err)
		}
		if err := db.QueryRow(`SELECT count(*) FROM audit_log WHERE tenant_id=? AND action='startup_quarantine'`, matrix.intent.TenantID).Scan(&audits); err != nil || audits != 0 {
			t.Fatalf("audit failure left quarantine audit rows=%d err=%v", audits, err)
		}
		var reconciliations, transferQuarantineEvents int
		if err := db.QueryRow(`SELECT count(*) FROM transfer_reconciliations WHERE tenant_id=? AND transfer_id=?`, matrix.intent.TenantID, matrix.intent.ID).Scan(&reconciliations); err != nil || reconciliations != 0 {
			t.Fatalf("audit failure left transfer reconciliation rows=%d err=%v", reconciliations, err)
		}
		if err := db.QueryRow(`SELECT count(*) FROM transfer_audit_events WHERE tenant_id=? AND transfer_id=? AND disposition='startup_quarantine'`, matrix.intent.TenantID, matrix.intent.ID).Scan(&transferQuarantineEvents); err != nil || transferQuarantineEvents != 0 {
			t.Fatalf("audit failure left transfer quarantine audit events=%d err=%v", transferQuarantineEvents, err)
		}
		var rowVersion int64
		if err := db.QueryRow(`SELECT row_version FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, matrix.intent.TenantID, matrix.intent.ID).Scan(&rowVersion); err != nil || rowVersion != matrix.sourceRowVersion {
			t.Fatalf("audit failure changed row version=%d want=%d err=%v", rowVersion, matrix.sourceRowVersion, err)
		}
	})

	t.Run("stale_minimum_preserves_empty_destination_and_existing_destination", func(t *testing.T) {
		matrix := newRecoveryMatrixBackup(t, "clean_restore")
		defer matrix.server.Close()
		stale := matrix.restoreOpts
		stale.MinimumAuditSequence = matrix.restoreOpts.MinimumAuditSequence + 1
		emptyDestination := filepath.Join(matrix.workDir, "stale-empty.db")
		if _, err := matrix.backup.RestoreEnvelope(context.Background(), matrix.seal.EnvelopeID, emptyDestination, stale); !errors.Is(err, assurance.ErrBackupRollback) {
			t.Fatalf("stale signed minimum error=%v, want ErrBackupRollback", err)
		}
		if _, err := os.Stat(emptyDestination); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale restore created destination: %v", err)
		}
		preexisting := filepath.Join(matrix.workDir, "preexisting.db")
		const sentinel = "do-not-overwrite-existing-destination"
		if err := os.WriteFile(preexisting, []byte(sentinel), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := matrix.backup.RestoreEnvelope(context.Background(), matrix.seal.EnvelopeID, preexisting, matrix.restoreOpts); err == nil {
			t.Fatal("restore overwrote a pre-existing destination")
		}
		got, err := os.ReadFile(preexisting)
		if err != nil || string(got) != sentinel {
			t.Fatalf("pre-existing destination changed: %q err=%v", got, err)
		}
	})
}

type recoveryMatrixBackup struct {
	server                            *httptest.Server
	backup                            *assurance.AzureBackupTransport
	fence                             *AzureBlobFence
	seal                              assurance.BlobBackupSeal
	restoreOpts                       assurance.RestoreOptions
	workDir                           string
	intent                            Intent
	contradictoryTerminalWasCanonical bool
	sourceRowVersion                  int64
}

func newRecoveryMatrixBackup(t *testing.T, scenario string) recoveryMatrixBackup {
	t.Helper()
	ctx := context.Background()
	state, terminal := StateMachineVerified, true
	if scenario == "pending_staged" {
		state, terminal = StateStaged, false
	}
	store, _, auditLog, intent, _, inventory, _, _ := newRestoreScanFixture(t, state, terminal)
	var sourceRowVersion int64
	if err := store.db.QueryRow(`SELECT row_version FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, intent.TenantID, intent.ID).Scan(&sourceRowVersion); err != nil {
		t.Fatal(err)
	}
	if scenario == "pending_staged" {
		inventory.objects = nil
	}
	operator := identity.ContextWithPrincipal(ctx, identity.Principal{TenantID: intent.TenantID, ObjectID: intent.ActorID})
	auditEntry, err := auditLog.Append(operator, audit.Entry{Actor: intent.ActorID, Tool: "recovery_matrix_test", Action: "checkpoint", Target: intent.ID})
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
	contradictoryCanonical := false
	if scenario == "canonical_contradictory_terminal" {
		for i := range inventory.objects {
			if inventory.objects[i].Kind != "terminal" {
				continue
			}
			var prior terminalPayload
			if err := json.Unmarshal(inventory.objects[i].Body, &prior); err != nil {
				t.Fatal(err)
			}
			contradictory := terminalPayload{ClaimDigest: prior.ClaimDigest, EnvironmentDigest: prior.EnvironmentDigest, TenantDigest: prior.TenantDigest, TransferID: prior.TransferID, ProviderOutcomeDigest: digest("failed"), TerminalKind: "failed", Reason: "provider_failure", TerminalTime: prior.TerminalTime}
			body, err := json.Marshal(contradictory)
			if err != nil {
				t.Fatal(err)
			}
			if err := canonicalFenceBody(body, "terminal", intent.ID, "env-a", digest(intent.TenantID)); err != nil {
				t.Fatalf("construct contradictory terminal: %v", err)
			}
			inventory.objects[i].Body = body
			inventory.objects[i].Digest = digest(string(body))
			inventory.objects[i].FenceDigest = digest(string(body))
			if _, err := store.db.Exec(`UPDATE transfer_intents SET terminal_fence_digest=? WHERE tenant_id=? AND transfer_id=?`, digest(string(body)), intent.TenantID, intent.ID); err != nil {
				t.Fatal(err)
			}
			contradictoryCanonical = true
		}
	}
	if scenario == "audit_trigger" {
		if _, err := store.db.Exec(`CREATE TRIGGER deny_recovery_matrix_audit BEFORE INSERT ON audit_log WHEN NEW.action='startup_quarantine' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
			t.Fatal(err)
		}
	}

	blob := newRecoveryBlobFake()
	blob.service = time.Date(2026, 10, 7, 4, 0, 0, 0, time.UTC)
	server := newScanHTTPServer(t, http.HandlerFunc(blob.serve))
	backup := newRecoveryBackupTransport(t, server.URL)
	fence := newScanFence(t, server.URL, 5*time.Second, 8, 32, 8<<20)
	for _, object := range inventory.objects {
		if (scenario == "missing_claim" || scenario == "audit_trigger") && object.Kind == "claim" {
			continue
		}
		switch object.Kind {
		case "claim":
			if _, err := fence.CreateClaim(ctx, intent.ID, object.Body); err != nil {
				t.Fatalf("create real Blob claim: %v", err)
			}
		case "terminal":
			if _, err := fence.CreateTerminal(ctx, intent.ID, object.Body); err != nil {
				t.Fatalf("create real Blob terminal: %v", err)
			}
		}
	}
	if scenario == "orphan_claim" {
		orphan := claimPayload{EnvironmentDigest: "env-a", TenantDigest: digest(intent.TenantID), TransferID: "orphan-transfer", IdempotencyDigest: "orphan-key", RequestDigest: "orphan-request", ActorDigest: digest(intent.ActorID), SourceHash: digest("orphan-source"), TargetHash: digest("orphan-target"), IntendedHash: digest("orphan-intended"), PolicyHash: digest("orphan-policy"), MappingHash: digest("orphan-mapping"), ClaimState: "claimed", ClaimTime: blob.service.Format(time.RFC3339Nano), ClaimRowVersion: 2}
		body, err := json.Marshal(orphan)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fence.CreateClaim(ctx, "orphan-transfer", body); err != nil {
			t.Fatalf("create orphan Blob claim: %v", err)
		}
	}
	highWater, err := fence.CaptureHighWater(ctx, intent.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	sourcePath := filepath.Join(workDir, "source.db")
	if _, err := store.db.ExecContext(ctx, `VACUUM INTO ?`, sourcePath); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	sourceDB, err := sqlitedb.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	key := assurance.SigningKey{HMAC: []byte("recovery-matrix-test-signing-key-32")}
	envelopeDir := filepath.Join(workDir, "envelope")
	envelope, err := assurance.CreateBackup(ctx, sourceDB, envelopeDir, assurance.BackupOptions{TenantID: intent.TenantID, SigningKey: key, FenceHighWater: &highWater, CheckpointPublicKey: checkpointPublic})
	if err != nil {
		_ = sourceDB.Close()
		t.Fatalf("create signed recovery matrix envelope: %v", err)
	}
	restoreOpts := assurance.RestoreOptions{ExpectedTenantID: intent.TenantID, SigningKey: key, ExpectedDatabaseSHA256: envelope.Manifest.DatabaseSHA256, MinimumAuditSequence: envelope.Manifest.Audit.LastSequence, ExpectedSchemaMarkers: envelope.Manifest.SchemaMarkers, RequireFenceHighWater: true, CheckpointPublicKey: checkpointPublic}
	seal, err := backup.UploadEnvelope(ctx, envelopeDir, restoreOpts)
	if err != nil {
		_ = sourceDB.Close()
		t.Fatal(err)
	}
	if err := sourceDB.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{sourcePath, sourcePath + "-wal", sourcePath + "-shm", envelope.DatabasePath, envelope.DatabasePath + "-wal", envelope.DatabasePath + "-shm", envelope.ManifestPath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destroy exact recovery matrix artifact %q: %v", path, err)
		}
	}
	if err := os.Remove(envelopeDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sourcePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disposable source database still exists: %v", err)
	}
	if _, err := os.Stat(envelopeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local envelope directory still exists: %v", err)
	}
	return recoveryMatrixBackup{server: server, backup: backup, fence: fence, seal: seal, restoreOpts: restoreOpts, workDir: workDir, intent: intent, contradictoryTerminalWasCanonical: contradictoryCanonical, sourceRowVersion: sourceRowVersion}
}
