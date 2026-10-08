package transfer

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	_ "modernc.org/sqlite"
)

func TestRestoreQuarantineBlocksOldTransferIDReuse(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	store, err := NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	intent := testIntent()
	if _, err := db.Exec(`INSERT INTO assurance_transfer_startup_quarantines(tenant_id,environment_digest,transfer_id,disposition,reason,evidence_json,audit_id,created_at) VALUES(?, 'env-a', ?, 'quarantined', 'fence_orphan', '{}', 'audit', ?)`, intent.TenantID, intent.ID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stage(context.Background(), intent, "token", "key", "request", time.Now().UTC()); err == nil {
		t.Fatal("old transfer ID reused after external fence quarantine")
	}
}

func TestRestoreAuditFailureRollsBackQuarantineAndBlocksReadiness(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	store, err := NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	as, err := assurance.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	intent := testIntent()
	now := time.Now().UTC()
	if _, err := store.Stage(context.Background(), intent, "token", "key", "request", now); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.Claim(context.Background(), Claim{TenantID: intent.TenantID, ActorID: intent.ActorID, Permission: intent.Permission, TransferID: intent.ID, Token: "token", LeaseID: "lease", Now: now, LeaseUntil: now.Add(time.Hour)}); err != nil || !ok {
		t.Fatalf("claim=%v err=%v", ok, err)
	}
	if _, err := db.Exec(`CREATE TRIGGER deny_startup_audit BEFORE INSERT ON audit_log WHEN NEW.action='startup_quarantine' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: intent.TenantID})
	_, err = ScanAndJoinRestore(ctx, store, as, log, &restoreFenceInventory{}, RestoreScanOptions{TenantID: intent.TenantID, EnvironmentDigest: "env-a", HighWater: assurance.BackupFenceHighWater{TenantDigest: digest(intent.TenantID), EnvironmentDigest: "env-a", CapturedAt: now.Format(time.RFC3339Nano), Authority: "azure_blob_last_modified"}})
	if err == nil || as.Ready() == nil {
		t.Fatalf("audit failure err=%v readiness=%v", err, as.Ready())
	}
	var state string
	var count int
	if err := db.QueryRow(`SELECT state FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, intent.TenantID, intent.ID).Scan(&state); err != nil || state != "claimed" {
		t.Fatalf("rollback state=%s err=%v", state, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM assurance_transfer_startup_quarantines`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("quarantines=%d err=%v", count, err)
	}
}
