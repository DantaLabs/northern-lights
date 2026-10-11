package assurance

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/google/uuid"
)

const contentPlacementTool = "workiva_content_placement"

var regexpCell = regexp.MustCompile(`^[A-Z]{1,3}[1-9][0-9]{0,6}$`)

// ContentPlacementConfirmationRecord is the caller-owned, token-free view of
// one immutable placement intent and its durable confirmation evidence.
type ContentPlacementConfirmationRecord struct {
	PlacementIntentID     string          `json:"placement_intent_id"`
	ActorID               string          `json:"actor_id"`
	State                 string          `json:"state"`
	PreviewJSON           json.RawMessage `json:"preview_json"`
	PreviewSHA256         string          `json:"preview_sha256"`
	TargetHash            string          `json:"target_hash"`
	OperationReference    string          `json:"operation_reference"`
	UnknownReason         string          `json:"unknown_reason"`
	ReadbackJSON          json.RawMessage `json:"readback_json,omitempty"`
	ReadbackSHA256        string          `json:"readback_sha256"`
	ClaimFenceDigest      string          `json:"claim_fence_digest"`
	TerminalFenceDigest   string          `json:"terminal_fence_digest"`
	ConfirmationRecordID  string          `json:"confirmation_record_id"`
	MachineResultRecordID string          `json:"machine_result_record_id"`
	LeaseID               string          `json:"lease_id"`
	OwnerNonce            string          `json:"owner_nonce"`
	ClaimRowVersion       int64           `json:"claim_row_version"`
	RowVersion            int64           `json:"row_version"`
	CreatedAt             time.Time       `json:"created_at"`
	ExpiresAt             time.Time       `json:"expires_at"`
	ClaimedAt             time.Time       `json:"claimed_at"`
	LeaseExpiresAt        time.Time       `json:"lease_expires_at"`
	ReadbackAt            time.Time       `json:"readback_at"`
	TerminalAt            time.Time       `json:"terminal_at"`
}

type ContentPlacementConfirmationClaim struct {
	Reservation ReservationResult
	Record      ContentPlacementConfirmationRecord
	LeaseID     string
	Replay      bool
}

// ContentPlacementConfirmationResult is the immutable, token-free machine
// result consumed by a later visual acknowledgement phase.
type ContentPlacementConfirmationResult struct {
	Kind                string          `json:"kind"`
	Status              string          `json:"status"`
	PlacementIntentID   string          `json:"placement_intent_id"`
	State               string          `json:"state"`
	VisualState         string          `json:"visual_state"`
	NLAuditID           string          `json:"nl_audit_id"`
	ReadbackJSON        json.RawMessage `json:"readback_json"`
	ReadbackSHA256      string          `json:"readback_sha256"`
	OperationReference  string          `json:"operation_reference"`
	TargetHash          string          `json:"target_hash"`
	RowVersion          int64           `json:"row_version"`
	TerminalFenceDigest string          `json:"terminal_fence_digest"`
	// ResultSHA256 is a transport projection of the exact sealed envelope
	// response_hash; it is omitted from its own hash input.
	ResultSHA256 string `json:"result_sha256"`
}

type contentPlacementSealedEnvelope struct {
	Kind                string          `json:"kind"`
	Status              string          `json:"status"`
	PlacementIntentID   string          `json:"placement_intent_id"`
	State               string          `json:"state"`
	VisualState         string          `json:"visual_state"`
	NLAuditID           string          `json:"nl_audit_id"`
	ReadbackJSON        json.RawMessage `json:"readback_json"`
	ReadbackSHA256      string          `json:"readback_sha256"`
	OperationReference  string          `json:"operation_reference"`
	TargetHash          string          `json:"target_hash"`
	RowVersion          int64           `json:"row_version"`
	TerminalFenceDigest string          `json:"terminal_fence_digest"`
}

// ContentPlacementReadback carries the exact provider observation plus the
// independently persisted terminal fence digest. ReadbackJSON must be the
// canonical JSON of the uncached Workiva observation.
type ContentPlacementReadback struct {
	PlacementIntentID   string
	RecordID            string
	OwnerNonce          string
	ExpectedVersion     int64
	Outcome             string
	OperationReference  string
	ReadbackJSON        []byte
	ReadbackSHA256      string
	TerminalFenceDigest string
}

type contentPlacementPreviewBinding struct {
	PlacementIntentID string          `json:"placement_intent_id"`
	ContentHash       string          `json:"content_hash"`
	ActorBindingID    string          `json:"actor_binding_id"`
	IntendedValue     json.RawMessage `json:"intended_value"`
	CreatedAt         time.Time       `json:"created_at"`
	ExpiresAt         time.Time       `json:"expires_at"`
	Destination       struct {
		ID                    string `json:"destination_profile_id"`
		Revision              int    `json:"revision"`
		ContentHash           string `json:"content_hash"`
		ResourceID            string `json:"resource_id"`
		SheetID               string `json:"sheet_id"`
		Cell                  string `json:"cell"`
		IntentLifetimeSeconds int64  `json:"intent_lifetime_seconds"`
	} `json:"destination_profile"`
	PolicyBinding struct {
		ActiveBundleHash    string `json:"active_bundle_hash"`
		ActiveBundleVersion int    `json:"active_bundle_version"`
		AccessPolicyRef     struct {
			PolicyID    string `json:"policy_id"`
			Revision    int    `json:"revision"`
			ContentHash string `json:"content_hash"`
		} `json:"access_policy_ref"`
	} `json:"policy_binding"`
	ProviderMetadata struct {
		FormattingSHA256 string `json:"formatting_sha256"`
	} `json:"provider_metadata"`
}

