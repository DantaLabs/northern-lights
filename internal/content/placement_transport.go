package content

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/dantalabs/northern-lights/internal/assurance"
)

// PlacementTransportRequest is a strict projection of the published v1
// placement input. It is a DTO only; callers still invoke the phase service,
// which authorizes before private lookups and handles idempotent replay.
type PlacementTransportRequest struct {
	Phase          string
	IdempotencyKey string
	Ingest         *ContentIngestProjection
	Stage          *StageRequest
	Confirm        *ConfirmRequest
	Acknowledge    *PlacementAcknowledgeRequest
	Reconcile      *ReconcileRequest
}

// PlacementAcknowledgeRequest includes the public idempotency key alongside
// the service request so the transport adapter can derive the private digest.
type PlacementAcknowledgeRequest struct {
	Request        AcknowledgeRequest
	IdempotencyKey string
}

// PlacementInvocation is supplied by the transport boundary from the actual
// reservation disposition and audit record used for this invocation.
type PlacementInvocation struct {
	AuditID           string
	ReplayDisposition string
}

type PublicPlacementError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type PublicPlacementOutput struct {
	Phase                  string                 `json:"phase"`
	Status                 string                 `json:"status"`
	NLAuditID              string                 `json:"nl_audit_id"`
	Replayed               bool                   `json:"replayed"`
	NoMutationSubmitted    bool                   `json:"no_mutation_submitted"`
	ReconciliationRequired bool                   `json:"reconciliation_required"`
	Result                 any                    `json:"result"`
	Errors                 []PublicPlacementError `json:"errors"`
}

func (i PlacementInvocation) validate() error {
	if !validOpaqueID(i.AuditID) || (i.ReplayDisposition != "fresh" && i.ReplayDisposition != "sealed_replay") {
		return errors.New("content transport: actual audit ID and replay disposition are required")
	}
	return nil
}

