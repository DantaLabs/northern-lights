package assurance

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

func actorIdempotencyContext(actor string) context.Context {
	return identity.ContextWithPrincipal(context.Background(), identity.Principal{
		TenantID: testTenant, ObjectID: actor, Permissions: []identity.Permission{identity.PermissionEvidenceExport},
	})
}

func TestV13IdempotencyDigestUniquenessIsActorScopedOnly(t *testing.T) {
	_, db := openTestStore(t)
	rows, err := db.Query(`SELECT name, sql FROM sqlite_master WHERE type='table' AND name LIKE 'assurance_%' AND sql IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var idempotencyRecordSQL string
	for rows.Next() {
		var name, schema string
		if err := rows.Scan(&name, &schema); err != nil {
			t.Fatal(err)
		}
		normalized := strings.Join(strings.Fields(strings.ToLower(schema)), " ")
		if name == "assurance_idempotency_records" {
			idempotencyRecordSQL = normalized
			continue
		}
		if strings.Contains(normalized, "unique (tenant_id, idempotency_digest)") {
			t.Fatalf("%s still enforces tenant-global idempotency uniqueness", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(idempotencyRecordSQL, "unique (tenant_id, actor_id, tool, action, idempotency_digest)") {
		t.Fatalf("actor-scoped idempotency record uniqueness missing: %s", idempotencyRecordSQL)
	}
}

func TestV13ActorScopedMaterializationsPreserveRowsChildrenAndLegalHold(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := sqlitedb.Migrate(ctx, db, "assurance", migrations[:12]); err != nil {
		t.Fatalf("migrate through v12: %v", err)
	}
	seedV12MaterializedRows(t, db)
	materializedTables := []string{"assurance_validation_runs", "assurance_validation_results", "assurance_comparisons", "assurance_comparison_items", "assurance_evidence_manifests", "assurance_evidence_artifacts", "assurance_evidence_subjects", "assurance_legal_holds"}
	beforeColumns := snapshotTableColumns(t, db, materializedTables)
	beforeRows := snapshotTableRows(t, db, materializedTables)

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate v12 to v13: %v", err)
	}
	assertAssuranceVersion(t, db, 16)
	if got := snapshotTableColumns(t, db, materializedTables); !reflect.DeepEqual(got, beforeColumns) {
		t.Fatalf("materialized columns changed: before=%v after=%v", beforeColumns, got)
	}
	if got := snapshotTableRows(t, db, materializedTables); !reflect.DeepEqual(got, beforeRows) {
		t.Fatalf("materialized rows changed: before=%v after=%v", beforeRows, got)
	}
	assertV13RowsPreserved(t, db)
	assertV13AllowsSameDigestMaterializations(t, db)

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("idempotent v13 reopen: %v", err)
	}
	assertAssuranceVersion(t, db, 16)
}

func TestV13MigrationFailureRollsBackMarkerAndOriginalTables(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := sqlitedb.Migrate(ctx, db, "assurance", migrations[:12]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE assurance_validation_runs_v13 (sentinel TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_validation_runs (tenant_id, validation_run_id, snapshot_id, rule_set_id, rule_set_revision, idempotency_digest, status, created_at, fail_on_warning) VALUES (?, 'run-before-failure', 'snapshot', 'rules', 1, 'digest-before-failure', 'passed', ?, 0)`, testTenant, formatTimestamp(time.Unix(1, 0))); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(ctx, db); err == nil {
		t.Fatal("v13 migration collision unexpectedly succeeded")
	}
	assertAssuranceVersion(t, db, 12)
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_validation_runs WHERE validation_run_id='run-before-failure'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("original validation row count=%d, want 1", count)
	}
}