type contentPlacementReadbackObservation struct {
	SpreadsheetID                  string          `json:"spreadsheet_id"`
	SheetID                        string          `json:"sheet_id"`
	Locator                        string          `json:"locator"`
	RawValue                       json.RawMessage `json:"raw_value"`
	ValuePresent                   bool            `json:"value_present"`
	RawFormats                     json.RawMessage `json:"formats"`
	FormatsPresent                 bool            `json:"formats_present"`
	EffectiveFormats               json.RawMessage `json:"effective_formats"`
	EffectiveFormatsPresent        bool            `json:"effective_formats_present"`
	FormattingSHA256               string          `json:"formatting_sha256"`
	Protection                     string          `json:"protection"`
	Writable                       string          `json:"writable"`
	LiteralWriteFormatPreservation string          `json:"literal_write_format_preservation"`
	LiteralWriteAPIVersion         string          `json:"literal_write_api_version"`
	LiteralWriteEndpoint           string          `json:"literal_write_endpoint"`
	Provenance                     struct {
		Provider   string `json:"provider"`
		APIVersion string `json:"api_version"`
		Endpoint   string `json:"endpoint"`
		QueryRange string `json:"query_range"`
		Cache      string `json:"cache"`
	} `json:"provenance"`
}

// GetOwnedContentPlacementIntent authenticates content.confirm before reading
// private preview bytes. It never returns the stored confirmation-token digest.
func (s *Store) GetOwnedContentPlacementIntent(ctx context.Context, placementIntentID string) (ContentPlacementConfirmationRecord, error) {
	principal, err := requireContentConfirmPrincipal(ctx)
	if err != nil {
		return ContentPlacementConfirmationRecord{}, err
	}
	if s == nil || s.db == nil || !uuidIsCanonical(placementIntentID) {
		return ContentPlacementConfirmationRecord{}, domainError("content_confirmation_unavailable", "content placement intent is unavailable")
	}
	record, err := loadContentPlacementConfirmationRecord(ctx, s.db, principal.TenantID, placementIntentID)
	if err != nil || record.ActorID != principal.AuditActor() {
		return ContentPlacementConfirmationRecord{}, domainError("content_confirmation_unavailable", "content placement intent is unavailable")
	}
	return record, nil
}

