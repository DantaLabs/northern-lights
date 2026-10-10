package content

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/workiva"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
	"github.com/google/uuid"
)

type ReconcileRequest struct {
	PlacementIntentID        string `json:"placement_intent_id"`
	Action                   string `json:"action"`
	Classification           string `json:"classification,omitempty"`
	UncertaintyEpisodeID     string `json:"uncertainty_episode_id,omitempty"`
	LatestReadbackEvidenceID string `json:"latest_readback_evidence_id,omitempty"`
	NoEffectProofID          string `json:"no_effect_proof_id,omitempty"`
	Note                     string `json:"note,omitempty"`
	IdempotencyKey           string `json:"idempotency_key"`
}

// ReconciliationProvider has only read capabilities. No writer or submit
// interface is accepted by the reconciliation service.
type ReconciliationProvider interface {
	workivaprovider.ContentMetadataReader
	workivaprovider.OperationReader
}

type ReconciliationClock interface{ Now() time.Time }

type ReconciliationService struct {
	Store          *assurance.Store
	Resolver       *assurance.ContentPolicyResolver
	Provider       ReconciliationProvider
	Fence          ContentFence
	Clock          ReconciliationClock
	allowTestFence bool
}

type AssuranceReconciliationProfileResolver struct {
	Resolver *assurance.ContentPolicyResolver
}

func (r AssuranceReconciliationProfileResolver) ResolveDestination(ctx context.Context, id string, revision int, now time.Time) (ResolvedProfile, error) {
	if r.Resolver == nil {
		return ResolvedProfile{}, assurance.ErrNotReady
	}
	p, b, err := r.Resolver.ResolveContentDestinationForCapability(ctx, id, revision, now, identity.PermissionContentReconcile)
	if err != nil {
		return ResolvedProfile{}, err
	}
	return projectResolvedProfile(p, b)
}

func NewReconciliationService(store *assurance.Store, resolver *assurance.ContentPolicyResolver, provider ReconciliationProvider, fence ContentFence, clock ReconciliationClock) (*ReconciliationService, error) {
	if store == nil || resolver == nil || provider == nil || fence == nil || !fence.Durable() {
		return nil, errors.New("content: reconciliation dependencies are unavailable")
	}
	return &ReconciliationService{Store: store, Resolver: resolver, Provider: provider, Fence: fence, Clock: clock}, nil
}

func NewLocalTestReconciliationService(store *assurance.Store, resolver *assurance.ContentPolicyResolver, provider ReconciliationProvider, fence ContentFence, clock ReconciliationClock) (*ReconciliationService, error) {
	if store == nil || resolver == nil || provider == nil || fence == nil || fence.Durable() {
		return nil, errors.New("content: explicit local test fence is required")
	}
	return &ReconciliationService{Store: store, Resolver: resolver, Provider: provider, Fence: fence, Clock: clock, allowTestFence: true}, nil
}

// Reconcile collects one uncached typed readback or seals a local conservative
// classification. It has no content-writing capability; it never retries a
// provider operation or changes a destination cell.
func (s *ReconciliationService) Reconcile(ctx context.Context, auditID string, request ReconcileRequest) (assurance.ContentReconciliationResult, error) {
	p, ok := identity.PrincipalFromContext(ctx)
	if !ok || p.TokenType != identity.TokenTypeDelegated || p.UsedSubjectFallback || !p.HasPermission(identity.PermissionContentReconcile) || !isCanonicalUUID(p.TenantID) || !isCanonicalUUID(p.ObjectID) {
		return assurance.ContentReconciliationResult{}, &assurance.Error{Code: "forbidden", Message: "trusted delegated content.reconcile capability required"}
	}
	if s == nil || s.Store == nil {
		return assurance.ContentReconciliationResult{}, assurance.ErrNotReady
	}
	if err := validateReconcileRequest(request); err != nil {
		return assurance.ContentReconciliationResult{}, err
	}
	now := s.now()
	requestDigest, err := assurance.CanonicalJSON(struct {
		Phase          string `json:"phase"`
		Intent         string `json:"placement_intent_id"`
		Action         string `json:"action"`
		Classification string `json:"classification,omitempty"`
		Episode        string `json:"uncertainty_episode_id,omitempty"`
		Evidence       string `json:"latest_readback_evidence_id,omitempty"`
		NoEffect       string `json:"no_effect_proof_id,omitempty"`
		Note           string `json:"note,omitempty"`
	}{"reconcile", request.PlacementIntentID, request.Action, request.Classification, request.UncertaintyEpisodeID, request.LatestReadbackEvidenceID, request.NoEffectProofID, request.Note})
	if err != nil {
		return assurance.ContentReconciliationResult{}, err
	}
	requestHash := assurance.HashBytes(requestDigest)
	reservationRequest := assurance.ReservationRequest{ActorID: p.AuditActor(), Tool: stageTool, Action: request.Action, IdempotencyDigest: assurance.DigestIdempotencyKey(request.IdempotencyKey), RequestDigest: requestHash, RetentionClass: "signed_content_reconciliation"}
	prior, found, err := s.Store.LookupReservation(ctx, reservationRequest)
	if err != nil {
		return assurance.ContentReconciliationResult{}, err
	}
	if found {
		return reconciliationReplay(prior)
	}
	if s.Resolver == nil {
		return assurance.ContentReconciliationResult{}, &assurance.Error{Code: "content_policy_unavailable", Message: "configured signed policy resolver is required for new reconciliation"}
	}
	intent, err := s.Store.GetOwnedContentReconciliationIntent(ctx, request.PlacementIntentID)
	if err != nil {
		return assurance.ContentReconciliationResult{}, err
	}
	plan, err := decodeReconciliationPreview(intent, p)
	if err != nil {
		return assurance.ContentReconciliationResult{}, err
	}
	profile, bindings, err := s.resolveProfile(ctx, plan, now)
	if err != nil {
		return assurance.ContentReconciliationResult{}, err
	}
	if intent.State != "reconciliation_required" {
		return assurance.ContentReconciliationResult{}, &assurance.Error{Code: "content_reconciliation_conflict", Message: "placement intent is not awaiting reconciliation"}
	}
	recoveryCutoff := intent.CreatedAt.Add(time.Duration(bindings.Retention.RecoveryLifetimeSeconds) * time.Second)
	if bindings.Retention.RecoveryLifetimeSeconds < 1 || !recoveryCutoff.After(now) {
		return assurance.ContentReconciliationResult{}, &assurance.Error{Code: "content_recovery_expired", Message: "signed reconciliation recovery horizon is expired"}
	}
	if auditID == "" {
		return assurance.ContentReconciliationResult{}, &assurance.Error{Code: "invalid_request", Message: "rich audit ID is required for fresh reconciliation"}
	}
	if request.Action == "classify" {
		return s.classify(ctx, auditID, request, requestHash, reservationRequest, intent, plan, profile, bindings, p, now)
	}
	return s.readBack(ctx, auditID, request, requestHash, reservationRequest, intent, plan, profile, bindings, p, now)
}