// DecodePlacementTransportRequest validates the closed public request shape
// and maps only fields represented by the corresponding private service.
func DecodePlacementTransportRequest(raw []byte) (PlacementTransportRequest, error) {
	if len(raw) == 0 || len(raw) > contentIngressMaxBytes || !utf8.Valid(raw) || !json.Valid(raw) {
		return PlacementTransportRequest{}, errors.New("content transport: request is empty, oversized, or invalid JSON")
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return PlacementTransportRequest{}, err
	}
	var envelope struct {
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Phase == "" {
		return PlacementTransportRequest{}, errors.New("content transport: phase is required")
	}
	result := PlacementTransportRequest{Phase: envelope.Phase}
	switch envelope.Phase {
	case "ingest":
		projection, err := DecodeContentIngest(raw)
		if err != nil {
			return PlacementTransportRequest{}, err
		}
		var key struct {
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.Unmarshal(raw, &key); err != nil {
			return PlacementTransportRequest{}, err
		}
		result.IdempotencyKey = key.IdempotencyKey
		result.Ingest = &projection
	case "stage":
		var d struct {
			Phase     string          `json:"phase"`
			Key       string          `json:"idempotency_key"`
			SourceID  string          `json:"source_artifact_id"`
			ItemID    string          `json:"extracted_item_id"`
			DraftID   string          `json:"draft_artifact_id"`
			ProfileID string          `json:"destination_profile_id"`
			Revision  int             `json:"destination_profile_revision"`
			Workflow  json.RawMessage `json:"workflow_context"`
		}
		if err := decodePlacementObject(raw, &d); err != nil {
			return PlacementTransportRequest{}, err
		}
		if d.Phase != "stage" || !validStageKey(d.Key) || !validOpaqueID(d.ProfileID) || d.Revision < 1 || d.Revision > 1_000_000 || (d.ItemID == "") == (d.DraftID == "") || d.ItemID != "" && (!validOpaqueID(d.ItemID) || !validOpaqueID(d.SourceID)) || d.DraftID != "" && !validOpaqueID(d.DraftID) {
			return PlacementTransportRequest{}, errors.New("content transport: stage selectors are invalid")
		}
		if len(d.Workflow) != 0 {
			return PlacementTransportRequest{}, &assurance.Error{Code: "capability_unverified", Message: "stage workflow context is not represented by the private stage service"}
		}
		kind, id := "draft_artifact", d.DraftID
		if d.ItemID != "" {
			kind, id = "extracted_item", d.ItemID
		}
		result.IdempotencyKey = d.Key
		result.Stage = &StageRequest{CandidateKind: kind, CandidateID: id, SourceArtifactID: d.SourceID, DestinationProfileID: d.ProfileID, DestinationProfileRevision: d.Revision, IdempotencyKey: d.Key}
	case "confirm":
		var d struct {
			Phase string  `json:"phase"`
			Key   string  `json:"idempotency_key"`
			ID    string  `json:"placement_intent_id"`
			Token *string `json:"confirmation_token"`
		}
		if err := decodePlacementObject(raw, &d); err != nil {
			return PlacementTransportRequest{}, err
		}
		if d.Phase != "confirm" || !validStageKey(d.Key) || !validOpaqueID(d.ID) || d.Token != nil && (len(*d.Token) < 32 || len(*d.Token) > 512) {
			return PlacementTransportRequest{}, errors.New("content transport: confirm selectors are invalid")
		}
		token := ""
		if d.Token != nil {
			token = *d.Token
		}
		result.IdempotencyKey = d.Key
		result.Confirm = &ConfirmRequest{PlacementIntentID: d.ID, ConfirmationToken: token, IdempotencyKey: d.Key}
	case "acknowledge":
		var d struct {
			Phase       string `json:"phase"`
			Key         string `json:"idempotency_key"`
			ID          string `json:"placement_intent_id"`
			Observation string `json:"observation"`
			Hash        string `json:"result_sha256"`
			Note        string `json:"note"`
		}
		if err := decodePlacementObject(raw, &d); err != nil {
			return PlacementTransportRequest{}, err
		}
		if d.Phase != "acknowledge" || !validStageKey(d.Key) || !validOpaqueID(d.ID) || !validIngressSHA256(d.Hash) || len([]byte(d.Note)) > 2048 || !oneOf(d.Observation, "visually_confirmed", "visual_conflict", "not_reviewed") {
			return PlacementTransportRequest{}, errors.New("content transport: acknowledgement selectors are invalid")
		}
		result.IdempotencyKey = d.Key
		result.Acknowledge = &PlacementAcknowledgeRequest{Request: AcknowledgeRequest{PlacementIntentID: d.ID, Observation: d.Observation, ResultSHA256: d.Hash, VisualNotes: d.Note}, IdempotencyKey: d.Key}
	case "reconcile":
		var d struct {
			Phase          string `json:"phase"`
			Key            string `json:"idempotency_key"`
			ID             string `json:"placement_intent_id"`
			Action         string `json:"action"`
			Classification string `json:"classification"`
			Episode        string `json:"uncertainty_episode_id"`
			Readback       string `json:"latest_readback_evidence_id"`
			NoEffect       string `json:"no_effect_proof_id"`
			Note           string `json:"note"`
		}
		if err := decodePlacementObject(raw, &d); err != nil {
			return PlacementTransportRequest{}, err
		}
		request := ReconcileRequest{PlacementIntentID: d.ID, Action: d.Action, Classification: d.Classification, UncertaintyEpisodeID: d.Episode, LatestReadbackEvidenceID: d.Readback, NoEffectProofID: d.NoEffect, Note: d.Note, IdempotencyKey: d.Key}
		if d.Phase != "reconcile" || validateReconcileRequest(request) != nil {
			return PlacementTransportRequest{}, errors.New("content transport: reconciliation selectors are invalid")
		}
		result.IdempotencyKey = d.Key
		result.Reconcile = &request
	default:
		return PlacementTransportRequest{}, fmt.Errorf("content transport: unsupported phase %q", envelope.Phase)
	}
	return result, nil
}

func decodePlacementObject(raw []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return fmt.Errorf("content transport: invalid closed request: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("content transport: request must contain one JSON value")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return errors.New("content transport: request must be an object")
	}
	for name, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("content transport: %s cannot be null", name)
		}
	}
	return nil
}

