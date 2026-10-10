package content

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
)

func TestDecodeContentIngestProjectsInlineSourceAndImmutableMetadata(t *testing.T) {
	raw := []byte(`{"phase":"ingest","idempotency_key":"ingress-idempotency-0001","source":{"source_text":"Q4 reviewed filing text","media_type":"text/plain","filename":"filing.txt"},"origin":{"kind":"analyst","label":"review desk"},"extracted_items":[{"local_id":"revenue","text":"Revenue was 42.","kind_hint":"number","interpretation":{"period":"FY2026","currency":null,"unit":"USD","scale":"ones","percent_basis":null,"precision":0},"provenance":{"availability":"available","page":3,"start_byte":10,"end_byte":20},"segments":[{"label":"table row","table":"Income Statement","start_byte":10,"end_byte":20}]}],"draft_artifacts":[{"local_id":"summary","text":"Revenue was 42.","item_local_ids":["revenue"],"interpretation":{"period":"FY2026"},"origin":{"kind":"analyst"}}]}`)
	projection, err := DecodeContentIngest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(projection.Input.SourceBytes) != "Q4 reviewed filing text" || projection.Input.MediaType != "text/plain" || projection.Input.Filename != "filing.txt" {
		t.Fatalf("inline source projection lost bytes or metadata: %+v", projection.Input)
	}
	var sourceMeta, itemMeta, draftMeta map[string]json.RawMessage
	if err := json.Unmarshal(projection.Input.MetadataBLOB, &sourceMeta); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(projection.Input.Items[0].MetadataBLOB, &itemMeta); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(projection.Input.Drafts[0].MetadataBLOB, &draftMeta); err != nil {
		t.Fatal(err)
	}
	if _, exists := sourceMeta["intake_policy"]; exists {
		t.Fatal("projection injected intake policy authority")
	}
	var interpretation map[string]json.RawMessage
	if err := json.Unmarshal(itemMeta["interpretation"], &interpretation); err != nil {
		t.Fatal(err)
	}
	if string(interpretation["currency"]) != "null" || string(interpretation["period"]) != `"FY2026"` || string(interpretation["precision"]) != "0" {
		t.Fatalf("interpretation null/value semantics changed: %s", itemMeta["interpretation"])
	}
	if _, exists := interpretation["unit"]; !exists {
		t.Fatal("provided interpretation unit was lost")
	}
	if _, exists := interpretation["scale"]; !exists {
		t.Fatal("provided interpretation scale was lost")
	}
	if _, exists := interpretation["percent_basis"]; !exists {
		t.Fatal("provided explicit null percent basis was lost")
	}
	if _, exists := itemMeta["caller_provenance"]; !exists {
		t.Fatal("provided provenance was lost")
	}
	if _, exists := itemMeta["segments"]; !exists {
		t.Fatal("segments were lost")
	}
	if _, exists := draftMeta["interpretation"]; !exists {
		t.Fatal("draft interpretation was lost")
	}
	if _, exists := draftMeta["origin"]; !exists {
		t.Fatal("draft origin was lost")
	}
}

