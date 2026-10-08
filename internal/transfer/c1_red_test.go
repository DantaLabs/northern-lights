package transfer

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
	_ "modernc.org/sqlite"
)

func TestC1StageAuditFailureFailsBeforeAnyProviderRead(t *testing.T) {
	svc, fixture, store := newServiceFixture(t)
	if _, err := store.db.Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Stage(fixture.ctx, StageRequest{RouteID: "energy-copy", IdempotencyDigest: "c1-stage", RequestDigest: "c1-request", Now: time.Now().UTC()})
	if err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("stage audit failure err=%v", err)
	}
	if got := fixture.reads.Load(); got != 0 {
		t.Fatalf("stage read provider %d times after initial audit failure", got)
	}
	var transfers, reservations int
	if err := store.db.QueryRow(`SELECT count(*) FROM transfer_intents`).Scan(&transfers); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM assurance_idempotency_records`).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if transfers != 0 || reservations != 0 {
		t.Fatalf("audit failure left local rows transfers=%d reservations=%d", transfers, reservations)
	}
}

func TestC1StageSealsTransferAuditLinkAndReplayAtomically(t *testing.T) {
	svc, fixture, store := newServiceFixture(t)
	result, err := svc.Stage(fixture.ctx, StageRequest{RouteID: "energy-copy", IdempotencyDigest: "c1-stage-ok", RequestDigest: "c1-request-ok", Now: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	var transferLinks, idempotencyLinks, sealed int
	if err := store.db.QueryRow(`SELECT count(*) FROM assurance_audit_links WHERE entity_kind='transfer' AND entity_id=?`, result.TransferID).Scan(&transferLinks); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM assurance_audit_links WHERE entity_kind='idempotency_record' AND entity_id=(SELECT record_id FROM assurance_idempotency_records WHERE action='stage' AND entity_reference=?)`, result.TransferID).Scan(&idempotencyLinks); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM assurance_idempotency_records WHERE action='stage' AND entity_reference=? AND state='sealed'`, result.TransferID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if transferLinks != 1 || idempotencyLinks != 1 || sealed != 1 {
		t.Fatalf("stage atomic evidence transfer_links=%d idempotency_links=%d sealed=%d", transferLinks, idempotencyLinks, sealed)
	}
	var envelope string
	if err := store.db.QueryRow(`SELECT response_envelope FROM assurance_idempotency_records WHERE action='stage' AND entity_reference=?`, result.TransferID).Scan(&envelope); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(envelope, result.Token) || !strings.Contains(envelope, `"token_recoverable":false`) {
		t.Fatalf("unsafe or incomplete stage replay envelope: %s", envelope)
	}
}

func TestC1ConfirmPersistsReadbackOperationAuditAndReplayAtomically(t *testing.T) {
	svc, fixture, store := newServiceFixture(t)
	staged, tenant, actor, permission := stageForConfirm(t, svc, fixture)
	fixture.value = `{"kind":"number","number":"1"}`
	got, err := svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now().UTC(), IdempotencyDigest: "test-confirm-key", RequestDigest: "test-confirm-request"})
	if err != nil || got.MachineOutcome != "api_verified" {
		t.Fatalf("confirm=%+v err=%v", got, err)
	}
	var readbacks, polls, links, sealed, completed int
	if err := store.db.QueryRow(`SELECT count(*) FROM assurance_transfer_readbacks WHERE tenant_id=? AND transfer_id=? AND cache_bypassed=1`, tenant, staged.TransferID).Scan(&readbacks); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM assurance_transfer_poll_events WHERE tenant_id=? AND transfer_id=? AND event_json LIKE '%completed%'`, tenant, staged.TransferID).Scan(&polls); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM assurance_audit_links WHERE entity_kind='transfer' AND entity_id=?`, staged.TransferID).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM assurance_idempotency_records WHERE action='confirm' AND entity_reference=? AND state='sealed'`, staged.TransferID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT operation_completed FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if readbacks != 1 || polls != 1 || links != 2 || sealed != 1 || completed != 1 {
		t.Fatalf("completion evidence readbacks=%d polls=%d links=%d sealed=%d completed=%d", readbacks, polls, links, sealed, completed)
	}
}

