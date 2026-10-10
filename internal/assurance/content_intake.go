package assurance

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"mime"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
)

const (
	contentRequestMaxBytes = 1_048_576
	contentSourceMaxBytes  = 786_432
	contentTextSourceMax   = 262_144
	contentItemMaxBytes    = 16_384
	contentDraftMaxBytes   = 32_768
	contentMetadataMax     = 1_048_576
	contentItemsMax        = 64
	contentDraftsMax       = 8
	contentLocalIDMaxBytes = 64
	contentFilenameMax     = 255
)

// ContentIngest is the private assurance persistence input. Public ingress
// supplies its separately canonicalized request digest to the dedicated
// policy-bound finalizer; this type does not define that public request shape.
type ContentIngest struct {
	SourceBytes                  []byte              `json:"source_bytes"`
	MediaType                    string              `json:"media_type"`
	Filename                     string              `json:"filename,omitempty"`
	MetadataBLOB                 []byte              `json:"metadata_blob"`
	ExistingSourceArtifactID     string              `json:"existing_source_artifact_id,omitempty"`
	ExpectedExistingSourceSHA256 string              `json:"expected_existing_source_sha256,omitempty"`
	Items                        []ContentItemInput  `json:"items"`
	Drafts                       []ContentDraftInput `json:"drafts"`
}

type ContentItemInput struct {
	LocalID      string `json:"local_id"`
	Text         string `json:"text"`
	MetadataBLOB []byte `json:"metadata_blob"`
}

type ContentDraftInput struct {
	LocalID      string   `json:"local_id"`
	Text         string   `json:"text"`
	ItemLocalIDs []string `json:"item_local_ids"`
	MetadataBLOB []byte   `json:"metadata_blob"`
}

type ContentIngestResult struct {
	SourceArtifactID string                         `json:"source_artifact_id"`
	SourceSHA256     string                         `json:"source_sha256"`
	ItemIDs          map[string]string              `json:"item_ids"`
	DraftIDs         map[string]string              `json:"draft_ids"`
	PublicProjection *ContentIngestPublicProjection `json:"public_projection,omitempty"`
}

type ContentIngestPublicProjection struct {
	Kind                 string                                `json:"kind"`
	Source               ContentIngestSourceArtifactProjection `json:"source"`
	VerifiedEvidenceRefs []ContentIngestVerifiedEvidenceRef    `json:"verified_evidence_refs"`
	ExtractedItems       []ContentIngestItemProjection         `json:"extracted_items"`
	DraftArtifacts       []ContentIngestDraftProjection        `json:"draft_artifacts"`
}

type ContentIngestVerifiedEvidenceRef struct {
	EvidenceID  string `json:"evidence_id"`
	Revision    int    `json:"revision"`
	ContentHash string `json:"content_hash"`
}

type ContentIngestRetentionReference struct {
	PolicyID    string `json:"policy_id"`
	Revision    int    `json:"revision"`
	ContentHash string `json:"content_hash"`
	Class       string `json:"class"`
}

type ContentIngestSourceBinding struct {
	SourceArtifactID string `json:"source_artifact_id"`
	Revision         int    `json:"revision"`
	ContentHash      string `json:"content_hash"`
}

type ContentIngestSourceArtifactProjection struct {
	SourceArtifactID string                          `json:"source_artifact_id"`
	Revision         int                             `json:"revision"`
	ContentHash      string                          `json:"content_hash"`
	ByteLength       int                             `json:"byte_length"`
	MediaType        string                          `json:"media_type"`
	RetentionPolicy  ContentIngestRetentionReference `json:"retention_policy"`
}

type ContentIngestItemProjection struct {
	ExtractedItemID string                          `json:"extracted_item_id"`
	Revision        int                             `json:"revision"`
	ContentHash     string                          `json:"content_hash"`
	Source          ContentIngestSourceBinding      `json:"source"`
	CandidateOnly   bool                            `json:"candidate_only"`
	RetentionPolicy ContentIngestRetentionReference `json:"retention_policy"`
}

type ContentIngestDraftLineage struct {
	Kind   string                     `json:"kind"`
	Source ContentIngestSourceBinding `json:"source"`
}

type ContentIngestDraftProjection struct {
	DraftArtifactID string                          `json:"draft_artifact_id"`
	Revision        int                             `json:"revision"`
	ContentHash     string                          `json:"content_hash"`
	Lineage         ContentIngestDraftLineage       `json:"lineage"`
	OriginKind      string                          `json:"origin_kind"`
	CandidateOnly   bool                            `json:"candidate_only"`
	RetentionPolicy ContentIngestRetentionReference `json:"retention_policy"`
}

// ContentIntakePolicySnapshot is populated only by Store's signed resolver
// path. It records the active bundle identity used for reservation and source
// provenance; callers never supply a snapshot to authorize policy.
type ContentIntakePolicySnapshot struct {
	Access              ContentAccessPolicy
	Retention           ContentRetentionPolicy
	ActiveBundleHash    string
	ActiveBundleVersion int
}

// ContentSourceRecord is an owned private database record. SourceBytes is
// copied on read so callers cannot mutate the stored byte slice in memory.
type ContentSourceRecord struct {
	ID                string
	TenantID          string
	ActorID           string
	MediaType         string
	DetectedMediaType string
	Filename          string
	SourceBytes       []byte
	SourceSHA256      string
	MetadataBLOB      []byte
	MetadataSHA256    string
	ExpiresAt         time.Time
	CreatedAt         time.Time
}

type normalizedContentItem struct {
	localID, text string
	metadata      []byte
	id            string
}

type normalizedContentDraft struct {
	localID, text string
	itemLocalIDs  []string
	metadata      []byte
	id            string
}

// ContentIngestRequestDigest computes the private assurance reservation
// digest from canonical JSON for ContentIngest. It intentionally does not
// define or claim compatibility with the future public MCP request digest.
func ContentIngestRequestDigest(input ContentIngest) (Digest, error) {
	canonical, err := CanonicalJSON(input)
	if err != nil {
		return Digest{}, err
	}
	return HashBytes(canonical), nil
}

// ValidateContentIngest checks request bounds and canonical content structure
// without reading storage or making policy decisions.
func ValidateContentIngest(input ContentIngest) error {
	_, _, _, _, _, _, _, err := validateContentIngest(input)
	return err
}

// FinalizeContentIngest stores one immutable source/intake bundle and seals
// its existing reservation in the same SQLite transaction as rich audit.
// retentionUntil must already be selected by trusted policy; this method has
// no duration default and does not itself validate a signed policy bundle.
func (s *Store) FinalizeContentIngest(ctx context.Context, reservation ReservationResult, input ContentIngest, retentionUntil time.Time, auditID string, now time.Time) (ContentIngestResult, error) {
	return s.finalizeContentIngest(ctx, reservation, input, retentionUntil, auditID, now, nil, nil, false)
}

