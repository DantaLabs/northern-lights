package assurance

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
)

const contentReconcileAction = "reconcile"

// ContentReconciliationIntent is an actor-owned private intent projection.
// It deliberately excludes the confirmation token digest.
type ContentReconciliationIntent struct {
	PlacementIntentID            string
	ActorID                      string
	State                        string
	PreviewJSON                  []byte
	PreviewSHA256                string
	ExpiresAt                    time.Time
	ConfirmationRecordID         string
	MachineResultRecordID        string
	RowVersion                   int64
	OperationReference           string
	UnknownReason                string
	ClaimFenceDigest             string
	TerminalFenceDigest          string
	TargetHash                   string
	UncertaintyEpisodeID         string
	ReconciliationClassification string
	ClaimRowVersion              int64
	ClaimedAt                    time.Time
	ConfirmIdempotencyDigest     string
	ConfirmRequestDigest         string
	CreatedAt                    time.Time
}

// ContentReconciliationEvidence is an immutable, private provider observation.
// Raw read-back values stay in the assurance database and never enter audit metadata.
type ContentReconciliationEvidence struct {
	EvidenceID              string          `json:"evidence_id"`
	PlacementIntentID       string          `json:"placement_intent_id"`
	ActorID                 string          `json:"actor_id"`
	UncertaintyEpisodeID    string          `json:"uncertainty_episode_id"`
	Kind                    string          `json:"kind"`
	OperationReferenceHash  string          `json:"operation_reference_hash"`
	OperationStatus         string          `json:"status"`
	OperationInspectionJSON json.RawMessage `json:"operation_inspection_json"`
	OperationInspectionHash string          `json:"operation_inspection_hash"`
	TargetHash              string          `json:"target_hash"`
	IntendedHash            string          `json:"intended_hash"`
	ReadbackJSON            json.RawMessage `json:"readback_json"`
	ReadbackHash            string          `json:"readback_hash"`
	CacheBypassed           bool            `json:"cache_bypassed"`
	ProviderObservedAt      time.Time       `json:"provider_observed_at"`
	CreatedAt               time.Time       `json:"created_at"`
	ExpiresAt               time.Time       `json:"expires_at"`
}

// ContentReconciliationResult is safe for a sealed idempotency envelope.
type ContentReconciliationResult struct {
	Kind                   string   `json:"kind"`
	PlacementIntentID      string   `json:"placement_intent_id"`
	State                  string   `json:"state"`
	UncertaintyEpisodeID   string   `json:"uncertainty_episode_id"`
	OperationKnown         bool     `json:"operation_known"`
	ProviderOperationID    *string  `json:"provider_operation_id"`
	EvidenceIDs            []string `json:"evidence_ids"`
	EvidenceBindingHash    string   `json:"evidence_binding_content_hash"`
	LatestReadbackID       string   `json:"latest_readback_evidence_id,omitempty"`
	NoEffectProofID        string   `json:"no_effect_proof_id,omitempty"`
	ReconciliationRequired bool     `json:"reconciliation_required"`
	ResubmissionAllowed    bool     `json:"resubmission_allowed"`
	// ResultSHA256 is an out-of-band handle to the exact sealed response
	// envelope. It is computed after sealing and excluded from that envelope.
	ResultSHA256 string `json:"-"`
}

// ContentReconciliationMachineProof is private sealed evidence consumed by
// visual acknowledgement. It is separate from the public reconcile result.
type ContentReconciliationMachineProof struct {
	PlacementIntentID   string `json:"placement_intent_id"`
	State               string `json:"state"`
	ReadbackSHA256      string `json:"readback_sha256"`
	OperationReference  string `json:"operation_reference"`
	TargetHash          string `json:"target_hash"`
	TerminalFenceDigest string `json:"terminal_fence_digest"`
	RowVersion          int64  `json:"row_version"`
}

type ContentReconciliationEnvelope struct {
	Result       ContentReconciliationResult        `json:"result"`
	MachineProof *ContentReconciliationMachineProof `json:"machine_proof,omitempty"`
}