// ClaimContentPlacementConfirmation classifies idempotent replay before it
// reads an intent or inspects the one-time token. A fresh token-bound claim,
// reservation, audit evidence, and target lock commit in one SQLite transaction.
func (s *Store) ClaimContentPlacementConfirmation(ctx context.Context, resolver *ContentPolicyResolver, request ReservationRequest, placementIntentID, token, leaseID, auditID string, now time.Time) (ContentPlacementConfirmationClaim, error) {
	principal, err := requireContentConfirmPrincipal(ctx)
	if err != nil {
		return ContentPlacementConfirmationClaim{}, err
	}
	if s == nil || s.db == nil {
		return ContentPlacementConfirmationClaim{}, ErrNotReady
	}
	if request.ActorID != principal.AuditActor() || request.Tool != contentPlacementTool || request.Action != "confirm" || request.IdempotencyDigest == (Digest{}) || request.RequestDigest == (Digest{}) || !uuidIsCanonical(placementIntentID) {
		return ContentPlacementConfirmationClaim{}, domainError("invalid_request", "content confirmation scope is incomplete")
	}
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ContentPlacementConfirmationClaim{}, err
	}
	defer func() { _ = tx.Rollback() }()
	prior, found, err := loadReservationTx(ctx, tx, principal.TenantID, request)
	if err != nil {
		return ContentPlacementConfirmationClaim{}, err
	}
	if found {
		if err := tx.Commit(); err != nil {
			return ContentPlacementConfirmationClaim{}, err
		}
		return ContentPlacementConfirmationClaim{Reservation: prior, Replay: prior.Disposition == ReservationReplay}, nil
	}
	if request.RetainUntil.IsZero() || !uuidIsCanonical(leaseID) || len(token) < 32 || len(token) > 512 || !validContentAuditID(auditID) {
		return ContentPlacementConfirmationClaim{}, domainError("invalid_request", "fresh content confirmation execution fields are invalid")
	}
	if err := tx.Commit(); err != nil {
		return ContentPlacementConfirmationClaim{}, err
	}
	if err := s.Ready(); err != nil {
		return ContentPlacementConfirmationClaim{}, domainError("content_confirmation_unavailable", "assurance store is not ready")
	}
	if err := s.requireRichAudit(); err != nil {
		return ContentPlacementConfirmationClaim{}, err
	}
	record, err := loadContentPlacementConfirmationRecord(ctx, s.db, principal.TenantID, placementIntentID)
	if err != nil || record.ActorID != principal.AuditActor() || record.State != "staged" || !record.ExpiresAt.After(now) {
		return ContentPlacementConfirmationClaim{}, domainError("content_confirmation_unavailable", "content placement intent is unavailable")
	}
	preview, err := validateContentPlacementPreview(record.PreviewJSON, record.PreviewSHA256, placementIntentID, principal.AuditActor(), now, false)
	if err != nil {
		return ContentPlacementConfirmationClaim{}, domainError("content_confirmation_unavailable", "frozen content placement preview is invalid")
	}
	if resolver == nil || resolver.store != s {
		return ContentPlacementConfirmationClaim{}, domainError("content_policy_unavailable", "configured signed content policy resolver is required")
	}
	profile, bindings, err := resolver.ResolveContentDestinationForCapability(ctx, preview.Destination.ID, preview.Destination.Revision, now, identity.PermissionContentConfirm)
	if err != nil || profile.DestinationProfileID == nil || *profile.DestinationProfileID != preview.Destination.ID || profile.Revision == nil || *profile.Revision != preview.Destination.Revision || profile.ContentHash == nil || *profile.ContentHash != preview.Destination.ContentHash || profile.ResourceID == nil || *profile.ResourceID != preview.Destination.ResourceID || profile.SheetID == nil || *profile.SheetID != preview.Destination.SheetID || profile.Cell == nil || *profile.Cell != preview.Destination.Cell || bindings.ActiveBundleHash != preview.PolicyBinding.ActiveBundleHash || bindings.ActiveBundleVersion != preview.PolicyBinding.ActiveBundleVersion || bindings.AccessPolicy.PolicyID != preview.PolicyBinding.AccessPolicyRef.PolicyID || bindings.AccessPolicy.Revision != preview.PolicyBinding.AccessPolicyRef.Revision || bindings.AccessPolicy.ContentHash != preview.PolicyBinding.AccessPolicyRef.ContentHash {
		return ContentPlacementConfirmationClaim{}, domainError("content_policy_stale", "active signed confirmation policy does not match the frozen preview")
	}
	preflightPreview := append([]byte(nil), record.PreviewJSON...)
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return ContentPlacementConfirmationClaim{}, err
	}
	prior, found, err = loadReservationTx(ctx, tx, principal.TenantID, request)
	if err != nil {
		return ContentPlacementConfirmationClaim{}, err
	}
	if found {
		if err := tx.Commit(); err != nil {
			return ContentPlacementConfirmationClaim{}, err
		}
		return ContentPlacementConfirmationClaim{Reservation: prior, Replay: prior.Disposition == ReservationReplay}, nil
	}
	record, err = loadContentPlacementConfirmationRecord(ctx, tx, principal.TenantID, placementIntentID)
	if err != nil || record.ActorID != principal.AuditActor() || record.State != "staged" || !record.ExpiresAt.After(now) || !bytes.Equal(record.PreviewJSON, preflightPreview) || digestHex(HashBytes(record.PreviewJSON)) != record.PreviewSHA256 {
		return ContentPlacementConfirmationClaim{}, domainError("content_confirmation_unavailable", "content placement intent changed before claim")
	}
	var activeHash string
	var activeVersion int
	if err := tx.QueryRowContext(ctx, `SELECT content_hash,bundle_version FROM assurance_active_bundles WHERE tenant_id=? AND singleton=1`, principal.TenantID).Scan(&activeHash, &activeVersion); err != nil || activeHash != preview.PolicyBinding.ActiveBundleHash || activeVersion != preview.PolicyBinding.ActiveBundleVersion {
		return ContentPlacementConfirmationClaim{}, domainError("content_policy_stale", "active signed bundle changed after the preview")
	}
	leaseExpires := now.Add(reservationLease)
	wantRetainUntil := now.Add(time.Duration(bindings.Retention.IdempotencyLifetimeSeconds) * time.Second)
	intentExpiry := record.CreatedAt.Add(time.Duration(bindings.Retention.IntentLifetimeSeconds) * time.Second)
	if bindings.Retention.IdempotencyLifetimeSeconds < int64(reservationLease/time.Second) || bindings.Retention.IntentLifetimeSeconds != preview.Destination.IntentLifetimeSeconds || !intentExpiry.After(leaseExpires) || !request.RetainUntil.Equal(wantRetainUntil) {
		return ContentPlacementConfirmationClaim{}, domainError("policy_blocked", "confirmation idempotency retention must equal the signed lifetime and cover its lease")
	}
	var storedTokenDigest string
	if err := tx.QueryRowContext(ctx, `SELECT token_digest FROM assurance_content_placement_intents WHERE tenant_id=? AND placement_intent_id=?`, principal.TenantID, placementIntentID).Scan(&storedTokenDigest); err != nil {
		return ContentPlacementConfirmationClaim{}, domainError("content_confirmation_unavailable", "content confirmation is unavailable")
	}
	if tokenDigest := contentPlacementTokenDigest(token, record.PreviewJSON); subtle.ConstantTimeCompare([]byte(tokenDigest), []byte(storedTokenDigest)) != 1 {
		return ContentPlacementConfirmationClaim{}, domainError("content_confirmation_unavailable", "content confirmation is unavailable")
	}
	reservation, err := s.reserveTx(ctx, tx, request, now, nil)
	if err != nil {
		return ContentPlacementConfirmationClaim{}, err
	}
	if reservation.Disposition != ReservationOwned {
		if err := tx.Commit(); err != nil {
			return ContentPlacementConfirmationClaim{}, err
		}
		return ContentPlacementConfirmationClaim{Reservation: reservation, Replay: reservation.Disposition == ReservationReplay}, nil
	}
	targetHash, err := contentPlacementTargetHash(preview.Destination.ResourceID, preview.Destination.SheetID, preview.Destination.Cell)
	if err != nil {
		return ContentPlacementConfirmationClaim{}, err
	}
	claimedAt := now
	update, err := tx.ExecContext(ctx, `UPDATE assurance_content_placement_intents SET state='claimed',confirmation_record_id=?,lease_id=?,owner_nonce=?,lease_expires_at=?,claimed_at=?,claim_row_version=row_version+1,row_version=row_version+1,target_hash=?
	 WHERE tenant_id=? AND placement_intent_id=? AND actor_id=? AND state='staged' AND expires_at>? AND row_version=?`, reservation.RecordID, leaseID, reservation.OwnerNonce, formatTimestamp(leaseExpires), formatTimestamp(claimedAt), targetHash, principal.TenantID, placementIntentID, principal.AuditActor(), formatTimestamp(now), record.RowVersion)
	if err != nil {
		if isConstraintError(err) {
			return ContentPlacementConfirmationClaim{}, domainError("content_target_busy", "another active content intent owns this destination")
		}
		return ContentPlacementConfirmationClaim{}, err
	}
	if err := requireOneTransition(update); err != nil {
		return ContentPlacementConfirmationClaim{}, domainError("content_confirmation_conflict", "content intent claim compare-and-swap failed")
	}
	if err := setContentPlacementReservationClaim(ctx, tx, principal.TenantID, reservation, placementIntentID, leaseExpires, now); err != nil {
		return ContentPlacementConfirmationClaim{}, err
	}
	metadata, _ := CanonicalJSON(map[string]any{"placement_intent_id": placementIntentID, "preview_sha256": record.PreviewSHA256, "target_hash": targetHash, "claim_row_version": record.RowVersion + 1})
	if err := appendContentConfirmationAudit(ctx, tx, s, principal, reservation, placementIntentID, "claim", string(metadata), auditID, now); err != nil {
		return ContentPlacementConfirmationClaim{}, err
	}
	if err := tx.Commit(); err != nil {
		return ContentPlacementConfirmationClaim{}, err
	}
	claimed := record
	claimed.State = "claimed"
	claimed.TargetHash = targetHash
	claimed.ConfirmationRecordID = reservation.RecordID
	claimed.LeaseID = leaseID
	claimed.OwnerNonce = reservation.OwnerNonce
	claimed.ClaimedAt = claimedAt
	claimed.LeaseExpiresAt = leaseExpires
	claimed.ClaimRowVersion = record.RowVersion + 1
	claimed.RowVersion = record.RowVersion + 1
	return ContentPlacementConfirmationClaim{Reservation: reservation, Record: claimed, LeaseID: leaseID}, nil
}

