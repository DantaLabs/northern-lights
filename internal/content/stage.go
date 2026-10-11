package content

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
	"github.com/google/uuid"
)

const stageTool = "workiva_content_placement"

type StageRequest struct {
	CandidateKind              string `json:"candidate_kind"`
	CandidateID                string `json:"candidate_id"`
	SourceArtifactID           string `json:"source_artifact_id,omitempty"`
	DestinationProfileID       string `json:"destination_profile_id"`
	DestinationProfileRevision int    `json:"destination_profile_revision"`
	IdempotencyKey             string `json:"idempotency_key"`
}

type StageResponse struct {
	Kind                  string          `json:"kind"`
	Preview               json.RawMessage `json:"preview"`
	ConfirmationToken     string          `json:"confirmation_token,omitempty"`
	ReplayRequiresRestage bool            `json:"replay_requires_restage"`
}

// ResolvedProfile is returned only by a server-configured signed-policy
// resolver. It is deliberately a resolved projection, never request input.
type ResolvedProfile struct {
	Profile                     DestinationProfile
	Binding                     PolicyBinding
	IdempotencyRetentionSeconds int64
}

// AssuranceProfileResolver adapts the verified active assurance bundle to the
// engine projection. The resolver itself owns its trusted verification key.
type AssuranceProfileResolver struct {
	Resolver *assurance.ContentPolicyResolver
}

func (r AssuranceProfileResolver) ResolveDestination(ctx context.Context, id string, revision int, now time.Time) (ResolvedProfile, error) {
	if r.Resolver == nil {
		return ResolvedProfile{}, assurance.ErrNotReady
	}
	p, b, err := r.Resolver.ResolveContentDestination(ctx, id, revision, now)
	if err != nil {
		return ResolvedProfile{}, err
	}
	return projectResolvedProfile(p, b)
}

func candidateLineageHashes(record assurance.ContentCandidate, projectedLineage string) []string {
	return []string{record.SourceSHA256, projectedLineage}
}

type OwnedCandidateResolver interface {
	ResolveOwnedCandidate(context.Context, string, string, time.Time) (Candidate, error)
}

// AssuranceCandidateResolver binds StageService to the owned immutable V19
// candidate projection without exposing caller-provided text or metadata.
type AssuranceCandidateResolver struct{ Store *assurance.Store }

func (r AssuranceCandidateResolver) ResolveOwnedCandidate(ctx context.Context, kind, id string, now time.Time) (Candidate, error) {
	if r.Store == nil {
		return Candidate{}, assurance.ErrNotReady
	}
	var candidateKind assurance.ContentCandidateKind
	switch kind {
	case "extracted_item":
		candidateKind = assurance.ContentCandidateExtractedItem
	case "draft_artifact":
		candidateKind = assurance.ContentCandidateDraftArtifact
	default:
		return Candidate{}, errors.New("content: candidate kind is invalid")
	}
	record, err := r.Store.GetContentCandidateAt(ctx, candidateKind, id, now)
	if err != nil {
		return Candidate{}, err
	}
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok {
		return Candidate{}, errors.New("content: trusted principal is unavailable")
	}
	return ProjectExtractedTextCandidate(principal, record)
}

type TrustedProfileResolver interface {
	ResolveDestination(context.Context, string, int, time.Time) (ResolvedProfile, error)
}
type StageClock interface{ Now() time.Time }
type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

type StageService struct {
	Store      *assurance.Store
	Candidates OwnedCandidateResolver
	Profiles   TrustedProfileResolver
	Provider   workivaprovider.ContentMetadataReader
	Clock      StageClock
}

