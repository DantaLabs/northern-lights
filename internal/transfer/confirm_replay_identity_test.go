package transfer

import (
	"context"
	"errors"
	"github.com/dantalabs/northern-lights/internal/assurance"
	"reflect"
	"testing"
	"time"
)

func confirmReplayRequest(id, token string) ConfirmRequest {
	return ConfirmRequest{TransferID: id, Token: token, IdempotencyDigest: "ingress-key-digest", RequestDigest: "canonical-request-digest", Now: time.Now().UTC()}
}

func TestConfirmReplaySealedWithoutTokenAuditOrProvider(t *testing.T) {
	svc, f, store := newServiceFixture(t)
	staged, tenant, _, _ := stageForConfirm(t, svc, f)
	ctx := b1TrustedConfirmContext(tenant)
	req := confirmReplayRequest(staged.TransferID, staged.Token)
	first, err := svc.Confirm(ctx, req)
	if err != nil || first.State != StateMachineVerified {
		t.Fatalf("first confirm: %+v %v", first, err)
	}
	reads, posts := f.reads.Load(), f.posts.Load()
	if _, err := store.db.Exec(`UPDATE transfer_intents SET visual_state='changed',machine_outcome='changed',state='reconciliation_required' WHERE transfer_id=?`, staged.TransferID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	f.postErr = errors.New("provider unavailable")
	req.Token = ""
	replay, err := svc.Confirm(ctx, req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !reflect.DeepEqual(replay, first) {
		t.Fatalf("replay changed with mutable transfer: first=%+v replay=%+v", first, replay)
	}
	if f.reads.Load() != reads || f.posts.Load() != posts {
		t.Fatalf("replay called provider reads=%d posts=%d", f.reads.Load()-reads, f.posts.Load()-posts)
	}
	// Authorization is still checked before lookup.
	if _, err := svc.Confirm(context.Background(), req); err == nil {
		t.Fatal("anonymous replay")
	}
}

func TestConfirmConflictAndInProgressBeforeTokenAuditCASProvider(t *testing.T) {
	svc, f, store := newServiceFixture(t)
	staged, tenant, _, _ := stageForConfirm(t, svc, f)
	ctx := b1TrustedConfirmContext(tenant)
	req := confirmReplayRequest(staged.TransferID, staged.Token)
	reservationRequest := assurance.ReservationRequest{ActorID: tenant + "/actor-1", Tool: "workiva_transfer_value", Action: "confirm", IdempotencyDigest: stageReservationDigest(req.IdempotencyDigest), RequestDigest: stageReservationDigest(req.RequestDigest)}
	reserved, err := svc.assuranceStore.Reserve(ctx, reservationRequest, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if reserved.RecordID == "" {
		t.Fatal("reservation missing")
	}
	reads, posts := f.reads.Load(), f.posts.Load()
	if _, err := store.db.Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	req.Token = ""
	if _, err := svc.Confirm(ctx, req); err == nil || err.Error() != "idempotency_in_progress" {
		t.Fatalf("in progress: %v", err)
	}
	req.RequestDigest = "different-canonical-request"
	if _, err := svc.Confirm(ctx, req); err == nil || err.Error() != "idempotency_conflict" {
		t.Fatalf("conflict: %v", err)
	}
	if f.reads.Load() != reads || f.posts.Load() != posts {
		t.Fatal("reservation hit called provider")
	}
	got, err := store.Get(ctx, tenant, staged.TransferID)
	if err != nil || got.State != StateStaged {
		t.Fatalf("reservation hit mutated transfer: %+v %v", got, err)
	}
}

func TestConfirmMissingIngressDigestsRejected(t *testing.T) {
	svc, f, _ := newServiceFixture(t)
	staged, tenant, _, _ := stageForConfirm(t, svc, f)
	for _, req := range []ConfirmRequest{{TransferID: staged.TransferID, Token: staged.Token, RequestDigest: "request"}, {TransferID: staged.TransferID, Token: staged.Token, IdempotencyDigest: "key"}} {
		if _, err := svc.Confirm(b1TrustedConfirmContext(tenant), req); err == nil {
			t.Fatalf("accepted missing digest %+v", req)
		}
	}
	if f.posts.Load() != 0 {
		t.Fatal("missing digests posted")
	}
}
