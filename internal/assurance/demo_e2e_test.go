package assurance

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestWave2DemoEvidenceLifecycleEndToEnd(t *testing.T) {
	store, db := openTestStore(t)
	field := FieldDefinition{FieldID: "amount", ResourceID: "resource", ExternalResourceID: "sp", SubresourceID: "sh", Locator: "B3", Kind: ValueNumber, Unit: "kWh", Required: true, Order: 1}
	snapshotBundle(t, store, []FieldDefinition{field})
	retention := RetentionPolicy{RetentionClass: "long_term", Revision: 1, DurationSeconds: 86400, Status: "active"}
	retentionRaw, err := CanonicalJSON(retention)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_retention_policy_revisions (tenant_id, retention_class, revision, duration_seconds, policy_json, content_hash) VALUES (?, ?, ?, ?, ?, ?)`, testTenant, retention.RetentionClass, retention.Revision, retention.DurationSeconds, string(retentionRaw), digestHex(HashBytes(retentionRaw))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_active_bundle_objects (tenant_id, object_kind, object_id, object_revision, bundle_version) VALUES (?, 'retention_policy', ?, ?, 1)`, testTenant, retention.RetentionClass, retention.Revision); err != nil {
		t.Fatal(err)
	}

	reader := &scriptedReader{results: map[string]ProviderRead{"B3": {Value: ProviderValue{Value: json.Number("10")}, CacheBypassed: true}}, errors: map[string]error{}}
	service := SnapshotService{Store: store, Reader: reader, Now: func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }}
	prior, err := service.Capture(assuranceContext(), "actor-1", "demo-prior", SnapshotRequest{ReportID: "energy-report", Period: Period{Key: "2026-Q3"}, IdempotencyKey: "demo-prior-key"})
	if err != nil {
		t.Fatal(err)
	}
	reader.results["B3"] = ProviderRead{Value: ProviderValue{Value: json.Number("12")}, CacheBypassed: true}
	current, err := service.Capture(assuranceContext(), "actor-1", "demo-current", SnapshotRequest{ReportID: "energy-report", Period: Period{Key: "2026-Q3"}, IdempotencyKey: "demo-current-key"})
	if err != nil {
		t.Fatal(err)
	}
	if prior.Status != SnapshotCompleted || current.Status != SnapshotCompleted {
		t.Fatalf("snapshot statuses prior=%q current=%q", prior.Status, current.Status)
	}

	ruleSet := RuleSet{RuleSetID: "demo-rules", Revision: 1, Status: "active", Rules: []RuleDefinition{{RuleID: "demo-required", Revision: 1, Order: 1, Kind: RuleRequired, FieldIDs: []string{"amount"}}}}
	ruleRaw, err := CanonicalJSON(ruleSet)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_rule_sets (tenant_id, rule_set_id, revision, content_hash, status, definition_json) VALUES (?, ?, ?, ?, ?, ?)`, testTenant, ruleSet.RuleSetID, ruleSet.Revision, digestHex(HashBytes(ruleRaw)), ruleSet.Status, string(ruleRaw)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_active_bundle_objects (tenant_id, object_kind, object_id, object_revision, bundle_version) VALUES (?, 'rule_set', ?, ?, 1)`, testTenant, ruleSet.RuleSetID, ruleSet.Revision); err != nil {
		t.Fatal(err)
	}
	for _, rule := range ruleSet.Rules {
		raw, _ := CanonicalJSON(rule)
		if _, err := db.Exec(`INSERT INTO assurance_rule_set_rules (tenant_id, rule_set_id, revision, rule_id, rule_order, rule_json) VALUES (?, ?, ?, ?, ?, ?)`, testTenant, ruleSet.RuleSetID, ruleSet.Revision, rule.RuleID, rule.Order, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE assurance_report_revisions SET rule_set_id=? WHERE tenant_id=? AND report_id='energy-report' AND revision=1`, ruleSet.RuleSetID, testTenant); err != nil {
		t.Fatal(err)
	}
	validation, err := (ValidationService{Store: store}).Validate(assuranceContext(), "actor-1", "demo-validation", ValidationRequest{SnapshotID: current.SnapshotID, RuleSetID: ruleSet.RuleSetID, IdempotencyKey: "demo-validation-key"})
	if err != nil || validation.Status != ValidationPassed {
		t.Fatalf("validation=%#v err=%v", validation, err)
	}
	comparison, err := (CompareService{Store: store}).Compare(assuranceContext(), "actor-1", "demo-comparison", CompareRequest{CurrentSnapshotID: current.SnapshotID, PriorSnapshotID: prior.SnapshotID, MaterialityPolicyID: "mat-default", IdempotencyKey: "demo-comparison-key"})
	if err != nil || comparison.Status != ComparisonCompleted || len(comparison.Changes) != 1 {
		t.Fatalf("comparison=%#v err=%v", comparison, err)
	}

	storage := NewMemoryEvidenceStorage()
	store.SetEvidenceStorage(storage)
	export, err := store.ExportEvidence(assuranceContext(), "actor-1", "demo-export", EvidenceRequest{SubjectKind: "comparison", SubjectID: comparison.ComparisonID, Format: "csv", RedactionProfile: RedactionStandard, RetentionClass: "long_term", IdempotencyKey: "demo-export-key"}, storage)
	if err != nil {
		t.Fatal(err)
	}
	if export.Status != EvidenceCompleted || len(export.Artifacts) != 3 || export.Manifest.CSV == nil {
		t.Fatalf("export=%#v", export)
	}
	if err := VerifyManifest(export.Manifest, export.Artifacts, storage); err != nil {
		t.Fatalf("manifest verification failed: %v", err)
	}
	for _, artifact := range export.Artifacts {
		if err := VerifyStorageHash(context.Background(), storage, artifact.StorageRef, mustDigestHex(artifact.SHA256)); err != nil {
			t.Fatalf("artifact %s read-back failed: %v", artifact.Name, err)
		}
	}
}