func contentPlacementTokenDigest(token string, preview []byte) string {
	input := make([]byte, 0, len(token)+1+len(preview))
	input = append(input, token...)
	input = append(input, 0)
	input = append(input, preview...)
	return digestHex(HashBytes(input))
}

func validateContentPlacementPreview(raw []byte, hash, intentID, actor string, now time.Time, allowExpired bool) (contentPlacementPreviewBinding, error) {
	var preview contentPlacementPreviewBinding
	canonical, err := CanonicalJSONBytes(raw)
	if err != nil || !bytes.Equal(canonical, raw) || digestHex(HashBytes(raw)) != hash {
		return preview, errors.New("content preview canonical hash mismatch")
	}
	if err := json.Unmarshal(raw, &preview); err != nil {
		return preview, err
	}
	if preview.PlacementIntentID != intentID || preview.ActorBindingID != actor || !contentPolicyHash.MatchString(preview.ContentHash) || preview.Destination.ID == "" || preview.Destination.Revision < 1 || !contentPolicyHash.MatchString(preview.Destination.ContentHash) || !contentPolicyID.MatchString(preview.Destination.ResourceID) || !contentPolicyID.MatchString(preview.Destination.SheetID) || !regexpCell.MatchString(preview.Destination.Cell) || !contentPolicyHash.MatchString(preview.PolicyBinding.ActiveBundleHash) || preview.PolicyBinding.ActiveBundleVersion < 1 || !contentPolicyID.MatchString(preview.PolicyBinding.AccessPolicyRef.PolicyID) || preview.PolicyBinding.AccessPolicyRef.Revision < 1 || !contentPolicyHash.MatchString(preview.PolicyBinding.AccessPolicyRef.ContentHash) || len(preview.IntendedValue) == 0 {
		return preview, errors.New("content preview policy or destination binding is incomplete")
	}
	if preview.CreatedAt.IsZero() || preview.ExpiresAt.IsZero() || preview.CreatedAt.After(now) || (!allowExpired && !preview.ExpiresAt.After(now)) || preview.ExpiresAt.Sub(preview.CreatedAt) > 900*time.Second || preview.Destination.IntentLifetimeSeconds < 1 || preview.Destination.IntentLifetimeSeconds > 315360000 {
		return preview, errors.New("content preview lifetime is invalid")
	}
	if !validConfirmationJSONScalar(preview.IntendedValue) {
		return preview, errors.New("intended content value is not typed JSON")
	}
	return preview, nil
}

func contentPlacementTargetHash(resourceID, sheetID, cell string) (string, error) {
	canonical, err := CanonicalJSON(struct {
		ResourceID string `json:"resource_id"`
		SheetID    string `json:"sheet_id"`
		Cell       string `json:"cell"`
	}{resourceID, sheetID, cell})
	if err != nil {
		return "", err
	}
	return digestHex(HashBytes(canonical)), nil
}

