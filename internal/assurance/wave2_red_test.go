package assurance

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
)

func TestWave2RuleSetRejectsUnknownKindsAndDuplicateOrder(t *testing.T) {
	ruleSet := RuleSet{
		RuleSetID: "rules-energy", Revision: 1, Status: "active",
		Rules: []RuleDefinition{
			{RuleID: "r-1", Revision: 1, Kind: RuleRequired, FieldIDs: []string{"field-1"}},
			{RuleID: "r-1", Revision: 1, Kind: RuleType, FieldIDs: []string{"field-1"}},
		},
	}
	if err := ValidateRuleSet(ruleSet); err == nil {
		t.Fatal("ValidateRuleSet accepted duplicate rule IDs")
	}
	ruleSet.Rules[1].RuleID = "r-2"
	ruleSet.Rules[1].Kind = RuleKind("model_expression")
	if err := ValidateRuleSet(ruleSet); err == nil {
		t.Fatal("ValidateRuleSet accepted an open-ended rule kind")
	}
}

func TestWave2DecimalArithmeticDoesNotUseBinaryFloat(t *testing.T) {
	got, err := AddDecimal("9007199254740993.1", "0.9")
	if err != nil {
		t.Fatal(err)
	}
	if got != "9007199254740994" {
		t.Fatalf("AddDecimal = %q, want exact base-10 result", got)
	}
	if cmp, err := CompareDecimal("1.0000000000000000001", "1"); err != nil || cmp != 1 {
		t.Fatalf("CompareDecimal = (%d, %v), want 1", cmp, err)
	}
	if _, ok := any(big.NewFloat(1)).(*big.Float); !ok {
		t.Fatal("test setup")
	}
}

func TestWave2MaterialityPolicyIsServerOwned(t *testing.T) {
	policy := MaterialityPolicy{
		PolicyID: "mat-energy", Revision: 3, Status: "active",
		AbsoluteThreshold: "10.00", RelativeThreshold: "0.05",
		Direction: "absolute_or_relative", ZeroBaseline: "not_comparable",
		MissingBehavior: "not_comparable", TypeChangeBehavior: "not_comparable",
		Rounding: "half_even",
	}
	if err := ValidateMaterialityPolicy(policy); err != nil {
		t.Fatalf("ValidateMaterialityPolicy: %v", err)
	}
	policy.AbsoluteThreshold = "not-a-decimal"
	if err := ValidateMaterialityPolicy(policy); err == nil {
		t.Fatal("ValidateMaterialityPolicy accepted a non-decimal threshold")
	}
}