func (s *ReconciliationService) classify(ctx context.Context, auditID string, request ReconcileRequest, requestHash assurance.Digest, reservationRequest assurance.ReservationRequest, intent assurance.ContentReconciliationIntent, plan PreviewPlan, profile ResolvedProfile, bindings assurance.ContentDestinationBindings, p identity.Principal, now time.Time) (assurance.ContentReconciliationResult, error) {
	if request.UncertaintyEpisodeID != intent.UncertaintyEpisodeID || request.UncertaintyEpisodeID == "" {
		return assurance.ContentReconciliationResult{}, &assurance.Error{Code: "content_reconciliation_conflict", Message: "uncertainty episode does not match the owned intent"}
	}
	if request.Classification == "confirmed_not_applied" || request.NoEffectProofID != "" {
		return assurance.ContentReconciliationResult{}, &assurance.Error{Code: "capability_unverified", Message: "provider no-effect proof is unavailable; repeat submission remains disabled"}
	}
	if request.Classification == "confirmed_applied" {
		return s.confirmApplied(ctx, auditID, request, requestHash, reservationRequest, intent, plan, profile, bindings, p, now)
	}
	retention := bindings.Retention.IdempotencyLifetimeSeconds
	if retention < 1 {
		return assurance.ContentReconciliationResult{}, &assurance.Error{Code: "content_policy_invalid", Message: "signed reconciliation retention is invalid"}
	}
	reservationRequest.RetainUntil = now.Add(time.Duration(retention) * time.Second)
	reservation, err := s.Store.Reserve(ctx, reservationRequest, now)
	if err != nil {
		return assurance.ContentReconciliationResult{}, err
	}
	if reservation.Disposition != assurance.ReservationOwned {
		return reconciliationReplay(reservation)
	}
	episode := request.UncertaintyEpisodeID
	result := reconciliationResult(intent, episode, nil)
	result.State = request.Classification
	result.ReconciliationRequired = true
	result.EvidenceBindingHash = reconciliationBindingHash(intent.PlacementIntentID, episode, intent.OperationReference, "", "", "")
	policy := reconciliationPolicySnapshot(bindings)
	return s.Store.FinalizeContentReconciliationClassification(ctx, reservation, requestHash, intent.RowVersion, intent.PlacementIntentID, episode, request.Classification, "", intent.TargetHash, hashBytes(plan.IntendedValue), "", result, reservationRequest.RetainUntil, policy, auditID, now)
}

