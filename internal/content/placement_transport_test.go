package content

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/google/jsonschema-go/jsonschema"
)

func TestDecodePlacementTransportRequestProjectsPublishedPhases(t *testing.T) {
	key := "0123456789abcdef"
	stage := `{"phase":"stage","idempotency_key":"` + key + `","source_artifact_id":"source-1","extracted_item_id":"item-1","destination_profile_id":"profile-1","destination_profile_revision":4}`
	got, err := DecodePlacementTransportRequest([]byte(stage))
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != "stage" || got.Stage == nil || got.Stage.CandidateKind != "extracted_item" || got.Stage.CandidateID != "item-1" || got.Stage.SourceArtifactID != "source-1" || got.Stage.DestinationProfileRevision != 4 {
		t.Fatalf("stage projection mismatch: %#v", got)
	}
	confirm := `{"phase":"confirm","idempotency_key":"` + key + `","placement_intent_id":"intent-1"}`
	got, err = DecodePlacementTransportRequest([]byte(confirm))
	if err != nil || got.Confirm == nil || got.Confirm.ConfirmationToken != "" {
		t.Fatalf("token-free confirm replay request: %#v, %v", got, err)
	}
	ack := `{"phase":"acknowledge","idempotency_key":"` + key + `","placement_intent_id":"intent-1","observation":"visually_confirmed","result_sha256":"` + strings.Repeat("a", 64) + `"}`
	got, err = DecodePlacementTransportRequest([]byte(ack))
	if err != nil || got.Acknowledge == nil {
		t.Fatalf("ack projection: %#v, %v", got, err)
	}
	idemDigest, requestDigest, err := got.Acknowledge.IdempotencyDigests()
	if err != nil || idemDigest == (assurance.Digest{}) || requestDigest == (assurance.Digest{}) {
		t.Fatalf("ack digests: %x %x %v", idemDigest, requestDigest, err)
	}
	reconcile := `{"phase":"reconcile","idempotency_key":"` + key + `","placement_intent_id":"123e4567-e89b-42d3-a456-426614174000","action":"classify","classification":"still_unknown","uncertainty_episode_id":"episode-1"}`
	got, err = DecodePlacementTransportRequest([]byte(reconcile))
	if err != nil || got.Reconcile == nil || got.Reconcile.Classification != "still_unknown" {
		t.Fatalf("reconcile projection: %#v, %v", got, err)
	}
}

