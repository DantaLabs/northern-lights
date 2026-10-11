package assurance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
)

const contentAcknowledgeAction = "acknowledge"

type ContentAcknowledgementIntent struct {
	PlacementIntentID     string
	ActorID               string
	PreviewJSON           []byte
	PreviewSHA256         string
	State                 string
	ExpiresAt             time.Time
	CreatedAt             time.Time
	ConfirmationRecordID  string
	MachineResultRecordID string
	RowVersion            int64
	OperationReference    string
	ReadbackJSON          []byte
	ReadbackSHA256        string
	TerminalAt            time.Time
	TerminalFenceDigest   string
	TargetHash            string
}

// ContentReadbackEvidence is the typed proof persisted by confirm. RawValue
// is a JSON scalar; the remaining members bind it to an uncached, exact-cell
// observation and its critical metadata.
type ContentReadbackEvidence struct {
	SpreadsheetID           string          `json:"spreadsheet_id"`
	SheetID                 string          `json:"sheet_id"`
	Locator                 string          `json:"locator"`
	RawValue                json.RawMessage `json:"raw_value"`
	ValuePresent            bool            `json:"value_present"`
	RawFormats              json.RawMessage `json:"formats"`
	FormatsPresent          bool            `json:"formats_present"`
	EffectiveFormats        json.RawMessage `json:"effective_formats"`
	EffectiveFormatsPresent bool            `json:"effective_formats_present"`
	FormattingSHA256        string          `json:"formatting_sha256"`
	Protection              string          `json:"protection"`
	Writable                string          `json:"writable"`
	Provenance              struct {
		Provider   string `json:"provider"`
		APIVersion string `json:"api_version"`
		Endpoint   string `json:"endpoint"`
		QueryRange string `json:"query_range"`
		Cache      string `json:"cache"`
	} `json:"provenance"`
}

type ContentAcknowledgementRequest struct {
	PlacementIntentID string `json:"placement_intent_id"`
	Observation       string `json:"observation"`
	ResultSHA256      string `json:"result_sha256"`
	VisualNotes       string `json:"visual_notes,omitempty"`
}

type ContentAcknowledgementResult struct {
	Kind              string `json:"kind"`
	AcknowledgementID string `json:"acknowledgement_id"`
	Status            string `json:"status"`
	PlacementIntentID string `json:"placement_intent_id"`
	State             string `json:"state"`
	Observation       string `json:"observation"`
	ResultSHA256      string `json:"result_sha256"`
	AuditID           string `json:"nl_audit_id"`
}

func ContentAcknowledgementRequestDigest(request ContentAcknowledgementRequest) (Digest, error) {
	canonical, err := CanonicalJSON(request)
	if err != nil {
		return Digest{}, err
	}
	return HashBytes(canonical), nil
}

func requireContentAcknowledgePrincipal(ctx context.Context) (identity.Principal, error) {
	p, ok := identity.PrincipalFromContext(ctx)
	if !ok || p.TokenType != identity.TokenTypeDelegated || p.UsedSubjectFallback || !p.HasPermission(identity.PermissionContentAcknowledge) || !uuidIsCanonical(p.TenantID) || !uuidIsCanonical(p.ObjectID) {
		return identity.Principal{}, domainError("forbidden", "trusted delegated content.acknowledge capability required")
	}
	return p, nil
}

