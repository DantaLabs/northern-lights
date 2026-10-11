package content

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
	_ "modernc.org/sqlite"
)

type fixedStageClock struct{ at time.Time }

func (c fixedStageClock) Now() time.Time { return c.at }

type verifiedStageProvider struct{ calls atomic.Int32 }

func (p *verifiedStageProvider) ReadContentMetadata(_ context.Context, spreadsheet, sheet, cell string) (workivaprovider.ContentMetadata, error) {
	p.calls.Add(1)
	m := workivaprovider.ContentMetadata{SpreadsheetID: spreadsheet, SheetID: sheet, Locator: cell, RawValue: json.RawMessage("null"), ValuePresent: true, RawFormats: json.RawMessage(`{}`), FormatsPresent: true, EffectiveFormats: json.RawMessage(`{}`), EffectiveFormatsPresent: true, Provenance: workivaprovider.ContentMetadataProvenance{Provider: "workiva_rest", APIVersion: "2026-01-01", Endpoint: "GET /spreadsheets/{spreadsheetId}/sheets/{sheetId}/sheetdata", QueryRange: cell, Cache: "bypassed"}, Protection: "unprotected", Writable: "writable", LiteralWriteFormatPreservation: "preserves", LiteralWriteAPIVersion: "2026-01-01", LiteralWriteEndpoint: "POST /spreadsheets/{spreadsheetId}/sheets/{sheetId}/update"}
	m.FormattingSHA256, _ = workivaprovider.ContentFormattingHash(m.RawFormats, true, m.EffectiveFormats, true)
	return m, nil
}

func TestStageServicePersistsAgainstActiveSignedPolicyAndOwnedV19Candidate(t *testing.T) {
	store, db, ctx, now, profileID, profileHash, policyResolver := stageIntegrationFixture(t)
	provider := &verifiedStageProvider{}
	service := StageService{Store: store, Candidates: AssuranceCandidateResolver{Store: store}, Profiles: AssuranceProfileResolver{Resolver: policyResolver}, Provider: provider, Clock: fixedStageClock{now}}
	input := assurance.ContentIngest{SourceBytes: []byte("exact staged text"), MediaType: "text/plain", MetadataBLOB: []byte(`{"origin":{"kind":"analyst"},"provenance":{"availability":"unavailable"}}`), Items: []assurance.ContentItemInput{{LocalID: "text", Text: " exact staged text\n", MetadataBLOB: []byte(`{"kind_hint":"text"}`)}}}
	digest, err := assurance.ContentIngestRequestDigest(input)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := store.Reserve(ctx, assurance.ReservationRequest{ActorID: testTenant + "/" + testActor, Tool: "workiva_content_placement", Action: "ingest", IdempotencyDigest: assurance.DigestIdempotencyKey("stage-fixture-ingest-key"), RequestDigest: digest, RetainUntil: now.Add(1200 * time.Second)}, now)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.FinalizePolicyBoundContentIngest(ctx, reservation, input, digest, policyResolver, "audit-stage-fixture-ingest", now)
	if err != nil {
		t.Fatal(err)
	}
	request := StageRequest{CandidateKind: "extracted_item", CandidateID: created.ItemIDs["text"], SourceArtifactID: created.SourceArtifactID, DestinationProfileID: profileID, DestinationProfileRevision: 1, IdempotencyKey: "stage-integration-key-0001"}
	first, err := service.Stage(ctx, "audit-stage-integration", request)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if first.ConfirmationToken == "" || first.ReplayRequiresRestage || provider.calls.Load() != 1 {
		t.Fatalf("first response/provider calls: %+v/%d", first, provider.calls.Load())
	}
	var preview map[string]json.RawMessage
	if err := json.Unmarshal(first.Preview, &preview); err != nil {
		t.Fatal(err)
	}
	var frozen struct {
		OriginalText          string `json:"original_text"`
		CandidateMetadataHash string `json:"candidate_metadata_hash"`
		CandidateLineageHash  string `json:"candidate_lineage_hash"`
		PolicyBinding         struct {
			ActiveBundleHash string `json:"active_bundle_hash"`
		} `json:"policy_binding"`
	}
	if err := json.Unmarshal(first.Preview, &frozen); err != nil || frozen.OriginalText != " exact staged text\n" || frozen.CandidateMetadataHash == "" || frozen.CandidateLineageHash == "" || frozen.PolicyBinding.ActiveBundleHash != profileHash {
		t.Fatalf("preview lacks exact signed bindings: %+v err=%v", frozen, err)
	}
	var storedExpiry string
	if err := db.QueryRow(`SELECT expires_at FROM assurance_idempotency_records WHERE tenant_id=? AND tool='workiva_content_placement' AND action='stage'`, testTenant).Scan(&storedExpiry); err != nil {
		t.Fatal(err)
	}
	if got, err := time.Parse(time.RFC3339Nano, storedExpiry); err != nil || !got.Equal(now.Add(1200*time.Second)) {
		t.Fatalf("reservation retention=%s err=%v", storedExpiry, err)
	}
	// A sealed replay needs no policy, candidate, provider, or audit dependency.
	replayService := StageService{Store: store, Clock: fixedStageClock{now}}
	replay, err := replayService.Stage(ctx, "", request)
	if err != nil || replay.ConfirmationToken != "" || !replay.ReplayRequiresRestage || string(replay.Preview) != string(first.Preview) || provider.calls.Load() != 1 {
		t.Fatalf("replay=%+v calls=%d err=%v", replay, provider.calls.Load(), err)
	}
	changed := request
	changed.IdempotencyKey = "stage-integration-key-0002"
	if _, err := replayService.Stage(ctx, "", changed); err == nil || provider.calls.Load() != 1 {
		t.Fatalf("new key bypassed missing dependencies: err=%v provider calls=%d", err, provider.calls.Load())
	}
}

