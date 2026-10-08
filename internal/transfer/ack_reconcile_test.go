package transfer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
)

func trustedTransferOperatorContext(tenant string) context.Context {
	return identity.ContextWithPrincipal(context.Background(), identity.Principal{
		TenantID: tenant, ObjectID: "actor-1", Permissions: identity.AllPermissions(),
	})
}

func TestAcknowledgeUsesIndependentVisualEvidenceAndTokenFreeReplay(t *testing.T) {
	svc, fixture, store := newServiceFixture(t)
	staged, tenant, actor, permission := stageForConfirm(t, svc, fixture)
	if _, err := svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{
		TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID,
		Token: staged.Token, Now: time.Now().UTC(), IdempotencyDigest: "ack-confirm-key", RequestDigest: "ack-confirm-request",
	}); err != nil {
		t.Fatal(err)
	}
	ctx := trustedTransferOperatorContext(tenant)
	request := AcknowledgeRequest{
		TransferID: staged.TransferID, Observation: "matches", Refreshed: true,
		UILocationJSON:    `{"resource_id":"dst","locator":"Results!C4"}`,
		IdempotencyDigest: "ack-key", RequestDigest: "ack-request", Now: time.Now().UTC(),
	}
	first, err := svc.Acknowledge(ctx, request)
	if err != nil || first.Status != "visually_acknowledged" || first.ReconciliationRequired {
		t.Fatalf("acknowledgement=%+v err=%v", first, err)
	}
	if got, err := store.Get(ctx, tenant, staged.TransferID); err != nil || got.State != StateVisuallyAcknowledged || got.MachineOutcome != "api_verified" {
		t.Fatalf("visual ack changed machine result: %+v err=%v", got, err)
	}
	reads, posts := fixture.reads.Load(), fixture.posts.Load()
	request.UILocationJSON = `{"resource_id":"other","locator":"Other!A1"}`
	replay, err := svc.Acknowledge(ctx, request)
	if err != nil || replay.Status != "idempotency_replay" || fixture.reads.Load() != reads || fixture.posts.Load() != posts {
		t.Fatalf("ack replay=%+v err=%v reads=%d posts=%d", replay, err, fixture.reads.Load(), fixture.posts.Load())
	}
	var envelope string
	if err := store.db.QueryRow(`SELECT response_envelope FROM assurance_idempotency_records WHERE action='acknowledge' AND entity_reference=?`, staged.TransferID).Scan(&envelope); err != nil {
		t.Fatal(err)
	}
	if containsBytes([]byte(envelope), staged.Token) {
		t.Fatal("acknowledgement replay envelope persisted the confirmation token")
	}
}

func TestReconcileReadBackAndClassificationNeverSubmitAgain(t *testing.T) {
	svc, fixture, store := newServiceFixture(t)
	staged, tenant, actor, permission := stageForConfirm(t, svc, fixture)
	fixture.waitErr = errors.New("operation status unavailable")
	_, err := svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{
		TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID,
		Token: staged.Token, Now: time.Now().UTC(), IdempotencyDigest: "reconcile-confirm-key", RequestDigest: "reconcile-confirm-request",
	})
	if err == nil || fixture.posts.Load() != 1 {
		t.Fatalf("expected one ambiguous provider submission, err=%v posts=%d", err, fixture.posts.Load())
	}
	ctx := trustedTransferOperatorContext(tenant)
	readback, err := svc.Reconcile(ctx, ReconcileRequest{TransferID: staged.TransferID, Action: "read_back", IdempotencyDigest: "readback-key", RequestDigest: "readback-request", Now: time.Now().UTC()})
	if err != nil || readback.ReadbackID == "" || !readback.ReadbackCacheBypassed || readback.Status != string(StateReconciliationRequired) {
		t.Fatalf("read-back=%+v err=%v", readback, err)
	}
	closed, err := svc.Reconcile(ctx, ReconcileRequest{TransferID: staged.TransferID, Action: "classify", Classification: "confirmed_applied", Note: "equal uncached read-back", IdempotencyDigest: "classify-key", RequestDigest: "classify-request", Now: time.Now().UTC()})
	if err != nil || closed.Status != string(StateMachineVerified) || closed.MachineOutcomeProvenance != "reconciliation" {
		t.Fatalf("classification=%+v err=%v", closed, err)
	}
	if fixture.posts.Load() != 1 {
		t.Fatalf("reconciliation submitted a second mutation: %d", fixture.posts.Load())
	}
	got, err := store.Get(ctx, tenant, staged.TransferID)
	if err != nil || got.State != StateMachineVerified {
		t.Fatalf("reconciled state=%+v err=%v", got, err)
	}
}

func TestReconcileNotAppliedNeedsAuthoritativeOperationFailure(t *testing.T) {
	svc, fixture, store := newServiceFixture(t)
	staged, tenant, actor, permission := stageForConfirm(t, svc, fixture)
	fixture.waitErr = errors.New("operation status unavailable")
	_, _ = svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{
		TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID,
		Token: staged.Token, Now: time.Now().UTC(), IdempotencyDigest: "not-applied-confirm-key", RequestDigest: "not-applied-confirm-request",
	})
	ctx := trustedTransferOperatorContext(tenant)
	_, err := svc.Reconcile(ctx, ReconcileRequest{TransferID: staged.TransferID, Action: "classify", Classification: "confirmed_not_applied", Note: "target differs", IdempotencyDigest: "not-applied-key", RequestDigest: "not-applied-request", Now: time.Now().UTC()})
	if err == nil {
		t.Fatal("unequal target/readback without failed operation proof was accepted")
	}
	got, getErr := store.Get(ctx, tenant, staged.TransferID)
	if getErr != nil || got.State != StateReconciliationRequired || fixture.posts.Load() != 1 {
		t.Fatalf("failed not-applied classification state=%+v err=%v posts=%d", got, getErr, fixture.posts.Load())
	}
}