// PublicConfirmResult maps a completed, terminally verified machine result to
// the published v1 response. The public result hash is the private sealed
// envelope hash that acknowledgement requires; the provider operation ID and
// applied state are copied from the accepted proof.
type PublicConfirmResult struct {
	Kind                   string  `json:"kind"`
	PlacementIntentID      string  `json:"placement_intent_id"`
	State                  string  `json:"state"`
	OperationKnown         bool    `json:"operation_known"`
	ProviderOperationID    *string `json:"provider_operation_id"`
	ResultContentHash      *string `json:"result_content_hash"`
	ReconciliationRequired bool    `json:"reconciliation_required"`
	ResubmissionAllowed    bool    `json:"resubmission_allowed"`
}

// ProjectConfirmResult fails closed when the service result does not provide
// the public information needed for a complete projection.
func ProjectConfirmResult(r ConfirmResponse) (PublicConfirmResult, error) {
	digest := assurance.HashBytes(r.ReadbackJSON)
	canonical, canonicalErr := assurance.CanonicalJSONBytes(r.ReadbackJSON)
	if r.Kind != "confirm" || r.Status != "machine_verified_visual_ack_pending" || r.State != r.Status || r.VisualState != "pending" || !validOpaqueID(r.PlacementIntentID) || !validOpaqueID(r.OperationReference) || !validIngressSHA256(r.ResultSHA256) || !validIngressSHA256(r.ReadbackSHA256) || !validIngressSHA256(r.TargetHash) || !validIngressSHA256(r.TerminalFenceDigest) || canonicalErr != nil || !bytes.Equal(canonical, r.ReadbackJSON) || r.ReadbackSHA256 != hex.EncodeToString(digest[:]) {
		return PublicConfirmResult{}, errors.New("content transport: confirmation result is not a complete terminal machine proof")
	}
	operation := r.OperationReference
	hash := r.ResultSHA256
	return PublicConfirmResult{Kind: "confirm", PlacementIntentID: r.PlacementIntentID, State: "verified", OperationKnown: true, ProviderOperationID: &operation, ResultContentHash: &hash, ReconciliationRequired: false, ResubmissionAllowed: false}, nil
}

// PublicReconcileResult is a direct safe projection of the persisted
// reconciliation result. ProviderOperationID remains nullable, evidence
// references and binding hash are copied exactly, and no proof is synthesized.
type PublicReconcileResult struct {
	Kind                       string   `json:"kind"`
	PlacementIntentID          string   `json:"placement_intent_id"`
	State                      string   `json:"state"`
	UncertaintyEpisodeID       string   `json:"uncertainty_episode_id"`
	OperationKnown             bool     `json:"operation_known"`
	ProviderOperationID        *string  `json:"provider_operation_id"`
	EvidenceIDs                []string `json:"evidence_ids"`
	EvidenceBindingContentHash string   `json:"evidence_binding_content_hash"`
	ResultContentHash          string   `json:"result_content_hash,omitempty"`
	LatestReadbackEvidenceID   string   `json:"latest_readback_evidence_id,omitempty"`
	NoEffectProofID            string   `json:"no_effect_proof_id,omitempty"`
	ReconciliationRequired     bool     `json:"reconciliation_required"`
	ResubmissionAllowed        bool     `json:"resubmission_allowed"`
}

