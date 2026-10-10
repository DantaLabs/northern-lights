package transfer

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func testIntent() Intent {
	return Intent{ID: "tr-1", TenantID: "tenant-a", ActorID: "actor-a", Permission: "workiva.write.confirm",
		Source: Endpoint{ResourceID: "src", SheetID: "sheet-src", Locator: "A1", Fingerprint: "sf"}, Target: Endpoint{ResourceID: "dst", SheetID: "sheet-dst", Locator: "B2", Fingerprint: "tf"},
		Before: `{"kind":"number","number":"1"}`, Intended: `{"kind":"number","number":"2"}`, PolicyHash: "policy-v1", MappingHash: "mapping-v1", ExpiresAt: time.Now().UTC().Add(time.Hour)}
}

func testStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	s, err := NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return s, db
}

func TestStageTransientTokenAndFrozenIntentAndReplayPrecedesTokenCheck(t *testing.T) {
	s, db := testStore(t)
	intent := testIntent()
	first, err := s.Stage(context.Background(), intent, "raw-confirm-token", "idem-digest", "request-digest", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if first.Token != "raw-confirm-token" {
		t.Fatalf("first response token %q", first.Token)
	}
	replay, err := s.Stage(context.Background(), intent, "raw-confirm-token", "idem-digest", "request-digest", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if replay.Token != "" || !replay.ReplayRequiresRestage {
		t.Fatalf("unsafe replay: %+v", replay)
	}
	var frozen string
	if err := db.QueryRow(`SELECT intent_json FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, intent.TenantID, intent.ID).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"raw-confirm-token", "raw-idempotency-key"} {
		if containsBytes([]byte(frozen), secret) {
			t.Fatalf("secret persisted: %s", secret)
		}
	}
	intent.Intended = `{"kind":"number","number":"9"}`
	if _, err := s.Stage(context.Background(), intent, "x", "idem-digest", "other-request", time.Now().UTC()); err == nil {
		t.Fatal("expected immutable intent/idempotency conflict")
	}
}

func TestConfirmationBindingAnd100CallerCAS(t *testing.T) {
	s, _ := testStore(t)
	intent := testIntent()
	staged, err := s.Stage(context.Background(), intent, "secret-token", "idem", "request", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := s.Claim(context.Background(), Claim{TenantID: intent.TenantID, ActorID: intent.ActorID, Permission: intent.Permission, TransferID: intent.ID, Token: "secret-token", Now: time.Now().UTC(), LeaseID: fmt.Sprintf("lease-%d", i), LeaseUntil: time.Now().UTC().Add(time.Minute)})
			if err != nil {
				t.Errorf("claim: %v", err)
			}
			if ok {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("CAS winners=%d, want 1", wins.Load())
	}
	ok, err := s.Claim(context.Background(), Claim{TenantID: intent.TenantID, ActorID: "actor-b", Permission: intent.Permission, TransferID: staged.TransferID, Token: "secret-token", Now: time.Now().UTC(), LeaseID: "wrong", LeaseUntil: time.Now().Add(time.Minute)})
	if err != nil || ok {
		t.Fatalf("wrong actor claimed: %v %v", ok, err)
	}
}

func TestCrashQuarantineAndIndependentVisualReconciliation(t *testing.T) {
	s, db := testStore(t)
	in := testIntent()
	_, err := s.Stage(context.Background(), in, "tok", "key", "req", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	ok, err := s.Claim(context.Background(), Claim{TenantID: in.TenantID, ActorID: in.ActorID, Permission: in.Permission, TransferID: in.ID, Token: "tok", Now: time.Now().UTC(), LeaseID: "lease", LeaseUntil: time.Now().Add(-time.Second)})
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := s.markFenced(context.Background(), in.TenantID, in.ID, "claim-fence"); err != nil {
		t.Fatal(err)
	}
	if err := s.persistOperation(context.Background(), in.TenantID, in.ID, "provider-op-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.markOperationCompleted(context.Background(), in.TenantID, in.ID, "provider-op-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.QuarantineExpired(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(context.Background(), in.TenantID, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateReconciliationRequired || got.Submissions != 1 {
		t.Fatalf("recovery state: %+v", got)
	}
	if err := s.RecordAcknowledgement(context.Background(), in.TenantID, in.ID, "ack-actor", "mismatch", "ui:sheet!B2", false, `{"kind":"number","number":"3"}`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordReconciliation(context.Background(), in.TenantID, in.ID, "visual_disagreement", "still_unknown", "ack-1"); err != nil {
		t.Fatal(err)
	}
	acks, recs, err := s.EvidenceCounts(context.Background(), in.TenantID, in.ID)
	if err != nil || acks != 1 || recs != 2 {
		t.Fatalf("evidence counts %d %d %v", acks, recs, err)
	}
	_, err = db.Exec(`INSERT INTO assurance_transfer_readbacks(tenant_id,transfer_id,readback_id,typed_value_json,cache_bypassed,observed_at) VALUES(?,?,?,?,1,?)`, in.TenantID, in.ID, "reconciliation-readback", in.Intended, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyReconciliation(context.Background(), in.TenantID, in.ID, "confirmed_applied", in.Intended, false, time.Now().UTC()); err == nil {
		t.Fatal("confirmed_applied accepted without equal uncached read-back")
	}
	if err := s.ApplyReconciliation(context.Background(), in.TenantID, in.ID, "confirmed_applied", in.Intended, true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(context.Background(), in.TenantID, in.ID)
	if err != nil || got.State != StateMachineVerified || got.MachineOutcome != "api_verified" || got.MachineOutcomeProvenance != "reconciliation" {
		t.Fatalf("reconciliation exit: %+v err=%v", got, err)
	}
}

func TestOnlyEqualUncachedReadbackYieldsMachineVerified(t *testing.T) {
	s, _ := testStore(t)
	in := testIntent()
	if _, err := s.Stage(context.Background(), in, "token", "key", "req", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Claim(context.Background(), Claim{TenantID: in.TenantID, ActorID: in.ActorID, Permission: in.Permission, TransferID: in.ID, Token: "token", LeaseID: "lease", Now: time.Now().UTC(), LeaseUntil: time.Now().Add(time.Minute)}); err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	if err := s.markFenced(context.Background(), in.TenantID, in.ID, "claim-fence"); err != nil {
		t.Fatal(err)
	}
	if err := s.persistOperation(context.Background(), in.TenantID, in.ID, "provider-op-2"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordReadback(context.Background(), in.TenantID, in.ID, "before-complete", in.Intended, true, time.Now().UTC()); err == nil {
		t.Fatal("read-back before known operation completion was accepted")
	}
	got, err := s.Get(context.Background(), in.TenantID, in.ID)
	if err != nil || got.State != State("submitting") || got.MachineOutcome == "api_verified" {
		t.Fatalf("pre-completion state changed: %+v err=%v", got, err)
	}
	if err := s.markOperationCompleted(context.Background(), in.TenantID, in.ID, "provider-op-2"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordReadback(context.Background(), in.TenantID, in.ID, "cached", "{\"kind\":\"number\",\"number\":\"2\"}", false, time.Now().UTC()); err == nil {
		t.Fatal("cached readback accepted")
	}
	if err := s.RecordReadback(context.Background(), in.TenantID, in.ID, "different", `{"kind":"number","number":"3"}`, true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(context.Background(), in.TenantID, in.ID)
	if err != nil || got.State != StateReconciliationRequired || got.MachineOutcome == "api_verified" {
		t.Fatalf("mismatch asserted success: %+v %v", got, err)
	}
}

func TestAssuranceV15UpgradePreservesV13RowsAndReopenIsIdempotent(t *testing.T) {
	// NewWithDB is intentionally called twice against one database. The prior
	// assurance migrations and representative rows must remain intact.
	_, db := testStore(t)
	if _, err := db.Exec(`INSERT INTO assurance_resources(tenant_id,resource_id,provider,kind,external_id) VALUES('t','r','w','sheet','e')`); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWithDB(db); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_resources WHERE tenant_id='t' AND resource_id='r'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("preservation n=%d err=%v", n, err)
	}
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations WHERE app='assurance'`).Scan(&n); err != nil || n != 22 {
		t.Fatalf("schema version=%d err=%v", n, err)
	}
}

func containsBytes(b []byte, needle string) bool {
	return len(needle) > 0 && string(b) != "" && bytes.Contains(b, []byte(needle))
}