// FinalizePolicyBoundContentIngest rechecks the active signed intake and
// retention policies inside assurance before enriching canonical source
// metadata. requestDigest remains bound to the original caller input so
// retries remain classifiable after policy changes.
func (s *Store) FinalizePolicyBoundContentIngest(ctx context.Context, reservation ReservationResult, input ContentIngest, requestDigest Digest, resolver *ContentPolicyResolver, auditID string, now time.Time) (ContentIngestResult, error) {
	actualDigest, err := ContentIngestRequestDigest(input)
	if err != nil || actualDigest != requestDigest {
		return ContentIngestResult{}, domainError("idempotency_conflict", "content ingest request digest does not match original input")
	}
	return s.finalizePolicyBoundContentIngest(ctx, reservation, input, requestDigest, resolver, auditID, now, true, false)
}

// FinalizePolicyBoundContentIngestWithRequestDigest binds a separately
// canonicalized public request digest to the owned reservation while
// independently validating and persisting the private ContentIngest artifacts.
// The digest does not authorize metadata or policy: policy and provenance are
// still resolved and injected by this assurance store from its signed bundle.
func (s *Store) FinalizePolicyBoundContentIngestWithRequestDigest(ctx context.Context, reservation ReservationResult, input ContentIngest, requestDigest Digest, resolver *ContentPolicyResolver, auditID string, now time.Time) (ContentIngestResult, error) {
	return s.finalizePolicyBoundContentIngest(ctx, reservation, input, requestDigest, resolver, auditID, now, false, true)
}

func (s *Store) finalizePolicyBoundContentIngest(ctx context.Context, reservation ReservationResult, input ContentIngest, requestDigest Digest, resolver *ContentPolicyResolver, auditID string, now time.Time, requirePrivateDigestMatch, includePublicProjection bool) (ContentIngestResult, error) {
	_, err := requireContentPrincipal(ctx)
	if err != nil {
		return ContentIngestResult{}, err
	}
	if requestDigest == (Digest{}) {
		return ContentIngestResult{}, domainError("invalid_request", "original content ingest request digest is required")
	}
	if requirePrivateDigestMatch {
		actualDigest, err := ContentIngestRequestDigest(input)
		if err != nil || actualDigest != requestDigest {
			return ContentIngestResult{}, domainError("idempotency_conflict", "content ingest request digest does not match original input")
		}
	}
	if err := rejectCallerIntakePolicyMetadata(input); err != nil {
		return ContentIngestResult{}, err
	}
	policy, err := s.ResolveContentIntakePolicySnapshot(ctx, resolver, now.UTC())
	if err != nil {
		return ContentIngestResult{}, err
	}
	access, retention := policy.Access, policy.Retention
	if access.PolicyID == "" || access.Revision < 1 || access.ContentHash == "" || retention.PolicyID == "" || retention.Revision < 1 || retention.ContentHash == "" {
		return ContentIngestResult{}, domainError("content_policy_integrity_failed", "resolved intake policy references are incomplete")
	}
	if retention.IdempotencyLifetimeSeconds < int64(reservationLease/time.Second) {
		return ContentIngestResult{}, domainError("content_policy_invalid", "signed idempotency lifetime is shorter than the reservation lease")
	}
	sourceExpiry := now.UTC().Add(time.Duration(retention.SourceLifetimeSeconds) * time.Second)
	draftExpiry := now.UTC().Add(time.Duration(retention.DraftLifetimeSeconds) * time.Second)
	input = cloneContentIngest(input)
	if input.ExistingSourceArtifactID == "" {
		input.MetadataBLOB, err = bindContentIntakePolicyMetadata(input.MetadataBLOB, policy, sourceExpiry, draftExpiry)
		if err != nil {
			return ContentIngestResult{}, err
		}
	}
	// The caller-bound digest is passed separately from the enriched input.
	return s.finalizeContentIngest(ctx, reservation, input, sourceExpiry, auditID, now, &requestDigest, &policy, includePublicProjection)
}

// ResolveContentIntakePolicy returns policy values only from the resolver
// cryptographically bound to this store and the trusted caller context.
func (s *Store) ResolveContentIntakePolicy(ctx context.Context, resolver *ContentPolicyResolver, now time.Time) (ContentAccessPolicy, ContentRetentionPolicy, error) {
	policy, err := s.ResolveContentIntakePolicySnapshot(ctx, resolver, now)
	return policy.Access, policy.Retention, err
}

// ResolveContentIntakePolicySnapshot verifies the resolver is bound to this
// store and returns the exact signed active bundle identity with its policies.
func (s *Store) ResolveContentIntakePolicySnapshot(ctx context.Context, resolver *ContentPolicyResolver, now time.Time) (ContentIntakePolicySnapshot, error) {
	if s == nil || s.db == nil || resolver == nil || resolver.store != s {
		return ContentIntakePolicySnapshot{}, domainError("content_policy_unavailable", "a configured resolver bound to this assurance store is required")
	}
	if _, err := requireContentPrincipal(ctx); err != nil {
		return ContentIntakePolicySnapshot{}, err
	}
	bundle, tenant, err := resolver.active(ctx, identity.PermissionContentIngest)
	if err != nil {
		return ContentIntakePolicySnapshot{}, err
	}
	principal, _ := identity.PrincipalFromContext(ctx)
	var access *ContentAccessPolicy
	for i := range bundle.Bundle.ContentAccessPolicies {
		policy := &bundle.Bundle.ContentAccessPolicies[i]
		if policy.PolicyID == resolver.accessPolicyID && policy.State == "active" && policy.TenantID == tenant && policy.ActorObjectID == principal.ObjectID && contentPolicyContains(policy.Capabilities, "content.ingest") {
			access = policy
			break
		}
	}
	if access == nil || expired(access.ValidUntil, now.UTC()) {
		return ContentIntakePolicySnapshot{}, domainError("content_policy_denied", "active content ingest policy is missing or expired")
	}
	var retention *ContentRetentionPolicy
	for i := range bundle.Bundle.ContentRetentionPolicies {
		policy := &bundle.Bundle.ContentRetentionPolicies[i]
		if policy.State == "active" {
			if retention != nil {
				return ContentIntakePolicySnapshot{}, domainError("content_policy_ambiguous", "an explicit content retention policy selector is required")
			}
			retention = policy
		}
	}
	if retention == nil {
		return ContentIntakePolicySnapshot{}, domainError("content_policy_missing", "active content retention policy is missing")
	}
	return ContentIntakePolicySnapshot{Access: *access, Retention: *retention, ActiveBundleHash: bundle.ContentHash, ActiveBundleVersion: bundle.Bundle.BundleVersion}, nil
}

