package content

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/mutation"
	"github.com/dantalabs/northern-lights/internal/workiva"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
	"github.com/google/uuid"
)

const confirmTool = "workiva_content_placement"

type ConfirmRequest struct {
	PlacementIntentID string `json:"placement_intent_id"`
	ConfirmationToken string `json:"confirmation_token"`
	IdempotencyKey    string `json:"idempotency_key"`
}

type ConfirmResponse = assurance.ContentPlacementConfirmationResult

type UnknownConfirmationError struct {
	IntentID               string
	Reason                 string
	OperationReference     string
	ReconciliationRequired bool
	ResubmissionAllowed    bool
	Cause                  error
}

func (e *UnknownConfirmationError) Error() string {
	return "content: confirmation outcome is unknown and requires reconciliation"
}
func (e *UnknownConfirmationError) Unwrap() error { return e.Cause }

type ConfirmationProvider interface {
	workivaprovider.ContentMetadataReader
	UpdateSheetWithRetryAfter(context.Context, string, string, workiva.SheetUpdate) (string, time.Duration, error)
	WaitOperationWithInitialRetryAfter(context.Context, string, time.Duration) (string, error)
}

// AssuranceConfirmationProfileResolver resolves the signed active destination
// specifically for content.confirm. Stage capability alone cannot authorize it.
type AssuranceConfirmationProfileResolver struct {
	Resolver *assurance.ContentPolicyResolver
}

func (r AssuranceConfirmationProfileResolver) ResolveDestination(ctx context.Context, id string, revision int, now time.Time) (ResolvedProfile, error) {
	if r.Resolver == nil {
		return ResolvedProfile{}, assurance.ErrNotReady
	}
	p, b, err := r.Resolver.ResolveContentDestinationForCapability(ctx, id, revision, now, identity.PermissionContentConfirm)
	if err != nil {
		return ResolvedProfile{}, err
	}
	return projectResolvedProfile(p, b)
}

type ConfirmationClock interface{ Now() time.Time }

type ConfirmationService struct {
	Store          *assurance.Store
	Candidates     OwnedCandidateResolver
	PolicyResolver *assurance.ContentPolicyResolver
	Provider       ConfirmationProvider
	Fence          ContentFence
	Clock          ConfirmationClock
	allowTestFence bool
}

func NewConfirmationService(store *assurance.Store, candidates OwnedCandidateResolver, resolver *assurance.ContentPolicyResolver, provider ConfirmationProvider, fence ContentFence, clock ConfirmationClock) (*ConfirmationService, error) {
	if store == nil || candidates == nil || resolver == nil || provider == nil || fence == nil || !fence.Durable() {
		return nil, errors.New("content: durable confirmation dependencies are unavailable")
	}
	return &ConfirmationService{Store: store, Candidates: candidates, PolicyResolver: resolver, Provider: provider, Fence: fence, Clock: clock}, nil
}

// NewLocalTestConfirmationService is the only constructor that accepts the
// process-local fence. It must never be used for production confirmation.
func NewLocalTestConfirmationService(store *assurance.Store, candidates OwnedCandidateResolver, resolver *assurance.ContentPolicyResolver, provider ConfirmationProvider, fence ContentFence, clock ConfirmationClock) (*ConfirmationService, error) {
	if store == nil || candidates == nil || resolver == nil || provider == nil || fence == nil || fence.Durable() {
		return nil, errors.New("content: explicit local test fence and confirmation dependencies are required")
	}
	return &ConfirmationService{Store: store, Candidates: candidates, PolicyResolver: resolver, Provider: provider, Fence: fence, Clock: clock, allowTestFence: true}, nil
}