// GetOwnedContentReconciliationIntent authorizes content.reconcile before
// looking up private intent data. Expired intents remain visible to the owner
// so the caller can receive a conservative unknown outcome.
func (s *Store) GetOwnedContentReconciliationIntent(ctx context.Context, id string) (ContentReconciliationIntent, error) {
	p, err := requireContentReconcilePrincipal(ctx)
	if err != nil {
		return ContentReconciliationIntent{}, err
	}
	if s == nil || s.db == nil || !uuidIsCanonical(id) {
		return ContentReconciliationIntent{}, domainError("not_found_or_forbidden", "content placement intent is unavailable")
	}
	var record ContentReconciliationIntent
	var created, expires, claimed, idempotencyDigest, requestDigest string
	err = s.db.QueryRowContext(ctx, `SELECT placement_intent_id,actor_id,state,preview_json,preview_sha256,expires_at,
	 confirmation_record_id,row_version,operation_reference,unknown_reason,claim_fence_digest,terminal_fence_digest,
	 machine_result_record_id,target_hash,uncertainty_episode_id,reconciliation_classification,created_at,claim_row_version,claimed_at,
	 (SELECT idempotency_digest FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=confirmation_record_id),
	 (SELECT request_digest FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=confirmation_record_id)
	 FROM assurance_content_placement_intents WHERE tenant_id=? AND actor_id=? AND placement_intent_id=?`,
		p.TenantID, p.TenantID, p.TenantID, p.AuditActor(), id).Scan(&record.PlacementIntentID, &record.ActorID, &record.State, &record.PreviewJSON,
		&record.PreviewSHA256, &expires, &record.ConfirmationRecordID, &record.RowVersion, &record.OperationReference,
		&record.UnknownReason, &record.ClaimFenceDigest, &record.TerminalFenceDigest, &record.MachineResultRecordID, &record.TargetHash,
		&record.UncertaintyEpisodeID, &record.ReconciliationClassification, &created, &record.ClaimRowVersion, &claimed, &idempotencyDigest, &requestDigest)
	if err != nil {
		return ContentReconciliationIntent{}, domainError("not_found_or_forbidden", "content placement intent is unavailable")
	}
	canonical, canonicalErr := CanonicalJSONBytes(record.PreviewJSON)
	if canonicalErr != nil || !bytes.Equal(canonical, record.PreviewJSON) || digestHex(HashBytes(record.PreviewJSON)) != record.PreviewSHA256 {
		return ContentReconciliationIntent{}, domainError("content_preview_integrity_failed", "stored content preview integrity verification failed")
	}
	if record.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires); err != nil {
		return ContentReconciliationIntent{}, domainError("content_intent_integrity_failed", "stored content intent expiry is invalid")
	}
	if record.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return ContentReconciliationIntent{}, domainError("content_intent_integrity_failed", "stored content intent creation time is invalid")
	}
	if claimed != "" {
		if record.ClaimedAt, err = time.Parse(time.RFC3339Nano, claimed); err != nil {
			return ContentReconciliationIntent{}, domainError("content_intent_integrity_failed", "stored claim timestamp is invalid")
		}
	}
	record.ConfirmIdempotencyDigest, record.ConfirmRequestDigest = idempotencyDigest, requestDigest
	if record.ActorID != p.AuditActor() || !oneOfContentIntentState(record.State) || record.RowVersion < 1 {
		return ContentReconciliationIntent{}, domainError("not_found_or_forbidden", "content placement intent is unavailable")
	}
	record.PreviewJSON = append([]byte(nil), record.PreviewJSON...)
	return record, nil
}

func (s *Store) GetOwnedContentReconciliationEvidence(ctx context.Context, intentID, episodeID, evidenceID string) (ContentReconciliationEvidence, error) {
	return s.GetOwnedContentReconciliationEvidenceAt(ctx, intentID, episodeID, evidenceID, time.Now().UTC())
}

func (s *Store) GetOwnedContentReconciliationEvidenceAt(ctx context.Context, intentID, episodeID, evidenceID string, now time.Time) (ContentReconciliationEvidence, error) {
	p, err := requireContentReconcilePrincipal(ctx)
	if err != nil {
		return ContentReconciliationEvidence{}, err
	}
	if s == nil || s.db == nil || !uuidIsCanonical(intentID) || !uuidIsCanonical(episodeID) || !uuidIsCanonical(evidenceID) {
		return ContentReconciliationEvidence{}, domainError("not_found_or_forbidden", "content reconciliation evidence is unavailable")
	}
	return getOwnedContentReconciliationEvidence(ctx, s.db, p.TenantID, p.AuditActor(), intentID, episodeID, evidenceID, now.UTC())
}

func (s *Store) GetLatestOwnedContentReconciliationEvidence(ctx context.Context, intentID, episodeID string) (ContentReconciliationEvidence, error) {
	return s.GetLatestOwnedContentReconciliationEvidenceAt(ctx, intentID, episodeID, time.Now().UTC())
}

func (s *Store) GetLatestOwnedContentReconciliationEvidenceAt(ctx context.Context, intentID, episodeID string, now time.Time) (ContentReconciliationEvidence, error) {
	p, err := requireContentReconcilePrincipal(ctx)
	if err != nil {
		return ContentReconciliationEvidence{}, err
	}
	if s == nil || s.db == nil || !uuidIsCanonical(intentID) || !uuidIsCanonical(episodeID) {
		return ContentReconciliationEvidence{}, domainError("not_found_or_forbidden", "content reconciliation evidence is unavailable")
	}
	var id string
	err = s.db.QueryRowContext(ctx, `SELECT e.evidence_id FROM assurance_content_reconciliation_evidence e
	 JOIN assurance_content_placement_intents i ON i.tenant_id=e.tenant_id AND i.placement_intent_id=e.intent_id
	 WHERE e.tenant_id=? AND e.actor_id=? AND e.intent_id=? AND e.episode_id=? AND e.expires_at>? AND i.actor_id=?
		ORDER BY e.rowid DESC LIMIT 1`, p.TenantID, p.AuditActor(), intentID, episodeID, now.UTC().Format(time.RFC3339Nano), p.AuditActor()).Scan(&id)
	if err != nil {
		return ContentReconciliationEvidence{}, domainError("not_found_or_forbidden", "content reconciliation evidence is unavailable")
	}
	return getOwnedContentReconciliationEvidence(ctx, s.db, p.TenantID, p.AuditActor(), intentID, episodeID, id, now.UTC())
}