func (s *Store) finalizeContentIngest(ctx context.Context, reservation ReservationResult, input ContentIngest, retentionUntil time.Time, auditID string, now time.Time, requestDigestOverride *Digest, policySnapshot *ContentIntakePolicySnapshot, includePublicProjection bool) (ContentIngestResult, error) {
	principal, err := requireContentPrincipal(ctx)
	if err != nil {
		return ContentIngestResult{}, err
	}
	now = now.UTC()
	retentionUntil = retentionUntil.UTC()
	if !retentionUntil.After(now) {
		return ContentIngestResult{}, domainError("policy_blocked", "explicit content retention expiry must be in the future")
	}
	if auditID == "" {
		return ContentIngestResult{}, domainError("invalid_request", "content ingest audit ID is required")
	}
	if policySnapshot == nil {
		if err := rejectCallerIntakePolicyMetadata(input); err != nil {
			return ContentIngestResult{}, err
		}
	}
	canonicalRequest, requestDigest, normalized, normalizedDrafts, metadata, mediaType, detectedType, err := validateContentIngest(input)
	if err != nil {
		return ContentIngestResult{}, err
	}
	if len(canonicalRequest) > contentRequestMaxBytes {
		return ContentIngestResult{}, domainError("invalid_request", "canonical content ingest request exceeds byte limit")
	}
	if requestDigestOverride != nil {
		requestDigest = *requestDigestOverride
	}
	if reservation.Disposition != ReservationOwned || reservation.State != ReservationStateReserved || reservation.RecordID == "" || reservation.OwnerNonce == "" {
		return ContentIngestResult{}, domainError("idempotency_in_progress", "content ingest reservation is not owned")
	}
	if err := s.RequireRichAudit(ctx); err != nil {
		return ContentIngestResult{}, err
	}
	if s == nil || s.db == nil {
		return ContentIngestResult{}, ErrNotReady
	}

	tenant := principal.TenantID
	actor := principal.AuditActor()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ContentIngestResult{}, fmt.Errorf("assurance: begin content intake: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var storedActor, storedTool, storedAction, storedRequestDigest, storedOwnerNonce, leaseExpiry, retentionExpiry, correlationID, state string
	if err := tx.QueryRowContext(ctx, `SELECT actor_id,tool,action,request_digest,owner_nonce,lease_expires_at,expires_at,correlation_id,state
	 FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=?`, tenant, reservation.RecordID).Scan(
		&storedActor, &storedTool, &storedAction, &storedRequestDigest, &storedOwnerNonce, &leaseExpiry, &retentionExpiry, &correlationID, &state); err != nil {
		if err == sql.ErrNoRows {
			return ContentIngestResult{}, domainError("idempotency_conflict", "content ingest reservation was not found")
		}
		return ContentIngestResult{}, fmt.Errorf("assurance: read content reservation: %w", err)
	}
	if storedActor != actor || storedTool != "workiva_content_placement" || storedAction != "ingest" || storedRequestDigest != digestHex(requestDigest) || subtle.ConstantTimeCompare([]byte(storedOwnerNonce), []byte(reservation.OwnerNonce)) != 1 {
		return ContentIngestResult{}, domainError("idempotency_conflict", "content ingest reservation binding does not match")
	}
	storedRetainUntil, retainErr := time.Parse(time.RFC3339Nano, retentionExpiry)
	if retainErr != nil || !storedRetainUntil.After(now) {
		return ContentIngestResult{}, domainError("idempotency_in_progress", "content ingest reservation retention expired")
	}
	if (requestDigestOverride == nil) != (policySnapshot == nil) {
		return ContentIngestResult{}, domainError("content_policy_integrity_failed", "policy-bound content finalization is incomplete")
	}
	if includePublicProjection && (requestDigestOverride == nil || policySnapshot == nil) {
		return ContentIngestResult{}, domainError("content_policy_integrity_failed", "public content projection requires signed policy-bound finalization")
	}
	if policySnapshot != nil {
		wantRetainUntil := now.Add(time.Duration(policySnapshot.Retention.IdempotencyLifetimeSeconds) * time.Second)
		if !storedRetainUntil.Equal(wantRetainUntil) {
			return ContentIngestResult{}, domainError("content_policy_changed", "reservation horizon does not match the signed idempotency lifetime")
		}
		if err := verifyActiveContentBundleTx(ctx, tx, tenant, *policySnapshot); err != nil {
			return ContentIngestResult{}, err
		}
	}
	if state != string(ReservationStateReserved) {
		return ContentIngestResult{}, domainError("idempotency_in_progress", "content ingest reservation is no longer active")
	}
	leaseUntil, err := time.Parse(time.RFC3339Nano, leaseExpiry)
	if err != nil || !leaseUntil.After(now) {
		return ContentIngestResult{}, domainError("idempotency_in_progress", "content ingest reservation lease expired")
	}

	sourceID := "src_" + uuid.NewString()
	sourceBytes := append([]byte(nil), input.SourceBytes...)
	sourceHash := digestHex(HashBytes(sourceBytes))
	sourceMetadata := metadata
	if input.ExistingSourceArtifactID != "" {
		if input.ExpectedExistingSourceSHA256 == "" {
			return ContentIngestResult{}, domainError("artifact_hash_mismatch", "re-ingest requires the expected existing source digest")
		}
		var storedBytes, storedMetadata []byte
		var storedHash, storedMetadataHash, storedExpires, storedMediaType string
		if err := tx.QueryRowContext(ctx, `SELECT source_bytes,source_sha256,metadata_blob,metadata_sha256,expires_at,media_type
		 FROM assurance_content_sources WHERE tenant_id=? AND actor_id=? AND source_artifact_id=?`, tenant, actor, input.ExistingSourceArtifactID).Scan(
			&storedBytes, &storedHash, &storedMetadata, &storedMetadataHash, &storedExpires, &storedMediaType); err != nil {
			return ContentIngestResult{}, domainError("not_found_or_forbidden", "existing source is unavailable")
		}
		storedMetadataCanonical, metadataErr := CanonicalJSONBytes(storedMetadata)
		if digestHex(HashBytes(storedBytes)) != storedHash || digestHex(HashBytes(storedMetadata)) != storedMetadataHash || metadataErr != nil || !bytes.Equal(storedMetadataCanonical, storedMetadata) {
			return ContentIngestResult{}, domainError("artifact_hash_mismatch", "stored source integrity verification failed")
		}
		if sourceHash != storedHash || input.ExpectedExistingSourceSHA256 != storedHash || mediaType != storedMediaType {
			return ContentIngestResult{}, domainError("artifact_hash_mismatch", "supplied source bytes do not match the owned source")
		}
		expiresAt, err := time.Parse(time.RFC3339Nano, storedExpires)
		if err != nil || !expiresAt.After(now) {
			return ContentIngestResult{}, domainError("not_found_or_forbidden", "existing source is unavailable")
		}
		if policySnapshot != nil {
			provenance, found, provenanceErr := parseContentIntakePolicyMetadata(storedMetadata)
			if provenanceErr != nil || !found || provenance.ActiveBundleHash == "" || provenance.ActiveBundleVersion < 1 || provenance.SourceExpiresAt != formatTimestamp(expiresAt) {
				return ContentIngestResult{}, domainError("artifact_hash_mismatch", "existing source lacks verified server intake policy provenance")
			}
			if len(input.Items) > 0 || len(input.Drafts) > 0 {
				draftExpiry, parseErr := time.Parse(time.RFC3339Nano, provenance.DraftExpiresAt)
				if parseErr != nil || !draftExpiry.After(now) || draftExpiry.After(expiresAt) {
					return ContentIngestResult{}, domainError("not_found_or_forbidden", "existing source draft retention is unavailable")
				}
			}
			if includePublicProjection {
				retentionUntil = expiresAt
			}
		}
		sourceID = input.ExistingSourceArtifactID
		sourceBytes = storedBytes
		sourceHash = storedHash
		sourceMetadata = storedMetadata
	} else if input.ExpectedExistingSourceSHA256 != "" {
		return ContentIngestResult{}, domainError("invalid_request", "expected existing source digest requires an existing source ID")
	}

	result := ContentIngestResult{
		SourceArtifactID: sourceID,
		SourceSHA256:     sourceHash,
		ItemIDs:          make(map[string]string, len(normalized)),
		DraftIDs:         make(map[string]string, len(input.Drafts)),
	}
	if input.ExistingSourceArtifactID == "" {
		_, err = tx.ExecContext(ctx, `INSERT INTO assurance_content_sources
		 (tenant_id,source_artifact_id,actor_id,media_type,detected_media_type,filename,source_bytes,source_sha256,metadata_blob,metadata_sha256,expires_at,created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, tenant, sourceID, actor, mediaType, detectedType, input.Filename, sourceBytes,
			sourceHash, sourceMetadata, digestHex(HashBytes(sourceMetadata)), formatTimestamp(retentionUntil), formatTimestamp(now))
		if err != nil {
			return ContentIngestResult{}, fmt.Errorf("assurance: insert content source: %w", err)
		}
	}
	for i := range normalized {
		item := &normalized[i]
		item.id = "itm_" + uuid.NewString()
		result.ItemIDs[item.localID] = item.id
		_, err = tx.ExecContext(ctx, `INSERT INTO assurance_content_items
			 (tenant_id,item_id,source_artifact_id,actor_id,item_text,item_sha256,metadata_blob,metadata_sha256,created_at)
			 VALUES(?,?,?,?,?,?,?,?,?)`, tenant, item.id, sourceID, actor, item.text, digestHex(HashBytes([]byte(item.text))), item.metadata, digestHex(HashBytes(item.metadata)), formatTimestamp(now))
		if err != nil {
			return ContentIngestResult{}, fmt.Errorf("assurance: insert extracted item: %w", err)
		}
	}
	for i := range normalizedDrafts {
		draft := &normalizedDrafts[i]
		draft.id = "drf_" + uuid.NewString()
		itemIDs := make([]string, 0, len(draft.itemLocalIDs))
		for _, localID := range draft.itemLocalIDs {
			itemID, ok := result.ItemIDs[localID]
			if !ok {
				return ContentIngestResult{}, domainError("invalid_request", "draft references an unknown extracted item")
			}
			itemIDs = append(itemIDs, itemID)
		}
		canonicalItemIDs, err := CanonicalJSON(itemIDs)
		if err != nil {
			return ContentIngestResult{}, err
		}
		result.DraftIDs[draft.localID] = draft.id
		_, err = tx.ExecContext(ctx, `INSERT INTO assurance_content_drafts
			 (tenant_id,draft_artifact_id,source_artifact_id,actor_id,draft_text,draft_sha256,item_ids_json,lineage_sha256,metadata_blob,metadata_sha256,created_at)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?)`, tenant, draft.id, sourceID, actor, draft.text, digestHex(HashBytes([]byte(draft.text))), string(canonicalItemIDs), digestHex(HashBytes(canonicalItemIDs)), draft.metadata, digestHex(HashBytes(draft.metadata)), formatTimestamp(now))
		if err != nil {
			return ContentIngestResult{}, fmt.Errorf("assurance: insert content draft: %w", err)
		}
	}
	if includePublicProjection {
		projection, err := buildContentIngestPublicProjectionTx(ctx, tx, tenant, actor, sourceID, result.ItemIDs, result.DraftIDs, now)
		if err != nil {
			return ContentIngestResult{}, err
		}
		if projection.Source.SourceArtifactID != result.SourceArtifactID || projection.Source.ContentHash != result.SourceSHA256 || len(projection.ExtractedItems) != len(result.ItemIDs) || len(projection.DraftArtifacts) != len(result.DraftIDs) {
			return ContentIngestResult{}, domainError("artifact_hash_mismatch", "saved public projection does not match the reserved ingest result")
		}
		for _, item := range projection.ExtractedItems {
			if !containsContentID(result.ItemIDs, item.ExtractedItemID) {
				return ContentIngestResult{}, domainError("artifact_hash_mismatch", "saved public item set does not match the reserved ingest result")
			}
		}
		for _, draft := range projection.DraftArtifacts {
			if !containsContentID(result.DraftIDs, draft.DraftArtifactID) {
				return ContentIngestResult{}, domainError("artifact_hash_mismatch", "saved public draft set does not match the reserved ingest result")
			}
		}
		result.PublicProjection = &projection
	}

	envelope, err := CanonicalJSON(result)
	if err != nil {
		return ContentIngestResult{}, err
	}
	if _, err := s.auditLog.AppendTx(ctx, tx, audit.Entry{
		Actor: actor, Tool: "workiva_content_placement", Action: "ingest", Target: sourceID,
		AfterJSON: string(envelope), AuditID: auditID,
	}); err != nil {
		return ContentIngestResult{}, err
	}
	if err := insertContentAuditLink(ctx, tx, tenant, "content_source_artifact", sourceID, auditID, correlationID, now); err != nil {
		return ContentIngestResult{}, err
	}
	for _, id := range result.ItemIDs {
		if err := insertContentAuditLink(ctx, tx, tenant, "content_extracted_item", id, auditID, correlationID, now); err != nil {
			return ContentIngestResult{}, err
		}
	}
	for _, id := range result.DraftIDs {
		if err := insertContentAuditLink(ctx, tx, tenant, "content_draft_artifact", id, auditID, correlationID, now); err != nil {
			return ContentIngestResult{}, err
		}
	}
	if err := insertContentAuditLink(ctx, tx, tenant, "idempotency_record", reservation.RecordID, auditID, correlationID, now); err != nil {
		return ContentIngestResult{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET state='sealed',response_envelope=?,response_hash=?,response_status='success',
	 entity_reference=?,audit_id=?,terminal_at=?,lease_expires_at=? WHERE tenant_id=? AND record_id=? AND actor_id=? AND tool=? AND action=?
	 AND state='reserved' AND owner_nonce=? AND request_digest=? AND lease_expires_at>?`, string(envelope), digestHex(HashBytes(envelope)), sourceID,
		auditID, formatTimestamp(now), formatTimestamp(now), tenant, reservation.RecordID, actor, "workiva_content_placement", "ingest",
		reservation.OwnerNonce, digestHex(requestDigest), formatTimestamp(now))
	if err != nil {
		return ContentIngestResult{}, fmt.Errorf("assurance: seal content intake reservation: %w", err)
	}
	if err := requireOneTransition(res); err != nil {
		return ContentIngestResult{}, err
	}
	if policySnapshot != nil {
		if err := verifyActiveContentBundleTx(ctx, tx, tenant, *policySnapshot); err != nil {
			return ContentIngestResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ContentIngestResult{}, fmt.Errorf("assurance: commit content intake: %w", err)
	}
	return result, nil
}

func verifyActiveContentBundleTx(ctx context.Context, tx *sql.Tx, tenant string, snapshot ContentIntakePolicySnapshot) error {
	var hash string
	var version int
	err := tx.QueryRowContext(ctx, `SELECT content_hash,bundle_version FROM assurance_active_bundles WHERE tenant_id=? AND singleton=1`, tenant).Scan(&hash, &version)
	if err != nil || hash != snapshot.ActiveBundleHash || version != snapshot.ActiveBundleVersion {
		return domainError("content_policy_changed", "active signed policy bundle changed during content intake")
	}
	return nil
}

func buildContentIngestPublicProjectionTx(ctx context.Context, tx *sql.Tx, tenant, actor, sourceID string, itemIDs, draftIDs map[string]string, now time.Time) (ContentIngestPublicProjection, error) {
	var sourceBytes, sourceMetadata []byte
	var sourceHash, sourceMetadataHash, mediaType, sourceExpiry string
	if err := tx.QueryRowContext(ctx, `SELECT source_bytes,source_sha256,metadata_blob,metadata_sha256,media_type,expires_at
		FROM assurance_content_sources WHERE tenant_id=? AND actor_id=? AND source_artifact_id=?`, tenant, actor, sourceID).Scan(
		&sourceBytes, &sourceHash, &sourceMetadata, &sourceMetadataHash, &mediaType, &sourceExpiry); err != nil {
		return ContentIngestPublicProjection{}, domainError("content_projection_invalid", "saved source is unavailable for public projection")
	}
	if digestHex(HashBytes(sourceBytes)) != sourceHash || digestHex(HashBytes(sourceMetadata)) != sourceMetadataHash {
		return ContentIngestPublicProjection{}, domainError("artifact_hash_mismatch", "saved source integrity verification failed")
	}
	canonicalMetadata, err := CanonicalJSONBytes(sourceMetadata)
	if err != nil || !bytes.Equal(canonicalMetadata, sourceMetadata) {
		return ContentIngestPublicProjection{}, domainError("artifact_hash_mismatch", "saved source metadata is not canonical")
	}
	provenance, found, err := parseContentIntakePolicyMetadata(sourceMetadata)
	if err != nil || !found {
		return ContentIngestPublicProjection{}, domainError("content_projection_invalid", "source lacks sealed intake retention provenance")
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, sourceExpiry)
	if err != nil || !expiresAt.After(now) || formatTimestamp(expiresAt) != provenance.SourceExpiresAt {
		return ContentIngestPublicProjection{}, domainError("content_projection_invalid", "saved source expiry does not match its signed provenance")
	}
	draftExpiry, err := time.Parse(time.RFC3339Nano, provenance.DraftExpiresAt)
	if err != nil || draftExpiry.After(expiresAt) {
		return ContentIngestPublicProjection{}, domainError("content_projection_invalid", "saved derived-content expiry exceeds source retention")
	}
	if (len(itemIDs) > 0 || len(draftIDs) > 0) && !draftExpiry.After(now) {
		return ContentIngestPublicProjection{}, domainError("content_projection_invalid", "saved derived-content expiry has elapsed")
	}
	retention := ContentIngestRetentionReference{PolicyID: provenance.RetentionPolicy.PolicyID, Revision: provenance.RetentionPolicy.Revision, ContentHash: provenance.RetentionPolicy.ContentHash, Class: "source"}
	derivedRetention := retention
	derivedRetention.Class = "draft"
	projection := ContentIngestPublicProjection{
		Kind: "ingest",
		Source: ContentIngestSourceArtifactProjection{
			SourceArtifactID: sourceID, Revision: 1, ContentHash: sourceHash, ByteLength: len(sourceBytes), MediaType: mediaType, RetentionPolicy: retention,
		},
		VerifiedEvidenceRefs: []ContentIngestVerifiedEvidenceRef{},
		ExtractedItems:       make([]ContentIngestItemProjection, 0, len(itemIDs)),
		DraftArtifacts:       make([]ContentIngestDraftProjection, 0, len(draftIDs)),
	}
	sourceBinding := ContentIngestSourceBinding{SourceArtifactID: sourceID, Revision: 1, ContentHash: sourceHash}
	itemLocalIDs := make([]string, 0, len(itemIDs))
	for localID := range itemIDs {
		itemLocalIDs = append(itemLocalIDs, localID)
	}
	sort.Strings(itemLocalIDs)
	for _, localID := range itemLocalIDs {
		id := itemIDs[localID]
		var storedText, textHash, storedMetadataHash string
		var storedMetadata []byte
		var storedSource, storedActor string
		if err := tx.QueryRowContext(ctx, `SELECT item_text,item_sha256,metadata_blob,metadata_sha256,source_artifact_id,actor_id
			FROM assurance_content_items WHERE tenant_id=? AND item_id=?`, tenant, id).Scan(&storedText, &textHash, &storedMetadata, &storedMetadataHash, &storedSource, &storedActor); err != nil {
			return ContentIngestPublicProjection{}, domainError("content_projection_invalid", "saved extracted item is unavailable")
		}
		if storedActor != actor || storedSource != sourceID || digestHex(HashBytes([]byte(storedText))) != textHash || digestHex(HashBytes(storedMetadata)) != storedMetadataHash {
			return ContentIngestPublicProjection{}, domainError("artifact_hash_mismatch", "saved extracted item integrity or source binding failed")
		}
		canonical, err := CanonicalJSONBytes(storedMetadata)
		if err != nil || !bytes.Equal(canonical, storedMetadata) {
			return ContentIngestPublicProjection{}, domainError("artifact_hash_mismatch", "saved extracted item metadata is not canonical")
		}
		projection.ExtractedItems = append(projection.ExtractedItems, ContentIngestItemProjection{
			ExtractedItemID: id, Revision: 1, ContentHash: textHash, Source: sourceBinding, CandidateOnly: true, RetentionPolicy: derivedRetention,
		})
	}
	draftLocalIDs := make([]string, 0, len(draftIDs))
	for localID := range draftIDs {
		draftLocalIDs = append(draftLocalIDs, localID)
	}
	sort.Strings(draftLocalIDs)
	for _, localID := range draftLocalIDs {
		id := draftIDs[localID]
		var storedText, textHash, itemIDsJSON, lineageHash, metadataHash, storedSource, storedActor string
		var storedMetadata []byte
		if err := tx.QueryRowContext(ctx, `SELECT draft_text,draft_sha256,item_ids_json,lineage_sha256,metadata_blob,metadata_sha256,source_artifact_id,actor_id
			FROM assurance_content_drafts WHERE tenant_id=? AND draft_artifact_id=?`, tenant, id).Scan(
			&storedText, &textHash, &itemIDsJSON, &lineageHash, &storedMetadata, &metadataHash, &storedSource, &storedActor); err != nil {
			return ContentIngestPublicProjection{}, domainError("content_projection_invalid", "saved draft is unavailable")
		}
		if storedActor != actor || storedSource != sourceID || digestHex(HashBytes([]byte(storedText))) != textHash || digestHex(HashBytes(storedMetadata)) != metadataHash {
			return ContentIngestPublicProjection{}, domainError("artifact_hash_mismatch", "saved draft integrity or source binding failed")
		}
		canonicalMetadata, err := CanonicalJSONBytes(storedMetadata)
		if err != nil || !bytes.Equal(canonicalMetadata, storedMetadata) {
			return ContentIngestPublicProjection{}, domainError("artifact_hash_mismatch", "saved draft metadata is not canonical")
		}
		var referencedItems []string
		if err := json.Unmarshal([]byte(itemIDsJSON), &referencedItems); err != nil || len(referencedItems) == 0 {
			return ContentIngestPublicProjection{}, domainError("artifact_hash_mismatch", "saved draft item lineage is invalid")
		}
		canonicalItemIDs, err := CanonicalJSON(referencedItems)
		if err != nil || string(canonicalItemIDs) != itemIDsJSON || digestHex(HashBytes(canonicalItemIDs)) != lineageHash {
			return ContentIngestPublicProjection{}, domainError("artifact_hash_mismatch", "saved draft item lineage integrity failed")
		}
		seen := make(map[string]struct{}, len(referencedItems))
		for _, itemID := range referencedItems {
			if _, exists := seen[itemID]; exists {
				return ContentIngestPublicProjection{}, domainError("artifact_hash_mismatch", "saved draft item lineage contains duplicates")
			}
			seen[itemID] = struct{}{}
			var itemText, itemHash, itemMetaHash, itemSource, itemActor string
			var itemMetadata []byte
			if err := tx.QueryRowContext(ctx, `SELECT item_text,item_sha256,metadata_blob,metadata_sha256,source_artifact_id,actor_id
				FROM assurance_content_items WHERE tenant_id=? AND item_id=?`, tenant, itemID).Scan(&itemText, &itemHash, &itemMetadata, &itemMetaHash, &itemSource, &itemActor); err != nil {
				return ContentIngestPublicProjection{}, domainError("artifact_hash_mismatch", "saved draft references an unavailable item")
			}
			canonicalItemMetadata, err := CanonicalJSONBytes(itemMetadata)
			if itemActor != actor || itemSource != sourceID || digestHex(HashBytes([]byte(itemText))) != itemHash || digestHex(HashBytes(itemMetadata)) != itemMetaHash || err != nil || !bytes.Equal(canonicalItemMetadata, itemMetadata) {
				return ContentIngestPublicProjection{}, domainError("artifact_hash_mismatch", "saved draft item reference integrity failed")
			}
		}
		var metadataObject struct {
			Origin struct {
				Kind string `json:"kind"`
			} `json:"origin"`
		}
		if err := json.Unmarshal(storedMetadata, &metadataObject); err != nil || !validContentOriginKind(metadataObject.Origin.Kind) {
			return ContentIngestPublicProjection{}, domainError("content_projection_invalid", "saved draft origin kind is invalid")
		}
		projection.DraftArtifacts = append(projection.DraftArtifacts, ContentIngestDraftProjection{
			DraftArtifactID: id, Revision: 1, ContentHash: textHash,
			Lineage:    ContentIngestDraftLineage{Kind: "source_bytes", Source: sourceBinding},
			OriginKind: metadataObject.Origin.Kind, CandidateOnly: true, RetentionPolicy: derivedRetention,
		})
	}
	return projection, nil
}

func validContentOriginKind(kind string) bool {
	switch kind {
	case "analyst", "agent_flow", "copilot", "other":
		return true
	default:
		return false
	}
}

func containsContentID(ids map[string]string, id string) bool {
	for _, candidateID := range ids {
		if candidateID == id {
			return true
		}
	}
	return false
}

func cloneContentIngest(input ContentIngest) ContentIngest {
	input.SourceBytes = append([]byte(nil), input.SourceBytes...)
	input.MetadataBLOB = append([]byte(nil), input.MetadataBLOB...)
	input.Items = append([]ContentItemInput(nil), input.Items...)
	for i := range input.Items {
		input.Items[i].MetadataBLOB = append([]byte(nil), input.Items[i].MetadataBLOB...)
	}
	input.Drafts = append([]ContentDraftInput(nil), input.Drafts...)
	for i := range input.Drafts {
		input.Drafts[i].ItemLocalIDs = append([]string(nil), input.Drafts[i].ItemLocalIDs...)
		input.Drafts[i].MetadataBLOB = append([]byte(nil), input.Drafts[i].MetadataBLOB...)
	}
	return input
}

type contentIntakePolicyMetadata struct {
	ActiveBundleHash    string `json:"active_bundle_hash"`
	ActiveBundleVersion int    `json:"active_bundle_version"`
	AccessPolicy        struct {
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
}

func rejectCallerIntakePolicyMetadata(input ContentIngest) error {
	for _, raw := range [][]byte{input.MetadataBLOB} {
		if err := rejectIntakePolicyKey(raw); err != nil {
			return err
		}
	}
	for _, item := range input.Items {
		if err := rejectIntakePolicyKey(item.MetadataBLOB); err != nil {
			return err
		}
	}
	for _, draft := range input.Drafts {
		if err := rejectIntakePolicyKey(draft.MetadataBLOB); err != nil {
			return err
		}
	}
	return nil
}

func rejectIntakePolicyKey(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return domainError("invalid_request", "content metadata must be a JSON object")
	}
	if _, exists := object["intake_policy"]; exists {
		return domainError("invalid_request", "intake_policy metadata is server-owned")
	}
	return nil
}

func bindContentIntakePolicyMetadata(raw []byte, policy ContentIntakePolicySnapshot, sourceExpiry, draftExpiry time.Time) ([]byte, error) {
	canonical, err := canonicalContentMetadata(raw)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &object); err != nil || object == nil {
		return nil, domainError("invalid_request", "source metadata must be a JSON object")
	}
	if _, exists := object["intake_policy"]; exists {
		return nil, domainError("invalid_request", "intake_policy metadata is server-owned")
	}
	metadata := contentIntakePolicyMetadata{}
	metadata.ActiveBundleHash, metadata.ActiveBundleVersion = policy.ActiveBundleHash, policy.ActiveBundleVersion
	metadata.AccessPolicy.PolicyID, metadata.AccessPolicy.Revision, metadata.AccessPolicy.ContentHash, metadata.AccessPolicy.ValidUntil = policy.Access.PolicyID, policy.Access.Revision, policy.Access.ContentHash, policy.Access.ValidUntil
	metadata.RetentionPolicy.PolicyID, metadata.RetentionPolicy.Revision, metadata.RetentionPolicy.ContentHash = policy.Retention.PolicyID, policy.Retention.Revision, policy.Retention.ContentHash
	metadata.SourceExpiresAt, metadata.DraftExpiresAt = formatTimestamp(sourceExpiry), formatTimestamp(draftExpiry)
	encoded, err := CanonicalJSON(metadata)
	if err != nil {
		return nil, err
	}
	object["intake_policy"] = encoded
	bound, err := CanonicalJSON(object)
	if err != nil || len(bound) > contentMetadataMax {
		return nil, domainError("invalid_request", "policy-bound source metadata exceeds byte limit")
	}
	return bound, nil
}

func parseContentIntakePolicyMetadata(raw []byte) (contentIntakePolicyMetadata, bool, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return contentIntakePolicyMetadata{}, false, domainError("artifact_hash_mismatch", "stored source metadata is not an object")
	}
	encoded, exists := object["intake_policy"]
	if !exists {
		return contentIntakePolicyMetadata{}, false, nil
	}
	var metadata contentIntakePolicyMetadata
	if err := json.Unmarshal(encoded, &metadata); err != nil || !contentPolicyHash.MatchString(metadata.ActiveBundleHash) || metadata.ActiveBundleVersion < 1 || metadata.AccessPolicy.PolicyID == "" || metadata.AccessPolicy.Revision < 1 || !contentPolicyHash.MatchString(metadata.AccessPolicy.ContentHash) || metadata.RetentionPolicy.PolicyID == "" || metadata.RetentionPolicy.Revision < 1 || !contentPolicyHash.MatchString(metadata.RetentionPolicy.ContentHash) {
		return contentIntakePolicyMetadata{}, true, domainError("artifact_hash_mismatch", "stored intake policy provenance is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, metadata.AccessPolicy.ValidUntil); err != nil {
		return contentIntakePolicyMetadata{}, true, domainError("artifact_hash_mismatch", "stored intake access expiry is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, metadata.SourceExpiresAt); err != nil {
		return contentIntakePolicyMetadata{}, true, domainError("artifact_hash_mismatch", "stored intake source expiry is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, metadata.DraftExpiresAt); err != nil {
		return contentIntakePolicyMetadata{}, true, domainError("artifact_hash_mismatch", "stored intake draft expiry is invalid")
	}
	return metadata, true, nil
}

// GetContentSource returns a source only to its trusted delegated owner and
// verifies both the source-byte digest and canonical metadata digest on read.
func (s *Store) GetContentSource(ctx context.Context, sourceArtifactID string) (ContentSourceRecord, error) {
	return s.getContentSourceAt(ctx, sourceArtifactID, time.Now().UTC())
}

func (s *Store) getContentSourceAt(ctx context.Context, sourceArtifactID string, now time.Time) (ContentSourceRecord, error) {
	principal, err := requireContentPrincipal(ctx)
	if err != nil {
		return ContentSourceRecord{}, err
	}
	if s == nil || s.db == nil || sourceArtifactID == "" {
		return ContentSourceRecord{}, domainError("not_found_or_forbidden", "source is unavailable")
	}
	var source ContentSourceRecord
	var metadata []byte
	var expiresAt, createdAt string
	err = s.db.QueryRowContext(ctx, `SELECT source_artifact_id,tenant_id,actor_id,media_type,detected_media_type,filename,source_bytes,source_sha256,metadata_blob,metadata_sha256,expires_at,created_at
	 FROM assurance_content_sources WHERE tenant_id=? AND actor_id=? AND source_artifact_id=?`, principal.TenantID, principal.AuditActor(), sourceArtifactID).Scan(
		&source.ID, &source.TenantID, &source.ActorID, &source.MediaType, &source.DetectedMediaType, &source.Filename,
		&source.SourceBytes, &source.SourceSHA256, &metadata, &source.MetadataSHA256, &expiresAt, &createdAt)
	if err != nil {
		return ContentSourceRecord{}, domainError("not_found_or_forbidden", "source is unavailable")
	}
	canonicalMetadata, err := CanonicalJSONBytes(metadata)
	if err != nil || !bytes.Equal(canonicalMetadata, metadata) || digestHex(HashBytes(metadata)) != source.MetadataSHA256 || digestHex(HashBytes(source.SourceBytes)) != source.SourceSHA256 {
		return ContentSourceRecord{}, domainError("artifact_hash_mismatch", "stored source integrity verification failed")
	}
	source.MetadataBLOB = append([]byte(nil), metadata...)
	source.SourceBytes = append([]byte(nil), source.SourceBytes...)
	source.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || !source.ExpiresAt.After(now.UTC()) {
		return ContentSourceRecord{}, domainError("not_found_or_forbidden", "source is unavailable")
	}
	source.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return ContentSourceRecord{}, domainError("artifact_hash_mismatch", "stored source creation metadata is invalid")
	}
	return source, nil
}

func requireContentPrincipal(ctx context.Context) (identity.Principal, error) {
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok || principal.TokenType != identity.TokenTypeDelegated || principal.UsedSubjectFallback || !principal.HasPermission(identity.PermissionContentIngest) || !isCanonicalUUID(principal.TenantID) || !isCanonicalUUID(principal.ObjectID) {
		return identity.Principal{}, domainError("forbidden", "trusted delegated content.ingest capability required")
	}
	return principal, nil
}

func isCanonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == strings.ToLower(value)
}

func insertContentAuditLink(ctx context.Context, tx *sql.Tx, tenant, kind, id, auditID, correlationID string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO assurance_audit_links
	 (tenant_id,link_id,entity_kind,entity_id,audit_id,request_id,correlation_id,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		tenant, uuid.NewString(), kind, id, auditID, RequestIDFromContext(ctx), correlationID, formatTimestamp(now))
	return err
}

func validateContentIngest(input ContentIngest) ([]byte, Digest, []normalizedContentItem, []normalizedContentDraft, []byte, string, string, error) {
	canonicalRequest, err := CanonicalJSON(input)
	if err != nil {
		return nil, Digest{}, nil, nil, nil, "", "", domainError("invalid_request", "content ingest request is not canonical JSON")
	}
	requestDigest := HashBytes(canonicalRequest)
	if len(canonicalRequest) > contentRequestMaxBytes || len(input.SourceBytes) == 0 || len(input.SourceBytes) > contentSourceMaxBytes {
		return nil, Digest{}, nil, nil, nil, "", "", domainError("invalid_request", "content ingest request or source exceeds byte bounds")
	}
	if len(input.Items) > contentItemsMax || len(input.Drafts) > contentDraftsMax {
		return nil, Digest{}, nil, nil, nil, "", "", domainError("invalid_request", "content ingest item or draft count exceeds bounds")
	}
	if len(input.Filename) > contentFilenameMax || !utf8.ValidString(input.Filename) || strings.ContainsAny(input.Filename, "/\\\r\n\x00") {
		return nil, Digest{}, nil, nil, nil, "", "", domainError("invalid_request", "content filename is invalid")
	}
	mediaType, detectedType, err := validateContentMedia(input.MediaType, input.SourceBytes)
	if err != nil {
		return nil, Digest{}, nil, nil, nil, "", "", err
	}
	if mediaType == "text/plain" && len(input.SourceBytes) > contentTextSourceMax {
		return nil, Digest{}, nil, nil, nil, "", "", domainError("invalid_request", "text source exceeds byte limit")
	}
	metadata, err := canonicalContentMetadata(input.MetadataBLOB)
	if err != nil {
		return nil, Digest{}, nil, nil, nil, "", "", err
	}
	items := make([]normalizedContentItem, 0, len(input.Items))
	itemIDs := make(map[string]struct{}, len(input.Items))
	for _, item := range input.Items {
		if !validContentLocalID(item.LocalID) || !utf8.ValidString(item.Text) || len(item.Text) == 0 || len([]byte(item.Text)) > contentItemMaxBytes {
			return nil, Digest{}, nil, nil, nil, "", "", domainError("invalid_request", "extracted item ID or text is invalid")
		}
		if _, duplicate := itemIDs[item.LocalID]; duplicate {
			return nil, Digest{}, nil, nil, nil, "", "", domainError("invalid_request", "extracted item local IDs must be unique")
		}
		itemIDs[item.LocalID] = struct{}{}
		itemMetadata, err := canonicalContentMetadata(item.MetadataBLOB)
		if err != nil {
			return nil, Digest{}, nil, nil, nil, "", "", err
		}
		items = append(items, normalizedContentItem{localID: item.LocalID, text: item.Text, metadata: itemMetadata})
	}
	drafts := make([]normalizedContentDraft, 0, len(input.Drafts))
	draftIDs := make(map[string]struct{}, len(input.Drafts))
	for _, draft := range input.Drafts {
		if !validContentLocalID(draft.LocalID) || !utf8.ValidString(draft.Text) || len(draft.Text) == 0 || len([]byte(draft.Text)) > contentDraftMaxBytes || len(draft.ItemLocalIDs) == 0 || len(draft.ItemLocalIDs) > contentItemsMax {
			return nil, Digest{}, nil, nil, nil, "", "", domainError("invalid_request", "draft ID, text, or item references are invalid")
		}
		if _, duplicate := draftIDs[draft.LocalID]; duplicate {
			return nil, Digest{}, nil, nil, nil, "", "", domainError("invalid_request", "draft local IDs must be unique")
		}
		draftIDs[draft.LocalID] = struct{}{}
		refs := make([]string, 0, len(draft.ItemLocalIDs))
		seenRefs := make(map[string]struct{}, len(draft.ItemLocalIDs))
		for _, localID := range draft.ItemLocalIDs {
			if _, ok := itemIDs[localID]; !ok {
				return nil, Digest{}, nil, nil, nil, "", "", domainError("invalid_request", "draft references an unknown extracted item")
			}
			if _, duplicate := seenRefs[localID]; duplicate {
				return nil, Digest{}, nil, nil, nil, "", "", domainError("invalid_request", "draft item references must be unique")
			}
			seenRefs[localID] = struct{}{}
			refs = append(refs, localID)
		}
		draftMetadata, err := canonicalContentMetadata(draft.MetadataBLOB)
		if err != nil {
			return nil, Digest{}, nil, nil, nil, "", "", err
		}
		drafts = append(drafts, normalizedContentDraft{localID: draft.LocalID, text: draft.Text, itemLocalIDs: refs, metadata: draftMetadata})
	}
	// The digest is over the caller's private ContentIngest shape (including
	// exact metadata bytes), not the future public MCP projection.
	return canonicalRequest, requestDigest, items, drafts, metadata, mediaType, detectedType, nil
}

func canonicalContentMetadata(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return []byte("{}"), nil
	}
	if len(raw) > contentMetadataMax {
		return nil, domainError("invalid_request", "content metadata exceeds byte limit")
	}
	canonical, err := CanonicalJSONBytes(raw)
	if err != nil || len(canonical) > contentMetadataMax {
		return nil, domainError("invalid_request", "content metadata must be bounded valid JSON")
	}
	return canonical, nil
}