func ProjectReconcileResult(r assurance.ContentReconciliationResult) (PublicReconcileResult, error) {
	if r.Kind != "reconcile" || !validOpaqueID(r.PlacementIntentID) || !validOpaqueID(r.UncertaintyEpisodeID) || !validIngressSHA256(r.EvidenceBindingHash) || len(r.EvidenceIDs) > 32 || r.ResubmissionAllowed {
		return PublicReconcileResult{}, errors.New("content transport: reconciliation result is incomplete or unsafe to project")
	}
	if r.OperationKnown != (r.ProviderOperationID != nil) || r.ProviderOperationID != nil && !validOpaqueID(*r.ProviderOperationID) {
		return PublicReconcileResult{}, errors.New("content transport: operation reference does not match known-operation flag")
	}
	if r.State == "confirmed_not_applied" {
		return PublicReconcileResult{}, errors.New("content transport: backend has no verified no-effect proof for this classification")
	}
	if !oneOf(r.State, "still_unknown", "confirmed_applied", "provider_evidence_inconsistent", "reconciliation_required") {
		return PublicReconcileResult{}, errors.New("content transport: reconciliation state is outside public contract")
	}
	if r.State == "confirmed_applied" {
		if r.ReconciliationRequired || !validOpaqueID(r.LatestReadbackID) || !validIngressSHA256(r.ResultSHA256) {
			return PublicReconcileResult{}, errors.New("content transport: applied result lacks its accepted readback binding")
		}
		found := false
		for _, id := range r.EvidenceIDs {
			found = found || id == r.LatestReadbackID
		}
		if !found {
			return PublicReconcileResult{}, errors.New("content transport: latest readback is absent from persisted evidence references")
		}
	} else if r.LatestReadbackID != "" || r.NoEffectProofID != "" {
		return PublicReconcileResult{}, errors.New("content transport: non-applied result carries a terminal evidence reference")
	}
	for _, id := range r.EvidenceIDs {
		if !validOpaqueID(id) {
			return PublicReconcileResult{}, errors.New("content transport: invalid reconciliation evidence reference")
		}
	}
	resultHash := ""
	if r.State == "confirmed_applied" {
		resultHash = r.ResultSHA256
	}
	return PublicReconcileResult{Kind: r.Kind, PlacementIntentID: r.PlacementIntentID, State: r.State, UncertaintyEpisodeID: r.UncertaintyEpisodeID, OperationKnown: r.OperationKnown, ProviderOperationID: r.ProviderOperationID, EvidenceIDs: append([]string{}, r.EvidenceIDs...), EvidenceBindingContentHash: r.EvidenceBindingHash, ResultContentHash: resultHash, LatestReadbackEvidenceID: r.LatestReadbackID, NoEffectProofID: r.NoEffectProofID, ReconciliationRequired: r.ReconciliationRequired, ResubmissionAllowed: false}, nil
}

func (r PlacementAcknowledgeRequest) IdempotencyDigests() (assurance.Digest, assurance.Digest, error) {
	request, err := assurance.ContentAcknowledgementRequestDigest(r.Request)
	if err != nil {
		return assurance.Digest{}, assurance.Digest{}, err
	}
	return assurance.DigestIdempotencyKey(r.IdempotencyKey), request, nil
}

func ProjectAcknowledgeOutput(r AcknowledgeResponse, invocation PlacementInvocation) (PublicPlacementOutput, error) {
	if err := invocation.validate(); err != nil {
		return PublicPlacementOutput{}, err
	}
	if r.Kind != "acknowledge" || !validOpaqueID(r.AcknowledgementID) || !validOpaqueID(r.PlacementIntentID) || !validIngressSHA256(r.ResultSHA256) || r.AuditID != invocation.AuditID || !oneOf(r.Observation, "visually_confirmed", "visual_conflict", "not_reviewed") {
		return PublicPlacementOutput{}, errors.New("content transport: acknowledgement response lacks its sealed reservation binding")
	}
	wantStatus := map[string]string{"visually_confirmed": "visually_acknowledged", "visual_conflict": "reconciliation_required", "not_reviewed": "machine_verified_visual_ack_pending"}[r.Observation]
	if r.Status != wantStatus || r.State != wantStatus {
		return PublicPlacementOutput{}, errors.New("content transport: acknowledgement state is outside public contract")
	}
	result := map[string]any{"kind": "acknowledge", "placement_intent_id": r.PlacementIntentID, "observation": r.Observation, "acknowledgement_id": r.AcknowledgementID, "result_content_hash": r.ResultSHA256}
	return PublicPlacementOutput{Phase: "acknowledge", Status: "ok", NLAuditID: invocation.AuditID, Replayed: invocation.ReplayDisposition == "sealed_replay", NoMutationSubmitted: true, ReconciliationRequired: r.Status == "reconciliation_required", Result: result, Errors: []PublicPlacementError{}}, nil
}

type PublicStageResult struct {
	Kind                  string         `json:"kind"`
	Preview               map[string]any `json:"preview"`
	ReplayRequiresRestage bool           `json:"replay_requires_restage"`
}