// GetContentAcknowledgementIntentAt returns only the caller's owned intent.
// Authorization must precede its use and any replay lookup in the service.
func (s *Store) GetContentAcknowledgementIntentAt(ctx context.Context, id string, now time.Time) (ContentAcknowledgementIntent, error) {
	p, err := requireContentAcknowledgePrincipal(ctx)
	if err != nil {
		return ContentAcknowledgementIntent{}, err
	}
	if s == nil || s.db == nil || !uuidIsCanonical(id) {
		return ContentAcknowledgementIntent{}, domainError("not_found_or_forbidden", "content placement intent is unavailable")
	}
	var record ContentAcknowledgementIntent
	var expires, terminal, created string
	err = s.db.QueryRowContext(ctx, `SELECT placement_intent_id,actor_id,preview_json,preview_sha256,state,expires_at,
	 confirmation_record_id,machine_result_record_id,row_version,operation_reference,readback_json,readback_sha256,terminal_at,terminal_fence_digest,target_hash,created_at
	 FROM assurance_content_placement_intents WHERE tenant_id=? AND actor_id=? AND placement_intent_id=?`,
		p.TenantID, p.AuditActor(), id).Scan(&record.PlacementIntentID, &record.ActorID, &record.PreviewJSON, &record.PreviewSHA256,
		&record.State, &expires, &record.ConfirmationRecordID, &record.MachineResultRecordID, &record.RowVersion, &record.OperationReference, &record.ReadbackJSON,
		&record.ReadbackSHA256, &terminal, &record.TerminalFenceDigest, &record.TargetHash, &created)
	if err != nil {
		return ContentAcknowledgementIntent{}, domainError("not_found_or_forbidden", "content placement intent is unavailable")
	}
	if record.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires); err != nil {
		return ContentAcknowledgementIntent{}, domainError("content_machine_proof_invalid", "content placement intent expiry is invalid")
	}
	if record.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return ContentAcknowledgementIntent{}, domainError("content_machine_proof_invalid", "content placement intent creation time is invalid")
	}
	if terminal != "" {
		record.TerminalAt, err = time.Parse(time.RFC3339Nano, terminal)
		if err != nil {
			return ContentAcknowledgementIntent{}, domainError("content_machine_proof_invalid", "content machine result is invalid")
		}
	}
	record.PreviewJSON = append([]byte(nil), record.PreviewJSON...)
	record.ReadbackJSON = append([]byte(nil), record.ReadbackJSON...)
	preview, err := decodeAcknowledgementPreview(record.PreviewJSON, record.PreviewSHA256, id, p.AuditActor())
	if err != nil || !preview.CreatedAt.Equal(record.CreatedAt) || !now.UTC().Before(record.CreatedAt.Add(time.Duration(preview.Destination.IntentLifetimeSeconds)*time.Second)) {
		return ContentAcknowledgementIntent{}, domainError("not_found_or_forbidden", "content placement intent is unavailable")
	}
	return record, nil
}

// ResolveContentAcknowledgementPolicy applies the configured signed resolver
// to the exact destination frozen in the owned preview.
func (s *Store) ResolveContentAcknowledgementPolicy(ctx context.Context, resolver *ContentPolicyResolver, profileID string, revision int, now time.Time) (DestinationProfile, ContentDestinationBindings, error) {
	if s == nil || s.db == nil || resolver == nil || resolver.store != s {
		return DestinationProfile{}, ContentDestinationBindings{}, domainError("content_policy_unavailable", "configured resolver bound to this assurance store is required")
	}
	if _, err := requireContentAcknowledgePrincipal(ctx); err != nil {
		return DestinationProfile{}, ContentDestinationBindings{}, err
	}
	return resolver.ResolveContentDestinationForCapability(ctx, profileID, revision, now.UTC(), identity.PermissionContentAcknowledge)
}

// ResolveOwnedContentAcknowledgementPolicy reads only the caller-owned frozen
// preview and resolves its exact destination using the signed capability.
func (s *Store) ResolveOwnedContentAcknowledgementPolicy(ctx context.Context, resolver *ContentPolicyResolver, intentID string, now time.Time) (ContentDestinationBindings, error) {
	intent, err := s.GetContentAcknowledgementIntentAt(ctx, intentID, now)
	if err != nil {
		return ContentDestinationBindings{}, err
	}
	p, err := requireContentAcknowledgePrincipal(ctx)
	if err != nil {
		return ContentDestinationBindings{}, err
	}
	preview, err := decodeAcknowledgementPreview(intent.PreviewJSON, intent.PreviewSHA256, intentID, p.AuditActor())
	if err != nil {
		return ContentDestinationBindings{}, err
	}
	_, bindings, err := s.ResolveContentAcknowledgementPolicy(ctx, resolver, preview.Destination.ID, preview.Destination.Revision, now)
	return bindings, err
}