func validContentLocalID(value string) bool {
	if len(value) == 0 || len(value) > contentLocalIDMaxBytes {
		return false
	}
	for i, r := range value {
		alphanumeric := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		valid := alphanumeric || strings.ContainsRune("._:-", r)
		if !valid || (i == 0 && !alphanumeric) {
			return false
		}
	}
	return true
}

func validateContentMedia(claim string, source []byte) (string, string, error) {
	mediaType, _, err := mime.ParseMediaType(claim)
	if err != nil || mediaType == "" {
		return "", "", domainError("unsupported_media_type", "source media type is invalid")
	}
	switch mediaType {
	case "text/plain":
		if !utf8.Valid(source) || bytes.IndexByte(source, 0) >= 0 {
			return "", "", domainError("unsupported_media_type", "declared text source is not valid UTF-8 text")
		}
		return mediaType, "text/plain", nil
	case "text/csv":
		if !utf8.Valid(source) || bytes.IndexByte(source, 0) >= 0 {
			return "", "", domainError("unsupported_media_type", "declared CSV source is not valid UTF-8 text")
		}
		csvReader := csv.NewReader(bytes.NewReader(source))
		csvReader.FieldsPerRecord = -1
		if _, err := csvReader.ReadAll(); err != nil {
			return "", "", domainError("unsupported_media_type", "declared CSV source is malformed")
		}
		return mediaType, "text/csv", nil
	case "application/pdf":
		// This is only a MIME/signature consistency check. A PDF signature does
		// not establish that the document is valid or safe to parse; this storage
		// foundation does not parse PDF content.
		if !bytes.HasPrefix(source, []byte("%PDF-")) {
			return "", "", domainError("unsupported_media_type", "source bytes do not match declared PDF media type")
		}
		return mediaType, "application/pdf", nil
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		reader, err := zip.NewReader(bytes.NewReader(source), int64(len(source)))
		if err != nil {
			return "", "", domainError("unsupported_media_type", "source bytes are not a valid DOCX package")
		}
		contentTypes, document := false, false
		if len(reader.File) > 4096 {
			return "", "", domainError("invalid_request", "DOCX package contains too many entries")
		}
		for _, file := range reader.File {
			if file.Name == "[Content_Types].xml" {
				contentTypes = true
			}
			if file.Name == "word/document.xml" {
				document = true
			}
			if path.IsAbs(file.Name) || strings.Contains(file.Name, "..") || strings.ContainsRune(file.Name, '\\') {
				return "", "", domainError("unsupported_media_type", "DOCX package contains an unsafe entry name")
			}
		}
		if !contentTypes || !document {
			return "", "", domainError("unsupported_media_type", "DOCX package is missing required document parts")
		}
		return mediaType, mediaType, nil
	default:
		return "", "", domainError("unsupported_media_type", "source media type is unsupported")
	}
}