// ProjectExtractedTextCandidate is the conservative v1 projection from the
// owned V19 getter. It only accepts an immutable text-kind hint and derives
// the JSON string literal directly from OriginalText; numeric facts are never
// inferred from presentation text.
func ProjectExtractedTextCandidate(principal identity.Principal, record assurance.ContentCandidate) (Candidate, error) {
	if (record.Kind != assurance.ContentCandidateExtractedItem && record.Kind != assurance.ContentCandidateDraftArtifact) || record.ID == "" || principal.TokenType != identity.TokenTypeDelegated || principal.TenantID == "" || principal.ObjectID == "" {
		return Candidate{}, errors.New("content: extracted text candidate is unavailable")
	}
	var hints map[string]json.RawMessage
	if err := json.Unmarshal(record.MetadataBLOB, &hints); err != nil || hints == nil {
		return Candidate{}, errors.New("content: immutable candidate interpretation metadata is invalid")
	}
	var kind string
	originKind := ""
	if record.Kind == assurance.ContentCandidateDraftArtifact {
		var origin struct {
			Kind string `json:"kind"`
		}
		if len(hints["origin"]) == 0 || json.Unmarshal(hints["origin"], &origin) != nil {
			return Candidate{}, errors.New("content: immutable draft origin is unavailable")
		}
		switch origin.Kind {
		case "analyst", "agent_flow", "copilot", "other":
			originKind = origin.Kind
		default:
			return Candidate{}, errors.New("content: immutable draft origin kind is invalid")
		}
		// Draft artifacts are text by contract. A contradictory private hint
		// cannot change their type or authorize a conversion.
		if rawHint, present := hints["kind_hint"]; present {
			if json.Unmarshal(rawHint, &kind) != nil || kind != "text" {
				return Candidate{}, errors.New("content: immutable draft literal kind is invalid")
			}
		}
		kind = "text"
	} else if json.Unmarshal(hints["kind_hint"], &kind) != nil || kind == "" {
		return Candidate{}, errors.New("content: immutable candidate does not establish a literal kind")
	}
	if kind != "text" && kind != "number" && kind != "date" {
		return Candidate{}, errors.New("content: immutable candidate literal kind is unsupported")
	}
	if !sourceIntakePolicyBound(record) {
		return Candidate{}, errors.New("content: candidate source is missing trusted intake-policy lifetime evidence")
	}
	value, interpretation, err := projectImmutableLiteral(record.OriginalText, kind, hints)
	if err != nil {
		return Candidate{}, err
	}
	metadata := append(json.RawMessage(nil), record.MetadataBLOB...)
	sourceMetadata := append(json.RawMessage(nil), record.SourceMetadataBLOB...)
	sourceMetadataDigest := assurance.HashBytes(sourceMetadata)
	sourceMetadataHash := hex.EncodeToString(sourceMetadataDigest[:])
	lineageHash := record.LineageSHA256
	if lineageHash == "" {
		binding, err := assurance.CanonicalJSON(struct {
			SourceID           string `json:"source_artifact_id"`
			SourceHash         string `json:"source_sha256"`
			SourceMetadataHash string `json:"source_metadata_sha256"`
			CandidateHash      string `json:"candidate_sha256"`
			MetadataHash       string `json:"metadata_sha256"`
		}{record.SourceArtifactID, record.SourceSHA256, sourceMetadataHash, record.CandidateSHA256, record.MetadataSHA256})
		if err != nil {
			return Candidate{}, err
		}
		digest := assurance.HashBytes(binding)
		lineageHash = hex.EncodeToString(digest[:])
	}
	return Candidate{
		Kind: string(record.Kind), CandidateOriginKind: originKind, ID: record.ID, Revision: 1, TenantID: principal.TenantID, ActorID: principal.AuditActor(),
		Text: record.OriginalText, TextSHA256: record.CandidateSHA256, MetadataSHA256: record.MetadataSHA256,
		LineageSHA256: lineageHash, SourceID: record.SourceArtifactID, SourceSHA256: record.SourceSHA256, SourceMetadataSHA256: sourceMetadataHash,
		LineageHashes: candidateLineageHashes(record, lineageHash), Value: value, Interpretation: interpretation,
		Metadata: metadata, Provenance: sourceMetadata, CreatedAt: record.CreatedAt, ExpiresAt: record.ExpiresAt,
	}, nil
}

