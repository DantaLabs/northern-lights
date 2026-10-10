package assurance

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	"gopkg.in/yaml.v3"
)

const contentTestActor = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

func contentTestBundle(t *testing.T, private ed25519.PrivateKey, profileState, actor string, destinationCapabilities ...identity.Permission) ([]byte, []byte) {
	t.Helper()
	capability := identity.PermissionContentStage
	if len(destinationCapabilities) > 0 {
		capability = destinationCapabilities[0]
	}
	accessCapabilities := []string{"content.ingest"}
	for _, granted := range destinationCapabilities {
		accessCapabilities = append(accessCapabilities, string(granted))
	}
	if len(destinationCapabilities) == 0 {
		accessCapabilities = append(accessCapabilities, string(capability))
	}
	access := ContentAccessPolicy{PolicyID: "content-access", Revision: 1, State: "active", TenantID: testTenant, ActorObjectID: actor, Capabilities: accessCapabilities, ResourceIDs: []string{"resource-1"}, ValidUntil: "2027-01-01T00:00:00Z"}
	if err := validateContentAccessPolicy(&access, testTenant); err != nil {
		t.Fatal(err)
	}
	retention := ContentRetentionPolicy{PolicyID: "content-retention", Revision: 1, State: "active", SourceLifetimeSeconds: 86400, DraftLifetimeSeconds: 43200, IntentLifetimeSeconds: 3600, IdempotencyLifetimeSeconds: 7200, RecoveryLifetimeSeconds: 3600}
	if err := validateContentRetentionPolicy(&retention); err != nil {
		t.Fatal(err)
	}
	resource := ContentResourcePolicy{PolicyID: "resource-policy", Revision: 1, State: "active", ResourceID: "resource-1", SheetID: "sheet-1", Cell: "B4", AccessPolicyID: access.PolicyID, AccessPolicyRevision: access.Revision, AccessPolicyContentHash: access.ContentHash, Capability: string(capability)}
	if err := validateContentResourcePolicy(&resource); err != nil {
		t.Fatal(err)
	}
	provider := ContentProviderContract{PolicyID: "provider-contract", Revision: 1, State: "active", Provider: "workiva", APIVersion: "2026-01-01", SupportedOperations: []string{"single_cell_literal"}, RequiredFacts: []string{"formula", "protection", "writability", "formatting"}}
	if err := validateContentProviderContract(&provider); err != nil {
		t.Fatal(err)
	}
	conversion := ConversionPolicy{PolicyID: "exact-copy", Revision: 1, Mode: "typed_copy", SourceKind: ValueNumber, SourceUnit: "kWh", TargetKind: ValueNumber, TargetUnit: "kWh"}
	convRaw, _ := CanonicalJSON(conversionPolicyContent(conversion))
	convHash := digestHex(HashBytes(convRaw))
	profile := DestinationProfile{
		SchemaVersion: intPtr(1), DestinationProfileID: stringPtr("destination-q3"), Revision: intPtr(1), ContentHash: stringPtr(strings.Repeat("0", 64)), State: stringPtr(profileState), ResourceID: stringPtr(resource.ResourceID), SheetID: stringPtr(resource.SheetID), Cell: stringPtr(resource.Cell), Operation: stringPtr("single_cell_literal"),
		ExpectedInterpretation: &DestinationInterpretation{ValueKind: stringPtr("number"), Period: ExplicitNullableString{Set: true, Value: stringPtr("2026-Q3")}, Currency: ExplicitNullableString{Set: true}, Unit: ExplicitNullableString{Set: true, Value: stringPtr("kWh")}, StoredScale: numberPtr(json.Number("1.00000000000000000001")), DisplayScale: numberPtr(json.Number("1.00")), PercentBasis: stringPtr("not_percentage"), Precision: NullableInt{Set: true}},
		FormulaPolicy:          stringPtr("reject_formula_target"), LiteralPolicy: stringPtr("literal_only"), ProtectionPolicy: stringPtr("reject_protected"), FormatPolicy: stringPtr("preserve"), UnknownCriticalFacts: stringPtr("block"), ProvenanceRequirement: stringPtr("source_or_verified_evidence"),
		MetadataRequirements: &DestinationMetadataRequirements{Formula: stringPtr("provider_verified"), Protection: stringPtr("provider_verified"), Writability: stringPtr("provider_verified"), Formatting: stringPtr("provider_verified"), Period: stringPtr("operator_declared_allowed"), Currency: stringPtr("operator_declared_allowed"), Unit: stringPtr("operator_declared_allowed"), Scale: stringPtr("operator_declared_allowed"), PercentBasis: stringPtr("operator_declared_allowed"), Precision: stringPtr("operator_declared_allowed")},
		AllowedConversions:   &[]string{"typed_copy"}, ResourcePolicy: &ContentPolicyReference{PolicyID: resource.PolicyID, Revision: resource.Revision, ContentHash: resource.ContentHash}, ConversionPolicy: &ContentPolicyReference{PolicyID: conversion.PolicyID, Revision: conversion.Revision, ContentHash: convHash}, ProviderContract: &ContentPolicyReference{PolicyID: provider.PolicyID, Revision: provider.Revision, ContentHash: provider.ContentHash}, RetentionPolicy: &ContentRetentionReference{PolicyID: retention.PolicyID, Revision: retention.Revision, ContentHash: retention.ContentHash, Class: "workflow"}, PreviewTTLSeconds: intPtr(300),
	}
	profileRaw, _ := CanonicalJSON(destinationProfileContent(profile))
	profileHash := digestHex(HashBytes(profileRaw))
	profile.ContentHash = &profileHash
	return signedBundle(t, private, func(b *Bundle) {
		b.DestinationProfiles = []DestinationProfile{profile}
		b.ContentAccessPolicies = []ContentAccessPolicy{access}
		b.ContentRetentionPolicies = []ContentRetentionPolicy{retention}
		b.ContentResourcePolicies = []ContentResourcePolicy{resource}
		b.ContentProviderContracts = []ContentProviderContract{provider}
		b.ConversionPolicies = []ConversionPolicy{conversion}
	})
}