// Confirm performs read-only preflight before atomically claiming the target,
// then one shared mutation sequence. It never automatically retries a claim or
// provider submission after an uncertain outcome.
func (s *ConfirmationService) Confirm(ctx context.Context, auditID string, request ConfirmRequest) (ConfirmResponse, error) {
	p, ok := identity.PrincipalFromContext(ctx)
	if !ok || p.TokenType != identity.TokenTypeDelegated || p.UsedSubjectFallback || !p.HasPermission(identity.PermissionContentConfirm) || !isCanonicalUUID(p.TenantID) || !isCanonicalUUID(p.ObjectID) {
		return ConfirmResponse{}, errors.New("content: trusted delegated content.confirm principal required")
	}
	if s == nil || s.Store == nil {
		return ConfirmResponse{}, assurance.ErrNotReady
	}
	clock := s.Clock
	if clock == nil {
		clock = wallClock{}
	}
	if !validOpaqueID(request.PlacementIntentID) || !validStageKey(request.IdempotencyKey) {
		return ConfirmResponse{}, errors.New("content: invalid confirmation selectors")
	}
	requestBytes, err := assurance.CanonicalJSON(struct {
		Phase string `json:"phase"`
		ID    string `json:"placement_intent_id"`
	}{"confirm", request.PlacementIntentID})
	if err != nil {
		return ConfirmResponse{}, err
	}
	reservationRequest := assurance.ReservationRequest{ActorID: p.AuditActor(), Tool: confirmTool, Action: "confirm", IdempotencyDigest: assurance.DigestIdempotencyKey(request.IdempotencyKey), RequestDigest: assurance.HashBytes(requestBytes), RetentionClass: "workflow"}
	prior, found, err := s.Store.LookupReservation(ctx, reservationRequest)
	if err != nil {
		return ConfirmResponse{}, err
	}
	if found {
		return confirmationReplay(prior)
	}
	if len(request.ConfirmationToken) < 32 || len(request.ConfirmationToken) > 512 {
		return ConfirmResponse{}, errors.New("content: confirmation is unavailable")
	}
	if s.Candidates == nil || s.PolicyResolver == nil || s.Provider == nil || s.Fence == nil || auditID == "" || !s.Fence.Durable() && !s.allowTestFence {
		return ConfirmResponse{}, assurance.ErrNotReady
	}
	now := clock.Now().UTC()
	record, err := s.Store.GetOwnedContentPlacementIntent(ctx, request.PlacementIntentID)
	if err != nil {
		return ConfirmResponse{}, err
	}
	plan, err := decodeConfirmationPlan(record, p, now)
	if err != nil {
		return ConfirmResponse{}, err
	}
	profile, err := (AssuranceConfirmationProfileResolver{Resolver: s.PolicyResolver}).ResolveDestination(ctx, plan.Destination.ID, plan.Destination.Revision, now)
	if err != nil {
		return ConfirmResponse{}, err
	}
	candidate, err := s.Candidates.ResolveOwnedCandidate(ctx, plan.CandidateKind, plan.CandidateID, now)
	if err != nil {
		return ConfirmResponse{}, err
	}
	if err := matchConfirmationBindings(plan, candidate, profile, p, now); err != nil {
		return ConfirmResponse{}, err
	}
	metadata, err := s.readMetadata(ctx, profile.Profile)
	if err != nil {
		return ConfirmResponse{}, err
	}
	if err := validateConfirmationTarget(plan, profile.Profile, metadata); err != nil {
		return ConfirmResponse{}, err
	}
	writerContract, err := resolveLiteralWriteContract(ctx, s.Provider, profile.Profile, metadata)
	if err != nil {
		return ConfirmResponse{}, err
	}
	update, err := contentLiteralUpdate(plan)
	if err != nil {
		return ConfirmResponse{}, err
	}
	if profile.IdempotencyRetentionSeconds < profile.Profile.IntentLifetimeSeconds || profile.IdempotencyRetentionSeconds < 1 || profile.IdempotencyRetentionSeconds != profile.Profile.IdempotencyLifetimeSeconds {
		return ConfirmResponse{}, errors.New("content: signed confirmation retention is invalid")
	}
	claimNow := clock.Now().UTC()
	reservationRequest.RetainUntil = claimNow.Add(time.Duration(profile.IdempotencyRetentionSeconds) * time.Second)
	leaseID := uuid.NewString()
	claim, err := s.Store.ClaimContentPlacementConfirmation(ctx, s.PolicyResolver, reservationRequest, request.PlacementIntentID, request.ConfirmationToken, leaseID, auditID, claimNow)
	if err != nil {
		return ConfirmResponse{}, err
	}
	if claim.Replay || claim.Reservation.Disposition != assurance.ReservationOwned {
		return confirmationReplayOrDisposition(claim.Reservation)
	}
	if !bytes.Equal(claim.Record.PreviewJSON, record.PreviewJSON) || claim.Record.PreviewSHA256 != record.PreviewSHA256 || claim.Record.RowVersion < 1 || claim.Record.LeaseID != leaseID || claim.Record.OwnerNonce == "" {
		return ConfirmResponse{}, s.unknownAfterClaim(ctx, request.PlacementIntentID, claim.Reservation, claim.Record.RowVersion, "provider_outcome_unknown", "", errors.New("content: claimed preview changed during confirmation"), clock)
	}
	reservation := claim.Reservation
	version := claim.Record.RowVersion
	operation := ""
	var fenceClaim ContentClaim
	var fenceClaimDigest string
	// Claim creation and target refresh are both pre-submission. Once the
	// immutable claim fence exists, any failure retains this target lock.
	environmentDigest, tenantDigest, err := s.Fence.Identity(p.TenantID)
	if err != nil {
		return ConfirmResponse{}, s.unknownAfterClaim(ctx, request.PlacementIntentID, reservation, version, "provider_outcome_unknown", operation, err, clock)
	}
	fenceClaim, err = buildContentClaim(plan, candidate, profile, p, reservationRequest, record, claim.Record, environmentDigest, tenantDigest, clock.Now().UTC())
	if err != nil {
		return ConfirmResponse{}, s.unknownAfterClaim(ctx, request.PlacementIntentID, reservation, version, "provider_outcome_unknown", operation, err, clock)
	}
	result, execErr := mutation.Execute(ctx, mutation.Hooks{
		RenewLease: func(callCtx context.Context) error {
			next, renewErr := s.Store.RenewContentPlacementConfirmation(callCtx, request.PlacementIntentID, reservation.RecordID, reservation.OwnerNonce, leaseID, newContentAuditID(), version, clock.Now().UTC())
			if renewErr == nil {
				version = next
			}
			return renewErr
		},
		MarkSubmitting: func(callCtx context.Context) error {
			refresh, readErr := s.readMetadata(callCtx, profile.Profile)
			if readErr != nil {
				return readErr
			}
			if validateErr := validateConfirmationTarget(plan, profile.Profile, refresh); validateErr != nil || !sameConfirmationObservation(metadata, refresh) {
				return errors.New("content: destination changed after atomic target claim")
			}
			freshNow := clock.Now().UTC()
			if !plan.ExpiresAt.After(freshNow) {
				return errors.New("content: frozen preview expired before submission")
			}
			freshProfile, resolveErr := (AssuranceConfirmationProfileResolver{Resolver: s.PolicyResolver}).ResolveDestination(callCtx, plan.Destination.ID, plan.Destination.Revision, freshNow)
			if resolveErr != nil {
				return resolveErr
			}
			freshCandidate, candidateErr := s.Candidates.ResolveOwnedCandidate(callCtx, plan.CandidateKind, plan.CandidateID, freshNow)
			if candidateErr != nil {
				return candidateErr
			}
			if err := matchConfirmationBindings(plan, freshCandidate, freshProfile, p, freshNow); err != nil || !sameResolvedProfile(profile, freshProfile) || !sameCandidateProjection(candidate, freshCandidate) {
				return errors.New("content: source or signed policy changed before submission")
			}
			freshWriterContract, contractErr := resolveLiteralWriteContract(callCtx, s.Provider, freshProfile.Profile, refresh)
			if contractErr != nil || freshWriterContract != writerContract {
				return errors.New("content: configured literal writer changed or its write contract is unavailable")
			}
			digest, createErr := s.Fence.CreateClaim(callCtx, fenceClaim)
			if createErr != nil {
				return createErr
			}
			if verifyErr := s.Fence.VerifyClaim(callCtx, fenceClaim, digest); verifyErr != nil {
				return verifyErr
			}
			fenceClaimDigest = digest
			markNow := clock.Now().UTC()
			if !plan.ExpiresAt.After(markNow) {
				return errors.New("content: frozen preview expired before submission")
			}
			next, markErr := s.Store.MarkContentPlacementSubmitting(callCtx, request.PlacementIntentID, reservation.RecordID, reservation.OwnerNonce, leaseID, digest, newContentAuditID(), version, markNow)
			if markErr == nil {
				version = next
			}
			return markErr
		},
		Submit: func(callCtx context.Context) (string, time.Duration, error) {
			return s.Provider.UpdateSheetWithRetryAfter(callCtx, profile.Profile.ResourceID, profile.Profile.SheetID, update)
		},
		PersistOperation: func(callCtx context.Context, operationReference string) error {
			if operationReference == "" {
				return errors.New("content: provider operation reference is unavailable")
			}
			persistCtx, cancel := boundedDetached(callCtx)
			defer cancel()
			next, persistErr := s.Store.PersistContentPlacementOperation(persistCtx, request.PlacementIntentID, reservation.RecordID, reservation.OwnerNonce, operationReference, newContentAuditID(), version, clock.Now().UTC())
			if persistErr == nil {
				version = next
				operation = operationReference
			}
			return persistErr
		},
		Poll: func(callCtx context.Context, operationReference string, delay time.Duration) error {
			_, pollErr := s.Provider.WaitOperationWithInitialRetryAfter(callCtx, operationReference, delay)
			return pollErr
		},
		Readback: func(callCtx context.Context) (string, error) {
			observed, readErr := s.readMetadata(callCtx, profile.Profile)
			if readErr != nil {
				return "", readErr
			}
			encoded, encodeErr := assurance.CanonicalJSON(observed)
			return string(encoded), encodeErr
		},
		ValidateReadback: func(encoded string) error {
			var observed workivaprovider.ContentMetadata
			if err := json.Unmarshal([]byte(encoded), &observed); err != nil {
				return &mutation.InvalidReadback{Cause: err}
			}
			if err := validateAcceptedReadback(plan, profile.Profile, observed); err != nil {
				return &mutation.InvalidReadback{Cause: err}
			}
			return nil
		},
	})
	if execErr != nil {
		var failure *mutation.Failure
		if errors.As(execErr, &failure) {
			if failure.OperationReference != "" {
				operation = failure.OperationReference
			}
			reason := "provider_outcome_unknown"
			switch failure.Reason {
			case "readback_mismatch":
				reason = "readback_mismatch"
			case "lease_renewal_failed":
				reason = "lease_lost_after_submission"
			}
			if failure.OperationReference != "" && operation != "" {
				persistKnownOperationDetached(ctx, s.Store, request.PlacementIntentID, reservation, failure.OperationReference, &version, clock.Now())
			}
			return ConfirmResponse{}, s.unknownAfterClaim(ctx, request.PlacementIntentID, reservation, version, reason, operation, execErr, clock)
		}
		return ConfirmResponse{}, s.unknownAfterClaim(ctx, request.PlacementIntentID, reservation, version, "provider_outcome_unknown", operation, execErr, clock)
	}
	if operation == "" || fenceClaimDigest == "" {
		return ConfirmResponse{}, s.unknownAfterClaim(ctx, request.PlacementIntentID, reservation, version, "provider_outcome_unknown", operation, nil, clock)
	}
	readbackBytes := []byte(result.Readback)
	readbackHash := hashBytes(readbackBytes)
	terminal := ContentTerminal{SchemaVersion: 1, MutationKind: ContentMutationKind, EnvironmentDigest: environmentDigest, TenantDigest: tenantDigest, PlacementIntentID: request.PlacementIntentID, ClaimDigest: fenceClaimDigest, Outcome: ContentOutcomeAccepted, OperationReferenceHash: hashBytes([]byte(operation)), ReadbackHash: readbackHash, ReadbackCacheBypassed: true, TerminalAt: clock.Now().UTC(), TerminalRowVersion: version + 1}
	terminalCtx, terminalCancel := boundedDetached(ctx)
	terminalDigest, err := s.Fence.CreateTerminal(terminalCtx, fenceClaim, terminal)
	if err == nil {
		err = s.Fence.VerifyTerminal(terminalCtx, fenceClaim, terminal, terminalDigest)
	}
	terminalCancel()
	if err != nil {
		return ConfirmResponse{}, s.unknownAfterClaim(ctx, request.PlacementIntentID, reservation, version, "terminal_fence_unavailable", operation, err, clock)
	}
	persistCtx, cancel := boundedDetached(ctx)
	response, finalizeErr := s.Store.FinalizeContentPlacementReadback(persistCtx, assurance.ContentPlacementReadback{PlacementIntentID: request.PlacementIntentID, RecordID: reservation.RecordID, OwnerNonce: reservation.OwnerNonce, ExpectedVersion: version, Outcome: "accepted", OperationReference: operation, ReadbackJSON: readbackBytes, ReadbackSHA256: readbackHash, TerminalFenceDigest: terminalDigest}, newContentAuditID(), clock.Now().UTC())
	cancel()
	if finalizeErr != nil {
		return ConfirmResponse{}, s.unknownAfterClaim(ctx, request.PlacementIntentID, reservation, version, "terminal_audit_failed", operation, finalizeErr, clock)
	}
	return response, nil
}