func projectImmutableLiteral(text, kind string, fields map[string]json.RawMessage) (json.RawMessage, Interpretation, error) {
	if kind == "number" {
		// Public intake has only one categorical source-scale hint. It does not
		// establish separate stored/display scales or a private percent enum.
		// Keep that candidate immutable, but require clarification before staging.
		return nil, Interpretation{}, &assurance.Error{Code: "content_candidate_interpretation_incomplete", Message: "numeric source hints do not establish exact stored/display scale and percentage semantics"}
	}
	if kind != "text" && kind != "date" {
		return nil, Interpretation{}, errors.New("content: candidate type is not a supported literal scalar")
	}
	// Text and ISO date literals carry no numeric scale or percentage meaning.
	// These explicit non-applicable values are derived from the closed literal
	// kind, not inferred from caller text. Public semantic hints that fit the
	// preview contract are preserved; scale/percent conversions remain blocked.
	interpretation := Interpretation{ValueKind: kind, StoredScale: json.Number("1"), DisplayScale: json.Number("1"), PercentBasis: "not_percentage", Precision: json.RawMessage("null")}
	if raw, ok := fields["interpretation"]; ok {
		var public map[string]json.RawMessage
		if json.Unmarshal(raw, &public) != nil || public == nil {
			return nil, Interpretation{}, errors.New("content: public candidate interpretation is invalid")
		}
		for _, name := range []string{"scale", "percent_basis"} {
			if value, present := public[name]; present && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil, Interpretation{}, &assurance.Error{Code: "content_candidate_interpretation_incomplete", Message: "text/date candidate carries a numeric scale or percentage hint that the placement contract cannot represent exactly"}
			}
		}
		for name, dst := range map[string]**string{"period": &interpretation.Period, "currency": &interpretation.Currency, "unit": &interpretation.Unit} {
			value, present := public[name]
			if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				continue
			}
			var parsed string
			if json.Unmarshal(value, &parsed) != nil || parsed == "" {
				return nil, Interpretation{}, errors.New("content: public candidate semantic hint is invalid")
			}
			copy := parsed
			*dst = &copy
		}
		if value, present := public["precision"]; present {
			canonical, err := assurance.CanonicalJSONBytes(value)
			if err != nil {
				return nil, Interpretation{}, errors.New("content: public candidate precision hint is invalid")
			}
			interpretation.Precision = canonical
		}
	}
	if err := validateInterpretation(interpretation); err != nil {
		return nil, Interpretation{}, fmt.Errorf("content: candidate semantic hints are unsupported: %w", err)
	}
	var value json.RawMessage
	var err error
	switch kind {
	case "text":
		value, err = json.Marshal(text)
	case "date":
		if _, parseErr := time.Parse("2006-01-02", text); parseErr != nil {
			return nil, Interpretation{}, errors.New("content: immutable date text is not an exact ISO calendar date")
		}
		value, err = json.Marshal(text)
	}
	if err != nil {
		return nil, Interpretation{}, err
	}
	return append(json.RawMessage(nil), value...), interpretation, nil
}

func sourceIntakePolicyBound(record assurance.ContentCandidate) bool {
	var metadata struct {
		IntakePolicy struct {
			AccessPolicy struct {
				PolicyID    string `json:"policy_id"`
				Revision    int    `json:"revision"`
				ContentHash string `json:"content_hash"`
				ValidUntil  string `json:"valid_until"`
			} `json:"access_policy"`
			RetentionPolicy struct {
				PolicyID    string `json:"policy_id"`
				Revision    int    `json:"revision"`
				ContentHash string `json:"content_hash"`
			} `json:"retention_policy"`
			SourceExpiresAt string `json:"source_expires_at"`
			DraftExpiresAt  string `json:"draft_expires_at"`
		} `json:"intake_policy"`
	}
	if json.Unmarshal(record.SourceMetadataBLOB, &metadata) != nil {
		return false
	}
	p := metadata.IntakePolicy
	accessExpiry, e1 := time.Parse(time.RFC3339Nano, p.AccessPolicy.ValidUntil)
	sourceExpiry, e2 := time.Parse(time.RFC3339Nano, p.SourceExpiresAt)
	draftExpiry, e3 := time.Parse(time.RFC3339Nano, p.DraftExpiresAt)
	return p.AccessPolicy.PolicyID != "" && p.AccessPolicy.Revision > 0 && sha256Pattern.MatchString(p.AccessPolicy.ContentHash) && e1 == nil && record.CreatedAt.Before(accessExpiry) && p.RetentionPolicy.PolicyID != "" && p.RetentionPolicy.Revision > 0 && sha256Pattern.MatchString(p.RetentionPolicy.ContentHash) && e2 == nil && e3 == nil && !draftExpiry.After(sourceExpiry) && draftExpiry.Equal(record.ExpiresAt) && record.CreatedAt.Before(record.ExpiresAt)
}