func TestSignedResourceGrantCanBeUsedAcrossSeparatelyGrantedContentPhases(t *testing.T) {
	store, _ := openTestStore(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, sig := contentTestBundle(t, priv, "active", contentTestActor, identity.PermissionContentStage, identity.PermissionContentConfirm)
	validated, err := ValidateBundle(raw, sig, testTenant, pub)
	if err != nil {
		t.Fatal(err)
	}
	admin := contentStageContext(contentTestActor, identity.PermissionContentStage, identity.PermissionContentConfirm)
	if err = store.StageBundle(admin, validated); err != nil {
		t.Fatal(err)
	}
	if err = store.RequestActivation(admin, "bundle-1", 1); err != nil {
		t.Fatal(err)
	}
	if err = store.Bootstrap(admin, testTenant, pub); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewContentPolicyResolver(store, pub, "content-access")
	if err != nil {
		t.Fatal(err)
	}
	confirmCtx := contentStageContext(contentTestActor, identity.PermissionContentConfirm)
	profile, bindings, err := resolver.ResolveContentDestinationForCapability(confirmCtx, "destination-q3", 1, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), identity.PermissionContentConfirm)
	if err != nil {
		t.Fatalf("resolve separately granted confirmation phase: %v", err)
	}
	if *profile.Cell != "B4" || bindings.Resource.Capability != string(identity.PermissionContentStage) || !contentPolicyContains(bindings.AccessPolicy.Capabilities, string(identity.PermissionContentConfirm)) {
		t.Fatalf("resource/access phase bindings=%+v", bindings)
	}
}
func intPtr(v int) *int                    { return &v }
func numberPtr(v json.Number) *json.Number { return &v }
func stringPtr(v string) *string           { return &v }
func contentStageContext(actor string, permissions ...identity.Permission) context.Context {
	return identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: testTenant, ObjectID: actor, TokenType: identity.TokenTypeDelegated, Permissions: permissions})
}