func confirmationReplay(reservation assurance.ReservationResult) (ConfirmResponse, error) {
	return confirmationReplayOrDisposition(reservation)
}

func confirmationReplayOrDisposition(reservation assurance.ReservationResult) (ConfirmResponse, error) {
	switch reservation.Disposition {
	case assurance.ReservationConflict:
		return ConfirmResponse{}, errors.New("content: idempotency key conflicts with a different request")
	case assurance.ReservationInProgress:
		return ConfirmResponse{}, errors.New("content: confirmation is already in progress")
	case assurance.ReservationReplay:
		if reservation.State != assurance.ReservationStateSealed {
			return ConfirmResponse{}, errors.New("content: confirmation replay is unavailable")
		}
		var result ConfirmResponse
		if err := json.Unmarshal(reservation.Envelope, &result); err != nil || result.Status != "machine_verified_visual_ack_pending" || result.VisualState != "pending" || result.TerminalFenceDigest == "" {
			return ConfirmResponse{}, errors.New("content: sealed confirmation response is invalid")
		}
		result.ResultSHA256 = hashBytes(reservation.Envelope)
		return result, nil
	default:
		return ConfirmResponse{}, errors.New("content: confirmation reservation disposition is invalid")
	}
}

func (s *ConfirmationService) readMetadata(ctx context.Context, profile DestinationProfile) (workivaprovider.ContentMetadata, error) {
	readCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	return s.Provider.ReadContentMetadata(readCtx, profile.ResourceID, profile.SheetID, profile.Cell)
}