func setContentPlacementReservationClaim(ctx context.Context, tx *sql.Tx, tenant string, reservation ReservationResult, intentID string, leaseExpires, now time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET entity_reference=?,execution_started=1,last_heartbeat_at=?,lease_expires_at=?
	 WHERE tenant_id=? AND record_id=? AND state='reserved' AND owner_nonce=?`, intentID, formatTimestamp(now), formatTimestamp(leaseExpires), tenant, reservation.RecordID, reservation.OwnerNonce)
	if err != nil {
		return err
	}
	return requireOneTransition(result)
}

// MarkContentPlacementSubmitting accepts only the exact claim fence digest
// that the caller has durably created and verified. No provider submit may
// occur before this transition succeeds.
func (s *Store) MarkContentPlacementSubmitting(ctx context.Context, intentID, recordID, ownerNonce, leaseID, claimFenceDigest, auditID string, expectedVersion int64, now time.Time) (int64, error) {
	principal, err := requireContentConfirmPrincipal(ctx)
	if err != nil {
		return 0, err
	}
	if !contentPolicyHash.MatchString(claimFenceDigest) || !uuidIsCanonical(leaseID) || !validContentAuditID(auditID) || expectedVersion < 1 {
		return 0, domainError("invalid_request", "verified content claim fence and claim binding are required")
	}
	now = now.UTC()
	tx, reservation, correlationID, err := beginOwnedContentConfirmation(ctx, s, principal, intentID, recordID, ownerNonce, expectedVersion, "claimed", now, false)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	intent, err := loadContentPlacementConfirmationRecord(ctx, tx, principal.TenantID, intentID)
	if err != nil {
		return 0, err
	}
	frozen, err := validateContentPlacementPreview(intent.PreviewJSON, intent.PreviewSHA256, intentID, principal.AuditActor(), now, false)
	if err != nil {
		return 0, domainError("content_confirmation_expired", "confirmation preview expired before submit")
	}
	var activeHash string
	var activeVersion int
	if err := tx.QueryRowContext(ctx, `SELECT content_hash,bundle_version FROM assurance_active_bundles WHERE tenant_id=? AND singleton=1`, principal.TenantID).Scan(&activeHash, &activeVersion); err != nil || frozen.PolicyBinding.ActiveBundleHash != activeHash || frozen.PolicyBinding.ActiveBundleVersion != activeVersion {
		return 0, domainError("content_policy_stale", "active signed bundle changed before provider submission")
	}
	update, err := tx.ExecContext(ctx, `UPDATE assurance_content_placement_intents SET state='submitting',claim_fence_digest=?,row_version=row_version+1
	 WHERE tenant_id=? AND placement_intent_id=? AND confirmation_record_id=? AND owner_nonce=? AND lease_id=? AND state='claimed' AND row_version=? AND claim_row_version>0 AND claim_row_version<=? AND lease_expires_at>? AND claim_fence_digest=''`, claimFenceDigest, principal.TenantID, intentID, recordID, ownerNonce, leaseID, expectedVersion, expectedVersion, formatTimestamp(now))
	if err != nil {
		return 0, err
	}
	if err := requireOneTransition(update); err != nil {
		return 0, domainError("content_confirmation_conflict", "content submit fence compare-and-swap failed")
	}
	metadata, _ := CanonicalJSON(map[string]any{"placement_intent_id": intentID, "claim_fence_digest": claimFenceDigest, "row_version": expectedVersion + 1})
	if err := appendContentConfirmationAudit(ctx, tx, s, principal, reservation, intentID, "submit_fence", string(metadata), auditID, now); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	_ = correlationID
	return expectedVersion + 1, nil
}

// PersistContentPlacementOperation durably records a known opaque provider
// operation reference after submission. The reference is excluded from audit.
func (s *Store) PersistContentPlacementOperation(ctx context.Context, intentID, recordID, ownerNonce, operationReference, auditID string, expectedVersion int64, now time.Time) (int64, error) {
	principal, err := requireContentConfirmPrincipal(ctx)
	if err != nil {
		return 0, err
	}
	if !safeContentOperationReference(operationReference) || !validContentAuditID(auditID) || expectedVersion < 2 {
		return 0, domainError("invalid_request", "bounded operation reference and content claim are required")
	}
	now = now.UTC()
	tx, reservation, _, err := beginOwnedContentConfirmation(ctx, s, principal, intentID, recordID, ownerNonce, expectedVersion, "submitting", now, false)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	update, err := tx.ExecContext(ctx, `UPDATE assurance_content_placement_intents SET operation_reference=?,row_version=row_version+1
	 WHERE tenant_id=? AND placement_intent_id=? AND confirmation_record_id=? AND owner_nonce=? AND state='submitting' AND row_version=? AND operation_reference=''`, operationReference, principal.TenantID, intentID, recordID, ownerNonce, expectedVersion)
	if err != nil {
		return 0, err
	}
	if err := requireOneTransition(update); err != nil {
		return 0, domainError("content_confirmation_conflict", "content operation reference compare-and-swap failed")
	}
	refHash := digestHex(HashBytes([]byte(operationReference)))
	metadata, _ := CanonicalJSON(map[string]any{"placement_intent_id": intentID, "operation_reference_sha256": refHash, "row_version": expectedVersion + 1})
	if err := appendContentConfirmationAudit(ctx, tx, s, principal, reservation, intentID, "operation_observed", string(metadata), auditID, now); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return expectedVersion + 1, nil
}

// RenewContentPlacementConfirmation extends the same owned reservation and
// intent lease in one transaction. Renewal remains bounded by the explicit
// intent expiry and advances the intent CAS version.
func (s *Store) RenewContentPlacementConfirmation(ctx context.Context, intentID, recordID, ownerNonce, leaseID, auditID string, expectedVersion int64, now time.Time) (int64, error) {
	p, err := requireContentConfirmPrincipal(ctx)
	if err != nil {
		return 0, err
	}
	if !uuidIsCanonical(leaseID) || !validContentAuditID(auditID) || expectedVersion < 1 {
		return 0, domainError("invalid_request", "content confirmation lease binding is invalid")
	}
	now = now.UTC()
	tx, reservation, _, err := beginOwnedContentConfirmation(ctx, s, p, intentID, recordID, ownerNonce, expectedVersion, "claimed", now, false)
	if err != nil {
		// A submitting intent may also renew. Retry only the owned-state lookup;
		// no external action occurs in this fallback.
		tx, reservation, _, err = beginOwnedContentConfirmation(ctx, s, p, intentID, recordID, ownerNonce, expectedVersion, "submitting", now, false)
	}
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var storedLeaseID string
	var storedRecord ContentPlacementConfirmationRecord
	if err := tx.QueryRowContext(ctx, `SELECT lease_id FROM assurance_content_placement_intents WHERE tenant_id=? AND placement_intent_id=?`, p.TenantID, intentID).Scan(&storedLeaseID); err != nil {
		return 0, err
	}
	if storedLeaseID != leaseID {
		return 0, domainError("content_confirmation_conflict", "content lease identifier changed")
	}
	storedRecord, err = loadContentPlacementConfirmationRecord(ctx, tx, p.TenantID, intentID)
	if err != nil {
		return 0, err
	}
	preview, err := validateContentPlacementPreview(storedRecord.PreviewJSON, storedRecord.PreviewSHA256, intentID, p.AuditActor(), storedRecord.ClaimedAt, true)
	if err != nil {
		return 0, domainError("content_confirmation_conflict", "frozen intent lifetime binding is invalid")
	}
	intentExpiry := storedRecord.CreatedAt.Add(time.Duration(preview.Destination.IntentLifetimeSeconds) * time.Second)
	if !intentExpiry.After(now) {
		return 0, domainError("content_confirmation_lease_expired", "content intent expiry has passed")
	}
	leaseExpiry := now.Add(reservationLease)
	if leaseExpiry.After(intentExpiry) {
		leaseExpiry = intentExpiry
	}
	updated, err := tx.ExecContext(ctx, `UPDATE assurance_content_placement_intents SET lease_expires_at=?,row_version=row_version+1 WHERE tenant_id=? AND placement_intent_id=? AND confirmation_record_id=? AND owner_nonce=? AND lease_id=? AND row_version=? AND state IN ('claimed','submitting')`, formatTimestamp(leaseExpiry), p.TenantID, intentID, recordID, ownerNonce, leaseID, expectedVersion)
	if err != nil {
		return 0, err
	}
	if err := requireOneTransition(updated); err != nil {
		return 0, domainError("content_confirmation_conflict", "content lease renewal compare-and-swap failed")
	}
	updated, err = tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET last_heartbeat_at=?,lease_expires_at=? WHERE tenant_id=? AND record_id=? AND state='reserved' AND owner_nonce=?`, formatTimestamp(now), formatTimestamp(leaseExpiry), p.TenantID, recordID, ownerNonce)
	if err != nil {
		return 0, err
	}
	if err := requireOneTransition(updated); err != nil {
		return 0, domainError("content_confirmation_conflict", "reservation lease renewal compare-and-swap failed")
	}
	metadata, _ := CanonicalJSON(map[string]any{"placement_intent_id": intentID, "lease_expires_at": formatTimestamp(leaseExpiry), "row_version": expectedVersion + 1})
	if err := appendContentConfirmationAudit(ctx, tx, s, p, reservation, intentID, "lease_renewed", string(metadata), auditID, now); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return expectedVersion + 1, nil
}