func (s *ReconciliationService) confirmApplied(ctx context.Context, auditID string, request ReconcileRequest, requestHash assurance.Digest, reservationRequest assurance.ReservationRequest, intent assurance.ContentReconciliationIntent, plan PreviewPlan, profile ResolvedProfile, bindings assurance.ContentDestinationBindings, p identity.Principal, now time.Time) (assurance.ContentReconciliationResult, error) {
	if s.Fence == nil || !s.Fence.Durable() && !s.allowTestFence {
		return assurance.ContentReconciliationResult{}, &assurance.Error{Code: "capability_unverified", Message: "durable exact terminal fence is unavailable; outcome remains unknown"}
	}
	if intent.UncertaintyEpisodeID == "" || request.UncertaintyEpisodeID != intent.UncertaintyEpisodeID || intent.OperationReference == "" || intent.ClaimFenceDigest == "" {
		return assurance.ContentReconciliationResult{}, &assurance.Error{Code: "content_reconciliation_conflict", Message: "applied classification lacks an owned known-operation claim"}
	}
	retention := bindings.Retention.IdempotencyLifetimeSeconds
	recoverySeconds := bindings.Retention.RecoveryLifetimeSeconds
	if retention < 1 || recoverySeconds < 1 {
		return assurance.ContentReconciliationResult{}, &assurance.Error{Code: "content_policy_invalid", Message: "signed reconciliation retention is invalid"}
	}
	reservationRequest.RetainUntil = now.Add(time.Duration(retention) * time.Second)
	reservation, err := s.Store.Reserve(ctx, reservationRequest, now)
	if err != nil {
		return assurance.ContentReconciliationResult{}, err
	}
	if reservation.Disposition != assurance.ReservationOwned {
		return reconciliationReplay(reservation)
	}
	if err := s.Store.RenewReservation(ctx, reservation.RecordID, reservation.OwnerNonce, s.now()); err != nil {
		return s.failReadBack(ctx, reservation, auditID, "content_reconciliation_failed", "reconciliation lease could not be renewed", err)
	}
	// Reload both intent and latest evidence after obtaining the private
	// reservation so a concurrent newer readback cannot be classified stale.
	intent, err = s.Store.GetOwnedContentReconciliationIntent(ctx, intent.PlacementIntentID)
	if err != nil || intent.RowVersion < 1 || intent.UncertaintyEpisodeID != request.UncertaintyEpisodeID {
		return s.failReadBack(ctx, reservation, auditID, "content_evidence_inconsistent", "owned reconciliation intent changed", err)
	}
	evidence, err := s.Store.GetLatestOwnedContentReconciliationEvidenceAt(ctx, intent.PlacementIntentID, intent.UncertaintyEpisodeID, s.now())
	if err != nil || evidence.EvidenceID != request.LatestReadbackEvidenceID {
		return s.failReadBack(ctx, reservation, auditID, "content_evidence_inconsistent", "latest fresh readback evidence changed", err)
	}
	if err := validateAppliedEvidenceForFence(evidence, intent, plan, s.now()); err != nil {
		return s.failReadBack(ctx, reservation, auditID, "content_evidence_inconsistent", "latest evidence does not prove the exact accepted typed value", err)
	}
	claim, err := s.reconciliationClaim(ctx, intent, plan, profile, p)
	if err != nil {
		return s.failReadBack(ctx, reservation, auditID, "content_fence_invalid", "owned claim could not be reconstructed", err)
	}
	terminal, digest, err := s.Fence.ReadVerifiedTerminal(ctx, claim, "")
	createdTerminal := false
	if errors.Is(err, ErrContentFenceObjectNotFound) {
		createdTerminal = true
		terminal = ContentTerminal{SchemaVersion: 1, MutationKind: ContentMutationKind, EnvironmentDigest: claim.EnvironmentDigest, TenantDigest: claim.TenantDigest, PlacementIntentID: claim.PlacementIntentID, ClaimDigest: intent.ClaimFenceDigest, Outcome: ContentOutcomeAccepted, OperationReferenceHash: hashBytes([]byte(intent.OperationReference)), ReadbackHash: evidence.ReadbackHash, ReadbackCacheBypassed: true, TerminalAt: s.now(), TerminalRowVersion: intent.RowVersion + 1}
		terminalCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		digest, err = s.Fence.CreateTerminal(terminalCtx, claim, terminal)
		if err == nil {
			err = s.Fence.VerifyTerminal(terminalCtx, claim, terminal, digest)
		}
		cancel()
		if err != nil {
			return s.failReadBack(ctx, reservation, auditID, "content_fence_invalid", "accepted terminal could not be safely created", err)
		}
	} else if err != nil {
		return s.failReadBack(ctx, reservation, auditID, "content_fence_unavailable", "exact terminal inspection is ambiguous", err)
	}
	terminalBytes, err := CanonicalContentTerminal(claim, terminal)
	rowVersionMatches := terminal.TerminalRowVersion >= intent.ClaimRowVersion && terminal.TerminalRowVersion <= intent.RowVersion
	if createdTerminal {
		rowVersionMatches = terminal.TerminalRowVersion == intent.RowVersion+1
	}
	if err != nil || digest == "" || hashBytes(terminalBytes) != digest || terminal.ClaimDigest != intent.ClaimFenceDigest || terminal.Outcome != ContentOutcomeAccepted || terminal.OperationReferenceHash != hashBytes([]byte(intent.OperationReference)) || !sha256Pattern.MatchString(terminal.ReadbackHash) || !terminal.ReadbackCacheBypassed || !rowVersionMatches || terminal.TerminalAt.Before(intent.ClaimedAt) || terminal.TerminalAt.After(s.now()) {
		mismatch := fmt.Errorf("canonical=%v digest=%v claim=%v outcome=%v operation=%v readback-hash=%v cache=%v row-version=%v terminal-time-valid=%v", err, digest != "" && hashBytes(terminalBytes) == digest, terminal.ClaimDigest == intent.ClaimFenceDigest, terminal.Outcome, terminal.OperationReferenceHash == hashBytes([]byte(intent.OperationReference)), sha256Pattern.MatchString(terminal.ReadbackHash), terminal.ReadbackCacheBypassed, rowVersionMatches, !terminal.TerminalAt.Before(intent.ClaimedAt) && !terminal.TerminalAt.After(s.now()))
		return s.failReadBack(ctx, reservation, auditID, "content_fence_invalid", "accepted terminal does not match fresh readback and frozen claim", mismatch)
	}
	terminalDigest := digest
	result := reconciliationResult(intent, intent.UncertaintyEpisodeID, &evidence)
	result.State = "confirmed_applied"
	result.LatestReadbackID = evidence.EvidenceID
	result.ReconciliationRequired = true // assurance finalizer verifies this precondition
	result.EvidenceBindingHash = reconciliationBindingHash(intent.PlacementIntentID, intent.UncertaintyEpisodeID, intent.OperationReference, evidence.OperationStatus, evidence.TargetHash, evidence.ReadbackHash)
	policy := reconciliationPolicySnapshot(bindings)
	return s.Store.FinalizeContentReconciliationClassification(ctx, reservation, requestHash, intent.RowVersion, intent.PlacementIntentID, intent.UncertaintyEpisodeID, "confirmed_applied", evidence.EvidenceID, intent.TargetHash, hashBytes(plan.IntendedValue), terminalDigest, result, reservationRequest.RetainUntil, policy, auditID, s.now())
}