func (s *Store) FinalizeContentAcknowledgement(ctx context.Context, reservation ReservationResult, request ContentAcknowledgementRequest, requestDigest Digest, resolver *ContentPolicyResolver, auditID string, now time.Time) (ContentAcknowledgementResult, error) {
	p, err := requireContentAcknowledgePrincipal(ctx)
	if err != nil {
		return ContentAcknowledgementResult{}, err
	}
	if s == nil || s.db == nil || resolver == nil || resolver.store != s {
		return ContentAcknowledgementResult{}, domainError("content_policy_unavailable", "configured resolver bound to this assurance store is required")
	}
	if auditID == "" || requestDigest == (Digest{}) || request.PlacementIntentID == "" || !contentPolicyHash.MatchString(request.ResultSHA256) || !utf8.ValidString(request.VisualNotes) || len([]byte(request.VisualNotes)) > 2048 {
		return ContentAcknowledgementResult{}, domainError("invalid_request", "content acknowledgement request is incomplete or exceeds bounds")
	}
	if request.Observation != "visually_confirmed" && request.Observation != "visual_conflict" && request.Observation != "not_reviewed" {
		return ContentAcknowledgementResult{}, domainError("invalid_request", "content acknowledgement observation is invalid")
	}
	actualDigest, err := ContentAcknowledgementRequestDigest(request)
	if err != nil || actualDigest != requestDigest {
		return ContentAcknowledgementResult{}, domainError("idempotency_conflict", "content acknowledgement digest does not match the request")
	}
	if reservation.Disposition != ReservationOwned || reservation.State != ReservationStateReserved || reservation.RecordID == "" || reservation.OwnerNonce == "" {
		return ContentAcknowledgementResult{}, domainError("idempotency_in_progress", "content acknowledgement reservation is not owned")
	}
	if err := s.RequireRichAudit(ctx); err != nil {
		return ContentAcknowledgementResult{}, err
	}
	intent, err := s.GetContentAcknowledgementIntentAt(ctx, request.PlacementIntentID, now)
	if err != nil {
		return ContentAcknowledgementResult{}, err
	}
	preview, err := decodeAcknowledgementPreview(intent.PreviewJSON, intent.PreviewSHA256, request.PlacementIntentID, p.AuditActor())
	if err != nil {
		return ContentAcknowledgementResult{}, err
	}
	profile, bindings, err := s.ResolveContentAcknowledgementPolicy(ctx, resolver, preview.Destination.ID, preview.Destination.Revision, now)
	if err != nil {
		return ContentAcknowledgementResult{}, err
	}
	if !sameAcknowledgementDestination(profile, preview.Destination) {
		return ContentAcknowledgementResult{}, domainError("content_policy_changed", "active signed destination no longer matches the frozen preview")
	}
	if bindings.Retention.IdempotencyLifetimeSeconds < int64(reservationLease/time.Second) {
		return ContentAcknowledgementResult{}, domainError("content_policy_invalid", "signed idempotency lifetime is shorter than the reservation lease")
	}
	now = now.UTC()
	result := ContentAcknowledgementResult{Kind: "acknowledge", AcknowledgementID: reservation.RecordID, PlacementIntentID: request.PlacementIntentID, Observation: request.Observation, ResultSHA256: request.ResultSHA256, AuditID: auditID}
	switch request.Observation {
	case "visually_confirmed":
		result.Status, result.State = "visually_acknowledged", "visually_acknowledged"
	case "visual_conflict":
		result.Status, result.State = "reconciliation_required", "reconciliation_required"
	case "not_reviewed":
		result.Status, result.State = "machine_verified_visual_ack_pending", "machine_verified_visual_ack_pending"
	}
	envelope, err := CanonicalJSON(result)
	if err != nil {
		return ContentAcknowledgementResult{}, err
	}
	metadata, err := CanonicalJSON(map[string]any{
		"observation": request.Observation, "result_sha256": request.ResultSHA256,
		"preview_sha256": intent.PreviewSHA256, "readback_sha256": intent.ReadbackSHA256,
		"terminal_fence_digest": intent.TerminalFenceDigest,
		"visual_notes_sha256":   digestHex(HashBytes([]byte(request.VisualNotes))),
	})
	if err != nil {
		return ContentAcknowledgementResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ContentAcknowledgementResult{}, fmt.Errorf("assurance: begin content acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var actor, tool, action, storedRequestDigest, ownerNonce, leaseExpiry, retainExpiry, correlationID, reservationState string
	if err := tx.QueryRowContext(ctx, `SELECT actor_id,tool,action,request_digest,owner_nonce,lease_expires_at,expires_at,correlation_id,state
	 FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=?`, p.TenantID, reservation.RecordID).Scan(
		&actor, &tool, &action, &storedRequestDigest, &ownerNonce, &leaseExpiry, &retainExpiry, &correlationID, &reservationState); err != nil {
		return ContentAcknowledgementResult{}, domainError("idempotency_conflict", "content acknowledgement reservation is unavailable")
	}
	if actor != p.AuditActor() || tool != "workiva_content_placement" || action != contentAcknowledgeAction || storedRequestDigest != digestHex(requestDigest) || subtle.ConstantTimeCompare([]byte(ownerNonce), []byte(reservation.OwnerNonce)) != 1 || reservationState != string(ReservationStateReserved) {
		return ContentAcknowledgementResult{}, domainError("idempotency_conflict", "content acknowledgement reservation binding does not match")
	}
	lease, leaseErr := time.Parse(time.RFC3339Nano, leaseExpiry)
	retained, retainErr := time.Parse(time.RFC3339Nano, retainExpiry)
	wantRetained := now.Add(time.Duration(bindings.Retention.IdempotencyLifetimeSeconds) * time.Second)
	if leaseErr != nil || !lease.After(now) || retainErr != nil || !retained.Equal(wantRetained) {
		return ContentAcknowledgementResult{}, domainError("idempotency_conflict", "content acknowledgement reservation is expired or has a different signed horizon")
	}
	if err := verifyActiveContentBundleTx(ctx, tx, p.TenantID, ContentIntakePolicySnapshot{ActiveBundleHash: bindings.ActiveBundleHash, ActiveBundleVersion: bindings.ActiveBundleVersion}); err != nil {
		return ContentAcknowledgementResult{}, err
	}
	var current ContentAcknowledgementIntent
	var expires, terminal string
	err = tx.QueryRowContext(ctx, `SELECT placement_intent_id,actor_id,preview_json,preview_sha256,state,expires_at,
	 confirmation_record_id,machine_result_record_id,row_version,operation_reference,readback_json,readback_sha256,terminal_at,terminal_fence_digest,target_hash
	 FROM assurance_content_placement_intents WHERE tenant_id=? AND actor_id=? AND placement_intent_id=?`, p.TenantID, p.AuditActor(), request.PlacementIntentID).Scan(
		&current.PlacementIntentID, &current.ActorID, &current.PreviewJSON, &current.PreviewSHA256, &current.State, &expires,
		&current.ConfirmationRecordID, &current.MachineResultRecordID, &current.RowVersion, &current.OperationReference, &current.ReadbackJSON, &current.ReadbackSHA256,
		&terminal, &current.TerminalFenceDigest, &current.TargetHash)
	if err != nil {
		return ContentAcknowledgementResult{}, domainError("not_found_or_forbidden", "content placement intent is unavailable")
	}
	current.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires)
	if err != nil || !current.ExpiresAt.After(now) || current.State != "machine_verified_visual_ack_pending" || current.RowVersion != intent.RowVersion || current.ConfirmationRecordID == "" {
		return ContentAcknowledgementResult{}, domainError("content_acknowledgement_conflict", "content placement intent is not awaiting visual acknowledgement")
	}
	if terminal != "" {
		current.TerminalAt, err = time.Parse(time.RFC3339Nano, terminal)
	}
	if err != nil || current.TerminalAt.IsZero() || current.TerminalAt.After(now) || current.OperationReference == "" || current.MachineResultRecordID == "" || !contentPolicyHash.MatchString(current.ReadbackSHA256) || !contentPolicyHash.MatchString(current.TerminalFenceDigest) || !contentPolicyHash.MatchString(current.TargetHash) {
		return ContentAcknowledgementResult{}, domainError("content_machine_proof_invalid", "content machine result is incomplete")
	}
	if !bytes.Equal(current.PreviewJSON, intent.PreviewJSON) || current.PreviewSHA256 != intent.PreviewSHA256 || digestHex(HashBytes(current.PreviewJSON)) != current.PreviewSHA256 {
		return ContentAcknowledgementResult{}, domainError("content_machine_proof_invalid", "frozen preview integrity verification failed")
	}
	if err := verifyAcknowledgementReadback(current.ReadbackJSON, current.ReadbackSHA256, preview); err != nil {
		return ContentAcknowledgementResult{}, err
	}
	if err := verifyAcknowledgementResultTx(ctx, tx, p, current, request.ResultSHA256); err != nil {
		return ContentAcknowledgementResult{}, err
	}
	newState := result.State
	var updated sql.Result
	switch request.Observation {
	case "not_reviewed":
		updated, err = tx.ExecContext(ctx, `UPDATE assurance_content_placement_intents SET state=?
	 WHERE tenant_id=? AND placement_intent_id=? AND actor_id=? AND state='machine_verified_visual_ack_pending' AND row_version=?
	 AND confirmation_record_id=? AND readback_sha256=? AND terminal_fence_digest=?`, newState, p.TenantID, request.PlacementIntentID,
			p.AuditActor(), current.RowVersion, current.ConfirmationRecordID, current.ReadbackSHA256, current.TerminalFenceDigest)
	case "visual_conflict":
		episodeID := uuid.NewString()
		updated, err = tx.ExecContext(ctx, `UPDATE assurance_content_placement_intents SET state=?,uncertainty_episode_id=CASE WHEN uncertainty_episode_id='' THEN ? ELSE uncertainty_episode_id END,row_version=row_version+1
	 WHERE tenant_id=? AND placement_intent_id=? AND actor_id=? AND state='machine_verified_visual_ack_pending' AND row_version=?
	 AND confirmation_record_id=? AND readback_sha256=? AND terminal_fence_digest=?`, newState, episodeID, p.TenantID, request.PlacementIntentID,
			p.AuditActor(), current.RowVersion, current.ConfirmationRecordID, current.ReadbackSHA256, current.TerminalFenceDigest)
	case "visually_confirmed":
		updated, err = tx.ExecContext(ctx, `UPDATE assurance_content_placement_intents SET state=?,row_version=row_version+1
	 WHERE tenant_id=? AND placement_intent_id=? AND actor_id=? AND state='machine_verified_visual_ack_pending' AND row_version=?
	 AND confirmation_record_id=? AND readback_sha256=? AND terminal_fence_digest=?`, newState, p.TenantID, request.PlacementIntentID,
			p.AuditActor(), current.RowVersion, current.ConfirmationRecordID, current.ReadbackSHA256, current.TerminalFenceDigest)
	}
	if err != nil {
		return ContentAcknowledgementResult{}, err
	}
	if err := requireOneTransition(updated); err != nil {
		return ContentAcknowledgementResult{}, domainError("content_acknowledgement_conflict", "content acknowledgement compare-and-swap failed")
	}
	if _, err := s.auditLog.AppendTx(ctx, tx, audit.Entry{Ts: now, Actor: p.AuditActor(), Tool: tool, Action: action, Target: request.PlacementIntentID, AfterJSON: string(metadata), AuditID: auditID}); err != nil {
		return ContentAcknowledgementResult{}, domainError("audit_unavailable", "content acknowledgement rich audit append failed")
	}
	for _, entity := range []struct{ kind, id string }{{"content_placement_intent", request.PlacementIntentID}, {"idempotency_record", reservation.RecordID}} {
		if err := insertContentAuditLink(ctx, tx, p.TenantID, entity.kind, entity.id, auditID, correlationID, now); err != nil {
			return ContentAcknowledgementResult{}, fmt.Errorf("assurance: link content acknowledgement audit: %w", err)
		}
	}
	sealed, err := tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET state='sealed',response_envelope=?,response_hash=?,response_status=?,entity_reference=?,audit_id=?,terminal_at=?,lease_expires_at=?
	 WHERE tenant_id=? AND record_id=? AND actor_id=? AND tool=? AND action=? AND state='reserved' AND owner_nonce=? AND request_digest=? AND lease_expires_at>?`,
		string(envelope), digestHex(HashBytes(envelope)), result.Status, request.PlacementIntentID, auditID, formatTimestamp(now), formatTimestamp(now),
		p.TenantID, reservation.RecordID, p.AuditActor(), tool, action, reservation.OwnerNonce, digestHex(requestDigest), formatTimestamp(now))
	if err != nil {
		return ContentAcknowledgementResult{}, err
	}
	if err := requireOneTransition(sealed); err != nil {
		return ContentAcknowledgementResult{}, domainError("idempotency_conflict", "content acknowledgement reservation seal compare-and-swap failed")
	}
	if err := tx.Commit(); err != nil {
		return ContentAcknowledgementResult{}, fmt.Errorf("assurance: commit content acknowledgement: %w", err)
	}
	return result, nil
}

type acknowledgementPreview struct {
	PlacementIntentID string                            `json:"placement_intent_id"`
	ContentHash       string                            `json:"content_hash"`
	ActorBindingID    string                            `json:"actor_binding_id"`
	CreatedAt         time.Time                         `json:"created_at"`
	ExpiresAt         time.Time                         `json:"expires_at"`
	IntendedValue     json.RawMessage                   `json:"intended_value"`
	Destination       acknowledgementPreviewDestination `json:"destination_profile"`
	ProviderMetadata  struct {
		FormattingSHA256 string `json:"formatting_sha256"`
	} `json:"provider_metadata"`
}

func decodeAcknowledgementPreview(raw []byte, hash, id, actor string) (acknowledgementPreview, error) {
	var preview acknowledgementPreview
	canonical, err := CanonicalJSONBytes(raw)
	if err != nil || !bytes.Equal(canonical, raw) || digestHex(HashBytes(raw)) != hash || json.Unmarshal(raw, &preview) != nil || preview.PlacementIntentID != id || preview.ActorBindingID != actor || preview.ContentHash == "" || preview.CreatedAt.IsZero() || preview.ExpiresAt.IsZero() || preview.Destination.IntentLifetimeSeconds < 1 || preview.Destination.ID == "" || preview.Destination.Revision < 1 || preview.Destination.ResourceID == "" || preview.Destination.SheetID == "" || preview.Destination.Cell == "" || len(preview.IntendedValue) == 0 {
		return acknowledgementPreview{}, domainError("content_machine_proof_invalid", "frozen content preview is invalid")
	}
	return preview, nil
}

func sameAcknowledgementDestination(profile DestinationProfile, frozen acknowledgementPreviewDestination) bool {
	return profile.DestinationProfileID != nil && *profile.DestinationProfileID == frozen.ID && profile.Revision != nil && *profile.Revision == frozen.Revision && profile.ResourceID != nil && *profile.ResourceID == frozen.ResourceID && profile.SheetID != nil && *profile.SheetID == frozen.SheetID && profile.Cell != nil && *profile.Cell == frozen.Cell && profile.ContentHash != nil && *profile.ContentHash == frozen.ContentHash
}

type acknowledgementPreviewDestination struct {
	ID                    string `json:"destination_profile_id"`
	Revision              int    `json:"revision"`
	IntentLifetimeSeconds int64  `json:"intent_lifetime_seconds"`
	ResourceID            string `json:"resource_id"`
	SheetID               string `json:"sheet_id"`
	Cell                  string `json:"cell"`
	ContentHash           string `json:"content_hash"`
}

func verifyAcknowledgementReadback(raw []byte, hash string, preview acknowledgementPreview) error {
	canonical, err := CanonicalJSONBytes(raw)
	var evidence ContentReadbackEvidence
	if json.Unmarshal(raw, &evidence) != nil {
		return domainError("content_machine_proof_invalid", "uncached typed readback evidence is invalid or incomplete")
	}
	computedFormattingHash, formatErr := ContentFormattingHash(evidence.RawFormats, evidence.FormatsPresent, evidence.EffectiveFormats, evidence.EffectiveFormatsPresent)
	endpoint := "GET /spreadsheets/{spreadsheetId}/sheets/{sheetId}/sheetdata"
	if err != nil || formatErr != nil || !bytes.Equal(canonical, raw) || digestHex(HashBytes(raw)) != hash || !evidence.ValuePresent || len(evidence.RawValue) == 0 || !contentPolicyHash.MatchString(evidence.FormattingSHA256) || !evidence.FormatsPresent || !json.Valid(evidence.RawFormats) || !evidence.EffectiveFormatsPresent || !json.Valid(evidence.EffectiveFormats) || evidence.FormattingSHA256 != computedFormattingHash || evidence.Protection != "unprotected" || evidence.Writable != "writable" || evidence.SpreadsheetID != preview.Destination.ResourceID || evidence.SheetID != preview.Destination.SheetID || evidence.Locator != preview.Destination.Cell || evidence.Provenance.Provider != "workiva_rest" || evidence.Provenance.APIVersion != "2026-01-01" || evidence.Provenance.Endpoint != endpoint || evidence.Provenance.QueryRange != preview.Destination.Cell || evidence.Provenance.Cache != "bypassed" || !contentPolicyHash.MatchString(preview.ProviderMetadata.FormattingSHA256) || evidence.FormattingSHA256 != preview.ProviderMetadata.FormattingSHA256 {
		return domainError("content_machine_proof_invalid", "uncached typed readback evidence is invalid or incomplete")
	}
	actual, err := CanonicalJSONBytes(evidence.RawValue)
	want, wantErr := CanonicalJSONBytes(preview.IntendedValue)
	if err != nil || wantErr != nil || !bytes.Equal(actual, want) {
		return domainError("content_machine_proof_invalid", "verified readback does not match the frozen intended value")
	}
	var scalar any
	decoder := json.NewDecoder(bytes.NewReader(actual))
	decoder.UseNumber()
	if err := decoder.Decode(&scalar); err != nil || scalar == nil {
		return domainError("content_machine_proof_invalid", "verified readback value is not a supported literal scalar")
	}
	switch scalar.(type) {
	case string, bool, json.Number:
	default:
		return domainError("content_machine_proof_invalid", "verified readback value is not a supported literal scalar")
	}
	return nil
}

// ContentFormattingHash hashes canonical raw/effective format observations and
// their presence bits without coupling assurance to the provider package.
func ContentFormattingHash(formats json.RawMessage, formatsPresent bool, effective json.RawMessage, effectivePresent bool) (string, error) {
	raw, err := canonicalAcknowledgementFormats(formats, formatsPresent)
	if err != nil {
		return "", err
	}
	eff, err := canonicalAcknowledgementFormats(effective, effectivePresent)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(struct {
		Formats          json.RawMessage `json:"formats"`
		FormatsPresent   bool            `json:"formats_present"`
		Effective        json.RawMessage `json:"effective_formats"`
		EffectivePresent bool            `json:"effective_formats_present"`
	}{raw, formatsPresent, eff, effectivePresent})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func canonicalAcknowledgementFormats(raw json.RawMessage, present bool) (json.RawMessage, error) {
	if !present {
		if len(raw) != 0 {
			return nil, fmt.Errorf("format bytes supplied while absent")
		}
		return nil, nil
	}
	if len(raw) == 0 || len(raw) > 1<<20 {
		return nil, fmt.Errorf("format payload is empty or oversized")
	}
	canonical, err := CanonicalJSONBytes(raw)
	if err != nil {
		return nil, err
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value != nil {
		if _, ok := value.(map[string]any); !ok {
			return nil, fmt.Errorf("format payload must be object or null")
		}
	}
	return canonical, nil
}

func verifyAcknowledgementResultTx(ctx context.Context, tx *sql.Tx, principal identity.Principal, intent ContentAcknowledgementIntent, expectedHash string) error {
	var actor, tool, action, state, status, entity, storedHash, envelope string
	err := tx.QueryRowContext(ctx, `SELECT actor_id,tool,action,state,response_status,entity_reference,response_hash,response_envelope
	 FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=?`, principal.TenantID, intent.MachineResultRecordID).Scan(&actor, &tool, &action, &state, &status, &entity, &storedHash, &envelope)
	if err != nil || actor != principal.AuditActor() || tool != "workiva_content_placement" || (action != "confirm" && action != "classify") || state != string(ReservationStateSealed) || entity != intent.PlacementIntentID || storedHash != expectedHash || digestHex(HashBytes([]byte(envelope))) != storedHash {
		return domainError("content_machine_proof_invalid", "sealed machine confirmation result is unavailable or mismatched")
	}
	canonical, err := CanonicalJSONBytes([]byte(envelope))
	if err != nil || !bytes.Equal(canonical, []byte(envelope)) {
		return domainError("content_machine_proof_invalid", "sealed machine confirmation result is not canonical")
	}
	var result struct {
		Kind                 string   `json:"kind"`
		UncertaintyEpisodeID string   `json:"uncertainty_episode_id"`
		EvidenceBindingHash  string   `json:"evidence_binding_content_hash"`
		EvidenceIDs          []string `json:"evidence_ids"`
		PlacementIntentID    string   `json:"placement_intent_id"`
		State                string   `json:"state"`
		ReadbackSHA256       string   `json:"readback_sha256"`
		OperationReference   string   `json:"operation_reference"`
		TargetHash           string   `json:"target_hash"`
		TerminalFenceDigest  string   `json:"terminal_fence_digest"`
		RowVersion           int64    `json:"row_version"`
	}
	validResult := false
	if action == "confirm" {
		if json.Unmarshal(canonical, &result) == nil && result.PlacementIntentID == intent.PlacementIntentID {
			validResult = result.Kind == "confirm" && status == "machine_verified_visual_ack_pending" && result.State == status && result.ReadbackSHA256 == intent.ReadbackSHA256 && result.OperationReference == intent.OperationReference && result.TargetHash == intent.TargetHash && result.TerminalFenceDigest == intent.TerminalFenceDigest && result.RowVersion == intent.RowVersion
		}
	} else {
		var proof struct {
			PlacementIntentID   string `json:"placement_intent_id"`
			State               string `json:"state"`
			ReadbackSHA256      string `json:"readback_sha256"`
			OperationReference  string `json:"operation_reference"`
			TargetHash          string `json:"target_hash"`
			TerminalFenceDigest string `json:"terminal_fence_digest"`
			RowVersion          int64  `json:"row_version"`
		}
		var envelopeObject struct {
			Result       json.RawMessage `json:"result"`
			MachineProof json.RawMessage `json:"machine_proof"`
		}
		if json.Unmarshal(canonical, &envelopeObject) == nil && json.Unmarshal(envelopeObject.MachineProof, &proof) == nil {
			var publicResult struct {
				Kind                 string   `json:"kind"`
				State                string   `json:"state"`
				PlacementIntentID    string   `json:"placement_intent_id"`
				UncertaintyEpisodeID string   `json:"uncertainty_episode_id"`
				EvidenceBindingHash  string   `json:"evidence_binding_content_hash"`
				EvidenceIDs          []string `json:"evidence_ids"`
			}
			if json.Unmarshal(envelopeObject.Result, &publicResult) == nil && publicResult.PlacementIntentID == intent.PlacementIntentID {
				var evidenceID, evidenceReadback, evidenceOperation string
				if len(publicResult.EvidenceIDs) == 1 {
					_ = tx.QueryRowContext(ctx, `SELECT evidence_id,readback_hash,operation_reference_hash FROM assurance_content_reconciliation_evidence WHERE tenant_id=? AND evidence_id=? AND intent_id=? AND actor_id=? AND episode_id=? AND kind='read_back'`, principal.TenantID, publicResult.EvidenceIDs[0], intent.PlacementIntentID, principal.AuditActor(), publicResult.UncertaintyEpisodeID).Scan(&evidenceID, &evidenceReadback, &evidenceOperation)
				}
				validResult = publicResult.Kind == "reconcile" && status == "ok" && publicResult.State == "confirmed_applied" && publicResult.PlacementIntentID == intent.PlacementIntentID && uuidIsCanonical(publicResult.UncertaintyEpisodeID) && contentPolicyHash.MatchString(publicResult.EvidenceBindingHash) && proof.PlacementIntentID == intent.PlacementIntentID && proof.State == "machine_verified_visual_ack_pending" && proof.ReadbackSHA256 == evidenceReadback && proof.OperationReference == intent.OperationReference && proof.TargetHash == intent.TargetHash && proof.TerminalFenceDigest == intent.TerminalFenceDigest && proof.RowVersion == intent.RowVersion && evidenceID == publicResult.EvidenceIDs[0] && evidenceReadback != "" && evidenceOperation == digestHex(HashBytes([]byte(intent.OperationReference)))
				if validResult {
					var readback []byte
					_ = tx.QueryRowContext(ctx, `SELECT readback_json FROM assurance_content_reconciliation_evidence WHERE tenant_id=? AND evidence_id=?`, principal.TenantID, evidenceID).Scan(&readback)
					preview, err := decodeAcknowledgementPreview(intent.PreviewJSON, intent.PreviewSHA256, intent.PlacementIntentID, principal.AuditActor())
					validResult = err == nil && verifyAcknowledgementReadback(readback, evidenceReadback, preview) == nil
				}
			}
		}
	}
	if !validResult {
		return domainError("content_machine_proof_invalid", "sealed confirmation result does not bind the persisted readback")
	}
	return nil
}