// FreezeContentPlacementUnknown permanently holds the same-target lock while
// an uncertain submission is reconciled. It never clears a known operation.
func (s *Store) FreezeContentPlacementUnknown(ctx context.Context, intentID, recordID, ownerNonce, reason, auditID string, expectedVersion int64, now time.Time) error {
	principal, err := requireContentConfirmPrincipal(ctx)
	if err != nil {
		return err
	}
	if !oneOf(reason, "provider_outcome_unknown", "lease_lost_after_submission", "readback_mismatch", "terminal_fence_unavailable", "terminal_audit_failed") || !validContentAuditID(auditID) || expectedVersion < 2 {
		return domainError("invalid_request", "content uncertainty reason or claim version is invalid")
	}
	now = now.UTC()
	tx, reservation, _, err := beginOwnedContentConfirmation(ctx, s, principal, intentID, recordID, ownerNonce, expectedVersion, "submitting", now, true)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	episodeID := uuid.NewString()
	update, err := tx.ExecContext(ctx, `UPDATE assurance_content_placement_intents SET state='reconciliation_required',unknown_reason=?,uncertainty_episode_id=CASE WHEN uncertainty_episode_id='' THEN ? ELSE uncertainty_episode_id END,row_version=row_version+1
	 WHERE tenant_id=? AND placement_intent_id=? AND confirmation_record_id=? AND owner_nonce=? AND state='submitting' AND row_version=? AND claim_fence_digest<>''`, reason, episodeID, principal.TenantID, intentID, recordID, ownerNonce, expectedVersion)
	if err != nil {
		return err
	}
	if err := requireOneTransition(update); err != nil {
		return domainError("content_confirmation_conflict", "content uncertainty compare-and-swap failed")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET lease_expires_at=? WHERE tenant_id=? AND record_id=? AND state='reserved' AND owner_nonce=?`, formatTimestamp(now), principal.TenantID, recordID, ownerNonce); err != nil {
		return err
	}
	metadata, _ := CanonicalJSON(map[string]any{"placement_intent_id": intentID, "reason": reason, "operation_reference_present": true, "row_version": expectedVersion + 1})
	if err := appendContentConfirmationAudit(ctx, tx, s, principal, reservation, intentID, "reconciliation_required", string(metadata), auditID, now); err != nil {
		s.BlockReadiness(err)
		return err
	}
	return tx.Commit()
}

// FinalizeContentPlacementReadback stores only an accepted uncached read-back
// matching the frozen intended scalar. The terminal fence, result hash, audit,
// visual-ack-pending transition, and idempotency seal commit atomically.
func (s *Store) FinalizeContentPlacementReadback(ctx context.Context, request ContentPlacementReadback, auditID string, now time.Time) (ContentPlacementConfirmationResult, error) {
	principal, err := requireContentConfirmPrincipal(ctx)
	if err != nil {
		return ContentPlacementConfirmationResult{}, err
	}
	if request.Outcome != "accepted" || !safeContentOperationReference(request.OperationReference) || !contentPolicyHash.MatchString(request.ReadbackSHA256) || !contentPolicyHash.MatchString(request.TerminalFenceDigest) || !validContentAuditID(auditID) || request.ExpectedVersion < 2 || len(request.ReadbackJSON) < 2 || len(request.ReadbackJSON) > 1<<20 {
		return ContentPlacementConfirmationResult{}, domainError("invalid_request", "accepted content read-back evidence is incomplete")
	}
	canonicalReadback, err := CanonicalJSONBytes(request.ReadbackJSON)
	if err != nil || !bytes.Equal(canonicalReadback, request.ReadbackJSON) || digestHex(HashBytes(request.ReadbackJSON)) != request.ReadbackSHA256 {
		return ContentPlacementConfirmationResult{}, domainError("artifact_hash_mismatch", "content read-back is not canonical or its digest does not match")
	}
	var observed contentPlacementReadbackObservation
	if err := json.Unmarshal(canonicalReadback, &observed); err != nil || !observed.ValuePresent || !observed.FormatsPresent || !json.Valid(observed.RawFormats) || !observed.EffectiveFormatsPresent || !json.Valid(observed.EffectiveFormats) || observed.Protection != "unprotected" || observed.Writable != "writable" || observed.LiteralWriteFormatPreservation != "preserves" || observed.LiteralWriteAPIVersion != "2026-01-01" || !strings.HasPrefix(observed.LiteralWriteEndpoint, "POST ") || observed.Provenance.Provider != "workiva_rest" || observed.Provenance.APIVersion != "2026-01-01" || observed.Provenance.Endpoint == "" || observed.Provenance.Cache != "bypassed" {
		return ContentPlacementConfirmationResult{}, domainError("content_readback_invalid", "uncached Workiva REST read-back proof is incomplete")
	}
	if !validConfirmationJSONScalar(observed.RawValue) || !uuidIsCanonical(request.PlacementIntentID) {
		return ContentPlacementConfirmationResult{}, domainError("content_readback_invalid", "typed Workiva read-back value is invalid")
	}
	now = now.UTC()
	tx, reservation, _, err := beginOwnedContentConfirmation(ctx, s, principal, request.PlacementIntentID, request.RecordID, request.OwnerNonce, request.ExpectedVersion, "submitting", now, false)
	if err != nil {
		return ContentPlacementConfirmationResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	record, err := loadContentPlacementConfirmationRecord(ctx, tx, principal.TenantID, request.PlacementIntentID)
	if err != nil || record.OperationReference != request.OperationReference || record.ClaimFenceDigest == "" || record.TerminalFenceDigest != "" {
		return ContentPlacementConfirmationResult{}, domainError("content_confirmation_conflict", "content operation is not awaiting read-back finalization")
	}
	preview, err := validateContentPlacementPreview(record.PreviewJSON, record.PreviewSHA256, request.PlacementIntentID, principal.AuditActor(), record.ClaimedAt, true)
	intentExpiry := record.CreatedAt.Add(time.Duration(preview.Destination.IntentLifetimeSeconds) * time.Second)
	if err != nil || record.ClaimedAt.IsZero() || now.After(intentExpiry) || observed.SpreadsheetID != preview.Destination.ResourceID || observed.SheetID != preview.Destination.SheetID || observed.Locator != preview.Destination.Cell || observed.Provenance.QueryRange != preview.Destination.Cell {
		return ContentPlacementConfirmationResult{}, domainError("content_readback_invalid", "Workiva read-back coordinates do not match the frozen destination")
	}
	formattingHash, formatErr := ContentFormattingHash(observed.RawFormats, observed.FormatsPresent, observed.EffectiveFormats, observed.EffectiveFormatsPresent)
	if formatErr != nil || formattingHash != observed.FormattingSHA256 || formattingHash != preview.ProviderMetadata.FormattingSHA256 {
		return ContentPlacementConfirmationResult{}, domainError("content_readback_mismatch", "Workiva format evidence does not match its digest or frozen preview")
	}
	intended, err := CanonicalJSONBytes(preview.IntendedValue)
	actual, actualErr := CanonicalJSONBytes(observed.RawValue)
	if err != nil || actualErr != nil || !bytes.Equal(intended, actual) {
		return ContentPlacementConfirmationResult{}, domainError("content_readback_mismatch", "uncached Workiva value does not match the frozen intended value")
	}
	if !contentPolicyHash.MatchString(record.TargetHash) || !contentPolicyHash.MatchString(record.ClaimFenceDigest) {
		return ContentPlacementConfirmationResult{}, domainError("content_fence_invalid", "content claim fence binding is missing")
	}
	nextVersion := request.ExpectedVersion + 1
	result := ContentPlacementConfirmationResult{Kind: "confirm", Status: "machine_verified_visual_ack_pending", PlacementIntentID: request.PlacementIntentID, State: "machine_verified_visual_ack_pending", VisualState: "pending", NLAuditID: auditID, ReadbackJSON: append(json.RawMessage(nil), canonicalReadback...), ReadbackSHA256: request.ReadbackSHA256, OperationReference: request.OperationReference, TargetHash: record.TargetHash, RowVersion: nextVersion, TerminalFenceDigest: request.TerminalFenceDigest}
	envelope, err := CanonicalJSON(contentPlacementSealedEnvelope{result.Kind, result.Status, result.PlacementIntentID, result.State, result.VisualState, result.NLAuditID, result.ReadbackJSON, result.ReadbackSHA256, result.OperationReference, result.TargetHash, result.RowVersion, result.TerminalFenceDigest})
	if err != nil {
		return ContentPlacementConfirmationResult{}, err
	}
	result.ResultSHA256 = digestHex(HashBytes(envelope))
	metadata, _ := CanonicalJSON(map[string]any{"placement_intent_id": request.PlacementIntentID, "readback_sha256": request.ReadbackSHA256, "terminal_fence_digest": request.TerminalFenceDigest, "result_sha256": result.ResultSHA256, "row_version": nextVersion})
	update, err := tx.ExecContext(ctx, `UPDATE assurance_content_placement_intents SET state='machine_verified_visual_ack_pending',machine_result_record_id=?,readback_json=?,readback_sha256=?,readback_at=?,terminal_at=?,terminal_fence_digest=?,row_version=row_version+1
	 WHERE tenant_id=? AND placement_intent_id=? AND confirmation_record_id=? AND owner_nonce=? AND state='submitting' AND row_version=? AND operation_reference=? AND terminal_fence_digest=''`, request.RecordID, string(canonicalReadback), request.ReadbackSHA256, formatTimestamp(now), formatTimestamp(now), request.TerminalFenceDigest, principal.TenantID, request.PlacementIntentID, request.RecordID, request.OwnerNonce, request.ExpectedVersion, request.OperationReference)
	if err != nil {
		return ContentPlacementConfirmationResult{}, err
	}
	if err := requireOneTransition(update); err != nil {
		return ContentPlacementConfirmationResult{}, domainError("content_confirmation_conflict", "content terminal compare-and-swap failed")
	}
	if err := appendContentConfirmationAudit(ctx, tx, s, principal, reservation, request.PlacementIntentID, "machine_verified", string(metadata), auditID, now); err != nil {
		return ContentPlacementConfirmationResult{}, err
	}
	sealed, err := tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET state='sealed',response_envelope=?,response_hash=?,response_status='machine_verified_visual_ack_pending',entity_reference=?,audit_id=?,terminal_at=?,lease_expires_at=?
	 WHERE tenant_id=? AND record_id=? AND state='reserved' AND owner_nonce=?`, string(envelope), digestHex(HashBytes(envelope)), request.PlacementIntentID, auditID, formatTimestamp(now), formatTimestamp(now), principal.TenantID, request.RecordID, request.OwnerNonce)
	if err != nil {
		return ContentPlacementConfirmationResult{}, err
	}
	if err := requireOneTransition(sealed); err != nil {
		return ContentPlacementConfirmationResult{}, domainError("content_confirmation_conflict", "content confirmation reservation seal compare-and-swap failed")
	}
	if err := tx.Commit(); err != nil {
		return ContentPlacementConfirmationResult{}, err
	}
	return result, nil
}