func (s *ConfirmationService) unknownAfterClaim(ctx context.Context, intentID string, reservation assurance.ReservationResult, version int64, reason, operation string, cause error, clock ConfirmationClock) error {
	freezeCtx, cancel := boundedDetached(ctx)
	freezeErr := s.Store.FreezeContentPlacementUnknown(freezeCtx, intentID, reservation.RecordID, reservation.OwnerNonce, reason, newContentAuditID(), version, clock.Now().UTC())
	cancel()
	if freezeErr != nil {
		s.Store.BlockReadiness(errors.Join(errors.New("content: claimed confirmation could not be durably frozen"), freezeErr))
	}
	return &UnknownConfirmationError{IntentID: intentID, Reason: reason, OperationReference: operation, ReconciliationRequired: true, ResubmissionAllowed: false, Cause: errors.Join(cause, freezeErr)}
}

func decodeConfirmationPlan(record assurance.ContentPlacementConfirmationRecord, principal identity.Principal, now time.Time) (PreviewPlan, error) {
	canonical, err := assurance.CanonicalJSONBytes(record.PreviewJSON)
	if err != nil || !bytes.Equal(canonical, record.PreviewJSON) || hashBytes(canonical) != record.PreviewSHA256 {
		return PreviewPlan{}, errors.New("content: frozen preview integrity check failed")
	}
	var plan PreviewPlan
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	if err := decoder.Decode(&plan); err != nil {
		return PreviewPlan{}, errors.New("content: frozen preview is invalid")
	}
	contentHash, err := hashPlan(plan)
	if err != nil || contentHash != plan.ContentHash || plan.PlacementIntentID != record.PlacementIntentID || plan.ActorBindingID != principal.AuditActor() || !plan.ExpiresAt.After(now) || plan.CandidateKind == "" || plan.CandidateID == "" || plan.Destination.ID == "" {
		return PreviewPlan{}, errors.New("content: frozen preview binding or expiry is invalid")
	}
	return plan, nil
}

