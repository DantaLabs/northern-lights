package content

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/workiva"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

type reconciliationTestProvider struct {
	*confirmationFlowProvider
	inspectionCalls atomic.Int32
	readCalls       atomic.Int32
}

type reconciliationReadFailureProvider struct {
	*reconciliationTestProvider
	readErr error
}

func (p *reconciliationReadFailureProvider) ReadContentMetadata(context.Context, string, string, string) (workivaprovider.ContentMetadata, error) {
	p.readCalls.Add(1)
	return workivaprovider.ContentMetadata{}, p.readErr
}

func (p *reconciliationTestProvider) InspectOperation(_ context.Context, reference string) (workiva.OperationInspection, error) {
	p.inspectionCalls.Add(1)
	return workiva.OperationInspection{Reference: reference, Status: "completed"}, nil
}

func (p *reconciliationTestProvider) ReadContentMetadata(ctx context.Context, spreadsheet, sheet, cell string) (workivaprovider.ContentMetadata, error) {
	p.readCalls.Add(1)
	return p.confirmationFlowProvider.ReadContentMetadata(ctx, spreadsheet, sheet, cell)
}

var _ ReconciliationProvider = (*reconciliationTestProvider)(nil)

func TestReconcileRequestMatchesPublishedActionSelectorRules(t *testing.T) {
	base := ReconcileRequest{PlacementIntentID: "11111111-1111-4111-8111-111111111111", IdempotencyKey: "reconcile-idempotency-key-01", Action: "read_back"}
	if err := validateReconcileRequest(base); err != nil {
		t.Fatalf("valid read_back: %v", err)
	}
	invalid := []ReconcileRequest{
		{PlacementIntentID: base.PlacementIntentID, IdempotencyKey: base.IdempotencyKey, Action: "read_back", Classification: "still_unknown"},
		{PlacementIntentID: base.PlacementIntentID, IdempotencyKey: base.IdempotencyKey, Action: "classify", Classification: "still_unknown"},
		{PlacementIntentID: base.PlacementIntentID, IdempotencyKey: base.IdempotencyKey, Action: "classify", Classification: "confirmed_applied", UncertaintyEpisodeID: "episode-1"},
		{PlacementIntentID: base.PlacementIntentID, IdempotencyKey: base.IdempotencyKey, Action: "classify", Classification: "confirmed_not_applied", UncertaintyEpisodeID: "episode-1", NoEffectProofID: "proof-1", LatestReadbackEvidenceID: "readback-1"},
		{PlacementIntentID: base.PlacementIntentID, IdempotencyKey: base.IdempotencyKey, Action: "classify", Classification: "unknown", UncertaintyEpisodeID: "episode-1"},
	}
	for i, request := range invalid {
		if err := validateReconcileRequest(request); err == nil {
			t.Errorf("invalid selector case %d accepted", i)
		}
	}
	if err := validateReconcileRequest(ReconcileRequest{PlacementIntentID: base.PlacementIntentID, IdempotencyKey: base.IdempotencyKey, Action: "classify", Classification: "confirmed_applied", UncertaintyEpisodeID: "episode-1", LatestReadbackEvidenceID: "readback-1"}); err != nil {
		t.Fatalf("published applied selector rejected: %v", err)
	}
}