func validateAppliedEvidenceForFence(e assurance.ContentReconciliationEvidence, intent assurance.ContentReconciliationIntent, plan PreviewPlan, now time.Time) error {
	if e.Kind != "read_back" || e.EvidenceID == "" || e.ActorID != intent.ActorID || e.PlacementIntentID != intent.PlacementIntentID || e.UncertaintyEpisodeID != intent.UncertaintyEpisodeID || !e.CacheBypassed || !e.ExpiresAt.After(now) || !e.ProviderObservedAt.After(intent.ClaimedAt) || e.OperationStatus != "completed" || e.OperationReferenceHash != hashBytes([]byte(intent.OperationReference)) {
		return errors.New("content: reconciliation evidence identity, freshness or operation binding is invalid")
	}
	canonicalInspection, err := assurance.CanonicalJSONBytes(e.OperationInspectionJSON)
	var inspection struct {
		Reference string
		Status    string
	}
	if err != nil || !bytes.Equal(canonicalInspection, e.OperationInspectionJSON) || hashBytes(canonicalInspection) != e.OperationInspectionHash || json.Unmarshal(canonicalInspection, &inspection) != nil || inspection.Reference != intent.OperationReference || inspection.Status != "completed" {
		return errors.New("content: operation inspection does not prove the known completed operation")
	}
	canonicalReadback, err := assurance.CanonicalJSONBytes(e.ReadbackJSON)
	var metadata workivaprovider.ContentMetadata
	if err != nil || !bytes.Equal(canonicalReadback, e.ReadbackJSON) || hashBytes(canonicalReadback) != e.ReadbackHash || json.Unmarshal(canonicalReadback, &metadata) != nil {
		return errors.New("content: exact readback digest or encoding is invalid")
	}
	if err := workivaprovider.ValidateContentMetadata(metadata); err != nil {
		return err
	}
	if metadata.SpreadsheetID != plan.Destination.ResourceID || metadata.SheetID != plan.Destination.SheetID || metadata.Locator != plan.Destination.Cell || metadata.Provenance.Provider != "workiva_rest" || metadata.Provenance.APIVersion != workiva.DefaultAPIVersion || metadata.Provenance.Endpoint != "GET /spreadsheets/{spreadsheetId}/sheets/{sheetId}/sheetdata" || metadata.Provenance.QueryRange != plan.Destination.Cell || metadata.Provenance.Cache != "bypassed" || metadata.FormattingSHA256 != plan.ProviderMetadata.FormattingSHA256 || metadata.Protection != "unprotected" || metadata.Writable != "writable" {
		return errors.New("content: readback target, formats, protection, or writability differs from frozen preview")
	}
	actual, err := assurance.CanonicalJSONBytes(metadata.RawValue)
	want, wantErr := assurance.CanonicalJSONBytes(plan.IntendedValue)
	if err != nil || wantErr != nil || !bytes.Equal(actual, want) {
		return errors.New("content: readback literal differs from frozen intended value")
	}
	target, err := targetHashForPlan(plan)
	if err != nil || target != e.TargetHash || hashBytes(plan.IntendedValue) != e.IntendedHash {
		return errors.New("content: evidence target or intended hash differs from frozen preview")
	}
	return nil
}