func matchConfirmationBindings(plan PreviewPlan, candidate Candidate, profile ResolvedProfile, principal identity.Principal, now time.Time) error {
	if candidate.Kind != plan.CandidateKind || candidate.ID != plan.CandidateID || candidate.Revision != plan.CandidateRevision || candidate.TenantID != principal.TenantID || candidate.ActorID != principal.AuditActor() || candidate.TextSHA256 != plan.CandidateHash || candidate.Text != plan.OriginalText || candidate.MetadataSHA256 != plan.CandidateMetadataHash || candidate.SourceID != plan.SourceID || candidate.SourceSHA256 != plan.SourceHash || candidate.SourceMetadataSHA256 != plan.SourceMetadataHash || candidate.LineageSHA256 != plan.CandidateLineageHash || !candidate.ExpiresAt.After(now) {
		return errors.New("content: owned candidate changed since preview")
	}
	if !sameRawJSON(candidate.Value, plan.OriginalValue) || !sameRawJSON(candidate.Value, plan.IntendedValue) || !sameRawJSON(candidate.Provenance, plan.CandidateProvenance) || hashBytes(candidate.Provenance) != plan.SourceMetadataHash || !sameCanonical(candidate.Interpretation, plan.OriginalInterpretation) || !sameCanonical(candidate.LineageHashes, plan.LineageHashes) {
		return errors.New("content: candidate value, metadata provenance, or lineage changed since preview")
	}
	if plan.CandidateKind == "draft_artifact" && candidate.TextSHA256 != plan.CandidateHash || plan.CandidateKind != "draft_artifact" && plan.CandidateKind != "extracted_item" {
		return errors.New("content: candidate lineage kind is invalid")
	}
	previewProfile, _ := assurance.CanonicalJSON(plan.Destination)
	currentProfile, _ := assurance.CanonicalJSON(profile.Profile)
	previewBinding, _ := assurance.CanonicalJSON(plan.PolicyBinding)
	currentBinding, _ := assurance.CanonicalJSON(profile.Binding)
	if !bytes.Equal(previewProfile, currentProfile) || !bytes.Equal(previewBinding, currentBinding) || profile.IdempotencyRetentionSeconds != profile.Profile.IdempotencyLifetimeSeconds {
		return errors.New("content: signed destination policy changed since preview")
	}
	return nil
}