// Stage authorizes before examining private references. Replays are served
// from the sealed token-free envelope before policy, candidate, or provider
// resolution; they explicitly require a fresh stage to obtain a new token.
func (s StageService) Stage(ctx context.Context, auditID string, request StageRequest) (StageResponse, error) {
	p, ok := identity.PrincipalFromContext(ctx)
	if !ok || p.TokenType != identity.TokenTypeDelegated || p.UsedSubjectFallback || !p.HasPermission(identity.PermissionContentStage) || !isCanonicalUUID(p.TenantID) || !isCanonicalUUID(p.ObjectID) {
		return StageResponse{}, errors.New("content: trusted delegated content.stage principal required")
	}
	if s.Store == nil {
		return StageResponse{}, assurance.ErrNotReady
	}
	if s.Clock == nil {
		s.Clock = wallClock{}
	}
	if (request.CandidateKind != "extracted_item" && request.CandidateKind != "draft_artifact") || !validOpaqueID(request.CandidateID) || !validOpaqueID(request.DestinationProfileID) || request.DestinationProfileRevision < 1 || request.DestinationProfileRevision > 1_000_000 || !validStageKey(request.IdempotencyKey) || request.CandidateKind == "extracted_item" && !validOpaqueID(request.SourceArtifactID) || request.CandidateKind == "draft_artifact" && request.SourceArtifactID != "" && !validOpaqueID(request.SourceArtifactID) {
		return StageResponse{}, errors.New("content: invalid stage selectors")
	}
	// This is the private normalized stage request digest. A future ingress
	// adapter must validate the published schema and its mapping explicitly;
	// this encoding does not claim byte-shape equivalence with that schema.
	canonicalRequest, err := assurance.CanonicalJSON(struct {
		Phase    string `json:"phase"`
		Kind     string `json:"candidate_kind"`
		ID       string `json:"candidate_id"`
		Source   string `json:"source_artifact_id,omitempty"`
		Profile  string `json:"destination_profile_id"`
		Revision int    `json:"destination_profile_revision"`
	}{"stage", request.CandidateKind, request.CandidateID, request.SourceArtifactID, request.DestinationProfileID, request.DestinationProfileRevision})
	if err != nil {
		return StageResponse{}, err
	}
	reservationRequest := assurance.ReservationRequest{ActorID: p.AuditActor(), Tool: stageTool, Action: "stage", IdempotencyDigest: assurance.DigestIdempotencyKey(request.IdempotencyKey), RequestDigest: assurance.HashBytes(canonicalRequest), RetentionClass: "workflow"}
	reservation, found, err := s.Store.LookupReservation(ctx, reservationRequest)
	if err != nil {
		return StageResponse{}, err
	}
	if found {
		return stageReplay(reservation)
	}
	if s.Candidates == nil || s.Profiles == nil || s.Provider == nil || auditID == "" {
		return StageResponse{}, assurance.ErrNotReady
	}
	now := s.Clock.Now().UTC()
	profile, err := s.Profiles.ResolveDestination(ctx, request.DestinationProfileID, request.DestinationProfileRevision, now)
	if err != nil {
		return StageResponse{}, err
	}
	if profile.IdempotencyRetentionSeconds < profile.Profile.IntentLifetimeSeconds || profile.Profile.IdempotencyLifetimeSeconds != profile.IdempotencyRetentionSeconds || profile.IdempotencyRetentionSeconds < 1 {
		return StageResponse{}, errors.New("content: signed intent and idempotency retention are invalid")
	}
	reserveNow := s.Clock.Now().UTC()
	reservationRequest.RetainUntil = reserveNow.Add(time.Duration(profile.IdempotencyRetentionSeconds) * time.Second)
	if err := s.Store.RequireRichAudit(ctx); err != nil {
		return StageResponse{}, err
	}
	reservation, err = s.Store.Reserve(ctx, reservationRequest, reserveNow)
	if err != nil {
		return StageResponse{}, err
	}
	if reservation.Disposition != assurance.ReservationOwned {
		return stageReplay(reservation)
	}
	fail := func(cause error) (StageResponse, error) {
		at := s.Clock.Now().UTC()
		structured := assurance.StructuredError{Code: "content_stage_failed", Message: "content stage could not be completed"}
		var domain *assurance.Error
		if errors.As(cause, &domain) {
			structured.Code = domain.Code
			structured.Message = domain.Message
		}
		_ = s.Store.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, structured, at)
		return StageResponse{}, cause
	}
	if err := s.Store.RenewReservation(ctx, reservation.RecordID, reservation.OwnerNonce, s.Clock.Now().UTC()); err != nil {
		return fail(err)
	}
	created := s.Clock.Now().UTC()
	profile, err = s.Profiles.ResolveDestination(ctx, request.DestinationProfileID, request.DestinationProfileRevision, created)
	if err != nil {
		return fail(err)
	}
	candidate, err := s.Candidates.ResolveOwnedCandidate(ctx, request.CandidateKind, request.CandidateID, created)
	if err != nil {
		return fail(err)
	}
	if candidate.SourceID != request.SourceArtifactID && request.SourceArtifactID != "" {
		return fail(errors.New("content: source selector does not match owned candidate"))
	}
	if candidate.Kind != request.CandidateKind || candidate.TenantID != p.TenantID || candidate.ActorID != p.AuditActor() {
		return fail(errors.New("content: owned candidate binding is invalid"))
	}
	if err := s.Store.RenewReservation(ctx, reservation.RecordID, reservation.OwnerNonce, s.Clock.Now().UTC()); err != nil {
		return fail(err)
	}
	readCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	metadata, err := s.Provider.ReadContentMetadata(readCtx, profile.Profile.ResourceID, profile.Profile.SheetID, profile.Profile.Cell)
	cancel()
	if err != nil {
		return fail(fmt.Errorf("content: provider observation failed: %w", err))
	}
	observed := s.Clock.Now().UTC()
	// Refresh mutable trusted facts after the provider call; immutable hashes
	// must still match the candidate that the observation was requested for.
	if err := s.Store.RenewReservation(ctx, reservation.RecordID, reservation.OwnerNonce, observed); err != nil {
		return fail(err)
	}
	refreshedProfile, err := s.Profiles.ResolveDestination(ctx, request.DestinationProfileID, request.DestinationProfileRevision, observed)
	if err != nil {
		return fail(err)
	}
	refreshedCandidate, err := s.Candidates.ResolveOwnedCandidate(ctx, request.CandidateKind, request.CandidateID, observed)
	if err != nil {
		return fail(err)
	}
	oldCandidateBinding, oldCandidateErr := assurance.CanonicalJSON(candidate)
	newCandidateBinding, newCandidateErr := assurance.CanonicalJSON(refreshedCandidate)
	if oldCandidateErr != nil || newCandidateErr != nil || string(oldCandidateBinding) != string(newCandidateBinding) || refreshedProfile.Binding != profile.Binding || refreshedProfile.Profile.ContentHash != profile.Profile.ContentHash || refreshedProfile.IdempotencyRetentionSeconds != profile.IdempotencyRetentionSeconds || refreshedProfile.Profile.IntentLifetimeSeconds != profile.Profile.IntentLifetimeSeconds || refreshedProfile.Profile.IdempotencyLifetimeSeconds != profile.Profile.IdempotencyLifetimeSeconds {
		return fail(errors.New("content: source or active policy changed during provider observation"))
	}
	if err := s.Store.RenewReservation(ctx, reservation.RecordID, reservation.OwnerNonce, s.Clock.Now().UTC()); err != nil {
		return fail(err)
	}
	intentID := uuid.NewString()
	observationID, err := randomOpaque(24)
	if err != nil {
		return fail(err)
	}
	input := BuildInput{Candidate: refreshedCandidate, Profile: refreshedProfile.Profile, RequestedProfileID: request.DestinationProfileID, RequestedProfileRev: request.DestinationProfileRevision, ObservationID: observationID, ObservedAt: observed, Metadata: metadata, PlacementIntentID: intentID, CreatedAt: observed, PolicyBinding: refreshedProfile.Binding}
	plan, err := BuildPreviewPlan(ctx, input)
	if err != nil {
		return fail(err)
	}
	if len(plan.Blockers) != 0 {
		return fail(errors.New("content: provider literal-format preservation is unverified"))
	}
	// Enforce the supported first slice: only exact text literals derived from
	// the immutable candidate text. Numeric interpretation remains clarification.
	if candidate.Interpretation.ValueKind != "text" {
		return fail(errors.New("content: candidate value requires additional reviewed interpretation facts"))
	}
	preview, err := assurance.CanonicalJSON(plan)
	if err != nil {
		return fail(err)
	}
	token, err := randomOpaque(32)
	if err != nil {
		return fail(err)
	}
	digestInput := append(append([]byte(token), 0), preview...)
	tokenDigest := sha256.Sum256(digestInput)
	finalNow := s.Clock.Now().UTC()
	// Rebase preview timestamps on finalization time, then recompute its content hash.
	plan.CreatedAt = finalNow
	plan.ExpiresAt = finalNow.Add(time.Duration(refreshedProfile.Profile.PreviewTTLSeconds) * time.Second)
	if !plan.ExpiresAt.Before(refreshedCandidate.ExpiresAt) || !plan.ExpiresAt.Before(refreshedProfile.Binding.AccessExpiresAt) {
		return fail(errors.New("content: preview lifetime exceeds current source or access-policy expiry"))
	}
	plan.ContentHash, err = hashPlan(plan)
	if err != nil {
		return fail(err)
	}
	preview, err = assurance.CanonicalJSON(plan)
	if err != nil {
		return fail(err)
	}
	digestInput = append(append([]byte(token), 0), preview...)
	tokenDigest = sha256.Sum256(digestInput)
	result, err := s.Store.FinalizeContentStage(ctx, reservation, reservationRequest.RequestDigest, assurance.ContentStageIntent{PlacementIntentID: intentID, PreviewJSON: preview, PreviewSHA256: hex.EncodeToString(hashSum(preview)), TokenDigest: hex.EncodeToString(tokenDigest[:]), CreatedAt: finalNow, ExpiresAt: plan.ExpiresAt}, auditID, finalNow)
	if err != nil {
		return StageResponse{}, err
	}
	return StageResponse{Kind: result.Kind, Preview: result.Preview, ConfirmationToken: token, ReplayRequiresRestage: false}, nil
}