func getOwnedContentReconciliationEvidence(ctx context.Context, db *sql.DB, tenant, actor, intentID, episodeID, evidenceID string, now time.Time) (ContentReconciliationEvidence, error) {
	var e ContentReconciliationEvidence
	var inspect, readback []byte
	var observed, created, expires string
	var cache int
	err := db.QueryRowContext(ctx, `SELECT evidence_id,intent_id,actor_id,episode_id,kind,operation_reference_hash,status,
	 operation_inspection_json,operation_inspection_hash,target_hash,intended_hash,readback_json,readback_hash,
	 cache_bypassed,provider_observed_at,created_at,expires_at FROM assurance_content_reconciliation_evidence
	 WHERE tenant_id=? AND evidence_id=? AND intent_id=? AND actor_id=? AND episode_id=?`, tenant, evidenceID, intentID, actor, episodeID).Scan(
		&e.EvidenceID, &e.PlacementIntentID, &e.ActorID, &e.UncertaintyEpisodeID, &e.Kind, &e.OperationReferenceHash,
		&e.OperationStatus, &inspect, &e.OperationInspectionHash, &e.TargetHash, &e.IntendedHash, &readback, &e.ReadbackHash,
		&cache, &observed, &created, &expires)
	if err != nil {
		return ContentReconciliationEvidence{}, domainError("not_found_or_forbidden", "content reconciliation evidence is unavailable")
	}
	if digestHex(HashBytes(inspect)) != e.OperationInspectionHash || digestHex(HashBytes(readback)) != e.ReadbackHash {
		return ContentReconciliationEvidence{}, domainError("content_evidence_integrity_failed", "content reconciliation evidence integrity verification failed")
	}
	if e.ProviderObservedAt, err = time.Parse(time.RFC3339Nano, observed); err != nil {
		return ContentReconciliationEvidence{}, domainError("content_evidence_integrity_failed", "content reconciliation evidence time is invalid")
	}
	if e.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return ContentReconciliationEvidence{}, domainError("content_evidence_integrity_failed", "content reconciliation evidence time is invalid")
	}
	if e.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires); err != nil || !e.ExpiresAt.After(now.UTC()) {
		return ContentReconciliationEvidence{}, domainError("not_found_or_forbidden", "content reconciliation evidence is unavailable")
	}
	e.CacheBypassed = cache == 1
	e.OperationInspectionJSON = append(json.RawMessage(nil), inspect...)
	e.ReadbackJSON = append(json.RawMessage(nil), readback...)
	return e, nil
}