func TestValidationActorScopedIdempotencyHasIndependentRowsReplaysAndAuditActors(t *testing.T) {
	store, db := openTestStore(t)
	field := FieldDefinition{FieldID: "amount", ResourceID: "r", ExternalResourceID: "sp", SubresourceID: "sh", Locator: "B3", Kind: ValueNumber, Required: true, Order: 1}
	snapshotBundle(t, store, []FieldDefinition{field})
	reader := &scriptedReader{results: map[string]ProviderRead{"B3": {Value: ProviderValue{Value: json.Number("12")}, CacheBypassed: true}}, errors: map[string]error{}}
	snapshot, err := (SnapshotService{Store: store, Reader: reader}).Capture(actorIdempotencyContext("actor-a"), "actor-a", "audit-snapshot", SnapshotRequest{ReportID: "energy-report", Period: Period{Key: "2026-Q3"}, IdempotencyKey: "snapshot-validation"})
	if err != nil {
		t.Fatal(err)
	}
	ruleSetID := provisionValidationRuleSet(t, db)
	request := ValidationRequest{SnapshotID: snapshot.SnapshotID, RuleSetID: ruleSetID, IdempotencyKey: "same-validation-key"}
	firstA, err := (ValidationService{Store: store}).Validate(actorIdempotencyContext("actor-a"), "actor-a", "audit-validation-a", request)
	if err != nil {
		t.Fatalf("actor A validation: %v", err)
	}
	firstB, err := (ValidationService{Store: store}).Validate(actorIdempotencyContext("actor-b"), "actor-b", "audit-validation-b", request)
	if err != nil {
		t.Fatalf("actor B validation: %v", err)
	}
	if firstA.ValidationRunID == "" || firstA.ValidationRunID == firstB.ValidationRunID {
		t.Fatalf("validation entities were not independent: A=%#v B=%#v", firstA, firstB)
	}
	replayA, err := (ValidationService{Store: store}).Validate(actorIdempotencyContext("actor-a"), "actor-a", "audit-validation-a-replay", request)
	if err != nil {
		t.Fatal(err)
	}
	replayB, err := (ValidationService{Store: store}).Validate(actorIdempotencyContext("actor-b"), "actor-b", "audit-validation-b-replay", request)
	if err != nil {
		t.Fatal(err)
	}
	if replayA.Status != ValidationIdempotencyReplay || replayA.ValidationRunID != firstA.ValidationRunID || replayB.Status != ValidationIdempotencyReplay || replayB.ValidationRunID != firstB.ValidationRunID {
		t.Fatalf("validation replays A=%#v B=%#v", replayA, replayB)
	}
	assertMaterializedAuditActors(t, db, "validation_run", firstA.ValidationRunID, "actor-a", firstB.ValidationRunID, "actor-b")
}