func TestProjectStageOutputMapsSignedItemAndDraftPreviews(t *testing.T) {
	store, _, ctx, now, profileID, _, policyResolver := stageIntegrationFixture(t)
	provider := &verifiedStageProvider{}
	service := StageService{Store: store, Candidates: AssuranceCandidateResolver{Store: store}, Profiles: AssuranceProfileResolver{Resolver: policyResolver}, Provider: provider, Clock: fixedStageClock{now}}
	input := assurance.ContentIngest{
		SourceBytes: []byte("public output stage fixture"), MediaType: "text/plain",
		MetadataBLOB: []byte(`{"origin":{"kind":"analyst"},"provenance":{"availability":"unavailable"}}`),
		Items:        []assurance.ContentItemInput{{LocalID: "item", Text: "exact source text", MetadataBLOB: []byte(`{"kind_hint":"text"}`)}},
		Drafts:       []assurance.ContentDraftInput{{LocalID: "draft", Text: "reviewed draft text", ItemLocalIDs: []string{"item"}, MetadataBLOB: []byte(`{"origin":{"kind":"analyst"},"projection_trust":"caller_unverified"}`)}},
	}
	digest, err := assurance.ContentIngestRequestDigest(input)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := store.Reserve(ctx, assurance.ReservationRequest{ActorID: testTenant + "/" + testActor, Tool: "workiva_content_placement", Action: "ingest", IdempotencyDigest: assurance.DigestIdempotencyKey("stage-output-ingest-key-01"), RequestDigest: digest, RetainUntil: now.Add(1200 * time.Second)}, now)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.FinalizePolicyBoundContentIngest(ctx, reservation, input, digest, policyResolver, "audit-stage-output-ingest", now)
	if err != nil {
		t.Fatal(err)
	}
	stageAndCheck := func(req StageRequest, auditID string, wantKind, wantOrigin string) StageResponse {
		t.Helper()
		response, err := service.Stage(ctx, auditID, req)
		if err != nil {
			t.Fatalf("Stage(%s): %v", wantKind, err)
		}
		output, err := ProjectStageOutput(response, auditID)
		if err != nil {
			t.Fatalf("ProjectStageOutput(%s): %v", wantKind, err)
		}
		validatePublicOutputSchema(t, output)
		result := output.Result.(PublicStageResult)
		artifact := result.Preview["derived_artifact"].(map[string]any)
		if wantKind == "extracted_item" && artifact["extracted_item_id"] != req.CandidateID || wantKind == "draft_artifact" && (artifact["draft_artifact_id"] != req.CandidateID || artifact["origin_kind"] != wantOrigin) {
			t.Fatalf("derived artifact does not match immutable candidate: %#v", artifact)
		}
		facts := result.Preview["target_interpretation_facts"].(map[string]any)
		for name, raw := range facts {
			fact := raw.(map[string]any)
			if fact["provenance"] != "operator_declared" || fact["evidence_id"] != profileID {
				t.Fatalf("target fact %s lacks the exact signed profile evidence ID: %#v", name, fact)
			}
		}
		provenance := result.Preview["source_provenance"].(map[string]any)
		if provenance["availability"] != "unavailable" || provenance["reason"] != "not_verified" {
			t.Fatalf("unverified source coordinates were overstated: %#v", provenance)
		}
		return response
	}
	itemRequest := StageRequest{CandidateKind: "extracted_item", CandidateID: created.ItemIDs["item"], SourceArtifactID: created.SourceArtifactID, DestinationProfileID: profileID, DestinationProfileRevision: 1, IdempotencyKey: "stage-output-item-key-01"}
	first := stageAndCheck(itemRequest, "audit-stage-output-item", "extracted_item", "")
	initialOutput, err := ProjectStageOutput(first, "audit-stage-output-item")
	if err != nil {
		t.Fatal(err)
	}
	initialPreview := initialOutput.Result.(PublicStageResult).Preview
	if initialPreview["confirmation_token"] != first.ConfirmationToken || initialPreview["token_delivery"] != "ephemeral_initial_response_only" {
		t.Fatal("initial stage response did not overlay its ephemeral confirmation token")
	}
	replayService := StageService{Store: store, Clock: fixedStageClock{now}}
	replay, err := replayService.Stage(ctx, "", itemRequest)
	if err != nil {
		t.Fatal(err)
	}
	replayOutput, err := ProjectStageOutput(replay, "audit-stage-output-replay")
	if err != nil {
		t.Fatal(err)
	}
	validatePublicOutputSchema(t, replayOutput)
	replayResult := replayOutput.Result.(PublicStageResult)
	if !replayOutput.Replayed || !replayResult.ReplayRequiresRestage || replayResult.Preview["confirmation_token"] != nil || replayResult.Preview["token_delivery"] != nil || replayResult.Preview["content_hash"] != initialPreview["content_hash"] {
		t.Fatalf("stage replay disclosed/rebound token or changed public projection: %#v", replayOutput)
	}
	draftRequest := StageRequest{CandidateKind: "draft_artifact", CandidateID: created.DraftIDs["draft"], DestinationProfileID: profileID, DestinationProfileRevision: 1, IdempotencyKey: "stage-output-draft-key-01"}
	stageAndCheck(draftRequest, "audit-stage-output-draft", "draft_artifact", "analyst")
	if provider.calls.Load() != 2 {
		t.Fatalf("provider observations=%d, want one for each fresh item/draft stage", provider.calls.Load())
	}
}

func TestProjectAcknowledgeOutputTruthfullyProjectsConflict(t *testing.T) {
	invocation := PlacementInvocation{AuditID: "audit-acknowledge", ReplayDisposition: "fresh"}
	base := AcknowledgeResponse{Kind: "acknowledge", AcknowledgementID: "ack-record-1", PlacementIntentID: "intent-1", Observation: "visual_conflict", ResultSHA256: strings.Repeat("a", 64), AuditID: invocation.AuditID, Status: "reconciliation_required", State: "reconciliation_required"}
	output, err := ProjectAcknowledgeOutput(base, invocation)
	if err != nil {
		t.Fatal(err)
	}
	if output.Status != "ok" || !output.ReconciliationRequired || !output.NoMutationSubmitted || len(output.Errors) != 0 {
		t.Fatalf("successful visual conflict was mapped as a denial: %#v", output)
	}
	validatePublicOutputSchema(t, output)
	base.Observation, base.Status, base.State = "visually_confirmed", "visually_acknowledged", "visually_acknowledged"
	output, err = ProjectAcknowledgeOutput(base, invocation)
	if err != nil || output.ReconciliationRequired {
		t.Fatalf("visual acknowledgement mapping: %#v err=%v", output, err)
	}
	validatePublicOutputSchema(t, output)
	base.State = "reconciliation_required"
	if _, err := ProjectAcknowledgeOutput(base, invocation); err == nil {
		t.Fatal("inconsistent sealed acknowledgement state was projected")
	}
}

