package content

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

const (
	testTenant = "11111111-1111-4111-8111-111111111111"
	testActor  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

func TestBuildPreviewPlanPreservesExactTextAndDecimalAndReportsUnknownFacts(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	input := validInput(now)
	input.Candidate.Text = "  literal with spaces \n"
	input.Candidate.TextSHA256 = hashBytes([]byte(input.Candidate.Text))
	input.Candidate.Value, _ = json.Marshal(input.Candidate.Text)
	input.Candidate.Interpretation = textInterpretation()
	input.Profile.ExpectedInterpretation = input.Candidate.Interpretation
	input.Metadata = verifiedMetadata()

	plan, err := BuildPreviewPlan(stageContext(), input)
	if err != nil {
		t.Fatalf("BuildPreviewPlan: %v", err)
	}
	if plan.OriginalText != input.Candidate.Text {
		t.Fatalf("original text=%q want byte-exact %q", plan.OriginalText, input.Candidate.Text)
	}
	if string(plan.OriginalValue) != `"  literal with spaces \n"` || string(plan.IntendedValue) != string(plan.OriginalValue) {
		t.Fatalf("text literal changed: original=%s intended=%s", plan.OriginalValue, plan.IntendedValue)
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0] != "provider_format_preservation_unverified" {
		t.Fatalf("blockers=%v, want the unresolved provider format-preservation fact", plan.Blockers)
	}
	if plan.ProviderMetadata.RawValue == nil || plan.ProviderMetadata.RawFormats == nil {
		t.Fatal("provider observation raw values were not retained")
	}
	input.Metadata.LiteralWriteFormatPreservation = "preserves"
	input.Metadata.LiteralWriteAPIVersion = "2026-01-01"
	input.Metadata.LiteralWriteEndpoint = "POST /spreadsheets/{spreadsheetId}/sheets/{sheetId}/update"
	verifiedPlan, err := BuildPreviewPlan(stageContext(), input)
	if err != nil || len(verifiedPlan.Blockers) != 0 {
		t.Fatalf("explicit trusted write observation did not produce an unblocked plan: plan=%+v err=%v", verifiedPlan, err)
	}

	numeric := validInput(now)
	numeric.Metadata = verifiedMetadata()
	numeric.Candidate.Text = "123.4500"
	numeric.Candidate.TextSHA256 = hashBytes([]byte(numeric.Candidate.Text))
	numeric.Candidate.Value = json.RawMessage(`123.4500`)
	decimalPlan, err := BuildPreviewPlan(stageContext(), numeric)
	if err != nil {
		t.Fatalf("BuildPreviewPlan exact decimal: %v", err)
	}
	if string(decimalPlan.OriginalValue) != `123.4500` || string(decimalPlan.IntendedValue) != `123.4500` {
		t.Fatalf("decimal tokens changed: original=%s intended=%s", decimalPlan.OriginalValue, decimalPlan.IntendedValue)
	}
	if plan.ExpiresAt.Sub(plan.CreatedAt) > 900*time.Second || plan.ContentHash == "" {
		t.Fatalf("plan lifetime/hash invalid: created=%s expires=%s hash=%q", plan.CreatedAt, plan.ExpiresAt, plan.ContentHash)
	}
}

func TestBuildPreviewPlanRejectsAuthorizationOwnershipAndProfileMismatches(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		ctx    context.Context
		mutate func(*BuildInput)
	}{
		{name: "missing capability", ctx: identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: testTenant, ObjectID: testActor, TokenType: identity.TokenTypeDelegated})},
		{name: "cross actor candidate", ctx: stageContext(), mutate: func(i *BuildInput) { i.Candidate.ActorID = testTenant + "/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" }},
		{name: "stale profile", ctx: stageContext(), mutate: func(i *BuildInput) { i.Profile.Revision++ }},
		{name: "source lineage mismatch", ctx: stageContext(), mutate: func(i *BuildInput) { i.Candidate.SourceSHA256 = strings.Repeat("c", 64) }},
		{name: "expired candidate", ctx: stageContext(), mutate: func(i *BuildInput) { i.Candidate.ExpiresAt = now }},
		{name: "formula-like literal", ctx: stageContext(), mutate: func(i *BuildInput) {
			i.Candidate.Value = json.RawMessage(`"  =SUM(A1:A2)"`)
			i.Candidate.Interpretation = textInterpretation()
			i.Profile.ExpectedInterpretation = textInterpretation()
		}},
		{name: "formula target", ctx: stageContext(), mutate: func(i *BuildInput) { i.Metadata.RawValue = json.RawMessage(`"=A1+A2"`) }},
		{name: "unknown protection", ctx: stageContext(), mutate: func(i *BuildInput) { i.Metadata.Protection = "unknown"; i.Metadata.Writable = "writable" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := validInput(now)
			if tc.mutate != nil {
				tc.mutate(&input)
			}
			if _, err := BuildPreviewPlan(tc.ctx, input); err == nil {
				t.Fatal("BuildPreviewPlan succeeded for invalid authorization, ownership, profile, source or provider facts")
			}
		})
	}
}

