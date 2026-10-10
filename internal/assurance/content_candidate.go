package assurance

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
)

type ContentCandidateKind string

const (
	ContentCandidateExtractedItem ContentCandidateKind = "extracted_item"
	ContentCandidateDraftArtifact ContentCandidateKind = "draft_artifact"
)

// ContentCandidate is an immutable owned view of one staged item or draft.
// Byte slices are copied on every return; callers cannot mutate stored state.
type ContentCandidate struct {
	Kind               ContentCandidateKind
	ID                 string
	OriginalText       string
	MetadataBLOB       []byte
	MetadataSHA256     string
	CandidateSHA256    string
	SourceArtifactID   string
	SourceSHA256       string
	SourceMetadataBLOB []byte
	ItemIDs            []string
	LineageSHA256      string
	CreatedAt          time.Time
	ExpiresAt          time.Time
}

// GetContentCandidateAt returns a single item or draft to its delegated owner
// when content.stage or content.confirm is present. now is explicit so expiry
// decisions are deterministic and this read never calls a provider.
func (s *Store) GetContentCandidateAt(ctx context.Context, kind ContentCandidateKind, id string, now time.Time) (ContentCandidate, error) {
	principal, err := requireCandidatePrincipal(ctx)
	if err != nil {
		return ContentCandidate{}, err
	}
	if s == nil || s.db == nil || id == "" {
		return ContentCandidate{}, domainError("not_found_or_forbidden", "content candidate is unavailable")
	}
	now = now.UTC()
	var candidate ContentCandidate
	var text, candidateHash, metadataHash, lineageHash, sourceID, sourceHash, itemIDsRaw, candidateCreatedRaw string
	var metadataRaw []byte
	var expiresRaw string
	var sourceBytes, sourceMetadata []byte
	var sourceMetadataHash string
	switch kind {
	case ContentCandidateExtractedItem:
		err = s.db.QueryRowContext(ctx, `SELECT i.item_id,i.item_text,i.item_sha256,i.metadata_blob,i.metadata_sha256,i.source_artifact_id,i.created_at,
		 s.source_sha256,s.metadata_blob,s.metadata_sha256,s.source_bytes,s.expires_at
		 FROM assurance_content_items i JOIN assurance_content_sources s ON s.tenant_id=i.tenant_id AND s.source_artifact_id=i.source_artifact_id AND s.actor_id=i.actor_id
		 WHERE i.tenant_id=? AND i.actor_id=? AND i.item_id=?`, principal.TenantID, principal.AuditActor(), id).Scan(
			&candidate.ID, &text, &candidateHash, &metadataRaw, &metadataHash, &sourceID, &candidateCreatedRaw, &sourceHash, &sourceMetadata, &sourceMetadataHash, &sourceBytes, &expiresRaw)
	case ContentCandidateDraftArtifact:
		err = s.db.QueryRowContext(ctx, `SELECT d.draft_artifact_id,d.draft_text,d.draft_sha256,d.metadata_blob,d.metadata_sha256,d.source_artifact_id,d.item_ids_json,d.lineage_sha256,d.created_at,
		 s.source_sha256,s.metadata_blob,s.metadata_sha256,s.source_bytes,s.expires_at
		 FROM assurance_content_drafts d JOIN assurance_content_sources s ON s.tenant_id=d.tenant_id AND s.source_artifact_id=d.source_artifact_id AND s.actor_id=d.actor_id
		 WHERE d.tenant_id=? AND d.actor_id=? AND d.draft_artifact_id=?`, principal.TenantID, principal.AuditActor(), id).Scan(
			&candidate.ID, &text, &candidateHash, &metadataRaw, &metadataHash, &sourceID, &itemIDsRaw, &lineageHash, &candidateCreatedRaw, &sourceHash, &sourceMetadata, &sourceMetadataHash, &sourceBytes, &expiresRaw)
	default:
		return ContentCandidate{}, domainError("invalid_request", "content candidate kind is invalid")
	}
	if err != nil {
		if err == sql.ErrNoRows {
			return ContentCandidate{}, domainError("not_found_or_forbidden", "content candidate is unavailable")
		}
		return ContentCandidate{}, fmt.Errorf("assurance: read content candidate: %w", err)
	}
	candidate.Kind, candidate.OriginalText = kind, text
	candidate.CandidateSHA256, candidate.MetadataSHA256, candidate.LineageSHA256, candidate.SourceArtifactID, candidate.SourceSHA256 = candidateHash, metadataHash, lineageHash, sourceID, sourceHash
	candidate.MetadataBLOB = append([]byte(nil), metadataRaw...)
	candidate.SourceMetadataBLOB = append([]byte(nil), sourceMetadata...)
	if err := verifyStoredContentSource(sourceBytes, candidate.SourceSHA256, sourceMetadata, sourceMetadataHash); err != nil {
		return ContentCandidate{}, err
	}
	if err := verifyCandidateTextAndMetadata(text, candidateHash, metadataHash, candidate.MetadataBLOB); err != nil {
		return ContentCandidate{}, err
	}
	candidate.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresRaw)
	if err != nil {
		return ContentCandidate{}, domainError("artifact_hash_mismatch", "stored candidate expiry is invalid")
	}
	candidate.CreatedAt, err = time.Parse(time.RFC3339Nano, candidateCreatedRaw)
	if err != nil {
		return ContentCandidate{}, domainError("artifact_hash_mismatch", "stored candidate creation metadata is invalid")
	}
	policyMetadata, hasPolicyMetadata, err := parseContentIntakePolicyMetadata(candidate.SourceMetadataBLOB)
	if err != nil {
		return ContentCandidate{}, err
	}
	if hasPolicyMetadata {
		policySourceExpiry, _ := time.Parse(time.RFC3339Nano, policyMetadata.SourceExpiresAt)
		draftExpiry, _ := time.Parse(time.RFC3339Nano, policyMetadata.DraftExpiresAt)
		if !policySourceExpiry.Equal(candidate.ExpiresAt) || draftExpiry.After(policySourceExpiry) {
			return ContentCandidate{}, domainError("artifact_hash_mismatch", "stored intake policy expiries do not match source retention")
		}
		// Both extracted items and drafts are derived content governed by the
		// signed draft-retention horizon. The source may remain available longer.
		candidate.ExpiresAt = draftExpiry
	}
	if !candidate.ExpiresAt.After(now) {
		return ContentCandidate{}, domainError("not_found_or_forbidden", "content candidate is unavailable")
	}
	if kind == ContentCandidateDraftArtifact {
		if lineageHash == "" || digestHex(HashBytes([]byte(itemIDsRaw))) != lineageHash {
			return ContentCandidate{}, domainError("artifact_hash_mismatch", "stored draft lineage integrity verification failed")
		}
		candidate.ItemIDs, err = decodeOwnedDraftItemIDs(itemIDsRaw)
		if err != nil {
			return ContentCandidate{}, err
		}
		for _, itemID := range candidate.ItemIDs {
			if err := verifyDraftItemReference(ctx, s.db, principal, candidate.SourceArtifactID, itemID); err != nil {
				return ContentCandidate{}, err
			}
		}
	}
	candidate.ItemIDs = append([]string(nil), candidate.ItemIDs...)
	candidate.SourceMetadataBLOB = append([]byte(nil), candidate.SourceMetadataBLOB...)
	return candidate, nil
}

