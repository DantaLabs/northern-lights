package content

import (
	"errors"
	"fmt"
	"mime"

	"github.com/dantalabs/northern-lights/internal/assurance"
)

// ProjectIngestOutput projects only the saved-record payload sealed by the
// assurance finalization transaction. It performs no storage or policy reads,
// so fresh and replayed responses use the same proof-bearing projection.
func ProjectIngestOutput(result assurance.ContentIngestResult, invocation PlacementInvocation) (PublicPlacementOutput, error) {
	if err := invocation.validate(); err != nil {
		return PublicPlacementOutput{}, err
	}
	if err := validateIngestPublicProjection(result); err != nil {
		return PublicPlacementOutput{}, err
	}
	return PublicPlacementOutput{
		Phase: "ingest", Status: "ok", NLAuditID: invocation.AuditID,
		Replayed:            invocation.ReplayDisposition == "sealed_replay",
		NoMutationSubmitted: true, ReconciliationRequired: false,
		Result: result.PublicProjection, Errors: []PublicPlacementError{},
	}, nil
}

func validateIngestPublicProjection(result assurance.ContentIngestResult) error {
	p := result.PublicProjection
	if p == nil {
		return errors.New("content transport: sealed saved-record ingest projection is missing")
	}
	if p.Kind != "ingest" || p.Source.Revision != 1 || !validOpaqueID(p.Source.SourceArtifactID) || p.Source.SourceArtifactID != result.SourceArtifactID || !validIngressSHA256(p.Source.ContentHash) || p.Source.ContentHash != result.SourceSHA256 || p.Source.ByteLength < 1 || p.Source.ByteLength > 786432 || len(p.Source.MediaType) == 0 || len(p.Source.MediaType) > 128 {
		return errors.New("content transport: saved source projection does not bind the ingest result")
	}
	mediaType, _, err := mime.ParseMediaType(p.Source.MediaType)
	if err != nil || mediaType != p.Source.MediaType {
		return errors.New("content transport: saved source media type is invalid")
	}
	if err := validateIngestRetentionReference(p.Source.RetentionPolicy, "source"); err != nil {
		return fmt.Errorf("content transport: source retention reference: %w", err)
	}
	if p.VerifiedEvidenceRefs == nil || len(p.VerifiedEvidenceRefs) != 0 {
		return errors.New("content transport: public ingest cannot project unverified or unsupported evidence references")
	}
	if p.ExtractedItems == nil || p.DraftArtifacts == nil || len(p.ExtractedItems) > 64 || len(p.DraftArtifacts) > 8 || len(p.ExtractedItems) != len(result.ItemIDs) || len(p.DraftArtifacts) != len(result.DraftIDs) {
		return errors.New("content transport: saved candidate projection is missing or outside published bounds")
	}
	itemIDs := make(map[string]struct{}, len(result.ItemIDs))
	for _, id := range result.ItemIDs {
		if !validOpaqueID(id) {
			return errors.New("content transport: ingest result contains an invalid extracted-item ID")
		}
		if _, duplicate := itemIDs[id]; duplicate {
			return errors.New("content transport: ingest result repeats an extracted-item ID")
		}
		itemIDs[id] = struct{}{}
	}
	seenItems := make(map[string]struct{}, len(p.ExtractedItems))
	for _, item := range p.ExtractedItems {
		if !validOpaqueID(item.ExtractedItemID) || item.Revision != 1 || !validIngressSHA256(item.ContentHash) || !item.CandidateOnly || item.Source.SourceArtifactID != p.Source.SourceArtifactID || item.Source.Revision != p.Source.Revision || item.Source.ContentHash != p.Source.ContentHash {
			return errors.New("content transport: saved extracted-item projection has invalid source or immutable binding")
		}
		if _, expected := itemIDs[item.ExtractedItemID]; !expected {
			return errors.New("content transport: projected extracted item is not in the sealed ingest record")
		}
		if _, duplicate := seenItems[item.ExtractedItemID]; duplicate {
			return errors.New("content transport: saved extracted-item projection repeats an ID")
		}
		seenItems[item.ExtractedItemID] = struct{}{}
		if err := validateIngestRetentionReference(item.RetentionPolicy, "draft"); err != nil {
			return fmt.Errorf("content transport: extracted-item retention reference: %w", err)
		}
		if !sameIngestRetentionPolicy(p.Source.RetentionPolicy, item.RetentionPolicy) {
			return errors.New("content transport: extracted-item retention reference differs from the signed source policy")
		}
	}
	draftIDs := make(map[string]struct{}, len(result.DraftIDs))
	for _, id := range result.DraftIDs {
		if !validOpaqueID(id) {
			return errors.New("content transport: ingest result contains an invalid draft ID")
		}
		if _, duplicate := draftIDs[id]; duplicate {
			return errors.New("content transport: ingest result repeats a draft ID")
		}
		draftIDs[id] = struct{}{}
	}
	seenDrafts := make(map[string]struct{}, len(p.DraftArtifacts))
	for _, draft := range p.DraftArtifacts {
		if !validOpaqueID(draft.DraftArtifactID) || draft.Revision != 1 || !validIngressSHA256(draft.ContentHash) || draft.Lineage.Kind != "source_bytes" || draft.Lineage.Source.SourceArtifactID != p.Source.SourceArtifactID || draft.Lineage.Source.Revision != p.Source.Revision || draft.Lineage.Source.ContentHash != p.Source.ContentHash || !oneOf(draft.OriginKind, "analyst", "agent_flow", "copilot", "other") || !draft.CandidateOnly {
			return errors.New("content transport: saved draft projection has invalid immutable lineage or origin")
		}
		if _, expected := draftIDs[draft.DraftArtifactID]; !expected {
			return errors.New("content transport: projected draft is not in the sealed ingest record")
		}
		if _, duplicate := seenDrafts[draft.DraftArtifactID]; duplicate {
			return errors.New("content transport: saved draft projection repeats an ID")
		}
		seenDrafts[draft.DraftArtifactID] = struct{}{}
		if err := validateIngestRetentionReference(draft.RetentionPolicy, "draft"); err != nil {
			return fmt.Errorf("content transport: draft retention reference: %w", err)
		}
		if !sameIngestRetentionPolicy(p.Source.RetentionPolicy, draft.RetentionPolicy) {
			return errors.New("content transport: draft retention reference differs from the signed source policy")
		}
	}
	return nil
}

func sameIngestRetentionPolicy(source, derived assurance.ContentIngestRetentionReference) bool {
	return source.PolicyID == derived.PolicyID && source.Revision == derived.Revision && source.ContentHash == derived.ContentHash && source.Class == "source" && derived.Class == "draft"
}

func validateIngestRetentionReference(ref assurance.ContentIngestRetentionReference, class string) error {
	if !validOpaqueID(ref.PolicyID) || ref.Revision < 1 || ref.Revision > 1_000_000 || !validIngressSHA256(ref.ContentHash) || ref.Class != class {
		return errors.New("signed retention reference is malformed or has the wrong class")
	}
	return nil
}
