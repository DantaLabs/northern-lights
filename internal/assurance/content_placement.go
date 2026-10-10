package assurance

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
)

const contentStagePreviewMaxBytes = 1_048_576

type ContentStageIntent struct {
	PlacementIntentID string
	PreviewJSON       []byte
	PreviewSHA256     string
	TokenDigest       string
	ExpiresAt         time.Time
	CreatedAt         time.Time
}

// ContentStageResult is always token-free. The service that created the raw
// one-time confirmation token may add it to the initial response in memory.
type ContentStageResult struct {
	Kind                  string          `json:"kind"`
	Preview               json.RawMessage `json:"preview"`
	ReplayRequiresRestage bool            `json:"replay_requires_restage"`
}

type contentStagePreviewBinding struct {
	PlacementIntentID string `json:"placement_intent_id"`
	ContentHash       string `json:"content_hash"`
	ActorBindingID    string `json:"actor_binding_id"`
	CreatedAt         string `json:"created_at"`
	ExpiresAt         string `json:"expires_at"`
	ConfirmationToken string `json:"confirmation_token,omitempty"`
}

type contentStageAuditMetadata struct {
	PlacementIntentID string `json:"placement_intent_id"`
	PreviewSHA256     string `json:"preview_sha256"`
	ContentHash       string `json:"content_hash"`
}