func validateConfirmationTarget(plan PreviewPlan, profile DestinationProfile, observed workivaprovider.ContentMetadata) error {
	if err := workivaprovider.ValidateContentMetadata(observed); err != nil {
		return err
	}
	if observed.SpreadsheetID != profile.ResourceID || observed.SheetID != profile.SheetID || observed.Locator != profile.Cell || observed.Provenance.QueryRange != profile.Cell || observed.Provenance.Cache != "bypassed" || observed.Provenance.Provider != plan.ProviderMetadata.Provenance.Provider || observed.Provenance.APIVersion != profile.ProviderAPIVersion || !observed.ValuePresent || !observed.FormatsPresent || !observed.EffectiveFormatsPresent || observed.Protection != "unprotected" || observed.Writable != "writable" || observed.LiteralWriteFormatPreservation != literalWritePreserves || observed.LiteralWriteAPIVersion != profile.ProviderAPIVersion || observed.LiteralWriteEndpoint == "" {
		return errors.New("content: current provider target is not an approved uncached writable literal cell")
	}
	current, err := assurance.CanonicalJSONBytes(observed.RawValue)
	previewCurrent, previewErr := assurance.CanonicalJSONBytes(plan.CurrentValue)
	currentObservationHash, observationErr := hashCurrentObservation(observed)
	if err != nil || previewErr != nil || observationErr != nil || !bytes.Equal(current, previewCurrent) || currentObservationHash != plan.CurrentValueHash || observed.FormattingSHA256 != plan.ProviderMetadata.FormattingSHA256 || formulaLike(observed.RawValue) {
		return errors.New("content: provider target changed since preview")
	}
	return nil
}

func sameConfirmationObservation(a, b workivaprovider.ContentMetadata) bool {
	return a.SpreadsheetID == b.SpreadsheetID && a.SheetID == b.SheetID && a.Locator == b.Locator && a.Provenance.Provider == b.Provenance.Provider && a.Provenance.APIVersion == b.Provenance.APIVersion && a.Provenance.Cache == b.Provenance.Cache && a.FormattingSHA256 == b.FormattingSHA256 && a.RawValue != nil && b.RawValue != nil && bytes.Equal(canonicalOrNil(a.RawValue), canonicalOrNil(b.RawValue)) && a.Protection == b.Protection && a.Writable == b.Writable
}