func TestDecodePlacementTransportRequestRejectsSchemaAmbiguity(t *testing.T) {
	key := "0123456789abcdef"
	base := `{"phase":"stage","idempotency_key":"` + key + `","draft_artifact_id":"draft-1","destination_profile_id":"profile-1","destination_profile_revision":1}`
	cases := map[string]string{
		"duplicate":                      strings.Replace(base, `"phase":"stage"`, `"phase":"stage","phase":"stage"`, 1),
		"unknown":                        strings.TrimSuffix(base, "}") + `,"operator_override":true}`,
		"null optional":                  strings.TrimSuffix(base, "}") + `,"workflow_context":null}`,
		"both candidate variants":        strings.TrimSuffix(base, "}") + `,"source_artifact_id":"source-1","extracted_item_id":"item-1"}`,
		"missing key":                    `{"phase":"stage","draft_artifact_id":"draft-1","destination_profile_id":"profile-1","destination_profile_revision":1}`,
		"workflow context not projected": strings.TrimSuffix(base, "}") + `,"workflow_context":{"workflow_id":"w","step_id":"s","binding_id":"b"}}`,
		"unknown phase":                  `{"phase":"delete","idempotency_key":"` + key + `"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodePlacementTransportRequest([]byte(raw)); err == nil {
				t.Fatal("invalid public request accepted")
			}
		})
	}
}

func TestProjectConfirmResultUsesSealedHashAndSchemaState(t *testing.T) {
	readback := json.RawMessage(`{"raw_value":"x"}`)
	rawHash := assurance.HashBytes(readback)
	r := ConfirmResponse{Kind: "confirm", Status: "machine_verified_visual_ack_pending", PlacementIntentID: "intent-1", State: "machine_verified_visual_ack_pending", VisualState: "pending", OperationReference: "operation-1", ReadbackJSON: readback, ReadbackSHA256: hex.EncodeToString(rawHash[:]), TargetHash: strings.Repeat("b", 64), TerminalFenceDigest: strings.Repeat("c", 64), ResultSHA256: strings.Repeat("d", 64)}
	projected, err := ProjectConfirmResult(r)
	if err != nil {
		t.Fatal(err)
	}
	if projected.State != "verified" || projected.ResultContentHash == nil || *projected.ResultContentHash != r.ResultSHA256 || projected.ProviderOperationID == nil || *projected.ProviderOperationID != r.OperationReference || projected.ResubmissionAllowed {
		t.Fatalf("incorrect public confirm projection: %#v", projected)
	}
	validatePublicOutputSchema(t, map[string]any{"phase": "confirm", "status": "verified", "nl_audit_id": "audit-1", "replayed": false, "no_mutation_submitted": false, "reconciliation_required": false, "result": projected, "errors": []any{}})
	r.ResultSHA256 = ""
	if _, err := ProjectConfirmResult(r); err == nil {
		t.Fatal("incomplete private proof projected")
	}
}

func TestProjectReconcileResultCopiesOnlyPersistedEvidence(t *testing.T) {
	r := assurance.ContentReconciliationResult{Kind: "reconcile", PlacementIntentID: "intent-1", State: "still_unknown", UncertaintyEpisodeID: "episode-1", OperationKnown: false, ProviderOperationID: nil, EvidenceIDs: []string{}, EvidenceBindingHash: strings.Repeat("a", 64), ResultSHA256: strings.Repeat("d", 64), ReconciliationRequired: true, ResubmissionAllowed: false}
	projected, err := ProjectReconcileResult(r)
	if err != nil {
		t.Fatal(err)
	}
	if projected.State != r.State || projected.OperationKnown || projected.ProviderOperationID != nil || projected.EvidenceBindingContentHash != r.EvidenceBindingHash || projected.ResultContentHash != "" || projected.ResubmissionAllowed {
		t.Fatalf("reconcile projection mismatch: %#v", projected)
	}
	r.ResubmissionAllowed = true
	if _, err := ProjectReconcileResult(r); err == nil {
		t.Fatal("resubmission-enabled result projected")
	}
	r.ResubmissionAllowed = false
	r.State = "confirmed_applied"
	r.ReconciliationRequired = false
	r.LatestReadbackID = "readback-1"
	r.EvidenceIDs = []string{"readback-1"}
	r.ResultSHA256 = ""
	if _, err := ProjectReconcileResult(r); err == nil {
		t.Fatal("applied result without sealed response hash projected")
	}
	r.ResultSHA256 = strings.Repeat("d", 64)
	if _, err := ProjectReconcileResult(r); err != nil {
		t.Fatalf("verified applied result with exact evidence binding rejected: %v", err)
	}
	projected, err = ProjectReconcileResult(r)
	if err != nil {
		t.Fatal(err)
	}
	validatePublicOutputSchema(t, map[string]any{"phase": "reconcile", "status": "verified", "nl_audit_id": "audit-1", "replayed": false, "no_mutation_submitted": true, "reconciliation_required": false, "result": projected, "errors": []any{}})
	r.EvidenceIDs = []string{}
	if _, err := ProjectReconcileResult(r); err == nil {
		t.Fatal("applied result without persisted readback reference projected")
	}
}

func validatePublicOutputSchema(t *testing.T, instance any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "wave4", "schemas", "content-placement.output.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	rawInstance, err := json.Marshal(instance)
	if err != nil {
		t.Fatal(err)
	}
	var jsonInstance any
	if err := json.Unmarshal(rawInstance, &jsonInstance); err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(jsonInstance); err != nil {
		t.Fatalf("output does not match published content-placement schema: %v", err)
	}
}
