package content

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
)

func TestProjectIngestOutputUsesSealedSavedProjectionAndReplays(t *testing.T) {
	store, _, resolver, _, _ := openSignedIntakeStore(t, 86400, 43200, 7200, true)
	service, err := NewIntakeService(store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	ctx := intakeContext()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	raw := []byte(`{"phase":"ingest","idempotency_key":"ingest-output-adapter-key-01","source":{"source_text":"private source plaintext","media_type":"text/plain"},"origin":{"kind":"analyst"},"extracted_items":[{"local_id":"item","text":"private extracted plaintext","kind_hint":"text","segments":[]}],"draft_artifacts":[{"local_id":"draft","text":"private draft plaintext","item_local_ids":["item"],"origin":{"kind":"copilot"}}]}`)
	created, err := service.IngestPublic(ctx, raw, "audit-ingest-output", now)
	if err != nil {
		t.Fatalf("fresh ingest: %v", err)
	}
	if created.PublicProjection == nil {
		t.Fatal("finalization did not seal a saved-record public projection")
	}
	fresh, err := ProjectIngestOutput(created, PlacementInvocation{AuditID: "audit-ingest-output", ReplayDisposition: "fresh"})
	if err != nil {
		t.Fatalf("fresh output projection: %v", err)
	}
	validatePublicOutputSchema(t, fresh)
	if fresh.Phase != "ingest" || fresh.Status != "ok" || !fresh.NoMutationSubmitted || fresh.Replayed || fresh.ReconciliationRequired || len(fresh.Errors) != 0 {
		t.Fatalf("unexpected fresh ingest envelope: %#v", fresh)
	}
	encodedFresh, err := json.Marshal(fresh.Result)
	if err != nil {
		t.Fatal(err)
	}
	for _, plaintext := range []string{"private source plaintext", "private extracted plaintext", "private draft plaintext"} {
		if strings.Contains(string(encodedFresh), plaintext) {
			t.Fatalf("public ingest projection disclosed plaintext %q", plaintext)
		}
	}
	p := created.PublicProjection
	if p.Source.SourceArtifactID != created.SourceArtifactID || p.Source.Revision != 1 || p.Source.ContentHash != created.SourceSHA256 || p.Source.ByteLength != len([]byte("private source plaintext")) || p.Source.MediaType != "text/plain" || p.Source.RetentionPolicy.Class != "source" || len(p.VerifiedEvidenceRefs) != 0 || len(p.ExtractedItems) != 1 || !p.ExtractedItems[0].CandidateOnly || p.ExtractedItems[0].Revision != 1 || p.ExtractedItems[0].RetentionPolicy.Class != "draft" || len(p.DraftArtifacts) != 1 || !p.DraftArtifacts[0].CandidateOnly || p.DraftArtifacts[0].Revision != 1 || p.DraftArtifacts[0].OriginKind != "copilot" || p.DraftArtifacts[0].Lineage.Kind != "source_bytes" || p.DraftArtifacts[0].Lineage.Source.SourceArtifactID != p.Source.SourceArtifactID {
		t.Fatalf("saved source/item/draft facts are incomplete: %#v", p)
	}

	service.resolver = nil
	replayed, err := service.IngestPublic(ctx, raw, "", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("sealed replay consulted fresh policy dependencies: %v", err)
	}
	replayOutput, err := ProjectIngestOutput(replayed, PlacementInvocation{AuditID: "audit-ingest-output", ReplayDisposition: "sealed_replay"})
	if err != nil {
		t.Fatalf("replay output projection: %v", err)
	}
	validatePublicOutputSchema(t, replayOutput)
	if !replayOutput.Replayed {
		t.Fatal("sealed ingest replay was not marked replayed")
	}
	encodedReplay, err := json.Marshal(replayOutput.Result)
	if err != nil {
		t.Fatal(err)
	}
	if string(encodedFresh) != string(encodedReplay) {
		t.Fatalf("replay changed sealed public result: fresh=%s replay=%s", encodedFresh, encodedReplay)
	}
}

func TestProjectIngestOutputFailsClosedOnIncompleteOrMismatchedSavedProjection(t *testing.T) {
	store, _, resolver, _, _ := openSignedIntakeStore(t, 86400, 43200, 7200, true)
	service, err := NewIntakeService(store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"phase":"ingest","idempotency_key":"ingest-output-invalid-key-01","source":{"source_text":"saved source","media_type":"text/plain"},"origin":{"kind":"analyst"},"extracted_items":[{"local_id":"item","text":"saved item","kind_hint":"text","segments":[]}],"draft_artifacts":[{"local_id":"draft","text":"saved draft","item_local_ids":["item"],"origin":{"kind":"analyst"}}]}`)
	created, err := service.IngestPublic(intakeContext(), raw, "audit-ingest-output-invalid", time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	invocation := PlacementInvocation{AuditID: "audit-ingest-output-invalid", ReplayDisposition: "fresh"}
	cases := map[string]func(*assurance.ContentIngestResult){
		"missing sealed projection": func(r *assurance.ContentIngestResult) { r.PublicProjection = nil },
		"source result mismatch":    func(r *assurance.ContentIngestResult) { r.PublicProjection.Source.SourceArtifactID = "other-source" },
		"item source mismatch": func(r *assurance.ContentIngestResult) {
			r.PublicProjection.ExtractedItems[0].Source.ContentHash = strings.Repeat("0", 64)
		},
		"item retention differs from signed source policy": func(r *assurance.ContentIngestResult) {
			r.PublicProjection.ExtractedItems[0].RetentionPolicy.PolicyID = "other-policy"
		},
		"draft origin invalid": func(r *assurance.ContentIngestResult) {
			r.PublicProjection.DraftArtifacts[0].OriginKind = "caller_claim"
		},
		"unexpected evidence": func(r *assurance.ContentIngestResult) {
			r.PublicProjection.VerifiedEvidenceRefs = []assurance.ContentIngestVerifiedEvidenceRef{{EvidenceID: "claimed", Revision: 1, ContentHash: strings.Repeat("1", 64)}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			clone := created
			if created.PublicProjection != nil {
				projectionCopy := *created.PublicProjection
				projectionCopy.ExtractedItems = append([]assurance.ContentIngestItemProjection(nil), created.PublicProjection.ExtractedItems...)
				projectionCopy.DraftArtifacts = append([]assurance.ContentIngestDraftProjection(nil), created.PublicProjection.DraftArtifacts...)
				projectionCopy.VerifiedEvidenceRefs = append([]assurance.ContentIngestVerifiedEvidenceRef(nil), created.PublicProjection.VerifiedEvidenceRefs...)
				clone.PublicProjection = &projectionCopy
			}
			mutate(&clone)
			if _, err := ProjectIngestOutput(clone, invocation); err == nil {
				t.Fatal("invalid saved projection was accepted")
			}
		})
	}
}