func sameResolvedProfile(a, b ResolvedProfile) bool {
	return a.IdempotencyRetentionSeconds == b.IdempotencyRetentionSeconds && sameCanonical(a.Profile, b.Profile) && sameCanonical(a.Binding, b.Binding)
}

func resolveLiteralWriteContract(ctx context.Context, provider any, profile DestinationProfile, metadata workivaprovider.ContentMetadata) (workivaprovider.ContentLiteralWriteContract, error) {
	contractProvider, ok := provider.(workivaprovider.ContentLiteralWriteContractProvider)
	if !ok {
		return workivaprovider.ContentLiteralWriteContract{}, errors.New("content: configured writer has no explicit literal-write contract")
	}
	contract, err := contractProvider.ContentLiteralWriteContract(ctx)
	if err != nil {
		return workivaprovider.ContentLiteralWriteContract{}, err
	}
	if contract.ProviderFamily != profile.ProviderFamily || contract.APIVersion != profile.ProviderAPIVersion || contract.Endpoint == "" || contract.Endpoint != metadata.LiteralWriteEndpoint || contract.FormatPreservation != literalWritePreserves || contract.FormatPreservation != metadata.LiteralWriteFormatPreservation || contract.Authority == "" || metadata.LiteralWriteAPIVersion != contract.APIVersion {
		return workivaprovider.ContentLiteralWriteContract{}, errors.New("content: configured writer contract does not match signed provider and observed format facts")
	}
	return contract, nil
}

func sameCandidateProjection(a, b Candidate) bool { return sameCanonical(a, b) }