func TestStageServiceFreezesOwnedDraftOriginAndExactText(t *testing.T) {
	store, _, ctx, now, profileID, _, policyResolver := stageIntegrationFixture(t)
	provider := &verifiedStageProvider{}
	service := StageService{Store: store, Candidates: AssuranceCandidateResolver{Store: store}, Profiles: AssuranceProfileResolver{Resolver: policyResolver}, Provider: provider, Clock: fixedStageClock{now}}
	input := assurance.ContentIngest{
		SourceBytes: []byte("source for draft"), MediaType: "text/plain",
		MetadataBLOB: []byte(`{"origin":{"kind":"analyst"},"provenance":{"availability":"unavailable"}}`),
		Items:        []assurance.ContentItemInput{{LocalID: "source-item", Text: "supporting text", MetadataBLOB: []byte(`{"kind_hint":"text"}`)}},
		Drafts:       []assurance.ContentDraftInput{{LocalID: "draft", Text: "  Draft text, exactly.\n", ItemLocalIDs: []string{"source-item"}, MetadataBLOB: []byte(`{"origin":{"kind":"copilot"},"projection_trust":"caller_unverified"}`)}},
	}
	digest, err := assurance.ContentIngestRequestDigest(input)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := store.Reserve(ctx, assurance.ReservationRequest{ActorID: testTenant + "/" + testActor, Tool: "workiva_content_placement", Action: "ingest", IdempotencyDigest: assurance.DigestIdempotencyKey("draft-stage-ingest-key-0001"), RequestDigest: digest, RetainUntil: now.Add(1200 * time.Second)}, now)
	if err != nil {
		t.Fatal(err)
	}
	ingested, err := store.FinalizePolicyBoundContentIngest(ctx, reservation, input, digest, policyResolver, "audit-draft-stage-ingest", now)
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Stage(ctx, "audit-draft-stage", StageRequest{CandidateKind: "draft_artifact", CandidateID: ingested.DraftIDs["draft"], DestinationProfileID: profileID, DestinationProfileRevision: 1, IdempotencyKey: "draft-stage-preview-key-0001"})
	if err != nil {
		t.Fatalf("stage owned draft: %v", err)
	}
	var preview struct {
		CandidateKind       string `json:"candidate_kind"`
		CandidateOriginKind string `json:"origin_kind"`
		OriginalText        string `json:"original_text"`
	}
	if err := json.Unmarshal(response.Preview, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.CandidateKind != "draft_artifact" || preview.CandidateOriginKind != "copilot" || preview.OriginalText != "  Draft text, exactly.\n" || response.ConfirmationToken == "" || provider.calls.Load() != 1 {
		t.Fatalf("draft preview did not preserve trusted owned fields: %+v calls=%d", preview, provider.calls.Load())
	}
}

func TestStageRejectsExtractedCandidateAfterDerivedRetentionWhileSourceLives(t *testing.T) {
	store, _, ctx, now, profileID, _, policyResolver := stageIntegrationFixture(t)
	input := assurance.ContentIngest{SourceBytes: []byte("live source"), MediaType: "text/plain", MetadataBLOB: []byte(`{"origin":{"kind":"analyst"},"provenance":{"availability":"unavailable"}}`), Items: []assurance.ContentItemInput{{LocalID: "item", Text: "short-lived item", MetadataBLOB: []byte(`{"kind_hint":"text"}`)}}}
	digest, err := assurance.ContentIngestRequestDigest(input)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := store.Reserve(ctx, assurance.ReservationRequest{ActorID: testTenant + "/" + testActor, Tool: "workiva_content_placement", Action: "ingest", IdempotencyDigest: assurance.DigestIdempotencyKey("derived-expiry-ingest-key-01"), RequestDigest: digest, RetainUntil: now.Add(1200 * time.Second)}, now)
	if err != nil {
		t.Fatal(err)
	}
	ingested, err := store.FinalizePolicyBoundContentIngest(ctx, reservation, input, digest, policyResolver, "audit-derived-expiry-ingest", now)
	if err != nil {
		t.Fatal(err)
	}
	stageNow := now.Add(31 * time.Minute)
	provider := &verifiedStageProvider{}
	service := StageService{Store: store, Candidates: AssuranceCandidateResolver{Store: store}, Profiles: AssuranceProfileResolver{Resolver: policyResolver}, Provider: provider, Clock: fixedStageClock{stageNow}}
	_, err = service.Stage(ctx, "audit-expired-derived-item", StageRequest{CandidateKind: "extracted_item", CandidateID: ingested.ItemIDs["item"], SourceArtifactID: ingested.SourceArtifactID, DestinationProfileID: profileID, DestinationProfileRevision: 1, IdempotencyKey: "derived-expiry-stage-key-001"})
	var domain *assurance.Error
	if !errors.As(err, &domain) || domain.Code != "not_found_or_forbidden" || provider.calls.Load() != 0 {
		t.Fatalf("expired derived item stage err=%v provider calls=%d", err, provider.calls.Load())
	}
}

func TestStageFailureReplayPreservesStructuredSemanticCode(t *testing.T) {
	store, _, ctx, now, profileID, _, policyResolver := stageIntegrationFixture(t)
	provider := &verifiedStageProvider{}
	service := StageService{Store: store, Candidates: AssuranceCandidateResolver{Store: store}, Profiles: AssuranceProfileResolver{Resolver: policyResolver}, Provider: provider, Clock: fixedStageClock{now}}
	request := StageRequest{CandidateKind: "extracted_item", CandidateID: "missing-item", SourceArtifactID: "missing-source", DestinationProfileID: profileID, DestinationProfileRevision: 1, IdempotencyKey: "stage-failed-replay-key-001"}
	_, firstErr := service.Stage(ctx, "audit-stage-failed", request)
	var firstDomain *assurance.Error
	if !errors.As(firstErr, &firstDomain) || firstDomain.Code != "not_found_or_forbidden" || provider.calls.Load() != 0 {
		t.Fatalf("first failure=%v provider calls=%d", firstErr, provider.calls.Load())
	}
	replayService := StageService{Store: store, Clock: fixedStageClock{now}}
	_, replayErr := replayService.Stage(ctx, "", request)
	var replayDomain *assurance.Error
	if !errors.As(replayErr, &replayDomain) || replayDomain.Code != firstDomain.Code || provider.calls.Load() != 0 {
		t.Fatalf("replay failure=%v provider calls=%d", replayErr, provider.calls.Load())
	}
}

func stageIntegrationFixture(t *testing.T) (*assurance.Store, *sql.DB, context.Context, time.Time, string, string, *assurance.ContentPolicyResolver) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	store, err := assurance.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	store.SetAuditLog(log)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: testTenant, ObjectID: testActor, TokenType: identity.TokenTypeDelegated, Permissions: []identity.Permission{identity.PermissionContentStage, identity.PermissionContentIngest}})
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bundle := stageSignedBundle(t, priv)
	validated, err := assurance.ValidateBundle(bundle, ed25519.Sign(priv, bundle), testTenant, pub)
	if err != nil {
		t.Fatalf("validate fixture policy: %v", err)
	}
	if err = store.StageBundle(ctx, validated); err != nil {
		t.Fatal(err)
	}
	if err = store.RequestActivation(ctx, "content-stage-bundle", 1); err != nil {
		t.Fatal(err)
	}
	if err = store.Bootstrap(ctx, testTenant, pub); err != nil {
		t.Fatal(err)
	}
	resolver, err := assurance.NewContentPolicyResolver(store, pub, "content-stage-access")
	if err != nil {
		t.Fatal(err)
	}
	return store, db, ctx, now, "content-stage-profile", validated.ContentHash, resolver
}