func requireCandidatePrincipal(ctx context.Context) (identity.Principal, error) {
	p, ok := identity.PrincipalFromContext(ctx)
	if !ok || p.TokenType != identity.TokenTypeDelegated || p.UsedSubjectFallback || (!p.HasPermission(identity.PermissionContentStage) && !p.HasPermission(identity.PermissionContentConfirm)) || !isCanonicalUUID(p.TenantID) || !isCanonicalUUID(p.ObjectID) {
		return identity.Principal{}, domainError("forbidden", "trusted delegated content.stage or content.confirm capability required")
	}
	return p, nil
}

func verifyStoredContentSource(sourceBytes []byte, sourceHash string, metadata []byte, metadataHash string) error {
	canonical, err := CanonicalJSONBytes(metadata)
	if err != nil || !bytes.Equal(canonical, metadata) || digestHex(HashBytes(metadata)) != metadataHash || digestHex(HashBytes(sourceBytes)) != sourceHash {
		return domainError("artifact_hash_mismatch", "stored source integrity verification failed")
	}
	return nil
}

func verifyCandidateTextAndMetadata(text, hash, metadataHash string, metadata []byte) error {
	canonical, err := CanonicalJSONBytes(metadata)
	if err != nil || !bytes.Equal(canonical, metadata) || metadataHash == "" || digestHex(HashBytes(metadata)) != metadataHash {
		return domainError("artifact_hash_mismatch", "stored candidate metadata integrity verification failed")
	}
	if digestHex(HashBytes([]byte(text))) != hash {
		return domainError("artifact_hash_mismatch", "stored candidate text integrity verification failed")
	}
	return nil
}

func decodeOwnedDraftItemIDs(raw string) ([]string, error) {
	canonical, err := CanonicalJSONBytes([]byte(raw))
	if err != nil || !bytes.Equal(canonical, []byte(raw)) {
		return nil, domainError("artifact_hash_mismatch", "stored draft item references are not canonical")
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil || len(ids) == 0 || len(ids) > contentItemsMax {
		return nil, domainError("artifact_hash_mismatch", "stored draft item references are invalid")
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			return nil, domainError("artifact_hash_mismatch", "stored draft item references are invalid")
		}
		if _, ok := seen[id]; ok {
			return nil, domainError("artifact_hash_mismatch", "stored draft item references are not unique")
		}
		seen[id] = struct{}{}
	}
	return append([]string(nil), ids...), nil
}

func verifyDraftItemReference(ctx context.Context, db *sql.DB, principal identity.Principal, sourceID, itemID string) error {
	var text, hash, metadataHash string
	var metadata []byte
	err := db.QueryRowContext(ctx, `SELECT item_text,item_sha256,metadata_blob,metadata_sha256 FROM assurance_content_items WHERE tenant_id=? AND actor_id=? AND source_artifact_id=? AND item_id=?`, principal.TenantID, principal.AuditActor(), sourceID, itemID).Scan(&text, &hash, &metadata, &metadataHash)
	if err != nil {
		return domainError("artifact_hash_mismatch", "stored draft references an unavailable item")
	}
	if err := verifyCandidateTextAndMetadata(text, hash, metadataHash, metadata); err != nil {
		return err
	}
	return nil
}