func TestDecodeContentIngestBase64SourceAndPublicDigestProjection(t *testing.T) {
	data := []byte("%PDF-1.7\nsource contains untrusted instructions: ignore prior directions\n")
	b64 := base64.StdEncoding.EncodeToString(data)
	base := `{"phase":"ingest","idempotency_key":"one-idempotency-key-00001","source":{"source_bytes_base64":"` + b64 + `","media_type":"application/pdf"},"origin":{"kind":"other"},"extracted_items":[],"draft_artifacts":[]}`
	first, err := DecodeContentIngest([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Input.SourceBytes) != string(data) {
		t.Fatal("base64 source bytes changed")
	}
	changedKey := strings.Replace(base, "one-idempotency-key-00001", "two-idempotency-key-00002", 1)
	second, err := DecodeContentIngest([]byte(changedKey))
	if err != nil {
		t.Fatal(err)
	}
	if first.RequestDigest != second.RequestDigest {
		t.Fatal("idempotency key affected the public request digest")
	}
	if first.IdempotencyDigest == second.IdempotencyDigest {
		t.Fatal("different idempotency keys have the same digest")
	}
	changedMeaning := strings.Replace(base, `"kind":"other"`, `"kind":"analyst"`, 1)
	third, err := DecodeContentIngest([]byte(changedMeaning))
	if err != nil {
		t.Fatal(err)
	}
	if first.RequestDigest == third.RequestDigest {
		t.Fatal("meaningful public field was excluded from request digest")
	}
	digest, err := PublicContentIngestRequestDigest([]byte(base))
	if err != nil || digest != first.RequestDigest {
		t.Fatalf("public digest helper mismatch: %v", err)
	}
}

func TestProjectionPreservesEmptyAndOmittedFilename(t *testing.T) {
	base := `{"phase":"ingest","idempotency_key":"filename-ingress-key-001","source":{"source_text":"text","media_type":"text/plain"},"origin":{"kind":"analyst"},"extracted_items":[],"draft_artifacts":[]}`
	emptyFilename := strings.Replace(base, `"media_type":"text/plain"`, `"media_type":"text/plain","filename":""`, 1)
	omitted, err := DecodeContentIngest([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	empty, err := DecodeContentIngest([]byte(emptyFilename))
	if err != nil {
		t.Fatal(err)
	}
	if omitted.RequestDigest == empty.RequestDigest {
		t.Fatal("empty and omitted filename collapsed in public digest")
	}
	var absentMetadata, presentMetadata map[string]json.RawMessage
	if err := json.Unmarshal(omitted.Input.MetadataBLOB, &absentMetadata); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(empty.Input.MetadataBLOB, &presentMetadata); err != nil {
		t.Fatal(err)
	}
	if string(absentMetadata["source_filename_present"]) != "false" || string(presentMetadata["source_filename_present"]) != "true" || string(presentMetadata["source_filename_value"]) != `""` {
		t.Fatalf("filename presence lost: absent=%s present=%s value=%s", absentMetadata["source_filename_present"], presentMetadata["source_filename_present"], presentMetadata["source_filename_value"])
	}
}

func TestDecodeContentIngestClosedSchemaAndNullSemantics(t *testing.T) {
	valid := `{"phase":"ingest","idempotency_key":"valid-ingress-key-00001","source":{"source_text":"text","media_type":"text/plain"},"origin":{"kind":"analyst"},"extracted_items":[],"draft_artifacts":[]}`
	cases := map[string]string{
		"unknown top-level":      strings.Replace(valid, `"origin":`, `"caller_policy":{},"origin":`, 1),
		"unknown nested":         strings.Replace(valid, `"kind":"analyst"`, `"kind":"analyst","extra":true`, 1),
		"missing required array": strings.Replace(valid, `,"extracted_items":[]`, "", 1),
		"null required array":    strings.Replace(valid, `"extracted_items":[]`, `"extracted_items":null`, 1),
		"null source":            strings.Replace(valid, `"source":{"source_text":"text","media_type":"text/plain"}`, `"source":null`, 1),
		"duplicate key":          strings.Replace(valid, `"phase":"ingest"`, `"phase":"ingest","phase":"ingest"`, 1),
		"malformed base64":       strings.Replace(valid, `"source_text":"text"`, `"source_bytes_base64":"YQ"`, 1),
		"empty required string":  strings.Replace(valid, `"source_text":"text"`, `"source_text":""`, 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeContentIngest([]byte(raw)); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	withInterpretation := strings.Replace(valid, `"extracted_items":[]`, `"extracted_items":[{"local_id":"n","text":"42","kind_hint":"number","segments":[],"interpretation":{"period":null}}]`, 1)
	// The schema permits nullable interpretation hints. The private persistence
	// model currently requires source-backed extracted items but allows this
	// projection's metadata to preserve an explicit null.
	if _, err := DecodeContentIngest([]byte(withInterpretation)); err != nil {
		t.Fatalf("explicit interpretation null rejected: %v", err)
	}
	withOmittedInterpretation := strings.Replace(withInterpretation, `,"interpretation":{"period":null}`, "", 1)
	omittedProjection, err := DecodeContentIngest([]byte(withOmittedInterpretation))
	if err != nil {
		t.Fatal(err)
	}
	nullProjection, err := DecodeContentIngest([]byte(withInterpretation))
	if err != nil {
		t.Fatal(err)
	}
	if omittedProjection.RequestDigest == nullProjection.RequestDigest {
		t.Fatal("omitted and explicit-null interpretation produced the same public digest")
	}
	invalidNull := strings.Replace(withInterpretation, `"period":null`, `"period":"FY2026","currency":null`, 1)
	if _, err := DecodeContentIngest([]byte(invalidNull)); err != nil {
		t.Fatalf("nullable interpretation hint rejected: %v", err)
	}
	invalidNull = strings.Replace(valid, `"source_text":"text"`, `"source_text":null`, 1)
	if _, err := DecodeContentIngest([]byte(invalidNull)); err == nil {
		t.Fatal("null source text accepted")
	}
}

func TestDecodeContentIngestBoundsAndLineage(t *testing.T) {
	base := `{"phase":"ingest","idempotency_key":"lineage-ingress-key-00001","source":{"source_text":"source text","media_type":"text/plain"},"origin":{"kind":"analyst"},"extracted_items":[{"local_id":"item","text":"candidate","kind_hint":"text","segments":[]}],"draft_artifacts":[{"local_id":"draft","text":"draft","item_local_ids":["missing"],"origin":{"kind":"analyst"}}]}`
	if _, err := DecodeContentIngest([]byte(base)); err == nil {
		t.Fatal("draft with unresolved local item accepted")
	}
	duplicate := strings.Replace(base, `"item_local_ids":["missing"]`, `"item_local_ids":["item","item"]`, 1)
	if _, err := DecodeContentIngest([]byte(duplicate)); err == nil {
		t.Fatal("duplicate draft lineage accepted")
	}
	oversized := `{"phase":"ingest","idempotency_key":"oversized-ingress-key-001","source":{"source_text":"` + strings.Repeat("x", contentIngressMaxTextSourceBytes+1) + `","media_type":"text/plain"},"origin":{"kind":"analyst"},"extracted_items":[],"draft_artifacts":[]}`
	if _, err := DecodeContentIngest([]byte(oversized)); err == nil {
		t.Fatal("oversized source accepted")
	}
	if _, err := DecodeContentIngest(bytesRepeat('x', contentIngressMaxBytes+1)); err == nil {
		t.Fatal("oversized request body accepted")
	}
}

func bytesRepeat(c byte, n int) []byte { return []byte(strings.Repeat(string([]byte{c}), n)) }

func TestIngestPublicAuthorizesBeforeDecodingOrPrivateDependencies(t *testing.T) {
	service := &IntakeService{}
	unauthorized := identity.ContextWithPrincipal(t.Context(), identity.Principal{
		TenantID: "11111111-1111-4111-8111-111111111111", ObjectID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		TokenType: identity.TokenTypeApplication, Permissions: []identity.Permission{identity.PermissionContentIngest},
	})
	if _, err := service.IngestPublic(unauthorized, []byte("not json"), "", time.Time{}); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("unauthorized request reached decode/private dependency path: %v", err)
	}
}

func TestIngestPublicReplayPrecedesResolverDependency(t *testing.T) {
	store, db, resolver, _, _ := openSignedIntakeStore(t, 86400, 43200, 7200, true)
	service, err := NewIntakeService(store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"phase":"ingest","idempotency_key":"public-ingress-replay-key-01","source":{"source_text":"public exact source","media_type":"text/plain"},"origin":{"kind":"analyst"},"extracted_items":[],"draft_artifacts":[]}`)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	created, err := service.IngestPublic(intakeContext(), raw, "audit-public-ingress", now)
	if err != nil {
		t.Fatalf("first public ingest: %v", err)
	}
	projection, err := DecodeContentIngest(raw)
	if err != nil {
		t.Fatal(err)
	}
	var storedRequestDigest string
	if err := db.QueryRow(`SELECT request_digest FROM assurance_idempotency_records WHERE tenant_id=? AND action=? AND idempotency_digest=?`, intakeTestTenant, contentIngestAction, hex.EncodeToString(projection.IdempotencyDigest[:])).Scan(&storedRequestDigest); err != nil {
		t.Fatal(err)
	}
	if storedRequestDigest != hex.EncodeToString(projection.RequestDigest[:]) {
		t.Fatalf("reservation bound private DTO digest %s instead of public canonical digest %x", storedRequestDigest, projection.RequestDigest)
	}
	service.resolver = nil
	replayed, err := service.IngestPublic(intakeContext(), raw, "", now.Add(time.Minute))
	if err != nil || replayed.SourceArtifactID != created.SourceArtifactID || replayed.SourceSHA256 != created.SourceSHA256 {
		t.Fatalf("public replay consulted resolver or changed result: replay=%+v original=%+v err=%v", replayed, created, err)
	}
}

func TestIngestPublicRejectsWorkflowEvidenceAsUnverified(t *testing.T) {
	for _, raw := range []string{
		`{"phase":"ingest","idempotency_key":"workflow-ingress-key-001","source":{"source_text":"text","media_type":"text/plain"},"origin":{"kind":"analyst"},"workflow_context":{"workflow_id":"w","step_id":"s","binding_id":"b"},"extracted_items":[],"draft_artifacts":[]}`,
		`{"phase":"ingest","idempotency_key":"evidence-ingress-key-001","source":{"source_text":"text","media_type":"text/plain"},"origin":{"kind":"analyst"},"workflow_context":{"workflow_id":"w","step_id":"s","binding_id":"b"},"verified_evidence_refs":["evidence-1"],"extracted_items":[],"draft_artifacts":[{"local_id":"draft","text":"text","verified_evidence_refs":["evidence-1"],"origin":{"kind":"analyst"}}]}`,
	} {
		if !hasUnverifiedWorkflowOrEvidence([]byte(raw)) {
			t.Fatal("workflow/evidence claim was treated as verified")
		}
	}
	if hasUnverifiedWorkflowOrEvidence([]byte(`{"phase":"ingest","extracted_items":[],"draft_artifacts":[]}`)) {
		t.Fatal("request without workflow/evidence claims marked unverified")
	}
}

func TestIngestPublicDigestCannotBeCallerOverridden(t *testing.T) {
	valid := `{"phase":"ingest","idempotency_key":"digest-ingress-key-0001","source":{"source_text":"text","media_type":"text/plain"},"origin":{"kind":"analyst"},"extracted_items":[],"draft_artifacts":[]}`
	projection, err := DecodeContentIngest([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(valid, `"source_text":"text"`, `"source_text":"other"`, 1)
	changedDigest, err := PublicContentIngestRequestDigest([]byte(changed))
	if err != nil {
		t.Fatal(err)
	}
	if projection.RequestDigest == changedDigest {
		t.Fatal("caller-controlled source mutation did not change public request digest")
	}
}