// FinalizeContentStage atomically inserts the immutable preview and token
// digest, appends metadata-only rich audit, links both records, and seals the
// owned stage reservation. Caller must authorize, classify replay, resolve
// policy/candidate, and obtain an uncached provider observation before calling.
func (s *Store) FinalizeContentStage(ctx context.Context, reservation ReservationResult, requestDigest Digest, intent ContentStageIntent, auditID string, now time.Time) (ContentStageResult, error) {
	principal, err := requireContentStagePrincipal(ctx)
	if err != nil {
		return ContentStageResult{}, err
	}
	now = now.UTC()
	intent.CreatedAt = intent.CreatedAt.UTC()
	intent.ExpiresAt = intent.ExpiresAt.UTC()
	if s == nil || s.db == nil {
		return ContentStageResult{}, ErrNotReady
	}
	if reservation.Disposition != ReservationOwned || reservation.State != ReservationStateReserved || reservation.RecordID == "" || reservation.OwnerNonce == "" {
		return ContentStageResult{}, domainError("idempotency_in_progress", "content stage reservation is not owned")
	}
	if requestDigest == (Digest{}) || auditID == "" || intent.PlacementIntentID == "" || !uuidIsCanonical(intent.PlacementIntentID) {
		return ContentStageResult{}, domainError("invalid_request", "content stage intent, request digest, and audit ID are required")
	}
	if len(intent.PreviewJSON) < 2 || len(intent.PreviewJSON) > contentStagePreviewMaxBytes {
		return ContentStageResult{}, domainError("invalid_request", "content stage preview is empty or exceeds the byte limit")
	}
	canonical, err := CanonicalJSONBytes(intent.PreviewJSON)
	if err != nil || !bytes.Equal(canonical, intent.PreviewJSON) || digestHex(HashBytes(intent.PreviewJSON)) != intent.PreviewSHA256 {
		return ContentStageResult{}, domainError("artifact_hash_mismatch", "content stage preview is not canonical or its digest does not match")
	}
	if len(intent.TokenDigest) != 64 || strings.ToLower(intent.TokenDigest) != intent.TokenDigest {
		return ContentStageResult{}, domainError("invalid_request", "content stage confirmation token digest is invalid")
	}
	if _, err := hex.DecodeString(intent.TokenDigest); err != nil {
		return ContentStageResult{}, domainError("invalid_request", "content stage confirmation token digest is invalid")
	}
	var binding contentStagePreviewBinding
	if err := json.Unmarshal(canonical, &binding); err != nil || binding.PlacementIntentID != intent.PlacementIntentID || binding.ContentHash == "" || binding.ActorBindingID != principal.AuditActor() || binding.ConfirmationToken != "" {
		return ContentStageResult{}, domainError("invalid_request", "content stage preview binding is invalid")
	}
	createdInPreview, createdErr := time.Parse(time.RFC3339Nano, binding.CreatedAt)
	expiresInPreview, expiresErr := time.Parse(time.RFC3339Nano, binding.ExpiresAt)
	if createdErr != nil || expiresErr != nil || !createdInPreview.Equal(intent.CreatedAt) || !expiresInPreview.Equal(intent.ExpiresAt) || intent.CreatedAt.After(now) || !intent.ExpiresAt.After(now) || intent.ExpiresAt.Sub(intent.CreatedAt) > 900*time.Second {
		return ContentStageResult{}, domainError("policy_blocked", "content stage preview lifetime or timestamps are invalid")
	}
	if err := s.RequireRichAudit(ctx); err != nil {
		return ContentStageResult{}, err
	}
	result := ContentStageResult{Kind: "stage", Preview: append(json.RawMessage(nil), canonical...), ReplayRequiresRestage: false}
	envelope, err := CanonicalJSON(result)
	if err != nil {
		return ContentStageResult{}, err
	}
	metadata, err := CanonicalJSON(contentStageAuditMetadata{PlacementIntentID: intent.PlacementIntentID, PreviewSHA256: intent.PreviewSHA256, ContentHash: binding.ContentHash})
	if err != nil {
		return ContentStageResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ContentStageResult{}, fmt.Errorf("assurance: begin content stage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var actor, tool, action, storedRequestDigest, ownerNonce, leaseExpires, correlationID, state string
	if err := tx.QueryRowContext(ctx, `SELECT actor_id,tool,action,request_digest,owner_nonce,lease_expires_at,correlation_id,state
	 FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=?`, principal.TenantID, reservation.RecordID).Scan(
		&actor, &tool, &action, &storedRequestDigest, &ownerNonce, &leaseExpires, &correlationID, &state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ContentStageResult{}, domainError("idempotency_conflict", "content stage reservation was not found")
		}
		return ContentStageResult{}, fmt.Errorf("assurance: read content stage reservation: %w", err)
	}
	if actor != principal.AuditActor() || tool != "workiva_content_placement" || action != "stage" || storedRequestDigest != digestHex(requestDigest) || subtle.ConstantTimeCompare([]byte(ownerNonce), []byte(reservation.OwnerNonce)) != 1 {
		return ContentStageResult{}, domainError("idempotency_conflict", "content stage reservation binding does not match")
	}
	leaseUntil, err := time.Parse(time.RFC3339Nano, leaseExpires)
	if err != nil || !leaseUntil.After(now) || state != string(ReservationStateReserved) {
		return ContentStageResult{}, domainError("idempotency_in_progress", "content stage reservation is no longer active")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_content_placement_intents
	 (tenant_id,placement_intent_id,actor_id,preview_json,preview_sha256,token_digest,state,expires_at,created_at)
		 VALUES(?,?,?,?,?,?,'staged',?,?)`, principal.TenantID, intent.PlacementIntentID, principal.AuditActor(), string(canonical), intent.PreviewSHA256, intent.TokenDigest, formatTimestamp(intent.ExpiresAt), formatTimestamp(intent.CreatedAt)); err != nil {
		return ContentStageResult{}, fmt.Errorf("assurance: insert immutable content stage: %w", err)
	}
	if _, err := s.auditLog.AppendTx(ctx, tx, audit.Entry{
		Ts: now, Actor: principal.AuditActor(), Tool: tool, Action: action,
		Target: intent.PlacementIntentID, AfterJSON: string(metadata), AuditID: auditID,
	}); err != nil {
		return ContentStageResult{}, domainError("audit_unavailable", "content stage rich audit append failed")
	}
	if err := insertContentAuditLink(ctx, tx, principal.TenantID, "content_placement_intent", intent.PlacementIntentID, auditID, correlationID, now); err != nil {
		return ContentStageResult{}, fmt.Errorf("assurance: link content stage audit: %w", err)
	}
	if err := insertContentAuditLink(ctx, tx, principal.TenantID, "idempotency_record", reservation.RecordID, auditID, correlationID, now); err != nil {
		return ContentStageResult{}, fmt.Errorf("assurance: link content stage reservation audit: %w", err)
	}
	updated, err := tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET state='sealed',response_envelope=?,response_hash=?,response_status='ok',entity_reference=?,audit_id=?,terminal_at=?,lease_expires_at=?
	 WHERE tenant_id=? AND record_id=? AND actor_id=? AND tool=? AND action=? AND request_digest=? AND state='reserved' AND owner_nonce=? AND lease_expires_at>?`,
		string(envelope), digestHex(HashBytes(envelope)), intent.PlacementIntentID, auditID, formatTimestamp(now), formatTimestamp(now),
		principal.TenantID, reservation.RecordID, principal.AuditActor(), tool, action, digestHex(requestDigest), reservation.OwnerNonce, formatTimestamp(now))
	if err != nil {
		return ContentStageResult{}, fmt.Errorf("assurance: seal content stage reservation: %w", err)
	}
	if err := requireOneTransition(updated); err != nil {
		return ContentStageResult{}, domainError("idempotency_conflict", "content stage reservation compare-and-swap failed")
	}
	if err := tx.Commit(); err != nil {
		return ContentStageResult{}, fmt.Errorf("assurance: commit content stage: %w", err)
	}
	return result, nil
}

func requireContentStagePrincipal(ctx context.Context) (identity.Principal, error) {
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok || principal.TokenType != identity.TokenTypeDelegated || principal.UsedSubjectFallback || !principal.HasPermission(identity.PermissionContentStage) || !uuidIsCanonical(principal.TenantID) || !uuidIsCanonical(principal.ObjectID) {
		return identity.Principal{}, domainError("forbidden", "trusted delegated content.stage capability required")
	}
	return principal, nil
}