func TestC1PostSideEffectAuditFailureReconcilesWithoutRepeat(t *testing.T) {
	svc, fixture, store := newServiceFixture(t)
	staged, tenant, actor, permission := stageForConfirm(t, svc, fixture)
	fixture.value = `{"kind":"number","number":"1"}`
	fixture.afterWait = func() { _, _ = store.db.Exec(`DROP TABLE audit_log`) }
	got, err := svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now().UTC(), IdempotencyDigest: "test-confirm-key", RequestDigest: "test-confirm-request"})
	var unknown *ConfirmUnknownError
	if !errors.As(err, &unknown) || !unknown.ReconciliationRequired || !unknown.NoRepeat || unknown.OperationReference != fixture.op || unknown.Cause == nil {
		t.Fatalf("post-side-effect audit failure must be explicit unknown with operation and underlying error: %v", err)
	}
	if got.State != StateReconciliationRequired || fixture.posts.Load() != 1 {
		t.Fatalf("post-side-effect audit failure state=%s posts=%d", got.State, fixture.posts.Load())
	}
	var readbacks, polls int
	if err := store.db.QueryRow(`SELECT count(*) FROM assurance_transfer_readbacks WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&readbacks); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM assurance_transfer_poll_events WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&polls); err != nil {
		t.Fatal(err)
	}
	if readbacks != 0 || polls != 0 {
		t.Fatalf("failed finalization was partially committed readbacks=%d polls=%d", readbacks, polls)
	}
}

func TestC1CompletionEvidenceSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c1-reopen.db")
	db, err := sql.Open("sqlite", sqlitedb.SharedFileDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	store, err := NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &serviceFixture{value: `{"kind":"number","number":"1"}`, op: "https://provider.invalid/c1-reopen"}
	router, err := newRouter(fixture)
	if err != nil {
		t.Fatal(err)
	}
	assuranceStore, ctx := activateTransferRoute(t, store)
	fixture.ctx = ctx
	svc, err := NewTestService(store, assuranceStore, router, &FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	staged, tenant, actor, permission := stageForConfirm(t, svc, fixture)
	if _, err := svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now().UTC(), IdempotencyDigest: "test-confirm-key", RequestDigest: "test-confirm-request"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedDB, err := sql.Open("sqlite", sqlitedb.SharedFileDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopenedDB.Close() }()
	reopenedDB.SetMaxOpenConns(1)
	if _, err := assurance.NewWithDB(reopenedDB); err != nil {
		t.Fatal(err)
	}
	var readbacks, polls, links int
	if err := reopenedDB.QueryRow(`SELECT count(*) FROM assurance_transfer_readbacks WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&readbacks); err != nil {
		t.Fatal(err)
	}
	if err := reopenedDB.QueryRow(`SELECT count(*) FROM assurance_transfer_poll_events WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&polls); err != nil {
		t.Fatal(err)
	}
	if err := reopenedDB.QueryRow(`SELECT count(*) FROM assurance_audit_links WHERE entity_kind='transfer' AND entity_id=?`, staged.TransferID).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if readbacks != 1 || polls != 1 || links != 2 {
		t.Fatalf("reopened evidence readbacks=%d polls=%d links=%d", readbacks, polls, links)
	}
}

func TestC1OpenReconciliationClassesDoNotRequirePersistedEqualReadback(t *testing.T) {
	for _, classification := range []string{"still_unknown", "conflicting_value", "provider_evidence_inconsistent"} {
		t.Run(classification, func(t *testing.T) {
			store, _ := testStore(t)
			intent := testIntent()
			if _, err := store.Stage(context.Background(), intent, "token", "key-"+classification, "request", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if ok, err := store.Claim(context.Background(), Claim{TenantID: intent.TenantID, ActorID: intent.ActorID, Permission: intent.Permission, TransferID: intent.ID, Token: "token", LeaseID: "lease", Now: time.Now().UTC(), LeaseUntil: time.Now().Add(-time.Second)}); err != nil || !ok {
				t.Fatalf("claim=%v err=%v", ok, err)
			}
			if err := store.QuarantineExpired(context.Background(), time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err := store.ApplyReconciliation(context.Background(), intent.TenantID, intent.ID, classification, "", false, time.Now().UTC()); err != nil {
				t.Fatalf("open classification rejected without readback: %v", err)
			}
			got, err := store.Get(context.Background(), intent.TenantID, intent.ID)
			if err != nil || got.State != StateReconciliationRequired {
				t.Fatalf("state=%s err=%v", got.State, err)
			}
		})
	}
}

func TestC1ConfirmedNotAppliedRequiresAuthoritativeUnequalReadback(t *testing.T) {
	store, db := testStore(t)
	intent := testIntent()
	if _, err := store.Stage(context.Background(), intent, "token", "not-applied-key", "request", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.Claim(context.Background(), Claim{TenantID: intent.TenantID, ActorID: intent.ActorID, Permission: intent.Permission, TransferID: intent.ID, Token: "token", LeaseID: "lease", Now: time.Now().UTC(), LeaseUntil: time.Now().Add(-time.Second)}); err != nil || !ok {
		t.Fatalf("claim=%v err=%v", ok, err)
	}
	if err := store.QuarantineExpired(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	unequal := `{"formula":false,"kind":"number","number":"3"}`
	if _, err := db.Exec(`INSERT INTO assurance_transfer_readbacks(tenant_id,transfer_id,readback_id,typed_value_json,cache_bypassed,observed_at) VALUES(?,?,?,?,1,?)`, intent.TenantID, intent.ID, "not-applied-readback", unequal, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_transfer_poll_events(tenant_id,transfer_id,event_id,event_json,observed_at) VALUES(?,?,?,?,?)`, intent.TenantID, intent.ID, "not-applied-operation", `{"status":"failed"}`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyReconciliation(context.Background(), intent.TenantID, intent.ID, "confirmed_not_applied", unequal, false, time.Now().UTC()); err != nil {
		t.Fatalf("authoritative not-applied proof rejected: %v", err)
	}
	got, err := store.Get(context.Background(), intent.TenantID, intent.ID)
	if err != nil || got.State != StateReconciledNotApplied {
		t.Fatalf("state=%s err=%v", got.State, err)
	}
}

func newRouter(fixture *serviceFixture) (*workivaprovider.Router, error) {
	return workivaprovider.NewRouterChecked(fixture)
}
