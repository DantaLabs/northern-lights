package content

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/identity"
)

func TestAcknowledgeDeniesBeforeReservationLookup(t *testing.T) {
	store, db, _, _, _ := openSignedIntakeStore(t, 86400, 43200, 7200, true)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close fixture database: %v", err)
		}
	})
	service, err := NewAcknowledgeService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := AcknowledgeRequest{
		PlacementIntentID: "11111111-1111-4111-8111-111111111111", Observation: "not_reviewed",
		ResultSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	for _, principal := range []identity.Principal{
		{TenantID: intakeTestTenant, ObjectID: intakeTestActor, TokenType: identity.TokenTypeApplication, Permissions: []identity.Permission{identity.PermissionContentAcknowledge}},
		{TenantID: intakeTestTenant, ObjectID: intakeTestActor, TokenType: identity.TokenTypeDelegated, Permissions: []identity.Permission{identity.PermissionContentConfirm}},
	} {
		ctx := identity.ContextWithPrincipal(context.Background(), principal)
		_, err = service.Acknowledge(ctx, "audit-denied", request, assurance.DigestIdempotencyKey("denied"), time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
		if err == nil {
			t.Fatalf("unauthorized principal %#v was accepted for content.acknowledge", principal)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_idempotency_records WHERE action='acknowledge'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("denied request created %d private reservations", count)
	}
}

func TestAcknowledgeReplayNeedsOnlyStoreAfterAuditAndPolicyUnavailable(t *testing.T) {
	store, db, _, _, _ := openSignedIntakeStore(t, 86400, 43200, 7200, true)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close fixture database: %v", err)
		}
	})
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{
		TenantID: intakeTestTenant, ObjectID: intakeTestActor,
		TokenType: identity.TokenTypeDelegated, Permissions: []identity.Permission{identity.PermissionContentAcknowledge},
	})
	request := AcknowledgeRequest{PlacementIntentID: "11111111-1111-4111-8111-111111111111", Observation: "not_reviewed", ResultSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	requestDigest, err := assurance.ContentAcknowledgementRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	keyDigest := assurance.DigestIdempotencyKey("ack-replay")
	principal, _ := identity.PrincipalFromContext(ctx)
	reservationRequest := assurance.ReservationRequest{
		ActorID: principal.AuditActor(), Tool: stageTool, Action: acknowledgeAction,
		IdempotencyDigest: keyDigest, RequestDigest: requestDigest,
		RetentionClass: "signed_content_acknowledgement", RetainUntil: now.Add(time.Hour),
	}
	owned, err := store.Reserve(ctx, reservationRequest, now)
	if err != nil {
		t.Fatal(err)
	}
	sealed := AcknowledgeResponse{Kind: "acknowledge", Status: "machine_verified_visual_ack_pending", PlacementIntentID: request.PlacementIntentID,
		AcknowledgementID: owned.RecordID, State: "machine_verified_visual_ack_pending", Observation: "not_reviewed", ResultSHA256: request.ResultSHA256, AuditID: "ack-sealed-audit"}
	envelope, err := assurance.CanonicalJSON(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SealReservation(ctx, owned.RecordID, owned.OwnerNonce, envelope, sealed.Status, sealed.PlacementIntentID, sealed.AuditID, now); err != nil {
		t.Fatal(err)
	}
	store.SetAuditLog(nil)
	service, err := NewAcknowledgeService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.Acknowledge(ctx, "new-audit-must-not-be-used", request, keyDigest, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("sealed replay depended on fresh resolver/audit/private intent: %v", err)
	}
	if got != sealed {
		t.Fatalf("replay=%+v want %+v", got, sealed)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_idempotency_records WHERE action='acknowledge'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("replay created another reservation: count=%d err=%v", count, err)
	}
}

func TestSignedIntakeStageConfirmAcknowledgeVisualConfirmationAndReplay(t *testing.T) {
	fixture := newAcknowledgementFlow(t)
	request := fixture.request("visually_confirmed")
	key := assurance.DigestIdempotencyKey("ack-flow-success-key")
	service, err := NewAcknowledgeService(fixture.store, fixture.resolver)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Acknowledge(fixture.ctx, "audit-ack-success", request, key, fixture.now)
	if err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if result.Status != "visually_acknowledged" || result.State != "visually_acknowledged" || result.ResultSHA256 != fixture.confirmed.ResultSHA256 {
		t.Fatalf("unexpected acknowledgement result: %+v", result)
	}
	principal, ok := identity.PrincipalFromContext(fixture.ctx)
	if !ok {
		t.Fatal("fixture principal missing")
	}
	requestDigest, err := assurance.ContentAcknowledgementRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	reservation, found, err := fixture.store.LookupReservation(fixture.ctx, assurance.ReservationRequest{
		ActorID: principal.AuditActor(), Tool: "workiva_content_placement", Action: "acknowledge",
		IdempotencyDigest: key, RequestDigest: requestDigest,
	})
	if err != nil || !found || result.AcknowledgementID != reservation.RecordID {
		t.Fatalf("acknowledgement ID=%q reservation=%+v found=%v err=%v", result.AcknowledgementID, reservation, found, err)
	}
	intent, err := fixture.store.GetContentAcknowledgementIntentAt(fixture.ctx, request.PlacementIntentID, fixture.now)
	if err != nil || intent.State != "visually_acknowledged" || intent.MachineResultRecordID == "" || intent.ConfirmationRecordID == "" {
		t.Fatalf("acknowledged intent=%+v err=%v", intent, err)
	}
	if fixture.provider.updates != 1 {
		t.Fatalf("acknowledgement caused another provider write: updates=%d", fixture.provider.updates)
	}

	fixture.store.SetAuditLog(nil)
	replayService, err := NewAcknowledgeService(fixture.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := replayService.Acknowledge(fixture.ctx, "", request, key, fixture.now.Add(time.Minute))
	if err != nil || replay != result || replay.AcknowledgementID != result.AcknowledgementID || fixture.provider.updates != 1 {
		t.Fatalf("replay=%+v result=%+v updates=%d err=%v", replay, result, fixture.provider.updates, err)
	}
}

func TestNotReviewedPreservesMachineProofForLaterVisualConfirmation(t *testing.T) {
	fixture := newAcknowledgementFlow(t)
	service, err := NewAcknowledgeService(fixture.store, fixture.resolver)
	if err != nil {
		t.Fatal(err)
	}
	pendingRequest := fixture.request("not_reviewed")
	pending, err := service.Acknowledge(fixture.ctx, "audit-ack-not-reviewed", pendingRequest, assurance.DigestIdempotencyKey("ack-flow-not-reviewed-key"), fixture.now)
	if err != nil || pending.State != "machine_verified_visual_ack_pending" {
		t.Fatalf("not_reviewed result=%+v err=%v", pending, err)
	}
	intent, err := fixture.store.GetContentAcknowledgementIntentAt(fixture.ctx, pendingRequest.PlacementIntentID, fixture.now)
	if err != nil || intent.RowVersion != fixture.confirmed.RowVersion || intent.MachineResultRecordID == "" {
		t.Fatalf("not_reviewed invalidated sealed confirmation version: intent=%+v err=%v", intent, err)
	}
	confirmedRequest := fixture.request("visually_confirmed")
	confirmed, err := service.Acknowledge(fixture.ctx, "audit-ack-later-confirmed", confirmedRequest, assurance.DigestIdempotencyKey("ack-flow-confirmed-after-review-key"), fixture.now)
	if err != nil || confirmed.State != "visually_acknowledged" || confirmed.ResultSHA256 != fixture.confirmed.ResultSHA256 {
		t.Fatalf("later visual confirmation=%+v err=%v", confirmed, err)
	}
}

func TestVisualConflictFreezesStableEpisodeWithoutAnotherProviderWrite(t *testing.T) {
	fixture := newAcknowledgementFlow(t)
	service, err := NewAcknowledgeService(fixture.store, fixture.resolver)
	if err != nil {
		t.Fatal(err)
	}
	request := fixture.request("visual_conflict")
	result, err := service.Acknowledge(fixture.ctx, "audit-ack-conflict", request, assurance.DigestIdempotencyKey("ack-flow-conflict-key"), fixture.now)
	if err != nil || result.State != "reconciliation_required" {
		t.Fatalf("visual conflict result=%+v err=%v", result, err)
	}
	intent, err := fixture.store.GetOwnedContentReconciliationIntent(fixture.ctx, request.PlacementIntentID)
	if err != nil || intent.State != "reconciliation_required" || !isCanonicalUUID(intent.UncertaintyEpisodeID) {
		t.Fatalf("conflict intent=%+v err=%v", intent, err)
	}
	intentAgain, err := fixture.store.GetOwnedContentReconciliationIntent(fixture.ctx, request.PlacementIntentID)
	if err != nil || intentAgain.UncertaintyEpisodeID != intent.UncertaintyEpisodeID {
		t.Fatalf("uncertainty episode was not stable: first=%q second=%q err=%v", intent.UncertaintyEpisodeID, intentAgain.UncertaintyEpisodeID, err)
	}
	if fixture.provider.updates != 1 {
		t.Fatalf("visual conflict caused another provider submission: updates=%d", fixture.provider.updates)
	}
}

func TestAcknowledgeRejectsWrongProofActorAndCapability(t *testing.T) {
	fixture := newAcknowledgementFlow(t)
	service, err := NewAcknowledgeService(fixture.store, fixture.resolver)
	if err != nil {
		t.Fatal(err)
	}
	wrongProof := fixture.request("visually_confirmed")
	wrongProof.ResultSHA256 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	wrongKey := assurance.DigestIdempotencyKey("ack-flow-wrong-proof-key")
	if _, err := service.Acknowledge(fixture.ctx, "audit-ack-wrong-proof", wrongProof, wrongKey, fixture.now); err == nil {
		t.Fatal("acknowledgement accepted a result digest different from the sealed confirmation envelope")
	}
	intent, err := fixture.store.GetContentAcknowledgementIntentAt(fixture.ctx, wrongProof.PlacementIntentID, fixture.now)
	if err != nil || intent.State != "machine_verified_visual_ack_pending" {
		t.Fatalf("wrong proof changed intent: %+v err=%v", intent, err)
	}
	other := fixture.request("visually_confirmed")
	otherPrincipal := identity.Principal{TenantID: testTenant, ObjectID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", TokenType: identity.TokenTypeDelegated, Permissions: []identity.Permission{identity.PermissionContentAcknowledge}}
	otherCtx := identity.ContextWithPrincipal(context.Background(), otherPrincipal)
	if _, err := service.Acknowledge(otherCtx, "audit-ack-other-actor", other, assurance.DigestIdempotencyKey("ack-flow-other-actor-key"), fixture.now); err == nil {
		t.Fatal("different actor acknowledged a private placement intent")
	}
	confirmOnly := fixture.ctx
	principal, _ := identity.PrincipalFromContext(confirmOnly)
	principal.Permissions = []identity.Permission{identity.PermissionContentConfirm}
	confirmOnly = identity.ContextWithPrincipal(context.Background(), principal)
	if _, err := service.Acknowledge(confirmOnly, "audit-ack-cross-capability", other, assurance.DigestIdempotencyKey("ack-flow-cross-capability-key"), fixture.now); err == nil {
		t.Fatal("content.confirm capability authorized content.acknowledge")
	}
	validService, err := NewAcknowledgeService(fixture.store, fixture.resolver)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := validService.Acknowledge(fixture.ctx, "audit-ack-after-negative-cases", fixture.request("visually_confirmed"), assurance.DigestIdempotencyKey("ack-flow-valid-after-negatives"), fixture.now)
	if err != nil || valid.State != "visually_acknowledged" {
		t.Fatalf("negative cases damaged proof or intent: %+v err=%v", valid, err)
	}
}

func TestAcknowledgeAuditLinkFailureRollsBackStateAndSeal(t *testing.T) {
	fixture := newAcknowledgementFlow(t)
	if _, err := fixture.db.Exec(`CREATE TRIGGER fail_ack_intent_link BEFORE INSERT ON assurance_audit_links WHEN NEW.entity_kind='content_placement_intent' AND NEW.entity_id='` + fixture.intentID + `' BEGIN SELECT RAISE(ABORT,'injected acknowledgement link failure'); END`); err != nil {
		t.Fatal(err)
	}
	service, err := NewAcknowledgeService(fixture.store, fixture.resolver)
	if err != nil {
		t.Fatal(err)
	}
	request := fixture.request("visually_confirmed")
	key := assurance.DigestIdempotencyKey("ack-flow-audit-rollback-key")
	if _, err := service.Acknowledge(fixture.ctx, "audit-ack-rollback", request, key, fixture.now); err == nil {
		t.Fatal("injected audit-link failure unexpectedly committed acknowledgement")
	}
	intent, err := fixture.store.GetContentAcknowledgementIntentAt(fixture.ctx, fixture.intentID, fixture.now)
	if err != nil || intent.State != "machine_verified_visual_ack_pending" {
		t.Fatalf("audit failure changed intent: %+v err=%v", intent, err)
	}
	assertAckReservationStillOwned(t, fixture, request, key)
}

func TestAcknowledgeStateCASFailureRollsBackReservation(t *testing.T) {
	fixture := newAcknowledgementFlow(t)
	if _, err := fixture.db.Exec(`CREATE TRIGGER ignore_ack_state BEFORE UPDATE OF state ON assurance_content_placement_intents WHEN NEW.placement_intent_id='` + fixture.intentID + `' AND NEW.state='visually_acknowledged' BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	service, err := NewAcknowledgeService(fixture.store, fixture.resolver)
	if err != nil {
		t.Fatal(err)
	}
	request := fixture.request("visually_confirmed")
	key := assurance.DigestIdempotencyKey("ack-flow-cas-failure-key")
	if _, err := service.Acknowledge(fixture.ctx, "audit-ack-cas-failure", request, key, fixture.now); err == nil {
		t.Fatal("state CAS failure unexpectedly committed acknowledgement")
	}
	intent, err := fixture.store.GetContentAcknowledgementIntentAt(fixture.ctx, fixture.intentID, fixture.now)
	if err != nil || intent.State != "machine_verified_visual_ack_pending" {
		t.Fatalf("CAS failure changed intent: %+v err=%v", intent, err)
	}
	assertAckReservationStillOwned(t, fixture, request, key)
}

type acknowledgementFlow struct {
	store     *assurance.Store
	db        *sql.DB
	ctx       context.Context
	now       time.Time
	resolver  *assurance.ContentPolicyResolver
	provider  *confirmationFlowProvider
	intentID  string
	confirmed ConfirmResponse
}

func newAcknowledgementFlow(t *testing.T) acknowledgementFlow {
	t.Helper()
	store, db, ctx, now, resolver, provider, created, _ := confirmationFlowFixtureWithDB(t, nil, identity.PermissionContentAcknowledge, identity.PermissionContentReconcile)
	stage := stageConfirmationPreview(t, store, ctx, now, resolver, provider, created, "ack")
	intentID := placementIntentIDFromPreview(t, stage.Preview)
	fence, err := NewLocalTestFence(testTenant, "wave4-ack-test-env")
	if err != nil {
		t.Fatal(err)
	}
	confirmService, err := NewLocalTestConfirmationService(store, AssuranceCandidateResolver{Store: store}, resolver, provider, fence, fixedStageClock{now})
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := confirmService.Confirm(ctx, "audit-ack-confirm", ConfirmRequest{PlacementIntentID: intentID, ConfirmationToken: stage.ConfirmationToken, IdempotencyKey: "confirm-ack-flow-key-000001"})
	if err != nil {
		t.Fatalf("Confirm before acknowledgement: %v", err)
	}
	if confirmed.Status != "machine_verified_visual_ack_pending" || confirmed.ResultSHA256 == "" {
		t.Fatalf("confirm did not produce sealed machine proof: %+v", confirmed)
	}
	return acknowledgementFlow{store: store, db: db, ctx: ctx, now: now, resolver: resolver, provider: provider, intentID: intentID, confirmed: confirmed}
}

func (f acknowledgementFlow) request(observation string) AcknowledgeRequest {
	return AcknowledgeRequest{PlacementIntentID: f.intentID, Observation: observation, ResultSHA256: f.confirmed.ResultSHA256}
}

func assertAckReservationStillOwned(t *testing.T, f acknowledgementFlow, request AcknowledgeRequest, key assurance.Digest) {
	t.Helper()
	digest, err := assurance.ContentAcknowledgementRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := identity.PrincipalFromContext(f.ctx)
	prior, found, err := f.store.LookupReservation(f.ctx, assurance.ReservationRequest{ActorID: p.AuditActor(), Tool: stageTool, Action: acknowledgeAction, IdempotencyDigest: key, RequestDigest: digest})
	if err != nil || !found || prior.Disposition != assurance.ReservationInProgress {
		t.Fatalf("failed acknowledgement reservation=%+v found=%v err=%v", prior, found, err)
	}
}