func contentPolicyVariant(t *testing.T, private ed25519.PrivateKey, bundleID string, bundleVersion, reportRevision, profileRevision, previewTTL int) *ValidatedBundle {
	t.Helper()
	raw, _ := contentTestBundle(t, private, "active", contentTestActor)
	bundle, err := decodeStrictBundle(raw)
	if err != nil {
		t.Fatal(err)
	}
	bundle.BundleID = bundleID
	bundle.BundleVersion = bundleVersion
	bundle.Reports[0].Revision = reportRevision
	profile := &bundle.DestinationProfiles[0]
	profile.Revision = intPtr(profileRevision)
	profile.PreviewTTLSeconds = intPtr(previewTTL)
	canonical, err := CanonicalJSON(destinationProfileContent(*profile))
	if err != nil {
		t.Fatal(err)
	}
	hash := digestHex(HashBytes(canonical))
	profile.ContentHash = &hash
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err = CanonicalJSONBytes(encoded)
	if err != nil {
		t.Fatal(err)
	}
	pub := private.Public().(ed25519.PublicKey)
	validated, err := ValidateBundle(canonical, ed25519.Sign(private, canonical), testTenant, pub)
	if err != nil {
		t.Fatal(err)
	}
	return validated
}

func TestSignedContentPolicyActivationAndResolution(t *testing.T) {
	store, _ := openTestStore(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, sig := contentTestBundle(t, priv, "active", contentTestActor)
	validated, err := ValidateBundle(raw, sig, testTenant, pub)
	if err != nil {
		t.Fatalf("ValidateBundle: %v", err)
	}
	ctx := contentStageContext(contentTestActor, identity.PermissionContentStage, identity.PermissionContentIngest)
	if err = store.StageBundle(ctx, validated); err != nil {
		t.Fatalf("StageBundle: %v", err)
	}
	if err = store.RequestActivation(ctx, "bundle-1", 1); err != nil {
		t.Fatal(err)
	}
	if err = store.Bootstrap(ctx, testTenant, pub); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewContentPolicyResolver(store, pub, "content-access")
	if err != nil {
		t.Fatal(err)
	}
	profile, bindings, err := resolver.ResolveContentDestination(ctx, "destination-q3", 1, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ResolveContentDestination: %v", err)
	}
	if *profile.Cell != "B4" || bindings.Resource.ResourceID != "resource-1" || bindings.ProviderContract.Provider != "workiva" || bindings.AccessPolicy.PolicyID != "content-access" || bindings.ActiveBundleVersion != 1 || bindings.ActiveBundleHash != validated.ContentHash {
		t.Fatalf("unexpected resolved policy bindings: %+v", bindings)
	}
	if _, _, err = resolver.ResolveContentIntakePolicy(ctx, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("ResolveContentIntakePolicy: %v", err)
	}
}

func TestContentPolicyResolutionFailsClosedForIdentityRetirementAndTamper(t *testing.T) {
	for _, tc := range []struct {
		name, state, actor string
		permissions        []identity.Permission
		tamper             bool
		wantErr            bool
	}{
		{name: "cross actor", state: "active", actor: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", permissions: []identity.Permission{identity.PermissionContentStage}, wantErr: true},
		{name: "retired", state: "retired", actor: contentTestActor, permissions: []identity.Permission{identity.PermissionContentStage}, wantErr: true},
		{name: "missing capability", state: "active", actor: contentTestActor, wantErr: true},
		{name: "tampered active bundle", state: "active", actor: contentTestActor, permissions: []identity.Permission{identity.PermissionContentStage}, tamper: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, db := openTestStore(t)
			pub, priv, _ := ed25519.GenerateKey(rand.Reader)
			raw, sig := contentTestBundle(t, priv, tc.state, tc.actor)
			valid, err := ValidateBundle(raw, sig, testTenant, pub)
			if err != nil {
				t.Fatal(err)
			}
			ctx := contentStageContext(contentTestActor, identity.PermissionContentStage, identity.PermissionContentIngest)
			if err = store.StageBundle(ctx, valid); err != nil {
				t.Fatal(err)
			}
			if err = store.RequestActivation(ctx, "bundle-1", 1); err != nil {
				t.Fatal(err)
			}
			if err = store.Bootstrap(ctx, testTenant, pub); err != nil {
				t.Fatal(err)
			}
			if tc.tamper {
				if _, err = db.Exec(`UPDATE assurance_active_bundles SET bundle_json='{}' WHERE tenant_id=?`, testTenant); err != nil {
					t.Fatal(err)
				}
			}
			resolver, _ := NewContentPolicyResolver(store, pub, "content-access")
			callctx := contentStageContext(contentTestActor, tc.permissions...)
			_, _, err = resolver.ResolveContentDestination(callctx, "destination-q3", 1, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolve error=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestContentPolicyCapabilityIsCheckedBeforeBundleRead(t *testing.T) {
	store, _ := openTestStore(t)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	resolver, err := NewContentPolicyResolver(store, pub, "content-access")
	if err != nil {
		t.Fatal(err)
	}
	ctx := contentStageContext(contentTestActor)
	if _, _, err = resolver.ResolveContentDestination(ctx, "destination-q3", 1, time.Now()); err == nil || !strings.HasPrefix(err.Error(), "strong_identity_required:") {
		t.Fatalf("missing capability result=%v, expected authorization failure before store readiness lookup", err)
	}
}

func TestStageBundleRevalidatesMutableValidatedBundleFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ValidatedBundle)
	}{
		{name: "public projection", mutate: func(b *ValidatedBundle) { b.Bundle.BundleID = "mutated-bundle" }},
		{name: "stored canonical hash", mutate: func(b *ValidatedBundle) { b.ContentHash = strings.Repeat("f", 64) }},
		{name: "canonical bytes", mutate: func(b *ValidatedBundle) { b.CanonicalJSON = append(b.CanonicalJSON, ' ') }},
		{name: "signature", mutate: func(b *ValidatedBundle) { b.Signature[0] ^= 0xff }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, db := openTestStore(t)
			pub, priv, _ := ed25519.GenerateKey(rand.Reader)
			raw, signature := contentTestBundle(t, priv, "active", contentTestActor)
			bundle, err := ValidateBundle(raw, signature, testTenant, pub)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(bundle)
			ctx := contentStageContext(contentTestActor, identity.PermissionContentStage)
			if err := store.StageBundle(ctx, bundle); err == nil {
				t.Fatal("mutated validated bundle was staged")
			}
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM assurance_bundle_candidates`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("candidate persisted despite failed signature revalidation: %d", count)
			}
		})
	}
}

func TestContentDestinationCapabilityMustMatchCallerAndSignedScope(t *testing.T) {
	for _, tc := range []struct {
		name      string
		signed    identity.Permission
		caller    []identity.Permission
		requested identity.Permission
		wantErr   bool
	}{
		{name: "confirm exact grant", signed: identity.PermissionContentConfirm, caller: []identity.Permission{identity.PermissionContentConfirm}, requested: identity.PermissionContentConfirm},
		{name: "stage does not imply confirm", signed: identity.PermissionContentConfirm, caller: []identity.Permission{identity.PermissionContentStage}, requested: identity.PermissionContentConfirm, wantErr: true},
		{name: "confirm does not imply stage", signed: identity.PermissionContentStage, caller: []identity.Permission{identity.PermissionContentConfirm}, requested: identity.PermissionContentConfirm, wantErr: true},
		{name: "acknowledge exact grant", signed: identity.PermissionContentAcknowledge, caller: []identity.Permission{identity.PermissionContentAcknowledge}, requested: identity.PermissionContentAcknowledge},
		{name: "reconcile exact grant", signed: identity.PermissionContentReconcile, caller: []identity.Permission{identity.PermissionContentReconcile}, requested: identity.PermissionContentReconcile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := openTestStore(t)
			pub, priv, _ := ed25519.GenerateKey(rand.Reader)
			raw, sig := contentTestBundle(t, priv, "active", contentTestActor, tc.signed)
			validated, err := ValidateBundle(raw, sig, testTenant, pub)
			if err != nil {
				t.Fatal(err)
			}
			stageCtx := contentStageContext(contentTestActor, identity.PermissionContentStage, identity.PermissionContentIngest)
			if err = store.StageBundle(stageCtx, validated); err != nil {
				t.Fatal(err)
			}
			if err = store.RequestActivation(stageCtx, "bundle-1", 1); err != nil {
				t.Fatal(err)
			}
			if err = store.Bootstrap(stageCtx, testTenant, pub); err != nil {
				t.Fatal(err)
			}
			resolver, err := NewContentPolicyResolver(store, pub, "content-access")
			if err != nil {
				t.Fatal(err)
			}
			ctx := contentStageContext(contentTestActor, tc.caller...)
			_, bindings, err := resolver.ResolveContentDestinationForCapability(ctx, "destination-q3", 1, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), tc.requested)
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolve error=%v wantErr=%v", err, tc.wantErr)
			}
			if err == nil && (bindings.Resource.Capability != string(tc.requested) || !contentPolicyContains(bindings.AccessPolicy.Capabilities, string(tc.requested)) || bindings.ActiveBundleHash != validated.ContentHash) {
				t.Fatalf("returned bindings do not freeze the requested capability and active bundle: %+v", bindings)
			}
		})
	}
}

func TestContentDestinationRejectsUnsupportedCapabilityBeforeBundleRead(t *testing.T) {
	store, _ := openTestStore(t)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	resolver, err := NewContentPolicyResolver(store, pub, "content-access")
	if err != nil {
		t.Fatal(err)
	}
	ctx := contentStageContext(contentTestActor, identity.PermissionContentIngest)
	if _, _, err = resolver.ResolveContentDestinationForCapability(ctx, "destination-q3", 1, time.Now(), identity.PermissionContentIngest); err == nil || !strings.HasPrefix(err.Error(), "content_policy_capability_invalid:") {
		t.Fatalf("unsupported destination capability result=%v", err)
	}
}

func TestContentDestinationRequiredNullableMembersAndLegacyCanonicalSignature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	raw, _ := contentTestBundle(t, priv, "active", contentTestActor)
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	profiles := object["destination_profiles"].([]any)
	profile := profiles[0].(map[string]any)
	interpretation := profile["expected_interpretation"].(map[string]any)
	delete(interpretation, "period")
	badRaw, err := CanonicalJSON(object)
	if err != nil {
		t.Fatal(err)
	}
	badSig := ed25519.Sign(priv, badRaw)
	if _, err = ValidateBundle(badRaw, badSig, testTenant, pub); err == nil {
		t.Fatal("omitted required nullable period accepted")
	}
	oldRaw, oldSig := signedBundle(t, priv, nil)
	old, err := ValidateBundle(oldRaw, oldSig, testTenant, pub)
	if err != nil {
		t.Fatalf("legacy signed bundle rejected: %v", err)
	}
	var oldObject map[string]json.RawMessage
	if err = json.Unmarshal(old.CanonicalJSON, &oldObject); err != nil {
		t.Fatal(err)
	}
	if _, exists := oldObject["destination_profiles"]; exists {
		t.Fatal("empty additive policy fields changed legacy canonical bytes")
	}
}

func TestDestinationScalePreservesExactJSONAndYAMLLexemes(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	raw, sig := contentTestBundle(t, priv, "active", contentTestActor)
	validated, err := ValidateBundle(raw, sig, testTenant, pub)
	if err != nil {
		t.Fatal(err)
	}
	got := validated.Bundle.DestinationProfiles[0].ExpectedInterpretation
	if got.StoredScale == nil || got.StoredScale.String() != "1.00000000000000000001" || got.DisplayScale == nil || got.DisplayScale.String() != "1.00" {
		t.Fatalf("signed JSON scale lexemes changed: stored=%v display=%v", got.StoredScale, got.DisplayScale)
	}
	var profile DestinationProfile
	if err := yaml.Unmarshal([]byte("expected_interpretation:\n  stored_scale: 1.00000000000000000001\n  display_scale: 1.00\n"), &profile); err != nil {
		t.Fatal(err)
	}
	if profile.ExpectedInterpretation.StoredScale.String() != "1.00000000000000000001" || profile.ExpectedInterpretation.DisplayScale.String() != "1.00" {
		t.Fatalf("YAML scale lexemes changed: stored=%v display=%v", profile.ExpectedInterpretation.StoredScale, profile.ExpectedInterpretation.DisplayScale)
	}
}

func TestDestinationScaleValidationUsesExactDecimalBounds(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{{"0.00000000000000000001", true}, {"1e-30", true}, {"1000000000000", true}, {"1000000000000.0000001", false}, {"0", false}, {"-1", false}, {"1e999", false}} {
		n := json.Number(tc.value)
		if got := validPositiveContentScale(&n); got != tc.valid {
			t.Errorf("validPositiveContentScale(%q)=%t want %t", tc.value, got, tc.valid)
		}
	}
}

func TestContentPolicyMissingUnsignedWrongTenantAndRevisionReuse(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	store, _ := openTestStore(t)
	ctx := contentStageContext(contentTestActor, identity.PermissionContentStage)
	resolver, err := NewContentPolicyResolver(store, pub, "content-access")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = resolver.ResolveContentDestination(ctx, "unknown", 1, time.Now()); err == nil {
		t.Fatal("missing active bundle resolved")
	}
	raw, sig := contentTestBundle(t, priv, "active", contentTestActor)
	if _, err = ValidateBundle(raw, nil, testTenant, pub); err == nil {
		t.Fatal("unsigned policy bundle accepted")
	}
	if _, err = ValidateBundle(raw, sig, "22222222-2222-4222-8222-222222222222", pub); err == nil {
		t.Fatal("cross-tenant signed bundle accepted")
	}
	validated, err := ValidateBundle(raw, sig, testTenant, pub)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.StageBundle(ctx, validated); err != nil {
		t.Fatal(err)
	}
	if err = store.RequestActivation(ctx, "bundle-1", 1); err != nil {
		t.Fatal(err)
	}
	if err = store.Bootstrap(ctx, testTenant, pub); err != nil {
		t.Fatal(err)
	}
	bundle, err := decodeStrictBundle(raw)
	if err != nil {
		t.Fatal(err)
	}
	bundle.BundleID = "bundle-2"
	bundle.BundleVersion = 2
	bundle.Reports[0].Revision = 2
	ttl := *bundle.DestinationProfiles[0].PreviewTTLSeconds + 1
	bundle.DestinationProfiles[0].PreviewTTLSeconds = &ttl
	profileRaw, _ := CanonicalJSON(destinationProfileContent(bundle.DestinationProfiles[0]))
	profileHash := digestHex(HashBytes(profileRaw))
	bundle.DestinationProfiles[0].ContentHash = &profileHash
	encoded, _ := json.Marshal(bundle)
	canonical, _ := CanonicalJSONBytes(encoded)
	next, err := ValidateBundle(canonical, ed25519.Sign(priv, canonical), testTenant, pub)
	if err != nil {
		t.Fatalf("validate successor: %v", err)
	}
	if err = store.StageBundle(ctx, next); err == nil {
		t.Fatal("changed content reused a destination profile revision")
	}
}

func TestContentPolicyHistoryCheckAndCandidateInsertAreAtomic(t *testing.T) {
	db, err := sql.Open("sqlite", fmt.Sprintf("file:content-policy-%d?mode=memory&cache=shared", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	auditLog, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	store.SetAuditLog(auditLog)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	ctx := contentStageContext(contentTestActor)
	first := contentPolicyVariant(t, priv, "candidate-a", 1, 1, 1, 300)
	second := contentPolicyVariant(t, priv, "candidate-b", 1, 1, 1, 301)
	results := make(chan error, 2)
	go func() { results <- store.StageBundle(ctx, first) }()
	go func() { results <- store.StageBundle(ctx, second) }()
	err1, err2 := <-results, <-results
	if (err1 == nil) == (err2 == nil) {
		t.Fatalf("expected exactly one competing revision to stage, got %v / %v", err1, err2)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_bundle_candidates WHERE tenant_id=?`, testTenant).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("stored candidate count=%d, want one", count)
	}
}

func TestContentPolicyCandidateInsertFaultRollsBackStagingTransaction(t *testing.T) {
	store, db := openTestStore(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	ctx := contentStageContext(contentTestActor)
	candidate := contentPolicyVariant(t, priv, "fault-bundle", 1, 1, 1, 300)
	if _, err := db.Exec(`CREATE TRIGGER fail_content_candidate BEFORE INSERT ON assurance_bundle_candidates WHEN NEW.bundle_id='fault-bundle' BEGIN SELECT RAISE(ABORT,'injected candidate insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.StageBundle(ctx, candidate); err == nil {
		t.Fatal("injected candidate insert failure was ignored")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_bundle_candidates WHERE tenant_id=?`, testTenant).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("candidate transaction left %d rows", count)
	}
}

func TestContentPolicyRejectsReferenceBoundsAndResourceSubstitution(t *testing.T) {
	provider := ContentProviderContract{PolicyID: "provider", Revision: 1, State: "active", Provider: "workiva", APIVersion: "v1", SupportedOperations: []string{"single_cell_literal"}, RequiredFacts: []string{"formula", "protection", "writability", "formatting"}}
	if err := validateContentProviderContract(&provider); err == nil {
		t.Fatal("unsupported provider API version accepted")
	}
	provider.APIVersion = "2026-01-01"
	provider.RequiredFacts = append(provider.RequiredFacts, "caller_assertion")
	if err := validateContentProviderContract(&provider); err == nil {
		t.Fatal("unapproved provider fact accepted")
	}
	resource := ContentResourcePolicy{PolicyID: "resource", Revision: maxRevision + 1, State: "active", ResourceID: "resource-1", SheetID: "sheet-1", Cell: "B4", AccessPolicyID: "access", AccessPolicyRevision: 1, AccessPolicyContentHash: strings.Repeat("a", 64), Capability: "content.stage"}
	if err := validateContentResourcePolicy(&resource); err == nil {
		t.Fatal("out-of-bounds resource policy revision accepted")
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	raw, _ := contentTestBundle(t, priv, "active", contentTestActor)
	bundle, err := decodeStrictBundle(raw)
	if err != nil {
		t.Fatal(err)
	}
	bundle.DestinationProfiles[0].ResourceID = stringPtr("resource-2")
	canonical, err := CanonicalJSON(destinationProfileContent(bundle.DestinationProfiles[0]))
	if err != nil {
		t.Fatal(err)
	}
	hash := digestHex(HashBytes(canonical))
	bundle.DestinationProfiles[0].ContentHash = &hash
	encoded, _ := json.Marshal(bundle)
	signed, err := CanonicalJSONBytes(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ValidateBundle(signed, ed25519.Sign(priv, signed), testTenant, pub); err == nil {
		t.Fatal("profile substituted a resource outside its signed resource policy")
	}
}