func TestComparisonActorScopedIdempotencyHasIndependentRowsReplaysAndAuditActors(t *testing.T) {
	store, db := openTestStore(t)
	field := FieldDefinition{FieldID: "amount", ResourceID: "r", ExternalResourceID: "sp", SubresourceID: "sh", Locator: "B3", Kind: ValueNumber, Required: true, Order: 1}
	snapshotBundle(t, store, []FieldDefinition{field})
	prior, err := (SnapshotService{Store: store, Reader: &scriptedReader{results: map[string]ProviderRead{"B3": {Value: ProviderValue{Value: json.Number("10")}, CacheBypassed: true}}, errors: map[string]error{}}}).Capture(actorIdempotencyContext("actor-a"), "actor-a", "audit-prior", SnapshotRequest{ReportID: "energy-report", Period: Period{Key: "2026-Q3"}, IdempotencyKey: "snapshot-prior"})
	if err != nil {
		t.Fatal(err)
	}
	current, err := (SnapshotService{Store: store, Reader: &scriptedReader{results: map[string]ProviderRead{"B3": {Value: ProviderValue{Value: json.Number("12")}, CacheBypassed: true}}, errors: map[string]error{}}}).Capture(actorIdempotencyContext("actor-a"), "actor-a", "audit-current", SnapshotRequest{ReportID: "energy-report", Period: Period{Key: "2026-Q3"}, IdempotencyKey: "snapshot-current"})
	if err != nil {
		t.Fatal(err)
	}
	request := CompareRequest{CurrentSnapshotID: current.SnapshotID, PriorSnapshotID: prior.SnapshotID, MaterialityPolicyID: "mat-default", IdempotencyKey: "same-comparison-key"}
	firstA, err := (CompareService{Store: store}).Compare(actorIdempotencyContext("actor-a"), "actor-a", "audit-comparison-a", request)
	if err != nil {
		t.Fatalf("actor A comparison: %v", err)
	}
	firstB, err := (CompareService{Store: store}).Compare(actorIdempotencyContext("actor-b"), "actor-b", "audit-comparison-b", request)
	if err != nil {
		t.Fatalf("actor B comparison: %v", err)
	}
	if firstA.ComparisonID == "" || firstA.ComparisonID == firstB.ComparisonID {
		t.Fatalf("comparison entities were not independent: A=%#v B=%#v", firstA, firstB)
	}
	replayA, err := (CompareService{Store: store}).Compare(actorIdempotencyContext("actor-a"), "actor-a", "audit-comparison-a-replay", request)
	if err != nil {
		t.Fatal(err)
	}
	replayB, err := (CompareService{Store: store}).Compare(actorIdempotencyContext("actor-b"), "actor-b", "audit-comparison-b-replay", request)
	if err != nil {
		t.Fatal(err)
	}
	if replayA.Status != ComparisonIdempotencyReplay || replayA.ComparisonID != firstA.ComparisonID || replayB.Status != ComparisonIdempotencyReplay || replayB.ComparisonID != firstB.ComparisonID {
		t.Fatalf("comparison replays A=%#v B=%#v", replayA, replayB)
	}
	assertMaterializedAuditActors(t, db, "comparison", firstA.ComparisonID, "actor-a", firstB.ComparisonID, "actor-b")
}