func TestBuildPreviewPlanRejectsInterpretationGuessAndNonExactDecimal(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	t.Run("interpretation mismatch", func(t *testing.T) {
		input := validInput(now)
		period := "FY2025"
		input.Profile.ExpectedInterpretation.Period = &period
		if _, err := BuildPreviewPlan(stageContext(), input); err == nil {
			t.Fatal("candidate with missing period was accepted against a period-specific profile")
		}
	})
	t.Run("number cannot pass through float", func(t *testing.T) {
		input := validInput(now)
		input.Candidate.Value = json.RawMessage(`1e100000`)
		if _, err := BuildPreviewPlan(stageContext(), input); err == nil {
			t.Fatal("invalid numeric JSON scalar was accepted")
		}
	})
}

func TestBuildPreviewPlanPreservesSignedScaleNumberLexemes(t *testing.T) {
	input := validInput(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	input.Candidate.Interpretation.StoredScale = json.Number("1.00")
	input.Profile.ExpectedInterpretation.StoredScale = json.Number("1.00")
	input.Candidate.Interpretation.DisplayScale = json.Number("0.10000000000000001e1")
	input.Profile.ExpectedInterpretation.DisplayScale = json.Number("0.10000000000000001e1")
	plan, err := BuildPreviewPlan(stageContext(), input)
	if err != nil {
		t.Fatalf("exact signed scale lexemes rejected: %v", err)
	}
	if plan.TargetInterpretation.StoredScale != "1.00" || plan.TargetInterpretation.DisplayScale != "0.10000000000000001e1" {
		t.Fatalf("scale lexemes changed: %+v", plan.TargetInterpretation)
	}
}

func TestBuildPreviewPlanRequiresExactUncachedObservationCoordinates(t *testing.T) {
	input := validInput(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	input.Metadata.Provenance.Cache = "cached"
	if _, err := BuildPreviewPlan(stageContext(), input); err == nil {
		t.Fatal("cached observation accepted")
	}
	input = validInput(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	input.Metadata.Locator = "B2"
	if _, err := BuildPreviewPlan(stageContext(), input); err == nil {
		t.Fatal("observation for another cell accepted")
	}
}

func TestBuildPreviewPlanBindsProviderIdentityToSignedFamily(t *testing.T) {
	input := validInput(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	input.Metadata.Provenance.Provider = "arbitrary_adapter"
	if _, err := BuildPreviewPlan(stageContext(), input); err == nil {
		t.Fatal("provider identity outside signed Workiva family accepted")
	}
	input = validInput(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	input.Profile.ProviderAPIVersion = "future-version"
	if _, err := BuildPreviewPlan(stageContext(), input); err == nil {
		t.Fatal("profile provider API version not pinned")
	}
}

func TestBuildPreviewPlanBlocksWhenCriticalMetadataIsMissing(t *testing.T) {
	input := validInput(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	input.Metadata.FormatsPresent = false
	if _, err := BuildPreviewPlan(stageContext(), input); err == nil {
		t.Fatal("missing format-presence fact accepted")
	}
	input = validInput(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	input.Metadata = verifiedMetadata()
	input.Metadata.CalculatedValuePresent = true
	input.Metadata.CalculatedValue = json.RawMessage(`42`)
	if plan, err := BuildPreviewPlan(stageContext(), input); err != nil || plan.CurrentValueHash == "" {
		t.Fatalf("calculatedValue alone was treated as proof of a formula: plan=%+v err=%v", plan, err)
	}
}

func stageContext() context.Context {
	return identity.ContextWithPrincipal(context.Background(), identity.Principal{
		TenantID: testTenant, ObjectID: testActor, TokenType: identity.TokenTypeDelegated,
		Permissions: []identity.Permission{identity.PermissionContentStage},
	})
}

func validInput(now time.Time) BuildInput {
	interpretation := Interpretation{
		ValueKind: "number", StoredScale: json.Number("1"), DisplayScale: json.Number("1"),
		PercentBasis: "not_percentage", Precision: json.RawMessage(`4`),
	}
	candidateText := "123.4500"
	profileHash := strings.Repeat("a", 64)
	ref := func(id string) PolicyRef { return PolicyRef{PolicyID: id, Revision: 1, ContentHash: profileHash} }
	metadata := verifiedMetadata()
	candidateMetadata := json.RawMessage(`{"value_kind":"number"}`)
	sourceMetadata := json.RawMessage(`{"availability":"unavailable"}`)
	return BuildInput{
		Candidate: Candidate{
			Kind: "extracted_item", ID: "itm_1", Revision: 1, TenantID: testTenant,
			ActorID: testTenant + "/" + testActor, Text: candidateText, TextSHA256: hashBytes([]byte(candidateText)),
			Metadata: candidateMetadata, MetadataSHA256: hashBytes(candidateMetadata), LineageSHA256: strings.Repeat("d", 64),
			SourceID: "src_1", SourceSHA256: strings.Repeat("b", 64), SourceMetadataSHA256: hashBytes(sourceMetadata), LineageHashes: []string{strings.Repeat("b", 64)}, Provenance: sourceMetadata,
			Value: json.RawMessage(`123.4500`), Interpretation: interpretation,
			CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
		},
		Profile: DestinationProfile{
			TenantID: testTenant, ID: "profile-1", Revision: 3, ContentHash: profileHash, State: "active",
			ResourceID: "resource-1", SheetID: "sheet-1", Cell: "A1", Operation: "single_cell_literal",
			ExpectedInterpretation: interpretation, AllowedConversions: []string{"typed_copy"},
			ResourcePolicy: ref("resource"), ConversionPolicy: ref("conversion"), ProviderContract: ref("provider"),
			RetentionPolicy: RetentionRef{PolicyRef: ref("retention"), Class: "workflow"}, PreviewTTLSeconds: 300,
			ProvenanceRequirement: "source_or_verified_evidence",
			MetadataRequirements:  MetadataRequirements{Formula: "provider_verified", Protection: "provider_verified", Writability: "provider_verified", Formatting: "provider_verified", Period: "operator_declared_allowed", Currency: "operator_declared_allowed", Unit: "operator_declared_allowed", Scale: "operator_declared_allowed", PercentBasis: "operator_declared_allowed", Precision: "operator_declared_allowed"},
			IntentLifetimeSeconds: 3600, IdempotencyLifetimeSeconds: 7200,
			ProviderFamily: "workiva", ProviderAPIVersion: "2026-01-01",
		},
		RequestedProfileID: "profile-1", RequestedProfileRev: 3,
		ObservationID: "obs_1", ObservedAt: now.Add(-time.Second), Metadata: metadata,
		PlacementIntentID: "intent_1", CreatedAt: now,
		PolicyBinding: PolicyBinding{ActiveBundleHash: profileHash, ActiveBundleVersion: 4, AccessPolicyRef: ref("access"), AccessExpiresAt: now.Add(time.Hour)},
	}
}

func TestBuildPreviewPlanEnforcesSignedProvenanceAndSemanticRequirements(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	t.Run("fine grained source unavailable", func(t *testing.T) {
		input := validInput(now)
		input.Profile.ProvenanceRequirement = "fine_grained_available"
		if _, err := BuildPreviewPlan(stageContext(), input); err == nil {
			t.Fatal("missing fine-grained location accepted")
		}
	})
	t.Run("provider semantic fact unknown", func(t *testing.T) {
		input := validInput(now)
		input.Profile.MetadataRequirements.Period = "provider_verified"
		if _, err := BuildPreviewPlan(stageContext(), input); err == nil {
			t.Fatal("unsupported provider-verified period fact accepted")
		}
	})
	t.Run("fine grained source bound", func(t *testing.T) {
		input := validInput(now)
		input.Profile.ProvenanceRequirement = "fine_grained_available"
		input.Candidate.Provenance = json.RawMessage(`{"provenance":{"availability":"available","page":2,"segment":"paragraph 4"}}`)
		input.Candidate.SourceMetadataSHA256 = hashBytes(input.Candidate.Provenance)
		if _, err := BuildPreviewPlan(stageContext(), input); err != nil {
			t.Fatalf("valid source provenance rejected: %v", err)
		}
	})
}

func TestProjectExtractedTextCandidateDerivesExactLiteralAndExplicitNATextFacts(t *testing.T) {
	principal := identity.Principal{TenantID: testTenant, ObjectID: testActor, TokenType: identity.TokenTypeDelegated}
	text := "  preserve me exactly\n"
	metadata := json.RawMessage(`{"kind_hint":"text"}`)
	created := time.Now().UTC()
	expires := created.Add(time.Hour)
	sourceMetadata := json.RawMessage(`{"intake_policy":{"access_policy":{"policy_id":"access","revision":1,"content_hash":"` + strings.Repeat("a", 64) + `","valid_until":"2027-01-01T00:00:00Z"},"retention_policy":{"policy_id":"retention","revision":1,"content_hash":"` + strings.Repeat("b", 64) + `"},"source_expires_at":"` + expires.Add(time.Hour).Format(time.RFC3339Nano) + `","draft_expires_at":"` + expires.Format(time.RFC3339Nano) + `"}}`)
	record := assurance.ContentCandidate{Kind: assurance.ContentCandidateExtractedItem, ID: "item-1", OriginalText: text,
		MetadataBLOB: metadata, MetadataSHA256: hashBytes(metadata), CandidateSHA256: hashBytes([]byte(text)), SourceArtifactID: "source-1", SourceSHA256: strings.Repeat("b", 64), LineageSHA256: strings.Repeat("c", 64), SourceMetadataBLOB: sourceMetadata, CreatedAt: created, ExpiresAt: expires}
	candidate, err := ProjectExtractedTextCandidate(principal, record)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Text != text || string(candidate.Value) != `"  preserve me exactly\n"` || candidate.Interpretation.Period != nil || candidate.Interpretation.Currency != nil || candidate.Interpretation.Unit != nil || candidate.Interpretation.StoredScale.String() != "1" || candidate.Interpretation.DisplayScale.String() != "1" || candidate.Interpretation.PercentBasis != "not_percentage" || string(candidate.Interpretation.Precision) != "null" {
		t.Fatalf("text projection invented or changed facts: %+v value=%s", candidate, candidate.Value)
	}
}

func TestProjectOwnedDraftRequiresExactOriginAndUsesDraftTextSemantics(t *testing.T) {
	principal := identity.Principal{TenantID: testTenant, ObjectID: testActor, TokenType: identity.TokenTypeDelegated}
	text := "  draft stays exact\n"
	created := time.Now().UTC()
	expires := created.Add(time.Hour)
	sourceMetadata := json.RawMessage(`{"intake_policy":{"access_policy":{"policy_id":"access","revision":1,"content_hash":"` + strings.Repeat("a", 64) + `","valid_until":"2027-01-01T00:00:00Z"},"retention_policy":{"policy_id":"retention","revision":1,"content_hash":"` + strings.Repeat("b", 64) + `"},"source_expires_at":"` + expires.Add(time.Hour).Format(time.RFC3339Nano) + `","draft_expires_at":"` + expires.Format(time.RFC3339Nano) + `"}}`)
	makeRecord := func(metadata string) assurance.ContentCandidate {
		raw := json.RawMessage(metadata)
		return assurance.ContentCandidate{Kind: assurance.ContentCandidateDraftArtifact, ID: "draft-1", OriginalText: text, MetadataBLOB: raw, MetadataSHA256: hashBytes(raw), CandidateSHA256: hashBytes([]byte(text)), SourceArtifactID: "source-1", SourceSHA256: strings.Repeat("c", 64), SourceMetadataBLOB: sourceMetadata, LineageSHA256: strings.Repeat("d", 64), CreatedAt: created, ExpiresAt: expires}
	}
	candidate, err := ProjectExtractedTextCandidate(principal, makeRecord(`{"origin":{"kind":"copilot"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if candidate.CandidateOriginKind != "copilot" || candidate.Interpretation.ValueKind != "text" || candidate.Text != text || string(candidate.Value) != `"  draft stays exact\n"` {
		t.Fatalf("draft projection lost origin or exact text semantics: %+v value=%s", candidate, candidate.Value)
	}
	for _, metadata := range []string{`{}`, `{"origin":{"kind":"Copilot"}}`, `{"origin":{"kind":"analyst "}}`, `{"origin":{"kind":"analyst"},"kind_hint":"number"}`} {
		if _, err := ProjectExtractedTextCandidate(principal, makeRecord(metadata)); err == nil {
			t.Errorf("invalid draft metadata accepted: %s", metadata)
		}
	}
}

func TestProjectExtractedCandidateBlocksPublicNumericScaleWithoutExactProviderInterpretation(t *testing.T) {
	principal := identity.Principal{TenantID: testTenant, ObjectID: testActor, TokenType: identity.TokenTypeDelegated}
	now := time.Now().UTC()
	source := json.RawMessage(`{"intake_policy":{"access_policy":{"policy_id":"access","revision":1,"content_hash":"` + strings.Repeat("a", 64) + `","valid_until":"2027-01-01T00:00:00Z"},"retention_policy":{"policy_id":"retention","revision":1,"content_hash":"` + strings.Repeat("b", 64) + `"},"source_expires_at":"` + now.Add(time.Hour).Format(time.RFC3339Nano) + `","draft_expires_at":"` + now.Add(time.Hour).Format(time.RFC3339Nano) + `"}}`)
	makeRecord := func(text string, metadata json.RawMessage) assurance.ContentCandidate {
		return assurance.ContentCandidate{Kind: assurance.ContentCandidateExtractedItem, ID: "numeric-item", OriginalText: text, MetadataBLOB: metadata, MetadataSHA256: hashBytes(metadata), CandidateSHA256: hashBytes([]byte(text)), SourceArtifactID: "source", SourceSHA256: strings.Repeat("c", 64), SourceMetadataBLOB: source, LineageSHA256: strings.Repeat("d", 64), CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	}
	metadata := json.RawMessage(`{"kind_hint":"number","interpretation":{"period":"FY2025","currency":"USD","unit":"currency","scale":"millions","percent_basis":null,"precision":1}}`)
	if _, err := ProjectExtractedTextCandidate(principal, makeRecord("12.4", metadata)); err == nil {
		t.Fatal("public categorical scale was treated as exact stored/display scale")
	} else {
		var domain *assurance.Error
		if !errors.As(err, &domain) || domain.Code != "content_candidate_interpretation_incomplete" {
			t.Fatalf("numeric clarification error=%v", err)
		}
	}
}

func verifiedMetadata() workivaprovider.ContentMetadata {
	metadata := workivaprovider.ContentMetadata{
		SpreadsheetID: "resource-1", SheetID: "sheet-1", Locator: "A1",
		RawValue: json.RawMessage(`null`), ValuePresent: true,
		RawFormats: json.RawMessage(`{}`), FormatsPresent: true,
		EffectiveFormats: json.RawMessage(`{}`), EffectiveFormatsPresent: true,
		Provenance: workivaprovider.ContentMetadataProvenance{
			Provider: "workiva_rest", APIVersion: "2026-01-01",
			Endpoint: "GET /spreadsheets/{spreadsheetId}/sheets/{sheetId}/sheetdata", QueryRange: "A1", Cache: "bypassed",
		},
		Protection: "unprotected", Writable: "writable",
		LiteralWriteFormatPreservation: "unknown",
	}
	metadata.FormattingSHA256, _ = workivaprovider.ContentFormattingHash(metadata.RawFormats, metadata.FormatsPresent, metadata.EffectiveFormats, metadata.EffectiveFormatsPresent)
	return metadata
}

func textInterpretation() Interpretation {
	return Interpretation{
		ValueKind: "text", StoredScale: json.Number("1"), DisplayScale: json.Number("1"),
		PercentBasis: "not_percentage", Precision: json.RawMessage(`0`),
	}
}