func beginOwnedContentConfirmation(ctx context.Context, s *Store, principal identity.Principal, intentID, recordID, ownerNonce string, version int64, state string, now time.Time, allowExpiredLease bool) (*sql.Tx, ReservationResult, string, error) {
	if s == nil || s.db == nil || !uuidIsCanonical(intentID) || recordID == "" || ownerNonce == "" || version < 1 {
		return nil, ReservationResult{}, "", ErrNotReady
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ReservationResult{}, "", err
	}
	var actor, tool, action, storedNonce, correlationID, reservationState string
	if err := tx.QueryRowContext(ctx, `SELECT actor_id,tool,action,owner_nonce,correlation_id,state FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=?`, principal.TenantID, recordID).Scan(&actor, &tool, &action, &storedNonce, &correlationID, &reservationState); err != nil {
		_ = tx.Rollback()
		return nil, ReservationResult{}, "", domainError("content_confirmation_unavailable", "content confirmation reservation is unavailable")
	}
	if actor != principal.AuditActor() || tool != contentPlacementTool || action != "confirm" || reservationState != string(ReservationStateReserved) || subtle.ConstantTimeCompare([]byte(ownerNonce), []byte(storedNonce)) != 1 {
		_ = tx.Rollback()
		return nil, ReservationResult{}, "", domainError("content_confirmation_conflict", "content confirmation reservation ownership changed")
	}
	var storedActor, intentState, storedRecord, storedOwner string
	var rowVersion int64
	var leaseExpiry string
	if err := tx.QueryRowContext(ctx, `SELECT actor_id,state,confirmation_record_id,owner_nonce,row_version,lease_expires_at FROM assurance_content_placement_intents WHERE tenant_id=? AND placement_intent_id=?`, principal.TenantID, intentID).Scan(&storedActor, &intentState, &storedRecord, &storedOwner, &rowVersion, &leaseExpiry); err != nil || storedActor != principal.AuditActor() || intentState != state || storedRecord != recordID || storedOwner != ownerNonce || rowVersion != version {
		_ = tx.Rollback()
		return nil, ReservationResult{}, "", domainError("content_confirmation_conflict", "content placement claim binding changed")
	}
	lease, err := time.Parse(time.RFC3339Nano, leaseExpiry)
	if err != nil || (!allowExpiredLease && !lease.After(now)) {
		_ = tx.Rollback()
		return nil, ReservationResult{}, "", domainError("content_confirmation_lease_expired", "content confirmation lease expired")
	}
	return tx, ReservationResult{Disposition: ReservationOwned, State: ReservationStateReserved, RecordID: recordID, OwnerNonce: ownerNonce, CorrelationID: correlationID, EntityReference: intentID}, correlationID, nil
}