func stageReplay(reservation assurance.ReservationResult) (StageResponse, error) {
	switch reservation.Disposition {
	case assurance.ReservationConflict:
		return StageResponse{}, errors.New("content: idempotency key conflicts with a different request")
	case assurance.ReservationInProgress:
		return StageResponse{}, errors.New("content: stage request is already in progress")
	case assurance.ReservationReplay:
	default:
		return StageResponse{}, errors.New("content: reservation disposition is invalid")
	}
	if reservation.State == assurance.ReservationStateFailed {
		var failure assurance.StructuredError
		if err := json.Unmarshal(reservation.Envelope, &failure); err != nil || failure.Code == "" {
			return StageResponse{}, errors.New("content: failed stage result is unavailable")
		}
		return StageResponse{}, &assurance.Error{Code: failure.Code, Message: failure.Message, Retryable: failure.Retryable, ReconciliationRequired: failure.ReconciliationRequired}
	}
	if reservation.State != assurance.ReservationStateSealed {
		return StageResponse{}, errors.New("content: reservation is not a sealed stage result")
	}
	var stored assurance.ContentStageResult
	if err := json.Unmarshal(reservation.Envelope, &stored); err != nil || stored.Kind != "stage" || len(stored.Preview) == 0 {
		return StageResponse{}, errors.New("content: sealed stage response is unavailable")
	}
	return StageResponse{Kind: stored.Kind, Preview: append(json.RawMessage(nil), stored.Preview...), ReplayRequiresRestage: true}, nil
}

func randomOpaque(bytesNeeded int) (string, error) {
	b := make([]byte, bytesNeeded)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func hashSum(b []byte) []byte { sum := sha256.Sum256(b); return sum[:] }
func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func validOpaqueID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for i, r := range value {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !alnum && (i == 0 || !strings.ContainsRune("._:-", r)) {
			return false
		}
	}
	return true
}

func validStageKey(value string) bool {
	if len(value) < 16 || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if r < '!' || r > '~' {
			return false
		}
	}
	return true
}