func TestReconcileReadBackPersistsFreshEvidenceAndReplaysWithoutProvider(t *testing.T) {
	store, ctx, now, resolver, baseProvider, created, _ := confirmationFlowFixture(t, errors.New("accepted response lost"), identity.PermissionContentReconcile)
	staged := stageConfirmationPreview(t, store, ctx, now, resolver, baseProvider, created, "reconcile")
	fence, err := NewLocalTestFence(testTenant, "wave4-test-env")
	if err != nil {
		t.Fatal(err)
	}
	confirm, err := NewLocalTestConfirmationService(store, AssuranceCandidateResolver{Store: store}, resolver, baseProvider, fence, fixedStageClock{now})
	if err != nil {
		t.Fatal(err)
	}
	intentID := placementIntentIDFromPreview(t, staged.Preview)
	_, err = confirm.Confirm(ctx, "audit-confirm-before-reconcile", ConfirmRequest{PlacementIntentID: intentID, ConfirmationToken: staged.ConfirmationToken, IdempotencyKey: "confirm-reconcile-integration-01"})
	var unknown *UnknownConfirmationError
	if !errors.As(err, &unknown) || unknown.ResubmissionAllowed || !unknown.ReconciliationRequired {
		t.Fatalf("expected frozen unknown confirmation, got %v", err)
	}
	provider := &reconciliationTestProvider{confirmationFlowProvider: baseProvider}
	service, err := NewLocalTestReconciliationService(store, resolver, provider, fence, fixedStageClock{now.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	request := ReconcileRequest{PlacementIntentID: intentID, Action: "read_back", IdempotencyKey: "reconcile-readback-idempotency-01"}
	result, err := service.Reconcile(ctx, "audit-content-reconcile-read", request)
	if err != nil {
		var domain *assurance.Error
		if errors.As(err, &domain) {
			t.Fatalf("Reconcile read_back: %v cause=%v", err, domain.Cause)
		}
		t.Fatalf("Reconcile read_back: %v", err)
	}
	if result.Kind != "reconcile" || result.State != "still_unknown" || !result.ReconciliationRequired || result.ResubmissionAllowed || !result.OperationKnown || len(result.EvidenceIDs) != 1 || result.LatestReadbackID != "" || result.EvidenceBindingHash == "" || provider.inspectionCalls.Load() != 1 || provider.readCalls.Load() != 1 {
		t.Fatalf("unexpected readback result=%+v inspection=%d read=%d", result, provider.inspectionCalls.Load(), provider.readCalls.Load())
	}
	var episode string
	if err := store.DB().QueryRow(`SELECT uncertainty_episode_id FROM assurance_content_placement_intents WHERE placement_intent_id=?`, intentID).Scan(&episode); err != nil || episode != result.UncertaintyEpisodeID {
		t.Fatalf("episode=%q result=%q err=%v", episode, result.UncertaintyEpisodeID, err)
	}
	replay := &ReconciliationService{Store: store}
	replayed, err := replay.Reconcile(ctx, "", request)
	if err != nil || replayed.EvidenceBindingHash != result.EvidenceBindingHash || replayed.ResultSHA256 != result.ResultSHA256 || provider.inspectionCalls.Load() != 1 || provider.readCalls.Load() != 1 {
		t.Fatalf("replay=%+v err=%v provider calls=%d/%d", replayed, err, provider.inspectionCalls.Load(), provider.readCalls.Load())
	}
}

func TestReconcileAppliedRecoversExactTerminalLeftByPrecommitAuditFailure(t *testing.T) {
	store, ctx, now, resolver, provider, created, _ := confirmationFlowFixture(t, nil, identity.PermissionContentReconcile, identity.PermissionContentAcknowledge)
	staged := stageConfirmationPreview(t, store, ctx, now, resolver, provider, created, "reconcile-terminal-recovery")
	fence, err := NewLocalTestFence(testTenant, "wave4-test-env")
	if err != nil {
		t.Fatal(err)
	}
	confirm, err := NewLocalTestConfirmationService(store, AssuranceCandidateResolver{Store: store}, resolver, provider, fence, fixedStageClock{now})
	if err != nil {
		t.Fatal(err)
	}
	intentID := placementIntentIDFromPreview(t, staged.Preview)
	if _, err := store.DB().Exec(`CREATE TRIGGER fail_content_terminal_audit BEFORE INSERT ON audit_log WHEN NEW.action='confirm.machine_verified' BEGIN SELECT RAISE(FAIL,'injected terminal audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := confirm.Confirm(ctx, "audit-confirm-terminal-before-failure", ConfirmRequest{PlacementIntentID: intentID, ConfirmationToken: staged.ConfirmationToken, IdempotencyKey: "confirm-terminal-recovery-01"}); err == nil {
		t.Fatal("expected terminal audit failure after provider acceptance")
	} else {
		var unknown *UnknownConfirmationError
		if !errors.As(err, &unknown) {
			t.Fatalf("expected frozen unknown confirmation, got %v", err)
		}
	}
	provider.mu.Lock()
	writesBeforeRecovery := provider.updates
	provider.mu.Unlock()
	if writesBeforeRecovery != 1 {
		t.Fatalf("confirmation setup submitted %d writes, want exactly one", writesBeforeRecovery)
	}
	var episode string
	if err := store.DB().QueryRow(`SELECT uncertainty_episode_id FROM assurance_content_placement_intents WHERE placement_intent_id=?`, intentID).Scan(&episode); err != nil {
		t.Fatal(err)
	}
	if episode == "" {
		t.Fatal("confirmation did not freeze an uncertainty episode")
	}
	service, err := NewLocalTestReconciliationService(store, resolver, &reconciliationTestProvider{confirmationFlowProvider: provider}, fence, fixedStageClock{now.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	readback, err := service.Reconcile(ctx, "audit-reconcile-readback-before-applied", ReconcileRequest{PlacementIntentID: intentID, Action: "read_back", IdempotencyKey: "reconcile-terminal-read-01"})
	if err != nil || len(readback.EvidenceIDs) != 1 {
		t.Fatalf("readback=%+v err=%v", readback, err)
	}
	readback, err = service.Reconcile(ctx, "audit-reconcile-readback-again", ReconcileRequest{PlacementIntentID: intentID, Action: "read_back", IdempotencyKey: "reconcile-terminal-read-02"})
	if err != nil || len(readback.EvidenceIDs) != 1 {
		t.Fatalf("repeated readback=%+v err=%v", readback, err)
	}
	result, err := service.Reconcile(ctx, "audit-reconcile-applied-recovery", ReconcileRequest{PlacementIntentID: intentID, Action: "classify", Classification: "confirmed_applied", UncertaintyEpisodeID: episode, LatestReadbackEvidenceID: readback.EvidenceIDs[0], IdempotencyKey: "reconcile-terminal-classify-01"})
	if err != nil {
		var domain *assurance.Error
		if errors.As(err, &domain) {
			t.Fatalf("applied recovery: %v cause=%v", err, domain.Cause)
		}
		t.Fatalf("applied recovery: %v", err)
	}
	if result.State != "confirmed_applied" || result.ReconciliationRequired || result.ResubmissionAllowed || result.LatestReadbackID != readback.EvidenceIDs[0] {
		t.Fatalf("unexpected recovery result: %+v", result)
	}
	var state string
	if err := store.DB().QueryRow(`SELECT state FROM assurance_content_placement_intents WHERE placement_intent_id=?`, intentID).Scan(&state); err != nil || state != "machine_verified_visual_ack_pending" {
		t.Fatalf("final state=%q err=%v", state, err)
	}
	var originalConfirmState string
	if result.ResultSHA256 == "" {
		t.Fatal("applied reconciliation omitted its sealed result hash")
	}
	if err := store.DB().QueryRow(`SELECT state FROM assurance_idempotency_records WHERE entity_reference=? AND action='confirm'`, intentID).Scan(&originalConfirmState); err != nil || originalConfirmState != "reserved" {
		t.Fatalf("original confirmation error record changed: state=%q err=%v", originalConfirmState, err)
	}
	ack, err := NewAcknowledgeService(store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	acknowledged, err := ack.Acknowledge(ctx, "audit-ack-reconciled-machine-proof", AcknowledgeRequest{PlacementIntentID: intentID, Observation: "visually_confirmed", ResultSHA256: result.ResultSHA256}, assurance.DigestIdempotencyKey("ack-reconciled-machine-proof-01"), now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("acknowledge reconciled machine proof: %v", err)
	}
	if acknowledged.State != "visually_acknowledged" || acknowledged.Status != "visually_acknowledged" {
		t.Fatalf("reconciled result was not visually acknowledged: %+v", acknowledged)
	}
	var finalConfirmState string
	if err := store.DB().QueryRow(`SELECT state FROM assurance_idempotency_records WHERE entity_reference=? AND action='confirm'`, intentID).Scan(&finalConfirmState); err != nil || finalConfirmState != originalConfirmState {
		t.Fatalf("acknowledgement rewrote the original confirmation error: before=%q after=%q err=%v", originalConfirmState, finalConfirmState, err)
	}
	provider.mu.Lock()
	writesAfterRecovery := provider.updates
	provider.mu.Unlock()
	if writesAfterRecovery != writesBeforeRecovery {
		t.Fatalf("read_back/classify/ack submitted another provider write: before=%d after=%d", writesBeforeRecovery, writesAfterRecovery)
	}
}

func TestReconcileReplayRequiresDelegatedCapabilityBeforePrivateLookup(t *testing.T) {
	store, ctx, now, _, _, _, _ := confirmationFlowFixture(t, nil, identity.PermissionContentReconcile)
	service := &ReconciliationService{Store: store}
	request := ReconcileRequest{PlacementIntentID: "11111111-1111-4111-8111-111111111111", Action: "read_back", IdempotencyKey: "reconcile-auth-before-private-01"}
	principal, _ := identity.PrincipalFromContext(ctx)
	principal.Permissions = nil
	denied := identity.ContextWithPrincipal(context.Background(), principal)
	_, err := service.Reconcile(denied, "", request)
	var domain *assurance.Error
	if !errors.As(err, &domain) || domain.Code != "forbidden" {
		t.Fatalf("expected capability denial before selector lookup, got %v at %s", err, now)
	}
}

func TestReconcileProviderFailureSealsStructuredReplayWithoutRetry(t *testing.T) {
	store, ctx, now, resolver, baseProvider, created, _ := confirmationFlowFixture(t, errors.New("accepted response lost"), identity.PermissionContentReconcile)
	staged := stageConfirmationPreview(t, store, ctx, now, resolver, baseProvider, created, "reconcile-provider-failure")
	fence, err := NewLocalTestFence(testTenant, "wave4-test-env")
	if err != nil {
		t.Fatal(err)
	}
	confirm, err := NewLocalTestConfirmationService(store, AssuranceCandidateResolver{Store: store}, resolver, baseProvider, fence, fixedStageClock{now})
	if err != nil {
		t.Fatal(err)
	}
	intentID := placementIntentIDFromPreview(t, staged.Preview)
	_, err = confirm.Confirm(ctx, "audit-confirm-before-read-failure", ConfirmRequest{PlacementIntentID: intentID, ConfirmationToken: staged.ConfirmationToken, IdempotencyKey: "confirm-reconcile-failure-01"})
	var unknown *UnknownConfirmationError
	if !errors.As(err, &unknown) {
		t.Fatalf("expected frozen unknown confirmation, got %v", err)
	}
	provider := &reconciliationReadFailureProvider{reconciliationTestProvider: &reconciliationTestProvider{confirmationFlowProvider: baseProvider}, readErr: errors.New("provider unavailable")}
	service, err := NewLocalTestReconciliationService(store, resolver, provider, fence, fixedStageClock{now.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	request := ReconcileRequest{PlacementIntentID: intentID, Action: "read_back", IdempotencyKey: "reconcile-read-failure-idem-01"}
	_, err = service.Reconcile(ctx, "audit-content-reconcile-failure", request)
	var first *assurance.Error
	if !errors.As(err, &first) || first.Code != "provider_readback_unavailable" || provider.readCalls.Load() != 1 {
		t.Fatalf("first failure=%v calls=%d", err, provider.readCalls.Load())
	}
	replay := &ReconciliationService{Store: store}
	_, err = replay.Reconcile(ctx, "", request)
	var repeated *assurance.Error
	if !errors.As(err, &repeated) || repeated.Code != first.Code || provider.readCalls.Load() != 1 || provider.inspectionCalls.Load() != 1 {
		t.Fatalf("replayed failure=%v provider calls=%d/%d", err, provider.readCalls.Load(), provider.inspectionCalls.Load())
	}
}