func appendContentConfirmationAudit(ctx context.Context, tx *sql.Tx, s *Store, principal identity.Principal, reservation ReservationResult, intentID, action, metadata, auditID string, now time.Time) error {
	if !validContentAuditID(auditID) || s.auditLog == nil {
		return domainError("audit_unavailable", "content confirmation rich audit is required")
	}
	if _, err := s.auditLog.AppendTx(ctx, tx, audit.Entry{Ts: now, Actor: principal.AuditActor(), Tool: contentPlacementTool, Action: "confirm." + action, Target: intentID, AfterJSON: metadata, AuditID: auditID}); err != nil {
		return domainError("audit_unavailable", "content confirmation audit append failed")
	}
	for _, entity := range []struct{ kind, id string }{{"content_placement_intent", intentID}, {"idempotency_record", reservation.RecordID}} {
		if err := insertContentAuditLink(ctx, tx, principal.TenantID, entity.kind, entity.id, auditID, reservation.CorrelationID, now); err != nil {
			return fmt.Errorf("assurance: link content confirmation audit: %w", err)
		}
	}
	return nil
}

func loadContentPlacementConfirmationRecord(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, tenant, intentID string) (ContentPlacementConfirmationRecord, error) {
	var result ContentPlacementConfirmationRecord
	var preview, readback string
	var created, expires, claimed, leaseExpires, readbackAt, terminalAt string
	err := query.QueryRowContext(ctx, `SELECT placement_intent_id,actor_id,state,preview_json,preview_sha256,target_hash,operation_reference,unknown_reason,readback_json,readback_sha256,claim_fence_digest,terminal_fence_digest,confirmation_record_id,machine_result_record_id,lease_id,owner_nonce,claim_row_version,row_version,created_at,expires_at,claimed_at,lease_expires_at,readback_at,terminal_at
	 FROM assurance_content_placement_intents WHERE tenant_id=? AND placement_intent_id=?`, tenant, intentID).Scan(&result.PlacementIntentID, &result.ActorID, &result.State, &preview, &result.PreviewSHA256, &result.TargetHash, &result.OperationReference, &result.UnknownReason, &readback, &result.ReadbackSHA256, &result.ClaimFenceDigest, &result.TerminalFenceDigest, &result.ConfirmationRecordID, &result.MachineResultRecordID, &result.LeaseID, &result.OwnerNonce, &result.ClaimRowVersion, &result.RowVersion, &created, &expires, &claimed, &leaseExpires, &readbackAt, &terminalAt)
	if err != nil {
		return ContentPlacementConfirmationRecord{}, err
	}
	result.PreviewJSON = json.RawMessage(preview)
	if readback != "" {
		result.ReadbackJSON = json.RawMessage(readback)
	}
	for _, pair := range []struct {
		value       string
		destination *time.Time
	}{{created, &result.CreatedAt}, {expires, &result.ExpiresAt}} {
		parsed, err := time.Parse(time.RFC3339Nano, pair.value)
		if err != nil {
			return ContentPlacementConfirmationRecord{}, err
		}
		*pair.destination = parsed.UTC()
	}
	for _, pair := range []struct {
		value       string
		destination *time.Time
	}{{claimed, &result.ClaimedAt}, {leaseExpires, &result.LeaseExpiresAt}, {readbackAt, &result.ReadbackAt}, {terminalAt, &result.TerminalAt}} {
		if pair.value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, pair.value)
		if err != nil {
			return ContentPlacementConfirmationRecord{}, err
		}
		*pair.destination = parsed.UTC()
	}
	return result, nil
}

func requireContentConfirmPrincipal(ctx context.Context) (identity.Principal, error) {
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok || principal.TokenType != identity.TokenTypeDelegated || principal.UsedSubjectFallback || !principal.HasPermission(identity.PermissionContentConfirm) || !uuidIsCanonical(principal.TenantID) || !uuidIsCanonical(principal.ObjectID) {
		return identity.Principal{}, domainError("strong_identity_required", "trusted delegated content.confirm principal required")
	}
	return principal, nil
}

func validContentAuditID(value string) bool {
	return value != "" && len(value) <= 128 && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func safeContentOperationReference(value string) bool {
	return contentPolicyID.MatchString(value) && !strings.Contains(value, "://")
}

func validConfirmationJSONScalar(raw []byte) bool {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if len(raw) == 0 || decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return false
	}
	switch value := value.(type) {
	case bool, json.Number:
		return json.Valid(raw)
	case string:
		return json.Valid(raw) && !strings.HasPrefix(value, "=")
	default:
		return false
	}
}