func (s *ReconciliationService) reconciliationClaim(ctx context.Context, intent assurance.ContentReconciliationIntent, plan PreviewPlan, profile ResolvedProfile, p identity.Principal) (ContentClaim, error) {
	env, tenant, err := s.Fence.Identity(p.TenantID)
	if err != nil {
		return ContentClaim{}, err
	}
	profileHash, err := hashCanonical(profile.Profile)
	if err != nil {
		return ContentClaim{}, err
	}
	claim := ContentClaim{SchemaVersion: 1, MutationKind: ContentMutationKind, EnvironmentDigest: env, TenantDigest: tenant, PlacementIntentID: intent.PlacementIntentID, CandidateKind: plan.CandidateKind, ActorDigest: hashBytes([]byte(p.AuditActor())), IdempotencyDigest: intent.ConfirmIdempotencyDigest, RequestDigest: intent.ConfirmRequestDigest, PreviewHash: intent.PreviewSHA256, SourceArtifactHash: plan.SourceHash, CandidateHash: plan.CandidateHash, ProfileHash: profileHash, ResourcePolicyHash: profile.Profile.ResourcePolicy.ContentHash, ConversionPolicyHash: profile.Profile.ConversionPolicy.ContentHash, AccessPolicyHash: profile.Binding.AccessPolicyRef.ContentHash, ProviderPolicyHash: profile.Profile.ProviderContract.ContentHash, RetentionPolicyHash: profile.Profile.RetentionPolicy.ContentHash, ActiveBundleHash: profile.Binding.ActiveBundleHash, ActiveBundleVersion: profile.Binding.ActiveBundleVersion, TargetHash: intent.TargetHash, IntendedHash: hashBytes(plan.IntendedValue), ClaimedAt: intent.ClaimedAt.UTC(), ClaimRowVersion: intent.ClaimRowVersion}
	if plan.CandidateKind == "draft_artifact" {
		claim.DraftArtifactHash = plan.CandidateHash
	}
	if plan.CandidateLineageHash != "" {
		claim.CandidateLineageHash = plan.CandidateLineageHash
	}
	canonical, err := CanonicalContentClaim(claim)
	if err != nil || hashBytes(canonical) != intent.ClaimFenceDigest {
		return ContentClaim{}, errors.New("content: reconstructed claim digest differs from owned claim")
	}
	return claim, nil
}