// FinalizeContentReconciliationReadback atomically stores one immutable
// provider observation, updates the uncertainty episode with a row-version
// CAS, links metadata-only rich audit, and seals the caller's replay envelope.
func (s *Store) FinalizeContentReconciliationReadback(ctx context.Context, reservation ReservationResult, requestDigest Digest, expectedVersion int64, evidence ContentReconciliationEvidence, result ContentReconciliationResult, recoveryLifetime time.Duration, retainUntil time.Time, policy ContentIntakePolicySnapshot, auditID string, now time.Time) (ContentReconciliationResult, error) {
	p, err := requireContentReconcilePrincipal(ctx)
	if err != nil {
		return ContentReconciliationResult{}, err
	}
	if s == nil || s.db == nil {
		return ContentReconciliationResult{}, ErrNotReady
	}
	if reservation.Disposition != ReservationOwned || reservation.State != ReservationStateReserved || reservation.RecordID == "" || reservation.OwnerNonce == "" || requestDigest == (Digest{}) || expectedVersion < 1 || recoveryLifetime <= 0 || auditID == "" {
		return ContentReconciliationResult{}, domainError("invalid_request", "content reconciliation reservation is incomplete")
	}
	if err := validateReconciliationReadback(evidence, p, result); err != nil {
		return ContentReconciliationResult{}, err
	}
	if !result.ReconciliationRequired || result.ResubmissionAllowed || result.State != "still_unknown" || result.Kind != "reconcile" || len(result.EvidenceIDs) != 1 || result.EvidenceIDs[0] != evidence.EvidenceID {
		return ContentReconciliationResult{}, domainError("invalid_request", "read-back result is not a conservative reconciliation response")
	}
	now = now.UTC()
	retainUntil = retainUntil.UTC()
	evidence.CreatedAt, evidence.ProviderObservedAt = evidence.CreatedAt.UTC(), evidence.ProviderObservedAt.UTC()
	evidence.ExpiresAt = now.Add(recoveryLifetime)
	if evidence.ExpiresAt.After(retainUntil) {
		evidence.ExpiresAt = retainUntil
	}
	if !evidence.ExpiresAt.After(now) {
		return ContentReconciliationResult{}, domainError("content_policy_invalid", "signed recovery horizon has expired")
	}
	envelope, err := CanonicalJSON(ContentReconciliationEnvelope{Result: result})
	if err != nil {
		return ContentReconciliationResult{}, err
	}
	metadata, err := CanonicalJSON(map[string]any{"placement_intent_id": evidence.PlacementIntentID, "episode_id": evidence.UncertaintyEpisodeID, "evidence_id": evidence.EvidenceID, "operation_reference_sha256": evidence.OperationReferenceHash, "operation_inspection_sha256": evidence.OperationInspectionHash, "readback_sha256": evidence.ReadbackHash, "target_hash": evidence.TargetHash, "intended_hash": evidence.IntendedHash})
	if err != nil {
		return ContentReconciliationResult{}, err
	}
	if err := s.RequireRichAudit(ctx); err != nil {
		return ContentReconciliationResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ContentReconciliationResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	correlationID, err := verifyContentReconciliationReservation(ctx, tx, p, reservation, requestDigest, "read_back", retainUntil, now)
	if err != nil {
		return ContentReconciliationResult{}, err
	}
	if err := verifyActiveContentBundleTx(ctx, tx, p.TenantID, policy); err != nil {
		return ContentReconciliationResult{}, err
	}
	var state, episode, operationRef, targetHash, previewRaw, leaseExpiry string
	var rowVersion int64
	err = tx.QueryRowContext(ctx, `SELECT state,uncertainty_episode_id,operation_reference,target_hash,preview_json,row_version,lease_expires_at FROM assurance_content_placement_intents WHERE tenant_id=? AND actor_id=? AND placement_intent_id=?`, p.TenantID, p.AuditActor(), evidence.PlacementIntentID).Scan(&state, &episode, &operationRef, &targetHash, &previewRaw, &rowVersion, &leaseExpiry)
	if err != nil || state != "reconciliation_required" || rowVersion != expectedVersion || episode != "" && episode != evidence.UncertaintyEpisodeID || targetHash != evidence.TargetHash || digestHex(HashBytes([]byte(operationRef))) != evidence.OperationReferenceHash {
		return ContentReconciliationResult{}, domainError("content_reconciliation_conflict", "content intent changed or is not frozen for reconciliation")
	}
	canonicalInspect, e1 := CanonicalJSONBytes(evidence.OperationInspectionJSON)
	canonicalReadback, e2 := CanonicalJSONBytes(evidence.ReadbackJSON)
	if e1 != nil || e2 != nil || !bytes.Equal(canonicalInspect, evidence.OperationInspectionJSON) || !bytes.Equal(canonicalReadback, evidence.ReadbackJSON) || digestHex(HashBytes(canonicalInspect)) != evidence.OperationInspectionHash || digestHex(HashBytes(canonicalReadback)) != evidence.ReadbackHash || evidence.Kind != "read_back" || !evidence.CacheBypassed || evidence.ActorID != p.AuditActor() || evidence.PlacementIntentID == "" || evidence.UncertaintyEpisodeID == "" || !uuidIsCanonical(evidence.EvidenceID) || !uuidIsCanonical(evidence.UncertaintyEpisodeID) || evidence.ProviderObservedAt.After(now) {
		return ContentReconciliationResult{}, domainError("content_evidence_integrity_failed", "content reconciliation observation is invalid")
	}
	var preview struct {
		IntendedValue json.RawMessage `json:"intended_value"`
	}
	if json.Unmarshal([]byte(previewRaw), &preview) != nil || digestHex(HashBytes(preview.IntendedValue)) != evidence.IntendedHash {
		return ContentReconciliationResult{}, domainError("content_evidence_integrity_failed", "read-back intended binding does not match frozen preview")
	}
	if episode == "" {
		updated, e := tx.ExecContext(ctx, `UPDATE assurance_content_placement_intents SET uncertainty_episode_id=?,reconciliation_classification='still_unknown',reconciliation_updated_at=?,row_version=row_version+1 WHERE tenant_id=? AND placement_intent_id=? AND state='reconciliation_required' AND row_version=? AND uncertainty_episode_id=''`, evidence.UncertaintyEpisodeID, formatTimestamp(now), p.TenantID, evidence.PlacementIntentID, expectedVersion)
		if e != nil {
			return ContentReconciliationResult{}, e
		}
		if e = requireOneTransition(updated); e != nil {
			return ContentReconciliationResult{}, domainError("content_reconciliation_conflict", "uncertainty episode compare-and-swap failed")
		}
	} else {
		updated, e := tx.ExecContext(ctx, `UPDATE assurance_content_placement_intents SET reconciliation_classification='still_unknown',reconciliation_updated_at=?,row_version=row_version+1 WHERE tenant_id=? AND placement_intent_id=? AND state='reconciliation_required' AND row_version=? AND uncertainty_episode_id=?`, formatTimestamp(now), p.TenantID, evidence.PlacementIntentID, expectedVersion, evidence.UncertaintyEpisodeID)
		if e != nil {
			return ContentReconciliationResult{}, e
		}
		if e = requireOneTransition(updated); e != nil {
			return ContentReconciliationResult{}, domainError("content_reconciliation_conflict", "content reconciliation compare-and-swap failed")
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO assurance_content_reconciliation_evidence(tenant_id,evidence_id,intent_id,actor_id,episode_id,kind,operation_reference_hash,status,operation_inspection_json,operation_inspection_hash,target_hash,intended_hash,readback_json,readback_hash,cache_bypassed,provider_observed_at,created_at,expires_at) VALUES(?,?,?,?,?,'read_back',?,?,?,?,?,?,?,?,1,?,?,?)`, p.TenantID, evidence.EvidenceID, evidence.PlacementIntentID, p.AuditActor(), evidence.UncertaintyEpisodeID, evidence.OperationReferenceHash, evidence.OperationStatus, string(canonicalInspect), evidence.OperationInspectionHash, evidence.TargetHash, evidence.IntendedHash, string(canonicalReadback), evidence.ReadbackHash, formatTimestamp(evidence.ProviderObservedAt), formatTimestamp(now), formatTimestamp(evidence.ExpiresAt)); err != nil {
		return ContentReconciliationResult{}, fmt.Errorf("assurance: persist content reconciliation evidence: %w", err)
	}
	if err = s.appendContentReconciliationAudit(ctx, tx, p, auditID, evidence.PlacementIntentID, metadata, correlationID, now); err != nil {
		return ContentReconciliationResult{}, err
	}
	if err = insertContentAuditLink(ctx, tx, p.TenantID, "content_reconciliation_evidence", evidence.EvidenceID, auditID, correlationID, now); err != nil {
		return ContentReconciliationResult{}, err
	}
	if err = sealContentReconciliationReservation(ctx, tx, p, reservation, requestDigest, "read_back", envelope, evidence.PlacementIntentID, auditID, now); err != nil {
		return ContentReconciliationResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return ContentReconciliationResult{}, err
	}
	result.ResultSHA256 = digestHex(HashBytes(envelope))
	return result, nil
}

func validateReconciliationReadback(e ContentReconciliationEvidence, p identity.Principal, r ContentReconciliationResult) error {
	if e.EvidenceID == "" || e.PlacementIntentID == "" || e.ActorID != p.AuditActor() || e.Kind != "read_back" || !e.CacheBypassed || !contentPolicyHash.MatchString(e.OperationReferenceHash) || !contentPolicyHash.MatchString(e.OperationInspectionHash) || !contentPolicyHash.MatchString(e.TargetHash) || !contentPolicyHash.MatchString(e.IntendedHash) || !contentPolicyHash.MatchString(e.ReadbackHash) || len(e.OperationInspectionJSON) < 2 || len(e.ReadbackJSON) < 2 || len(e.OperationInspectionJSON) > 1<<20 || len(e.ReadbackJSON) > 1<<20 {
		return domainError("content_evidence_integrity_failed", "content reconciliation observation is invalid")
	}
	return nil
}

func (s *Store) FinalizeContentReconciliationClassification(ctx context.Context, reservation ReservationResult, requestDigest Digest, expectedVersion int64, intentID, episodeID, classification, evidenceID, targetHash, intendedHash, terminalFenceDigest string, result ContentReconciliationResult, retainUntil time.Time, policy ContentIntakePolicySnapshot, auditID string, now time.Time) (ContentReconciliationResult, error) {
	p, err := requireContentReconcilePrincipal(ctx)
	if err != nil {
		return ContentReconciliationResult{}, err
	}
	if s == nil || s.db == nil {
		return ContentReconciliationResult{}, ErrNotReady
	}
	if classification == "confirmed_not_applied" {
		return ContentReconciliationResult{}, domainError("capability_unverified", "provider no-effect evidence is unavailable; intent remains reconciliation_required")
	}
	if classification != "confirmed_applied" && classification != "still_unknown" && classification != "provider_evidence_inconsistent" {
		return ContentReconciliationResult{}, domainError("invalid_request", "content reconciliation classification is invalid")
	}
	if !uuidIsCanonical(intentID) || !uuidIsCanonical(episodeID) || expectedVersion < 1 || reservation.Disposition != ReservationOwned || reservation.State != ReservationStateReserved || requestDigest == (Digest{}) || auditID == "" || result.ResubmissionAllowed {
		return ContentReconciliationResult{}, domainError("invalid_request", "content reconciliation classification request is incomplete")
	}
	if classification == "confirmed_applied" && (!uuidIsCanonical(evidenceID) || !contentPolicyHash.MatchString(terminalFenceDigest) || !result.ReconciliationRequired || result.State != "confirmed_applied") {
		return ContentReconciliationResult{}, domainError("content_evidence_integrity_failed", "confirmed applied requires read-back evidence and accepted terminal proof")
	}
	if classification != "confirmed_applied" && (evidenceID != "" || terminalFenceDigest != "") {
		return ContentReconciliationResult{}, domainError("invalid_request", "unknown classifications cannot bind applied evidence")
	}
	now = now.UTC()
	retainUntil = retainUntil.UTC()
	metadata, err := CanonicalJSON(map[string]any{"placement_intent_id": intentID, "episode_id": episodeID, "classification": classification, "evidence_id": evidenceID, "target_hash": targetHash, "intended_hash": intendedHash, "terminal_fence_digest": terminalFenceDigest})
	if err != nil {
		return ContentReconciliationResult{}, err
	}
	if err = s.RequireRichAudit(ctx); err != nil {
		return ContentReconciliationResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ContentReconciliationResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	correlationID, err := verifyContentReconciliationReservation(ctx, tx, p, reservation, requestDigest, "classify", retainUntil, now)
	if err != nil {
		return ContentReconciliationResult{}, err
	}
	if err = verifyActiveContentBundleTx(ctx, tx, p.TenantID, policy); err != nil {
		return ContentReconciliationResult{}, err
	}
	var state, episode, currentTarget, previewRaw, previewHash, knownOperation, placementID, claimedAt string
	var version int64
	err = tx.QueryRowContext(ctx, `SELECT state,uncertainty_episode_id,target_hash,preview_json,row_version,operation_reference,placement_intent_id,preview_sha256,claimed_at FROM assurance_content_placement_intents WHERE tenant_id=? AND actor_id=? AND placement_intent_id=?`, p.TenantID, p.AuditActor(), intentID).Scan(&state, &episode, &currentTarget, &previewRaw, &version, &knownOperation, &placementID, &previewHash, &claimedAt)
	if err != nil || state != "reconciliation_required" || episode != episodeID || version != expectedVersion || currentTarget != targetHash {
		return ContentReconciliationResult{}, domainError("content_reconciliation_conflict", "uncertainty episode or intent changed")
	}
	var machineReadbackHash string
	var machineReadbackJSON []byte
	var machineReadbackAt time.Time
	if classification == "confirmed_applied" {
		var ev ContentReconciliationEvidence
		var opJSON, readJSON []byte
		var cache int
		var observed, created, expires string
		err = tx.QueryRowContext(ctx, `SELECT evidence_id,intent_id,actor_id,episode_id,kind,operation_reference_hash,status,operation_inspection_json,operation_inspection_hash,target_hash,intended_hash,readback_json,readback_hash,cache_bypassed,provider_observed_at,created_at,expires_at FROM assurance_content_reconciliation_evidence WHERE tenant_id=? AND evidence_id=? AND intent_id=? AND actor_id=? AND episode_id=? AND expires_at>?`, p.TenantID, evidenceID, intentID, p.AuditActor(), episodeID, formatTimestamp(now)).Scan(&ev.EvidenceID, &ev.PlacementIntentID, &ev.ActorID, &ev.UncertaintyEpisodeID, &ev.Kind, &ev.OperationReferenceHash, &ev.OperationStatus, &opJSON, &ev.OperationInspectionHash, &ev.TargetHash, &ev.IntendedHash, &readJSON, &ev.ReadbackHash, &cache, &observed, &created, &expires)
		var latest string
		latestErr := tx.QueryRowContext(ctx, `SELECT evidence_id FROM assurance_content_reconciliation_evidence WHERE tenant_id=? AND intent_id=? AND actor_id=? AND episode_id=? AND expires_at>? ORDER BY rowid DESC LIMIT 1`, p.TenantID, intentID, p.AuditActor(), episodeID, formatTimestamp(now)).Scan(&latest)
		preview, previewErr := decodeAcknowledgementPreview([]byte(previewRaw), previewHash, placementID, p.AuditActor())
		frozenTargetHash := ""
		frozenIntendedHash := ""
		if previewErr == nil {
			frozenTargetHash, previewErr = contentPlacementTargetHash(preview.Destination.ResourceID, preview.Destination.SheetID, preview.Destination.Cell)
			frozenIntendedHash = digestHex(HashBytes(preview.IntendedValue))
		}
		observedAt, observedErr := time.Parse(time.RFC3339Nano, observed)
		expiresAt, expiresErr := time.Parse(time.RFC3339Nano, expires)
		claimedTime, claimedErr := time.Parse(time.RFC3339Nano, claimedAt)
		if err != nil || latestErr != nil || latest != evidenceID || ev.EvidenceID != evidenceID || ev.PlacementIntentID != intentID || ev.ActorID != p.AuditActor() || ev.UncertaintyEpisodeID != episodeID || ev.Kind != "read_back" || ev.OperationStatus != "completed" || cache != 1 || ev.TargetHash != targetHash || frozenTargetHash != targetHash || ev.IntendedHash != intendedHash || frozenIntendedHash != intendedHash || digestHex(HashBytes(opJSON)) != ev.OperationInspectionHash || digestHex(HashBytes(readJSON)) != ev.ReadbackHash || !contentPolicyHash.MatchString(ev.OperationReferenceHash) || !contentPolicyHash.MatchString(ev.ReadbackHash) || !contentPolicyHash.MatchString(ev.OperationInspectionHash) || !contentPolicyHash.MatchString(ev.TargetHash) || !contentPolicyHash.MatchString(ev.IntendedHash) || previewErr != nil || expiresErr != nil || !expiresAt.After(now) || observedErr != nil || observedAt.IsZero() || claimedErr != nil || knownOperation == "" || ev.OperationReferenceHash != digestHex(HashBytes([]byte(knownOperation))) {
			return ContentReconciliationResult{}, domainError("content_evidence_inconsistent", "latest uncached applied evidence is missing or mismatched")
		}
		ev.OperationInspectionJSON = append(json.RawMessage(nil), opJSON...)
		ev.ReadbackJSON = append(json.RawMessage(nil), readJSON...)
		ev.CacheBypassed = cache == 1
		ev.ProviderObservedAt, ev.ExpiresAt = observedAt, expiresAt
		if err := verifyAppliedContentReconciliationEvidence(ev, preview, knownOperation, targetHash, intendedHash, claimedTime, now); err != nil {
			return ContentReconciliationResult{}, domainError("content_evidence_inconsistent", "latest uncached applied evidence does not prove the frozen value and target metadata")
		}
		machineReadbackHash = ev.ReadbackHash
		machineReadbackJSON = append([]byte(nil), ev.ReadbackJSON...)
		machineReadbackAt = ev.ProviderObservedAt
		if !contentPolicyHash.MatchString(terminalFenceDigest) {
			return ContentReconciliationResult{}, domainError("content_evidence_inconsistent", "accepted terminal fence proof is invalid")
		}
	}
	nextState := state
	if classification == "confirmed_applied" {
		nextState = "machine_verified_visual_ack_pending"
	}
	update, err := tx.ExecContext(ctx, `UPDATE assurance_content_placement_intents SET state=?,reconciliation_classification=?,reconciliation_updated_at=?,terminal_fence_digest=CASE WHEN ?='confirmed_applied' THEN ? ELSE terminal_fence_digest END,terminal_at=CASE WHEN ?='confirmed_applied' THEN ? ELSE terminal_at END,machine_result_record_id=CASE WHEN ?='confirmed_applied' THEN ? ELSE machine_result_record_id END,readback_json=CASE WHEN ?='confirmed_applied' THEN ? ELSE readback_json END,readback_sha256=CASE WHEN ?='confirmed_applied' THEN ? ELSE readback_sha256 END,readback_at=CASE WHEN ?='confirmed_applied' THEN ? ELSE readback_at END,row_version=row_version+1 WHERE tenant_id=? AND placement_intent_id=? AND state='reconciliation_required' AND row_version=? AND uncertainty_episode_id=?`, nextState, classification, formatTimestamp(now), classification, terminalFenceDigest, classification, formatTimestamp(now), classification, reservation.RecordID, classification, string(machineReadbackJSON), classification, machineReadbackHash, classification, formatTimestamp(machineReadbackAt), p.TenantID, intentID, expectedVersion, episodeID)
	if err != nil {
		return ContentReconciliationResult{}, err
	}
	if err = requireOneTransition(update); err != nil {
		return ContentReconciliationResult{}, domainError("content_reconciliation_conflict", "content reconciliation classification compare-and-swap failed")
	}
	result.Kind = "reconcile"
	result.PlacementIntentID = intentID
	result.State = classification
	result.UncertaintyEpisodeID = episodeID
	result.ResubmissionAllowed = false
	result.ReconciliationRequired = classification != "confirmed_applied"
	sealedEnvelope := ContentReconciliationEnvelope{Result: result}
	if classification == "confirmed_applied" {
		sealedEnvelope.MachineProof = &ContentReconciliationMachineProof{PlacementIntentID: intentID, State: nextState, ReadbackSHA256: machineReadbackHash, OperationReference: knownOperation, TargetHash: targetHash, TerminalFenceDigest: terminalFenceDigest, RowVersion: expectedVersion + 1}
	}
	envelope, err := CanonicalJSON(sealedEnvelope)
	if err != nil {
		return ContentReconciliationResult{}, err
	}
	if err = s.appendContentReconciliationAudit(ctx, tx, p, auditID, intentID, metadata, correlationID, now); err != nil {
		return ContentReconciliationResult{}, err
	}
	if err = sealContentReconciliationReservation(ctx, tx, p, reservation, requestDigest, "classify", envelope, intentID, auditID, now); err != nil {
		return ContentReconciliationResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return ContentReconciliationResult{}, err
	}
	result.ResultSHA256 = digestHex(HashBytes(envelope))
	return result, nil
}

func verifyAppliedContentReconciliationEvidence(e ContentReconciliationEvidence, preview acknowledgementPreview, operationReference, targetHash, intendedHash string, claimedAt, now time.Time) error {
	if e.Kind != "read_back" || !e.CacheBypassed || operationReference == "" || e.OperationStatus != "completed" || e.OperationReferenceHash != digestHex(HashBytes([]byte(operationReference))) || e.TargetHash != targetHash || e.IntendedHash != intendedHash || !e.ExpiresAt.After(now) || !e.ProviderObservedAt.After(claimedAt) {
		return fmt.Errorf("evidence binding, freshness, or operation status mismatch")
	}
	inspectionCanonical, err := CanonicalJSONBytes(e.OperationInspectionJSON)
	if err != nil || !bytes.Equal(inspectionCanonical, e.OperationInspectionJSON) || digestHex(HashBytes(inspectionCanonical)) != e.OperationInspectionHash {
		return fmt.Errorf("operation inspection digest mismatch")
	}
	var inspection struct {
		Reference string `json:"Reference"`
		Status    string `json:"Status"`
	}
	if json.Unmarshal(inspectionCanonical, &inspection) != nil || inspection.Reference != operationReference || inspection.Status != "completed" || inspection.Status != e.OperationStatus {
		return fmt.Errorf("operation inspection does not prove the known completed operation")
	}
	readbackCanonical, err := CanonicalJSONBytes(e.ReadbackJSON)
	if err != nil || !bytes.Equal(readbackCanonical, e.ReadbackJSON) || digestHex(HashBytes(readbackCanonical)) != e.ReadbackHash {
		return fmt.Errorf("readback digest mismatch")
	}
	frozenTarget, err := contentPlacementTargetHash(preview.Destination.ResourceID, preview.Destination.SheetID, preview.Destination.Cell)
	if err != nil || frozenTarget != targetHash || digestHex(HashBytes(preview.IntendedValue)) != intendedHash {
		return fmt.Errorf("frozen target or intended value hash mismatch")
	}
	if err := verifyAcknowledgementReadback(readbackCanonical, e.ReadbackHash, preview); err != nil {
		return err
	}
	return nil
}

func (s *Store) appendContentReconciliationAudit(ctx context.Context, tx *sql.Tx, p identity.Principal, auditID, intentID string, metadata []byte, correlation string, now time.Time) error {
	if !validContentAuditID(auditID) {
		return domainError("invalid_request", "content reconciliation audit ID is invalid")
	}
	if _, err := s.auditLog.AppendTx(ctx, tx, audit.Entry{Ts: now, Actor: p.AuditActor(), Tool: contentPlacementTool, Action: contentReconcileAction, Target: intentID, AfterJSON: string(metadata), AuditID: auditID}); err != nil {
		return domainError("audit_unavailable", "content reconciliation rich audit append failed")
	}
	if err := insertContentAuditLink(ctx, tx, p.TenantID, "content_placement_intent", intentID, auditID, correlation, now); err != nil {
		return err
	}
	return nil
}

func verifyContentReconciliationReservation(ctx context.Context, tx *sql.Tx, p identity.Principal, reservation ReservationResult, digest Digest, action string, retainUntil time.Time, now time.Time) (string, error) {
	var actor, tool, storedAction, requestHash, owner, leaseExpiry, retention, correlation, state string
	err := tx.QueryRowContext(ctx, `SELECT actor_id,tool,action,request_digest,owner_nonce,lease_expires_at,expires_at,correlation_id,state FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=?`, p.TenantID, reservation.RecordID).Scan(&actor, &tool, &storedAction, &requestHash, &owner, &leaseExpiry, &retention, &correlation, &state)
	if err != nil || actor != p.AuditActor() || tool != contentPlacementTool || storedAction != action || requestHash != digestHex(digest) || subtle.ConstantTimeCompare([]byte(owner), []byte(reservation.OwnerNonce)) != 1 || state != "reserved" {
		return "", domainError("idempotency_conflict", "content reconciliation reservation binding does not match")
	}
	lease, e1 := time.Parse(time.RFC3339Nano, leaseExpiry)
	retained, e2 := time.Parse(time.RFC3339Nano, retention)
	if e1 != nil || !lease.After(now) || e2 != nil || !retained.Equal(retainUntil) {
		return "", domainError("idempotency_in_progress", "content reconciliation reservation is expired or has a different signed horizon")
	}
	return correlation, nil
}

func sealContentReconciliationReservation(ctx context.Context, tx *sql.Tx, p identity.Principal, reservation ReservationResult, digest Digest, action string, envelope []byte, intentID, auditID string, now time.Time) error {
	if err := insertContentAuditLink(ctx, tx, p.TenantID, "idempotency_record", reservation.RecordID, auditID, reservation.CorrelationID, now); err != nil {
		return err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET state='sealed',response_envelope=?,response_hash=?,response_status='ok',entity_reference=?,audit_id=?,terminal_at=?,lease_expires_at=? WHERE tenant_id=? AND record_id=? AND actor_id=? AND tool=? AND action=? AND request_digest=? AND state='reserved' AND owner_nonce=? AND lease_expires_at>?`, string(envelope), digestHex(HashBytes(envelope)), intentID, auditID, formatTimestamp(now), formatTimestamp(now), p.TenantID, reservation.RecordID, p.AuditActor(), contentPlacementTool, action, digestHex(digest), reservation.OwnerNonce, formatTimestamp(now))
	if err != nil {
		return err
	}
	return requireOneTransition(updated)
}

func requireContentReconcilePrincipal(ctx context.Context) (identity.Principal, error) {
	p, ok := identity.PrincipalFromContext(ctx)
	if !ok || p.TokenType != identity.TokenTypeDelegated || p.UsedSubjectFallback || !p.HasPermission(identity.PermissionContentReconcile) || !uuidIsCanonical(p.TenantID) || !uuidIsCanonical(p.ObjectID) {
		return identity.Principal{}, domainError("forbidden", "trusted delegated content.reconcile capability required")
	}
	return p, nil
}

func oneOfContentIntentState(v string) bool {
	switch v {
	case "staged", "claimed", "submitting", "reconciliation_required", "machine_verified_visual_ack_pending", "visually_acknowledged", "reconciled_not_applied":
		return true
	}
	return false
}