func TestWave2EvidenceStorageUsesOpaqueReferencesAndVerifiesReadBack(t *testing.T) {
	storage := NewMemoryEvidenceStorage()
	ref, err := storage.Put(context.Background(), "subject.json", "application/json", []byte(`{"authoritative":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if ref == "" || ref[0] == '/' || ref[0] == '.' {
		t.Fatalf("storage reference %q exposes a filesystem path", ref)
	}
	data, err := storage.Read(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"authoritative":true}` {
		t.Fatalf("read-back = %s", data)
	}
	if err := VerifyStorageHash(context.Background(), storage, ref, HashBytes(data)); err != nil {
		t.Fatalf("VerifyStorageHash: %v", err)
	}
}

func TestWave2SignedBundleActivatesRulesMaterialityExportAndRetentionPolicies(t *testing.T) {
	store, _ := openTestStore(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rules := RuleSet{RuleSetID: "rules-energy", Revision: 1, Status: "active", Rules: []RuleDefinition{{RuleID: "required-energy", Revision: 1, Order: 1, Kind: RuleRequired, FieldIDs: []string{"scope2-kwh"}}}}
	policy := MaterialityPolicy{PolicyID: "mat-energy", Revision: 1, Status: "active", AbsoluteThreshold: "1", RelativeThreshold: "0.05", Direction: "absolute_or_relative", ZeroBaseline: "not_comparable", MissingBehavior: "not_comparable", TypeChangeBehavior: "not_comparable", Rounding: "half_even"}
	profile := ExportProfile{ProfileID: "profile-standard", Revision: 1, Status: "active", PermittedSubjects: []string{"snapshot", "validation_run", "comparison"}, RedactionProfile: "standard", RetentionClass: "standard", MaxRows: 100, MaxBytes: 1 << 20, DeliveryPolicy: "opaque_reference"}
	bundle := Bundle{
		SchemaVersion: 1, BundleID: "bundle-wave2", BundleVersion: 1, TenantID: testTenant,
		RuleSets: []RuleSet{rules}, MaterialityPolicies: []MaterialityPolicy{policy}, ExportProfiles: []ExportProfile{profile},
		RetentionPolicies: []RetentionPolicy{{TenantID: testTenant, Revision: 1, RetentionClass: "standard", DurationSeconds: 3600, Status: "active"}},
		Reports:           []ReportRevision{{ReportID: "energy-report", Revision: 1, Name: "Energy", Owner: "owner", Status: "active", RetentionClass: "standard", ResourcePolicyHash: strings.Repeat("a", 64), RuleSetID: rules.RuleSetID, MaterialityPolicyID: policy.PolicyID, ExportProfiles: []string{profile.ProfileID}, Periods: []Period{{Key: "q3", Label: "Q3", Start: "2026-07-01", End: "2026-09-30"}}, Fields: []FieldDefinition{{FieldID: "scope2-kwh", ResourceID: "r", ExternalResourceID: "sp", SubresourceID: "sh", Locator: "B3", Kind: ValueNumber, Unit: "kWh", Required: true, Order: 1}}}},
	}
	if err := ValidateRuleSet(rules); err != nil {
		t.Fatal(err)
	}
	raw, err := CanonicalJSON(bundle)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := ValidateBundle(raw, ed25519.Sign(private, raw), testTenant, public)
	if err != nil {
		t.Fatalf("ValidateBundle: %v", err)
	}
	ctx := assuranceContext()
	if err := store.StageBundle(ctx, validated); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestActivation(ctx, bundle.BundleID, bundle.BundleVersion); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(ctx, testTenant, public); err != nil {
		t.Fatal(err)
	}
	gotRules, err := store.ResolveRuleSet(ctx, rules.RuleSetID, 1)
	if err != nil || gotRules.ContentHash == "" {
		t.Fatalf("ResolveRuleSet = %#v, %v", gotRules, err)
	}
	gotPolicy, err := store.ResolveMaterialityPolicy(ctx, policy.PolicyID, 0)
	if err != nil || gotPolicy.Revision != 1 {
		t.Fatalf("ResolveMaterialityPolicy = %#v, %v", gotPolicy, err)
	}
}

func TestWave2ValidationIsProviderFreeAndNotEvaluableNeverPasses(t *testing.T) {
	store, db := openTestStore(t)
	field := FieldDefinition{FieldID: "amount", ResourceID: "r", ExternalResourceID: "sp", SubresourceID: "sh", Locator: "B3", Kind: ValueNumber, Unit: "USD", Required: true, Order: 1}
	snapshotBundle(t, store, []FieldDefinition{field})
	reader := &scriptedReader{results: map[string]ProviderRead{"B3": {Value: ProviderValue{Value: json.Number("12.5")}, CacheBypassed: true}}, errors: map[string]error{}}
	snapshot, err := (SnapshotService{Store: store, Reader: reader}).Capture(assuranceContext(), "actor-1", "audit-snapshot", SnapshotRequest{ReportID: "energy-report", Period: Period{Key: "2026-Q3"}, IdempotencyKey: "snapshot-for-validation"})
	if err != nil {
		t.Fatal(err)
	}
	ruleSet := RuleSet{RuleSetID: "rules-validation", Revision: 1, Status: "active", Rules: []RuleDefinition{
		{RuleID: "required", Revision: 1, Order: 1, Kind: RuleRequired, FieldIDs: []string{"amount"}},
		{RuleID: "range", Revision: 1, Order: 2, Kind: RuleNumericRange, FieldIDs: []string{"amount"}, Lower: "0", Upper: "10"},
	}}
	definition, _ := CanonicalJSON(ruleSet)
	if _, err := db.Exec(`INSERT INTO assurance_rule_sets (tenant_id, rule_set_id, revision, content_hash, status, definition_json) VALUES (?, ?, ?, ?, ?, ?)`, testTenant, ruleSet.RuleSetID, 1, "rules-hash", "active", definition); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_active_bundle_objects (tenant_id, object_kind, object_id, object_revision, bundle_version) VALUES (?, 'rule_set', ?, 1, 1)`, testTenant, ruleSet.RuleSetID); err != nil {
		t.Fatal(err)
	}
	for index, rule := range ruleSet.Rules {
		raw, _ := CanonicalJSON(rule)
		if _, err := db.Exec(`INSERT INTO assurance_rule_set_rules (tenant_id, rule_set_id, revision, rule_id, rule_order, rule_json) VALUES (?, ?, ?, ?, ?, ?)`, testTenant, ruleSet.RuleSetID, 1, rule.RuleID, index+1, raw); err != nil {
			t.Fatal(err)
		}
	}
	response, err := (ValidationService{Store: store}).Validate(assuranceContext(), "actor-1", "audit-validation", ValidationRequest{SnapshotID: snapshot.SnapshotID, RuleSetID: ruleSet.RuleSetID, IdempotencyKey: "validate-1"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != ValidationFailed || response.Counts["pass"] != 1 || response.Counts["fail"] != 1 {
		t.Fatalf("validation = %#v", response)
	}
	if reader.callCount() != 1 {
		t.Fatalf("validation touched provider: %d calls", reader.callCount())
	}
	partial, err := (SnapshotService{Store: store, Reader: &scriptedReader{results: map[string]ProviderRead{}, errors: map[string]error{"B3": errors.New("permission denied")}}}).Capture(assuranceContext(), "actor-1", "audit-partial", SnapshotRequest{ReportID: "energy-report", Period: Period{Key: "2026-Q3"}, AllowPartial: true, IdempotencyKey: "snapshot-partial-for-validation"})
	if err != nil {
		t.Fatal(err)
	}
	incomplete, err := (ValidationService{Store: store}).Validate(assuranceContext(), "actor-1", "audit-validation-partial", ValidationRequest{SnapshotID: partial.SnapshotID, RuleSetID: ruleSet.RuleSetID, IdempotencyKey: "validate-partial"})
	if err != nil {
		t.Fatal(err)
	}
	if incomplete.Counts["not_evaluable"] == 0 || incomplete.Counts["pass"] != 0 {
		t.Fatalf("partial validation = %#v", incomplete)
	}
}

func TestWave2ComparisonUsesStableFieldsExactDeltasAndPolicyOwnedPartialHandling(t *testing.T) {
	store, db := openTestStore(t)
	fields := []FieldDefinition{
		{FieldID: "amount", ResourceID: "r1", ExternalResourceID: "sp", SubresourceID: "sh", Locator: "B3", Kind: ValueNumber, Unit: "USD", Required: true, Order: 1},
		{FieldID: "optional", ResourceID: "r2", ExternalResourceID: "sp", SubresourceID: "sh", Locator: "B4", Kind: ValueNumber, Unit: "USD", Required: false, Order: 2},
	}
	snapshotBundle(t, store, fields)
	priorReader := &scriptedReader{results: map[string]ProviderRead{"B3": {Value: ProviderValue{Value: json.Number("0")}, CacheBypassed: true}, "B4": {Value: ProviderValue{Value: json.Number("2")}, CacheBypassed: true}}, errors: map[string]error{}}
	prior, err := (SnapshotService{Store: store, Reader: priorReader}).Capture(assuranceContext(), "actor-1", "audit-prior", SnapshotRequest{ReportID: "energy-report", Period: Period{Key: "2026-Q3"}, FieldIDs: []string{"amount", "optional"}, IdempotencyKey: "snapshot-prior"})
	if err != nil {
		t.Fatal(err)
	}
	currentReader := &scriptedReader{results: map[string]ProviderRead{"B3": {Value: ProviderValue{Value: json.Number("10.5")}, CacheBypassed: true}}, errors: map[string]error{"B4": errors.New("source unavailable")}}
	current, err := (SnapshotService{Store: store, Reader: currentReader}).Capture(assuranceContext(), "actor-1", "audit-current", SnapshotRequest{ReportID: "energy-report", Period: Period{Key: "2026-Q3"}, FieldIDs: []string{"amount", "optional"}, AllowPartial: true, IdempotencyKey: "snapshot-current"})
	if err != nil {
		t.Fatal(err)
	}
	policy := MaterialityPolicy{PolicyID: "mat-default", Revision: 1, Status: "active", AbsoluteThreshold: "1", RelativeThreshold: "0.05", Direction: "absolute_or_relative", ZeroBaseline: "not_comparable", MissingBehavior: "not_comparable", TypeChangeBehavior: "not_comparable", Rounding: "half_even"}
	raw, _ := CanonicalJSON(policy)
	if _, err := db.Exec(`UPDATE assurance_materiality_policies SET policy_json=? WHERE tenant_id=? AND policy_id=? AND revision=?`, raw, testTenant, policy.PolicyID, policy.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO assurance_active_bundle_objects (tenant_id, object_kind, object_id, object_revision, bundle_version) VALUES (?, 'materiality_policy', ?, 1, 1)`, testTenant, policy.PolicyID); err != nil {
		t.Fatal(err)
	}
	_, err = (CompareService{Store: store}).Compare(assuranceContext(), "actor-1", "audit-compare-rejected", CompareRequest{CurrentSnapshotID: current.SnapshotID, PriorSnapshotID: prior.SnapshotID, MaterialityPolicyID: policy.PolicyID, IdempotencyKey: "compare-rejected"})
	if err == nil {
		t.Fatal("Compare accepted partial input under a rejecting server policy")
	}
	policy.AllowPartialCompare = true
	raw, _ = CanonicalJSON(policy)
	if _, err := db.Exec(`UPDATE assurance_materiality_policies SET policy_json=? WHERE tenant_id=? AND policy_id=? AND revision=?`, raw, testTenant, policy.PolicyID, policy.Revision); err != nil {
		t.Fatal(err)
	}
	comparison, err := (CompareService{Store: store}).Compare(assuranceContext(), "actor-1", "audit-compare", CompareRequest{CurrentSnapshotID: current.SnapshotID, PriorSnapshotID: prior.SnapshotID, MaterialityPolicyID: policy.PolicyID, IdempotencyKey: "compare-1"})
	if err != nil {
		t.Fatal(err)
	}
	if comparison.Completeness != CompletenessIncomplete || len(comparison.Changes) != 2 {
		t.Fatalf("comparison = %#v", comparison)
	}
	if comparison.Changes[0].PercentageDelta != "" {
		t.Fatalf("zero baseline invented percentage: %#v", comparison.Changes[0])
	}
	if comparison.Changes[0].MaterialityStatus != "not_comparable" {
		t.Fatalf("zero baseline materiality = %q, want not_comparable", comparison.Changes[0].MaterialityStatus)
	}
	if comparison.Changes[0].FieldID != "amount" || comparison.Changes[0].AbsoluteDelta != "10.5" {
		t.Fatalf("comparison delta = %#v", comparison.Changes[0])
	}
}

func TestWave2EvidenceExportIsOneSubjectManifestV2AndTransferIsUnavailable(t *testing.T) {
	store, db := openTestStore(t)
	field := FieldDefinition{FieldID: "amount", ResourceID: "r", ExternalResourceID: "sp", SubresourceID: "sh", Locator: "B3", Kind: ValueText, Required: true, Order: 1}
	snapshotBundle(t, store, []FieldDefinition{field})
	if _, err := db.Exec(`INSERT INTO assurance_retention_policies (tenant_id, retention_class, duration_seconds, policy_json, content_hash) VALUES (?, 'long_term', 86400, '{}', 'retention-hash')`, testTenant); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_retention_policy_revisions (tenant_id, retention_class, revision, duration_seconds, policy_json, content_hash) VALUES (?, 'long_term', 0, 86400, '{}', 'retention-hash')`, testTenant); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_active_bundle_objects (tenant_id, object_kind, object_id, object_revision, bundle_version) VALUES (?, 'retention_policy', 'long_term', 0, 1)`, testTenant); err != nil {
		t.Fatal(err)
	}
	reader := &scriptedReader{results: map[string]ProviderRead{"B3": {Value: ProviderValue{Value: "authoritative"}, CacheBypassed: true}}, errors: map[string]error{}}
	snapshot, err := (SnapshotService{Store: store, Reader: reader}).Capture(assuranceContext(), "actor-1", "audit-snapshot-export", SnapshotRequest{ReportID: "energy-report", Period: Period{Key: "2026-Q3"}, IdempotencyKey: "snapshot-export"})
	if err != nil {
		t.Fatal(err)
	}
	storage := NewMemoryEvidenceStorage()
	store.SetEvidenceStorage(storage)
	export, err := store.ExportEvidence(assuranceContext(), "actor-1", "audit-export", EvidenceRequest{SubjectKind: "snapshot", SubjectID: snapshot.SnapshotID, Format: "csv", RedactionProfile: "standard", IncludeAuditChain: false, RetentionClass: "long_term", IdempotencyKey: "export-1"})
	if err != nil {
		t.Fatal(err)
	}
	if export.Status != EvidenceCompleted || export.Manifest.ManifestVersion != 2 || len(export.Manifest.Artifacts) != 2 {
		t.Fatalf("export = %#v", export)
	}
	for _, artifact := range export.Manifest.Artifacts {
		if artifact.StorageRef == "" || artifact.StorageRef[0] == '/' {
			t.Fatalf("artifact exposed path: %#v", artifact)
		}
		if err := VerifyStorageHash(context.Background(), storage, artifact.StorageRef, mustDigestHex(artifact.SHA256)); err != nil {
			t.Fatalf("artifact %s: %v", artifact.Name, err)
		}
	}
	_, err = store.ExportEvidence(assuranceContext(), "actor-1", "audit-transfer", EvidenceRequest{SubjectKind: "transfer", SubjectID: "missing-transfer", Format: "json", RetentionClass: "long_term", IdempotencyKey: "export-transfer"})
	var typed *Error
	if !errors.As(err, &typed) || (typed.Code != "subject_unavailable" && typed.Code != "subject_not_found") {
		t.Fatalf("transfer export error = %v", err)
	}
}

func mustDigestHex(value string) Digest {
	raw, _ := hex.DecodeString(value)
	var digest Digest
	copy(digest[:], raw)
	return digest
}