func (s *ReconciliationService) readBack(ctx context.Context, auditID string, request ReconcileRequest, requestHash assurance.Digest, reservationRequest assurance.ReservationRequest, intent assurance.ContentReconciliationIntent, plan PreviewPlan, profile ResolvedProfile, bindings assurance.ContentDestinationBindings, p identity.Principal, now time.Time) (assurance.ContentReconciliationResult, error) {
	if intent.UncertaintyEpisodeID != "" && !validOpaqueID(intent.UncertaintyEpisodeID) {
		return assurance.ContentReconciliationResult{}, &assurance.Error{Code: "content_intent_integrity_failed", Message: "stored uncertainty episode is invalid"}
	}
	if intent.UncertaintyEpisodeID == "" {
		intent.UncertaintyEpisodeID = uuid.NewString()
	}
	retention := bindings.Retention.IdempotencyLifetimeSeconds
	recovery := bindings.Retention.RecoveryLifetimeSeconds
	if retention < 1 || recovery < 1 {
		return assurance.ContentReconciliationResult{}, &assurance.Error{Code: "content_policy_invalid", Message: "signed recovery retention is invalid"}
	}
	reservationRequest.RetainUntil = now.Add(time.Duration(retention) * time.Second)
	recoveryCutoff := intent.CreatedAt.Add(time.Duration(recovery) * time.Second)
	if !recoveryCutoff.After(now) {
		return assurance.ContentReconciliationResult{}, &assurance.Error{Code: "content_recovery_expired", Message: "signed reconciliation recovery horizon is expired"}
	}
	reservation, err := s.Store.Reserve(ctx, reservationRequest, now)
	if err != nil {
		return assurance.ContentReconciliationResult{}, err
	}
	if reservation.Disposition != assurance.ReservationOwned {
		return reconciliationReplay(reservation)
	}
	if err := s.Store.RenewReservation(ctx, reservation.RecordID, reservation.OwnerNonce, s.now()); err != nil {
		return s.failReadBack(ctx, reservation, auditID, "provider_readback_unavailable", "reconciliation read lease could not be renewed", err)
	}
	if s.Provider == nil {
		return s.failReadBack(ctx, reservation, auditID, "provider_readback_unavailable", "reconciliation provider is unavailable", assurance.ErrNotReady)
	}
	callCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	var inspection workiva.OperationInspection
	if intent.OperationReference != "" {
		inspection, err = s.Provider.InspectOperation(callCtx, intent.OperationReference)
		if err != nil {
			inspection = workiva.OperationInspection{Reference: intent.OperationReference, Status: "inspection_unavailable"}
		}
	} else {
		inspection = workiva.OperationInspection{Status: "operation_unknown"}
	}
	if intent.OperationReference != "" && inspection.Reference != intent.OperationReference {
		inspection = workiva.OperationInspection{Reference: intent.OperationReference, Status: "inspection_inconsistent"}
	}
	metadata, metadataErr := s.Provider.ReadContentMetadata(callCtx, profile.Profile.ResourceID, profile.Profile.SheetID, profile.Profile.Cell)
	if metadataErr != nil {
		return s.failReadBack(ctx, reservation, auditID, "provider_readback_unavailable", "uncached typed destination readback is unavailable", metadataErr)
	}
	if err := workivaprovider.ValidateContentMetadata(metadata); err != nil {
		return s.failReadBack(ctx, reservation, auditID, "provider_evidence_inconsistent", "provider returned invalid typed destination metadata", err)
	}
	if metadata.SpreadsheetID != profile.Profile.ResourceID || metadata.SheetID != profile.Profile.SheetID || metadata.Locator != profile.Profile.Cell || metadata.Provenance.Cache != "bypassed" {
		return s.failReadBack(ctx, reservation, auditID, "provider_evidence_inconsistent", "provider readback did not bind to the frozen target", errors.New("target or cache provenance mismatch"))
	}
	observedAt := s.now()
	if err := s.Store.RenewReservation(ctx, reservation.RecordID, reservation.OwnerNonce, observedAt); err != nil {
		return s.failReadBack(ctx, reservation, auditID, "provider_readback_unavailable", "reconciliation reservation expired during provider reads", err)
	}
	inspectionJSON, err := assurance.CanonicalJSON(struct {
		Reference string
		Status    string
	}{inspection.Reference, inspection.Status})
	if err != nil {
		return s.failReadBack(ctx, reservation, auditID, "provider_evidence_inconsistent", "provider operation inspection could not be canonicalized", err)
	}
	readbackJSON, err := assurance.CanonicalJSON(metadata)
	if err != nil {
		return s.failReadBack(ctx, reservation, auditID, "provider_evidence_inconsistent", "typed destination readback could not be canonicalized", err)
	}
	targetHash, err := targetHashForPlan(plan)
	if err != nil {
		return s.failReadBack(ctx, reservation, auditID, "content_preview_integrity_failed", "frozen target could not be verified", err)
	}
	intendedHash := hashBytes(plan.IntendedValue)
	episode := intent.UncertaintyEpisodeID
	evidence := assurance.ContentReconciliationEvidence{EvidenceID: uuid.NewString(), PlacementIntentID: intent.PlacementIntentID, ActorID: p.AuditActor(), UncertaintyEpisodeID: episode, Kind: "read_back", OperationReferenceHash: hashBytes([]byte(intent.OperationReference)), OperationStatus: inspection.Status, OperationInspectionJSON: inspectionJSON, OperationInspectionHash: hashBytes(inspectionJSON), TargetHash: targetHash, IntendedHash: intendedHash, ReadbackJSON: readbackJSON, ReadbackHash: hashBytes(readbackJSON), CacheBypassed: metadata.Provenance.Cache == "bypassed", ProviderObservedAt: observedAt, CreatedAt: observedAt}
	result := reconciliationResult(intent, episode, &evidence)
	policy := reconciliationPolicySnapshot(bindings)
	recoveryRemaining := recoveryCutoff.Sub(observedAt)
	if recoveryRemaining <= 0 {
		return s.failReadBack(ctx, reservation, auditID, "content_recovery_expired", "signed reconciliation recovery horizon expired during provider reads", errors.New("recovery horizon expired"))
	}
	if recoveryRemaining < time.Duration(recovery)*time.Second {
		recovery = int64(recoveryRemaining / time.Second)
	}
	if recovery < 1 {
		return s.failReadBack(ctx, reservation, auditID, "content_recovery_expired", "signed reconciliation recovery horizon expired during provider reads", errors.New("recovery horizon expired"))
	}
	result, err = s.Store.FinalizeContentReconciliationReadback(ctx, reservation, requestHash, intent.RowVersion, evidence, result, time.Duration(recovery)*time.Second, reservationRequest.RetainUntil, policy, auditID, observedAt)
	if err != nil {
		return s.failReadBack(ctx, reservation, auditID, "content_reconciliation_failed", "provider readback could not be durably recorded", err)
	}
	return result, nil
}

func (s *ReconciliationService) failReadBack(ctx context.Context, reservation assurance.ReservationResult, auditID, code, message string, cause error) (assurance.ContentReconciliationResult, error) {
	if cause == nil {
		cause = errors.New(message)
	}
	failure := assurance.StructuredError{Code: code, Message: message, Retryable: false, ReconciliationRequired: true, NLAuditID: auditID}
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.Store.FailReservation(detached, reservation.RecordID, reservation.OwnerNonce, failure, s.now()); err != nil {
		return assurance.ContentReconciliationResult{}, fmt.Errorf("content: read-only reconciliation failed (%v) and failure replay could not be sealed: %w", cause, err)
	}
	return assurance.ContentReconciliationResult{}, &assurance.Error{Code: code, Message: message, ReconciliationRequired: true, Cause: cause}
}