func TestEvidenceActorScopedIdempotencyHasIndependentManifestsReplaysAndAuditActors(t *testing.T) {
	store, db := openTestStore(t)
	field := FieldDefinition{FieldID: "amount", ResourceID: "r", ExternalResourceID: "sp", SubresourceID: "sh", Locator: "B3", Kind: ValueText, Required: true, Order: 1}
	snapshotBundle(t, store, []FieldDefinition{field})
	provisionLongTermRetention(t, db)
	snapshot, err := (SnapshotService{Store: store, Reader: &scriptedReader{results: map[string]ProviderRead{"B3": {Value: ProviderValue{Value: "authoritative"}, CacheBypassed: true}}, errors: map[string]error{}}}).Capture(actorIdempotencyContext("actor-a"), "actor-a", "audit-evidence-snapshot", SnapshotRequest{ReportID: "energy-report", Period: Period{Key: "2026-Q3"}, IdempotencyKey: "snapshot-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	storage := NewMemoryEvidenceStorage()
	store.SetEvidenceStorage(storage)
	request := EvidenceRequest{SubjectKind: "snapshot", SubjectID: snapshot.SnapshotID, Format: "json", RedactionProfile: RedactionStandard, RetentionClass: "long_term", IdempotencyKey: "same-evidence-key"}
	firstA, err := store.ExportEvidence(actorIdempotencyContext("actor-a"), "actor-a", "audit-evidence-a", request)
	if err != nil {
		t.Fatalf("actor A evidence: %v", err)
	}
	firstB, err := store.ExportEvidence(actorIdempotencyContext("actor-b"), "actor-b", "audit-evidence-b", request)
	if err != nil {
		t.Fatalf("actor B evidence: %v", err)
	}
	if firstA.EvidenceManifestID == "" || firstA.EvidenceManifestID == firstB.EvidenceManifestID {
		t.Fatalf("evidence manifests were not independent: A=%#v B=%#v", firstA, firstB)
	}
	replayA, err := store.ExportEvidence(actorIdempotencyContext("actor-a"), "actor-a", "audit-evidence-a-replay", request)
	if err != nil {
		t.Fatal(err)
	}
	replayB, err := store.ExportEvidence(actorIdempotencyContext("actor-b"), "actor-b", "audit-evidence-b-replay", request)
	if err != nil {
		t.Fatal(err)
	}
	if replayA.Status != EvidenceIdempotencyReplay || replayA.EvidenceManifestID != firstA.EvidenceManifestID || replayB.Status != EvidenceIdempotencyReplay || replayB.EvidenceManifestID != firstB.EvidenceManifestID {
		t.Fatalf("evidence replays A=%#v B=%#v", replayA, replayB)
	}
	assertMaterializedAuditActors(t, db, "evidence_manifest", firstA.EvidenceManifestID, "actor-a", firstB.EvidenceManifestID, "actor-b")
}

func seedV12MaterializedRows(t *testing.T, db *sql.DB) {
	t.Helper()
	now := formatTimestamp(time.Unix(1, 0))
	if _, err := db.Exec(`INSERT INTO assurance_validation_runs (tenant_id, validation_run_id, snapshot_id, rule_set_id, rule_set_revision, idempotency_digest, status, created_at, fail_on_warning) VALUES (?, 'run-1', 'snapshot-1', 'rules-1', 2, 'digest-1', 'passed', ?, 1)`, testTenant, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_validation_results (tenant_id, validation_run_id, result_id, rule_id, status, result_json) VALUES (?, 'run-1', 'result-1', 'rule-1', 'pass', '{"ok":true}')`, testTenant); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_comparisons (tenant_id, comparison_id, current_snapshot_id, prior_snapshot_id, idempotency_digest, status, completeness, comparison_basis, policy_revision, created_at, materiality_policy_id, current_report_id, prior_report_id, current_definition_revision, prior_definition_revision, partial_policy, material_count) VALUES (?, 'comparison-1', 'current-1', 'prior-1', 'digest-2', 'completed', 'complete', 'definition_membership_union', 3, ?, 'policy-1', 'report-current', 'report-prior', 4, 5, 'reject', 6)`, testTenant, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_comparison_items (tenant_id, comparison_id, comparison_item_id, field_id, item_json) VALUES (?, 'comparison-1', 'item-1', 'field-1', '{"delta":"1"}')`, testTenant); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_evidence_manifests (tenant_id, manifest_id, manifest_version, subject_kind, subject_id, manifest_hash, manifest_json, audit_integrity, audit_completeness, expiry_at, legal_hold, idempotency_digest) VALUES (?, 'manifest-1', 2, 'comparison', 'comparison-1', 'manifest-hash', '{"manifest":true}', '{"chain_verified":true}', 'complete', ?, 1, 'digest-3')`, testTenant, formatTimestamp(time.Unix(2, 0))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_evidence_artifacts (tenant_id, artifact_id, manifest_id, artifact_hash, size_bytes, media_type, storage_reference, omissions_json, redaction_json) VALUES (?, 'artifact-1', 'manifest-1', 'artifact-hash', 7, 'application/json', 'opaque-1', '["none"]', '{"profile":"standard"}')`, testTenant); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_evidence_subjects (tenant_id, manifest_id, subject_kind, subject_id) VALUES (?, 'manifest-1', 'comparison', 'comparison-1')`, testTenant); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_legal_holds (tenant_id, hold_id, subject_kind, subject_id, reason, active, actor_id, created_at, released_at, manifest_id) VALUES (?, 'hold-1', 'comparison', 'comparison-1', 'retain', 1, 'actor-a', ?, '', 'manifest-1')`, testTenant, now); err != nil {
		t.Fatal(err)
	}
}

func snapshotTableColumns(t *testing.T, db *sql.DB, tables []string) map[string][]string {
	t.Helper()
	result := make(map[string][]string, len(tables))
	for _, table := range tables {
		columns, err := tableColumns(db, table)
		if err != nil {
			t.Fatal(err)
		}
		result[table] = columns
	}
	return result
}

func snapshotTableRows(t *testing.T, db *sql.DB, tables []string) map[string][]string {
	t.Helper()
	result := make(map[string][]string, len(tables))
	for _, table := range tables {
		rows, err := db.Query(`SELECT * FROM ` + table)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for index := range values {
				pointers[index] = &values[index]
			}
			if err := rows.Scan(pointers...); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			result[table] = append(result[table], fmt.Sprintf("%#v", values))
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		sort.Strings(result[table])
	}
	return result
}

func assertAssuranceVersion(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations WHERE app='assurance'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("assurance schema version=%d, want %d", got, want)
	}
}

func assertV13RowsPreserved(t *testing.T, db *sql.DB) {
	t.Helper()
	var validationDigest string
	var failOnWarning int
	if err := db.QueryRow(`SELECT idempotency_digest, fail_on_warning FROM assurance_validation_runs WHERE validation_run_id='run-1'`).Scan(&validationDigest, &failOnWarning); err != nil || validationDigest != "digest-1" || failOnWarning != 1 {
		t.Fatalf("validation row digest=%q fail_on_warning=%d err=%v", validationDigest, failOnWarning, err)
	}
	var comparisonDigest, policyID string
	var materialCount int
	if err := db.QueryRow(`SELECT idempotency_digest, materiality_policy_id, material_count FROM assurance_comparisons WHERE comparison_id='comparison-1'`).Scan(&comparisonDigest, &policyID, &materialCount); err != nil || comparisonDigest != "digest-2" || policyID != "policy-1" || materialCount != 6 {
		t.Fatalf("comparison row digest=%q policy=%q material_count=%d err=%v", comparisonDigest, policyID, materialCount, err)
	}
	var manifestHash, integrity, completeness string
	var legalHold int
	if err := db.QueryRow(`SELECT manifest_hash, audit_integrity, audit_completeness, legal_hold FROM assurance_evidence_manifests WHERE manifest_id='manifest-1'`).Scan(&manifestHash, &integrity, &completeness, &legalHold); err != nil || manifestHash != "manifest-hash" || integrity != `{"chain_verified":true}` || completeness != "complete" || legalHold != 1 {
		t.Fatalf("manifest row hash=%q integrity=%q completeness=%q legal_hold=%d err=%v", manifestHash, integrity, completeness, legalHold, err)
	}
	for table, want := range map[string]int{"assurance_validation_results": 1, "assurance_comparison_items": 1, "assurance_evidence_artifacts": 1, "assurance_evidence_subjects": 1, "assurance_legal_holds": 1} {
		var got int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s rows=%d want=%d err=%v", table, got, want, err)
		}
	}
}

func assertV13AllowsSameDigestMaterializations(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO assurance_validation_runs (tenant_id, validation_run_id, snapshot_id, rule_set_id, rule_set_revision, idempotency_digest, status, created_at, fail_on_warning) VALUES (?, 'run-2', 'snapshot-2', 'rules-1', 2, 'digest-1', 'passed', ?, 0)`, testTenant, formatTimestamp(time.Unix(3, 0))); err != nil {
		t.Fatalf("same validation digest still collides: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_comparisons (tenant_id, comparison_id, current_snapshot_id, prior_snapshot_id, idempotency_digest, status, completeness, comparison_basis, policy_revision, created_at, materiality_policy_id, current_report_id, prior_report_id, current_definition_revision, prior_definition_revision, partial_policy, material_count) VALUES (?, 'comparison-2', 'current-2', 'prior-2', 'digest-2', 'completed', 'complete', 'definition_membership_union', 3, ?, 'policy-1', 'report-current', 'report-prior', 4, 5, 'reject', 0)`, testTenant, formatTimestamp(time.Unix(4, 0))); err != nil {
		t.Fatalf("same comparison digest still collides: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_evidence_manifests (tenant_id, manifest_id, manifest_version, subject_kind, subject_id, manifest_hash, manifest_json, audit_integrity, audit_completeness, expiry_at, legal_hold, idempotency_digest) VALUES (?, 'manifest-2', 2, 'comparison', 'comparison-2', 'hash-2', '{}', '{}', 'unknown', '', 0, 'digest-3')`, testTenant); err != nil {
		t.Fatalf("same evidence digest still collides: %v", err)
	}
}

func provisionValidationRuleSet(t *testing.T, db *sql.DB) string {
	t.Helper()
	ruleSet := RuleSet{RuleSetID: "rules-validation-actors", Revision: 1, Status: "active", Rules: []RuleDefinition{{RuleID: "required", Revision: 1, Order: 1, Kind: RuleRequired, FieldIDs: []string{"amount"}}}}
	raw, err := CanonicalJSON(ruleSet)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_rule_sets (tenant_id, rule_set_id, revision, content_hash, status, definition_json) VALUES (?, ?, ?, ?, ?, ?)`, testTenant, ruleSet.RuleSetID, 1, "rules-hash", "active", raw); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_active_bundle_objects (tenant_id, object_kind, object_id, object_revision, bundle_version) VALUES (?, 'rule_set', ?, 1, 1)`, testTenant, ruleSet.RuleSetID); err != nil {
		t.Fatal(err)
	}
	for index, rule := range ruleSet.Rules {
		rawRule, err := CanonicalJSON(rule)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO assurance_rule_set_rules (tenant_id, rule_set_id, revision, rule_id, rule_order, rule_json) VALUES (?, ?, ?, ?, ?, ?)`, testTenant, ruleSet.RuleSetID, 1, rule.RuleID, index+1, rawRule); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE assurance_report_revisions SET rule_set_id=? WHERE tenant_id=? AND report_id='energy-report' AND revision=1`, ruleSet.RuleSetID, testTenant); err != nil {
		t.Fatal(err)
	}
	return ruleSet.RuleSetID
}

func provisionLongTermRetention(t *testing.T, db *sql.DB) {
	t.Helper()
	policy := RetentionPolicy{TenantID: testTenant, Revision: 1, RetentionClass: "long_term", DurationSeconds: 3600, Status: "active"}
	raw, err := CanonicalJSON(policy)
	if err != nil {
		t.Fatal(err)
	}
	hash := digestHex(HashBytes(raw))
	if _, err := db.Exec(`INSERT INTO assurance_retention_policies (tenant_id, retention_class, duration_seconds, policy_json, content_hash) VALUES (?, ?, ?, ?, ?)`, testTenant, policy.RetentionClass, policy.DurationSeconds, raw, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_retention_policy_revisions (tenant_id, retention_class, revision, duration_seconds, policy_json, content_hash) VALUES (?, ?, 1, ?, ?, ?)`, testTenant, policy.RetentionClass, policy.DurationSeconds, raw, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_active_bundle_objects (tenant_id, object_kind, object_id, object_revision, bundle_version) VALUES (?, 'retention_policy', ?, 1, 1)`, testTenant, policy.RetentionClass); err != nil {
		t.Fatal(err)
	}
}

func assertMaterializedAuditActors(t *testing.T, db *sql.DB, entityKind, firstID, firstActor, secondID, secondActor string) {
	t.Helper()
	for _, want := range []struct {
		id    string
		actor string
	}{
		{firstID, firstActor}, {secondID, secondActor},
	} {
		var got string
		if err := db.QueryRow(`SELECT actor FROM audit_log WHERE target=? ORDER BY seq DESC LIMIT 1`, want.id).Scan(&got); err != nil {
			t.Fatalf("audit actor for %s/%s: %v", entityKind, want.id, err)
		}
		if got != want.actor {
			t.Fatalf("audit actor for %s/%s=%q, want %q", entityKind, want.id, got, want.actor)
		}
	}
}