func stageSignedBundle(t *testing.T, priv ed25519.PrivateKey) []byte {
	t.Helper()
	// Access ends before the signed idempotency horizon; retention governs
	// replay evidence and must not be mistaken for fresh-write authorization.
	access := assurance.ContentAccessPolicy{PolicyID: "content-stage-access", Revision: 1, State: "active", TenantID: testTenant, ActorObjectID: testActor, Capabilities: []string{"content.stage", "content.ingest"}, ResourceIDs: []string{"content-stage-resource"}, ValidUntil: "2026-10-10T14:00:00Z"}
	access.ContentHash = policyFixtureHash(t, access)
	retention := assurance.ContentRetentionPolicy{PolicyID: "content-stage-retention", Revision: 1, State: "active", SourceLifetimeSeconds: 3600, DraftLifetimeSeconds: 1800, IntentLifetimeSeconds: 600, IdempotencyLifetimeSeconds: 1200, RecoveryLifetimeSeconds: 600}
	retention.ContentHash = policyFixtureHash(t, retention)
	resource := assurance.ContentResourcePolicy{PolicyID: "content-stage-resource-policy", Revision: 1, State: "active", ResourceID: "content-stage-resource", SheetID: "content-stage-sheet", Cell: "B4", AccessPolicyID: access.PolicyID, AccessPolicyRevision: 1, AccessPolicyContentHash: access.ContentHash, Capability: "content.stage"}
	resource.ContentHash = policyFixtureHash(t, resource)
	provider := assurance.ContentProviderContract{PolicyID: "content-stage-provider", Revision: 1, State: "active", Provider: "workiva", APIVersion: "2026-01-01", SupportedOperations: []string{"single_cell_literal"}, RequiredFacts: []string{"formula", "protection", "writability", "formatting"}}
	provider.ContentHash = policyFixtureHash(t, provider)
	conversion := assurance.ConversionPolicy{PolicyID: "content-stage-copy", Revision: 1, Mode: "typed_copy", SourceKind: assurance.ValueText, TargetKind: assurance.ValueText}
	conversion.ContentHash = policyFixtureHash(t, conversion)
	interpretation := &assurance.DestinationInterpretation{ValueKind: strptr("text"), Period: assurance.ExplicitNullableString{Set: true}, Currency: assurance.ExplicitNullableString{Set: true}, Unit: assurance.ExplicitNullableString{Set: true}, StoredScale: numberptr("1"), DisplayScale: numberptr("1"), PercentBasis: strptr("not_percentage"), Precision: assurance.NullableInt{Set: true}}
	profile := assurance.DestinationProfile{SchemaVersion: intptr(1), DestinationProfileID: strptr("content-stage-profile"), Revision: intptr(1), ContentHash: strptr(strings.Repeat("0", 64)), State: strptr("active"), ResourceID: strptr(resource.ResourceID), SheetID: strptr(resource.SheetID), Cell: strptr(resource.Cell), Operation: strptr("single_cell_literal"), ExpectedInterpretation: interpretation, FormulaPolicy: strptr("reject_formula_target"), LiteralPolicy: strptr("literal_only"), ProtectionPolicy: strptr("reject_protected"), FormatPolicy: strptr("preserve"), UnknownCriticalFacts: strptr("block"), ProvenanceRequirement: strptr("source_or_verified_evidence"), MetadataRequirements: &assurance.DestinationMetadataRequirements{Formula: strptr("provider_verified"), Protection: strptr("provider_verified"), Writability: strptr("provider_verified"), Formatting: strptr("provider_verified"), Period: strptr("operator_declared_allowed"), Currency: strptr("operator_declared_allowed"), Unit: strptr("operator_declared_allowed"), Scale: strptr("operator_declared_allowed"), PercentBasis: strptr("operator_declared_allowed"), Precision: strptr("operator_declared_allowed")}, AllowedConversions: ptrConversions([]string{"typed_copy"}), ResourcePolicy: &assurance.ContentPolicyReference{PolicyID: resource.PolicyID, Revision: 1, ContentHash: resource.ContentHash}, ConversionPolicy: &assurance.ContentPolicyReference{PolicyID: conversion.PolicyID, Revision: 1, ContentHash: conversion.ContentHash}, ProviderContract: &assurance.ContentPolicyReference{PolicyID: provider.PolicyID, Revision: 1, ContentHash: provider.ContentHash}, RetentionPolicy: &assurance.ContentRetentionReference{PolicyID: retention.PolicyID, Revision: 1, ContentHash: retention.ContentHash, Class: "workflow"}, PreviewTTLSeconds: intptr(60)}
	profile.ContentHash = ptrProfileHash(t, profile)
	bundle := assurance.Bundle{SchemaVersion: 1, BundleID: "content-stage-bundle", BundleVersion: 1, TenantID: testTenant, Reports: []assurance.ReportRevision{{ReportID: "content-stage-report", Revision: 1, Name: "stage fixture", Owner: "test", Status: "active", RetentionClass: "standard", ResourcePolicyHash: strings.Repeat("a", 64), Periods: []assurance.Period{{Key: "2026-Q3", Label: "Q3 2026", Start: "2026-07-01", End: "2026-09-30"}}, Fields: []assurance.FieldDefinition{{FieldID: "stage-text", ResourceID: resource.ResourceID, ExternalResourceID: resource.ResourceID, SubresourceID: resource.SheetID, Locator: resource.Cell, Kind: assurance.ValueText, Required: true, Order: 1}}}}, DestinationProfiles: []assurance.DestinationProfile{profile}, ContentAccessPolicies: []assurance.ContentAccessPolicy{access}, ContentRetentionPolicies: []assurance.ContentRetentionPolicy{retention}, ContentResourcePolicies: []assurance.ContentResourcePolicy{resource}, ContentProviderContracts: []assurance.ContentProviderContract{provider}, ConversionPolicies: []assurance.ConversionPolicy{conversion}}
	raw, err := assurance.CanonicalJSON(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func policyFixtureHash(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "content_hash")
	canonical, err := assurance.CanonicalJSON(fields)
	if err != nil {
		t.Fatal(err)
	}
	h := assurance.HashBytes(canonical)
	return hex.EncodeToString(h[:])
}
func ptrProfileHash(t *testing.T, v assurance.DestinationProfile) *string {
	t.Helper()
	raw, _ := json.Marshal(v)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	delete(fields, "content_hash")
	canonical, err := assurance.CanonicalJSON(fields)
	if err != nil {
		t.Fatal(err)
	}
	h := assurance.HashBytes(canonical)
	value := hex.EncodeToString(h[:])
	return &value
}
func intptr(v int) *int                   { return &v }
func numberptr(v string) *json.Number     { n := json.Number(v); return &n }
func strptr(v string) *string             { return &v }
func ptrConversions(v []string) *[]string { return &v }