func (s *ReconciliationService) resolveProfile(ctx context.Context, plan PreviewPlan, now time.Time) (ResolvedProfile, assurance.ContentDestinationBindings, error) {
	p, b, err := s.Resolver.ResolveContentDestinationForCapability(ctx, plan.Destination.ID, plan.Destination.Revision, now, identity.PermissionContentReconcile)
	if err != nil {
		return ResolvedProfile{}, assurance.ContentDestinationBindings{}, err
	}
	resolved, err := projectResolvedProfile(p, b)
	if err != nil {
		return ResolvedProfile{}, assurance.ContentDestinationBindings{}, err
	}
	frozen := plan.PolicyBinding
	if resolved.Binding.ActiveBundleHash != frozen.ActiveBundleHash || resolved.Binding.ActiveBundleVersion != frozen.ActiveBundleVersion || resolved.Binding.AccessPolicyRef != frozen.AccessPolicyRef || !resolved.Binding.AccessExpiresAt.Equal(frozen.AccessExpiresAt) || resolved.Profile.ID != plan.Destination.ID || resolved.Profile.Revision != plan.Destination.Revision || resolved.Profile.ContentHash != plan.Destination.ContentHash || resolved.Profile.ResourceID != plan.Destination.ResourceID || resolved.Profile.SheetID != plan.Destination.SheetID || resolved.Profile.Cell != plan.Destination.Cell || resolved.Profile.ProviderContract != plan.Destination.ProviderContract || resolved.Profile.ProviderFamily != plan.Destination.ProviderFamily || resolved.Profile.ProviderAPIVersion != plan.Destination.ProviderAPIVersion {
		return ResolvedProfile{}, assurance.ContentDestinationBindings{}, &assurance.Error{Code: "content_policy_stale", Message: "current signed destination binding differs from the frozen placement"}
	}
	return resolved, b, nil
}

func validateReconcileRequest(r ReconcileRequest) error {
	if !isCanonicalUUID(r.PlacementIntentID) || !validStageKey(r.IdempotencyKey) || len([]byte(r.Note)) > 2048 || r.Action != "read_back" && r.Action != "classify" {
		return &assurance.Error{Code: "invalid_request", Message: "content reconciliation request is invalid"}
	}
	if r.Action == "read_back" {
		if r.Classification != "" || r.UncertaintyEpisodeID != "" || r.LatestReadbackEvidenceID != "" || r.NoEffectProofID != "" {
			return &assurance.Error{Code: "invalid_request", Message: "read_back cannot include classification evidence selectors"}
		}
		return nil
	}
	if r.Classification == "" || !validOpaqueID(r.UncertaintyEpisodeID) || r.LatestReadbackEvidenceID != "" && !validOpaqueID(r.LatestReadbackEvidenceID) || r.NoEffectProofID != "" && !validOpaqueID(r.NoEffectProofID) {
		return &assurance.Error{Code: "invalid_request", Message: "classify selectors are incomplete or invalid"}
	}
	switch r.Classification {
	case "confirmed_applied":
		if r.LatestReadbackEvidenceID == "" || r.NoEffectProofID != "" {
			return &assurance.Error{Code: "invalid_request", Message: "confirmed_applied requires only latest readback evidence"}
		}
	case "confirmed_not_applied":
		if r.NoEffectProofID == "" || r.LatestReadbackEvidenceID != "" {
			return &assurance.Error{Code: "invalid_request", Message: "confirmed_not_applied requires only no-effect proof"}
		}
	case "still_unknown", "provider_evidence_inconsistent":
		if r.LatestReadbackEvidenceID != "" || r.NoEffectProofID != "" {
			return &assurance.Error{Code: "invalid_request", Message: "unknown classification cannot include evidence selectors"}
		}
	default:
		return &assurance.Error{Code: "invalid_request", Message: "classification is unsupported"}
	}
	return nil
}

func decodeReconciliationPreview(intent assurance.ContentReconciliationIntent, p identity.Principal) (PreviewPlan, error) {
	canonical, err := assurance.CanonicalJSONBytes(intent.PreviewJSON)
	var plan PreviewPlan
	if err != nil || !bytes.Equal(canonical, intent.PreviewJSON) || hashBytes(intent.PreviewJSON) != intent.PreviewSHA256 || json.Unmarshal(canonical, &plan) != nil || plan.PlacementIntentID != intent.PlacementIntentID || plan.ActorBindingID != p.TenantID+"/"+p.ObjectID || plan.ExpiresAt.IsZero() || plan.Destination.ID == "" || plan.PolicyBinding.ActiveBundleHash == "" || plan.PolicyBinding.ActiveBundleVersion < 1 || !sha256Pattern.MatchString(plan.PolicyBinding.AccessPolicyRef.ContentHash) {
		return PreviewPlan{}, &assurance.Error{Code: "content_preview_integrity_failed", Message: "frozen reconciliation preview is invalid"}
	}
	return plan, nil
}