func sameCanonical(a, b any) bool {
	left, leftErr := assurance.CanonicalJSON(a)
	right, rightErr := assurance.CanonicalJSON(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

func sameRawJSON(a, b json.RawMessage) bool {
	left, leftErr := assurance.CanonicalJSONBytes(a)
	right, rightErr := assurance.CanonicalJSONBytes(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

func canonicalOrNil(raw []byte) []byte { b, _ := assurance.CanonicalJSONBytes(raw); return b }

func validateAcceptedReadback(plan PreviewPlan, profile DestinationProfile, observed workivaprovider.ContentMetadata) error {
	if err := validateConfirmationTargetAfterWrite(plan, profile, observed); err != nil {
		return err
	}
	actual, err := assurance.CanonicalJSONBytes(observed.RawValue)
	intended, intendedErr := assurance.CanonicalJSONBytes(plan.IntendedValue)
	if err != nil || intendedErr != nil || !bytes.Equal(actual, intended) {
		return errors.New("typed uncached read-back does not equal the intended literal")
	}
	return nil
}

func validateConfirmationTargetAfterWrite(plan PreviewPlan, profile DestinationProfile, observed workivaprovider.ContentMetadata) error {
	if err := workivaprovider.ValidateContentMetadata(observed); err != nil {
		return err
	}
	if observed.SpreadsheetID != profile.ResourceID || observed.SheetID != profile.SheetID || observed.Locator != profile.Cell || observed.Provenance.QueryRange != profile.Cell || observed.Provenance.Cache != "bypassed" || observed.Provenance.Provider != plan.ProviderMetadata.Provenance.Provider || observed.Provenance.APIVersion != profile.ProviderAPIVersion || !observed.ValuePresent || !observed.FormatsPresent || !observed.EffectiveFormatsPresent || observed.Protection != "unprotected" || observed.Writable != "writable" || observed.LiteralWriteFormatPreservation != literalWritePreserves || observed.LiteralWriteAPIVersion != profile.ProviderAPIVersion || observed.LiteralWriteEndpoint == "" || observed.FormattingSHA256 != plan.ProviderMetadata.FormattingSHA256 {
		return errors.New("typed uncached read-back provenance or coordinates are invalid")
	}
	return nil
}

func contentLiteralUpdate(plan PreviewPlan) (workiva.SheetUpdate, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(plan.IntendedValue))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return workiva.SheetUpdate{}, err
	}
	switch value.(type) {
	case string, json.Number, bool:
	default:
		return workiva.SheetUpdate{}, errors.New("content: intended value is not one typed scalar")
	}
	rangeValue, err := workiva.A1ToRange(plan.Destination.Cell)
	if err != nil || rangeValue.StartCol < 0 || rangeValue.StartRow < 0 || rangeValue.StartCol != rangeValue.StopCol || rangeValue.StartRow != rangeValue.StopRow {
		return workiva.SheetUpdate{}, errors.New("content: destination is not one bounded cell")
	}
	return workiva.NewEditCellsUpdate([]workiva.CellEdit{{Column: rangeValue.StartCol, Row: rangeValue.StartRow, Value: value}}), nil
}

func buildContentClaim(plan PreviewPlan, candidate Candidate, profile ResolvedProfile, principal identity.Principal, request assurance.ReservationRequest, staged, claimed assurance.ContentPlacementConfirmationRecord, environmentDigest, tenantDigest string, now time.Time) (ContentClaim, error) {
	if tenantDigest == "" {
		return ContentClaim{}, errors.New("content: tenant fence digest is missing")
	}
	profileHash, err := hashCanonical(profile.Profile)
	if err != nil {
		return ContentClaim{}, err
	}
	claim := ContentClaim{SchemaVersion: 1, MutationKind: ContentMutationKind, EnvironmentDigest: environmentDigest, TenantDigest: tenantDigest, PlacementIntentID: plan.PlacementIntentID, CandidateKind: plan.CandidateKind, ActorDigest: hashBytes([]byte(principal.AuditActor())), IdempotencyDigest: hex.EncodeToString(request.IdempotencyDigest[:]), RequestDigest: hex.EncodeToString(request.RequestDigest[:]), PreviewHash: staged.PreviewSHA256, SourceArtifactHash: candidate.SourceSHA256, CandidateHash: candidate.TextSHA256, ProfileHash: profileHash, ResourcePolicyHash: profile.Profile.ResourcePolicy.ContentHash, ConversionPolicyHash: profile.Profile.ConversionPolicy.ContentHash, AccessPolicyHash: profile.Binding.AccessPolicyRef.ContentHash, ProviderPolicyHash: profile.Profile.ProviderContract.ContentHash, RetentionPolicyHash: profile.Profile.RetentionPolicy.ContentHash, ActiveBundleHash: profile.Binding.ActiveBundleHash, ActiveBundleVersion: profile.Binding.ActiveBundleVersion, TargetHash: claimed.TargetHash, IntendedHash: hashBytes(plan.IntendedValue), ClaimedAt: claimed.ClaimedAt.UTC(), ClaimRowVersion: claimed.ClaimRowVersion}
	if candidate.Kind == "draft_artifact" {
		claim.DraftArtifactHash = candidate.TextSHA256
		claim.CandidateLineageHash = candidate.LineageSHA256
	} else if candidate.LineageSHA256 != "" {
		claim.CandidateLineageHash = candidate.LineageSHA256
	}
	if _, err := CanonicalContentClaim(claim); err != nil {
		return ContentClaim{}, err
	}
	return claim, nil
}

func hashCanonical(value any) (string, error) {
	raw, err := assurance.CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	return hashBytes(raw), nil
}
func boundedDetached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

func newContentAuditID() string { return uuid.NewString() }

func persistKnownOperationDetached(ctx context.Context, store *assurance.Store, intentID string, reservation assurance.ReservationResult, operation string, version *int64, now time.Time) {
	persistCtx, cancel := boundedDetached(ctx)
	defer cancel()
	record, err := store.GetOwnedContentPlacementIntent(persistCtx, intentID)
	if err == nil && record.ConfirmationRecordID == reservation.RecordID && record.OperationReference == operation {
		*version = record.RowVersion
		return
	}
	if err == nil && record.ConfirmationRecordID == reservation.RecordID && record.OperationReference == "" && record.State == "submitting" {
		next, persistErr := store.PersistContentPlacementOperation(persistCtx, intentID, reservation.RecordID, reservation.OwnerNonce, operation, newContentAuditID(), record.RowVersion, now.UTC())
		if persistErr == nil {
			*version = next
		}
	}
}

func formulaLike(raw []byte) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && strings.HasPrefix(value, "=")
}