// ProjectStageOutput maps a successful stage invocation to the public envelope.
// Replay is derived from the service response, and only the initial response
// receives its ephemeral confirmation token overlay.
func ProjectStageOutput(r StageResponse, auditID string) (PublicPlacementOutput, error) {
	if !validOpaqueID(auditID) || r.Kind != "stage" {
		return PublicPlacementOutput{}, errors.New("content transport: stage invocation metadata is incomplete")
	}
	preview, err := projectPublicPreview(r)
	if err != nil {
		return PublicPlacementOutput{}, err
	}
	result := PublicStageResult{Kind: "stage", Preview: preview, ReplayRequiresRestage: r.ReplayRequiresRestage}
	return PublicPlacementOutput{Phase: "stage", Status: "ok", NLAuditID: auditID, Replayed: r.ReplayRequiresRestage, NoMutationSubmitted: true, ReconciliationRequired: false, Result: result, Errors: []PublicPlacementError{}}, nil
}

// projectPublicPreview computes a transport-projection hash over the token-free
// public representation. It is distinct from the sealed private PreviewPlan hash.
func projectPublicPreview(r StageResponse) (map[string]any, error) {
	if len(r.Preview) == 0 {
		return nil, errors.New("content transport: frozen stage preview is missing")
	}
	canonical, err := assurance.CanonicalJSONBytes(r.Preview)
	if err != nil || !bytes.Equal(canonical, r.Preview) {
		return nil, errors.New("content transport: frozen preview is not canonical JSON")
	}
	var plan PreviewPlan
	if err := json.Unmarshal(canonical, &plan); err != nil {
		return nil, errors.New("content transport: frozen preview cannot be decoded")
	}
	hash, err := hashPlan(plan)
	if err != nil || hash != plan.ContentHash || !isCanonicalUUID(plan.PlacementIntentID) || !isCanonicalActorBindingID(plan.ActorBindingID) || !validOpaqueID(plan.SourceID) || !validIngressSHA256(plan.SourceHash) || !validOpaqueID(plan.CandidateID) || plan.CandidateRevision < 1 || !validIngressSHA256(plan.CandidateHash) || !validIngressSHA256(plan.CurrentValueHash) || plan.Destination.State != "active" || !validOpaqueID(plan.Destination.ID) || plan.Destination.Revision < 1 || !validIngressSHA256(plan.Destination.ContentHash) || plan.Destination.ProvenanceRequirement == "fine_grained_available" {
		return nil, errors.New("content transport: frozen preview bindings cannot be projected safely")
	}
	if plan.CandidateKind != "extracted_item" && plan.CandidateKind != "draft_artifact" {
		return nil, errors.New("content transport: frozen preview candidate kind is invalid")
	}
	derived := map[string]any{"revision": plan.CandidateRevision, "content_hash": plan.CandidateHash}
	switch plan.CandidateKind {
	case "extracted_item":
		if plan.CandidateOriginKind != "" {
			return nil, errors.New("content transport: extracted item unexpectedly carries draft origin metadata")
		}
		derived["extracted_item_id"] = plan.CandidateID
	case "draft_artifact":
		if !oneOf(plan.CandidateOriginKind, "analyst", "agent_flow", "copilot", "other") {
			return nil, errors.New("content transport: frozen draft origin kind is unavailable or invalid")
		}
		derived["draft_artifact_id"] = plan.CandidateID
		derived["origin_kind"] = plan.CandidateOriginKind
	}
	if len(r.ConfirmationToken) == 0 && !r.ReplayRequiresRestage || len(r.ConfirmationToken) > 0 && (r.ReplayRequiresRestage || !validTokenForTransport(r.ConfirmationToken)) {
		return nil, errors.New("content transport: initial token and replay disposition do not agree")
	}
	profile := plan.Destination
	// Source rows are create-only and keyed by source_artifact_id, so their
	// immutable first creation is the published binding's revision 1.
	source := map[string]any{"source_artifact_id": plan.SourceID, "revision": 1, "content_hash": plan.SourceHash}
	destination := map[string]any{"schema_version": 1, "destination_profile_id": profile.ID, "revision": profile.Revision, "content_hash": profile.ContentHash, "state": profile.State, "resource_id": profile.ResourceID, "sheet_id": profile.SheetID, "cell": profile.Cell, "operation": "single_cell_literal", "expected_interpretation": profile.ExpectedInterpretation, "formula_policy": "reject_formula_target", "literal_policy": "literal_only", "protection_policy": "reject_protected", "format_policy": "preserve", "unknown_critical_facts": "block", "provenance_requirement": profile.ProvenanceRequirement, "metadata_requirements": profile.MetadataRequirements, "allowed_conversions": profile.AllowedConversions, "resource_policy": profile.ResourcePolicy, "conversion_policy": profile.ConversionPolicy, "provider_contract": profile.ProviderContract, "retention_policy": profile.RetentionPolicy, "preview_ttl_seconds": profile.PreviewTTLSeconds}
	metadata := plan.ProviderMetadata
	if metadata.Protection != "unprotected" || metadata.Writable != "writable" || metadata.LiteralWriteFormatPreservation != "preserves" || looksLikeFormula(metadata.RawValue) {
		return nil, errors.New("content transport: provider observation does not prove required literal-write facts")
	}
	providerFacts := map[string]any{"provenance": "provider_verified", "observation_id": plan.ProviderObservationID, "observed_at": plan.ProviderObservedAt, "formula_target": false, "protected_target": false, "write_permission_verified": true, "format_preservation_verified": true}
	operatorFacts := map[string]any{"provenance": "operator_declared", "resource_policy": profile.ResourcePolicy, "conversion_policy": profile.ConversionPolicy, "expected_interpretation": profile.ExpectedInterpretation}
	facts, err := projectInterpretationFacts(profile)
	if err != nil {
		return nil, err
	}
	provenance := map[string]any{"kind": "source_bytes", "availability": "unavailable", "source": source, "reason": "not_verified"}
	preview := map[string]any{"placement_intent_id": plan.PlacementIntentID, "content_hash": "", "actor_binding_id": plan.ActorBindingID, "source": source, "derived_artifact": derived, "lineage": map[string]any{"kind": "source_bytes", "source": source}, "original_text": plan.OriginalText, "original_value": plan.OriginalValue, "original_interpretation": plan.OriginalInterpretation, "target_interpretation": plan.TargetInterpretation, "intended_value": plan.IntendedValue, "current_value": plan.CurrentValue, "current_value_content_hash": plan.CurrentValueHash, "source_provenance": provenance, "destination_profile": destination, "conversion": plan.Conversion, "provider_facts": providerFacts, "operator_constraint_facts": operatorFacts, "target_interpretation_facts": facts, "critical_facts_resolved": true, "created_at": plan.CreatedAt, "expires_at": plan.ExpiresAt, "retention_policy": plan.RetentionPolicy}
	projection, err := assurance.CanonicalJSON(preview)
	if err != nil {
		return nil, err
	}
	digest := assurance.HashBytes(projection)
	preview["content_hash"] = hex.EncodeToString(digest[:])
	if r.ConfirmationToken != "" {
		preview["confirmation_token"] = r.ConfirmationToken
		preview["token_delivery"] = "ephemeral_initial_response_only"
	}
	return preview, nil
}

func isCanonicalActorBindingID(value string) bool {
	parts := strings.Split(value, "/")
	return len(parts) == 2 && isCanonicalUUID(parts[0]) && isCanonicalUUID(parts[1])
}

func projectInterpretationFacts(profile DestinationProfile) (map[string]any, error) {
	i := profile.ExpectedInterpretation
	if i.StoredScale.String() != i.DisplayScale.String() {
		return nil, errors.New("content transport: output facts cannot represent distinct stored and display scales")
	}
	fact := func(v any) map[string]any {
		return map[string]any{"provenance": "operator_declared", "critical": true, "value": v, "evidence_id": profile.ID}
	}
	return map[string]any{"period": fact(nullableString(i.Period)), "currency": fact(nullableString(i.Currency)), "unit": fact(nullableString(i.Unit)), "scale": fact(i.DisplayScale), "percent_basis": fact(i.PercentBasis), "precision": fact(nullableNumber(i.Precision))}, nil
}

func nullableString(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}
func nullableNumber(raw json.RawMessage) any {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var n json.Number
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&n) != nil {
		return nil
	}
	return n
}
func validTokenForTransport(s string) bool {
	if len(s) < 32 || len(s) > 512 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