func reconciliationPolicySnapshot(b assurance.ContentDestinationBindings) assurance.ContentIntakePolicySnapshot {
	return assurance.ContentIntakePolicySnapshot{Access: b.AccessPolicy, Retention: b.Retention, ActiveBundleHash: b.ActiveBundleHash, ActiveBundleVersion: b.ActiveBundleVersion}
}

func reconciliationResult(intent assurance.ContentReconciliationIntent, episode string, evidence *assurance.ContentReconciliationEvidence) assurance.ContentReconciliationResult {
	result := assurance.ContentReconciliationResult{Kind: "reconcile", PlacementIntentID: intent.PlacementIntentID, State: "still_unknown", UncertaintyEpisodeID: episode, OperationKnown: intent.OperationReference != "", EvidenceIDs: []string{}, ReconciliationRequired: true, ResubmissionAllowed: false}
	if result.OperationKnown {
		op := intent.OperationReference
		result.ProviderOperationID = &op
	}
	if evidence == nil {
		result.EvidenceBindingHash = reconciliationBindingHash(intent.PlacementIntentID, episode, intent.OperationReference, "", "", "")
		return result
	}
	result.EvidenceIDs = []string{evidence.EvidenceID}
	result.EvidenceBindingHash = reconciliationBindingHash(intent.PlacementIntentID, episode, intent.OperationReference, evidence.OperationStatus, evidence.TargetHash, evidence.ReadbackHash)
	return result
}

func reconciliationBindingHash(intentID, episode, operation, status, target, readback string) string {
	body, _ := assurance.CanonicalJSON(struct {
		Intent        string `json:"intent_id"`
		Episode       string `json:"episode_id"`
		OperationHash string `json:"operation_reference_sha256"`
		Status        string `json:"operation_status"`
		TargetHash    string `json:"target_hash"`
		ReadbackHash  string `json:"readback_sha256"`
	}{intentID, episode, hashBytes([]byte(operation)), status, target, readback})
	return hashBytes(body)
}

func targetHashForPlan(plan PreviewPlan) (string, error) {
	return assuranceTargetHash(plan.Destination.ResourceID, plan.Destination.SheetID, plan.Destination.Cell)
}

func assuranceTargetHash(resource, sheet, cell string) (string, error) {
	body, err := assurance.CanonicalJSON(struct {
		ResourceID string `json:"resource_id"`
		SheetID    string `json:"sheet_id"`
		Cell       string `json:"cell"`
	}{resource, sheet, cell})
	if err != nil {
		return "", err
	}
	return hashBytes(body), nil
}

func (s *ReconciliationService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func reconciliationReplay(reservation assurance.ReservationResult) (assurance.ContentReconciliationResult, error) {
	switch reservation.Disposition {
	case assurance.ReservationConflict:
		return assurance.ContentReconciliationResult{}, &assurance.Error{Code: "idempotency_conflict", Message: "reconciliation idempotency key conflicts with another request"}
	case assurance.ReservationInProgress, assurance.ReservationOwned:
		return assurance.ContentReconciliationResult{}, &assurance.Error{Code: "idempotency_in_progress", Message: "reconciliation is already in progress", Retryable: true}
	case assurance.ReservationReplay:
		if reservation.State != assurance.ReservationStateSealed || len(reservation.Envelope) == 0 {
			if reservation.State == assurance.ReservationStateFailed {
				var failure assurance.StructuredError
				if err := json.Unmarshal(reservation.Envelope, &failure); err == nil && failure.Code != "" {
					return assurance.ContentReconciliationResult{}, &assurance.Error{Code: failure.Code, Message: failure.Message, Retryable: failure.Retryable, ReconciliationRequired: failure.ReconciliationRequired}
				}
			}
			return assurance.ContentReconciliationResult{}, &assurance.Error{Code: "idempotency_terminal", Message: "reconciliation has no sealed result"}
		}
		canonical, err := assurance.CanonicalJSONBytes(reservation.Envelope)
		var envelope assurance.ContentReconciliationEnvelope
		if err != nil || !bytes.Equal(canonical, reservation.Envelope) || json.Unmarshal(canonical, &envelope) != nil || envelope.Result.Kind != "reconcile" || envelope.Result.ResubmissionAllowed {
			return assurance.ContentReconciliationResult{}, &assurance.Error{Code: "idempotency_integrity_failed", Message: "sealed reconciliation envelope is invalid"}
		}
		envelope.Result.ResultSHA256 = hashBytes(reservation.Envelope)
		return envelope.Result, nil
	default:
		return assurance.ContentReconciliationResult{}, fmt.Errorf("content: unknown reconciliation reservation disposition %q", reservation.Disposition)
	}
}
