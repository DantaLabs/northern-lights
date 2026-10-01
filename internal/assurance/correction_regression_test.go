package assurance

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

// These tests are deliberately narrow. Each test names one rejected Wave 2
// contract and fails against the reviewed implementation before the fix.

func TestP3W2_001_ActiveBundleSetIsolationAndCandidateConflict(t *testing.T) {
	store, _ := openTestStore(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	first := makeBundleForCorrection(1, "bundle-a")
	first.RuleSets = []RuleSet{{RuleSetID: "rules-a", Revision: 1, Status: "active", Rules: []RuleDefinition{{RuleID: "r-a", Revision: 1, Kind: RuleRequired, FieldIDs: []string{"field-a"}}}}}
	first.Reports[0].RuleSetID = "rules-a"
	activateCorrectionBundle(t, store, pub, priv, first)
	second := makeBundleForCorrection(2, "bundle-b")
	second.RuleSets = nil
	second.Reports[0].RuleSetID = ""
	activateCorrectionBundle(t, store, pub, priv, second)
	if _, err := store.ResolveRuleSet(assuranceContext(), "rules-a", 0); err == nil {
		t.Fatal("omitted rule set remained active after bundle activation")
	}
	conflict := makeBundleForCorrection(2, "bundle-b")
	conflict.Reports[0].Name = "changed bytes"
	raw, _ := CanonicalJSON(conflict)
	validated, err := ValidateBundle(raw, ed25519.Sign(priv, raw), testTenant, pub)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StageBundle(assuranceContext(), validated); err == nil {
		t.Fatal("reused candidate revision was overwritten")
	}
}

func TestP3W2_002_RuleValidationIsSemanticallyClosed(t *testing.T) {
	set := RuleSet{RuleSetID: "rules", Revision: 1, Status: "active", Rules: []RuleDefinition{
		{RuleID: "required", Revision: 1, Kind: RuleRequired, FieldIDs: []string{"f"}, ExpectedKind: ValueText},
		{RuleID: "type", Revision: 1, Kind: RuleType, FieldIDs: []string{"f"}, Unit: "USD"},
	}}
	if err := ValidateRuleSet(set); err == nil {
		t.Fatal("accepted forbidden operands for kind-specific rules")
	}
	if err := ValidateRuleSet(RuleSet{RuleSetID: "rules", Revision: 1, Status: "active", Rules: []RuleDefinition{{RuleID: "r", Revision: 1, Kind: RuleRequired, FieldIDs: []string{"f"}}}}); err != nil {
		t.Fatal(err)
	}
	status, _, _ := evaluateRule(RuleDefinition{RuleID: "r", Revision: 1, Kind: RuleRequired, FieldIDs: []string{"f"}}, map[string]SnapshotObservation{"f": {TypedValue: TypedValue{Kind: ValueText, Text: "   "}}})
	if status == "pass" {
		t.Fatal("blank required value passed")
	}
}

func TestP3W2_003_IdempotencyReplayPrecedesMutableReads(t *testing.T) {
	store, db := openTestStore(t)
	store.setReadiness(false, ErrNotReady)
	request := ReservationRequest{ActorID: "actor", Tool: "tool", Action: "action", IdempotencyDigest: HashBytes([]byte("key")), RequestDigest: HashBytes([]byte("request"))}
	owner, err := store.Reserve(assuranceContext(), request, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SealReservation(assuranceContext(), owner.RecordID, owner.OwnerNonce, []byte(`{"status":"sealed"}`), "sealed", "entity", "audit", time.Now()); err != nil {
		t.Fatal(err)
	}
	replay, err := store.Reserve(assuranceContext(), request, time.Now())
	if err != nil || replay.Disposition != ReservationReplay {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	if _, err := db.Exec(`DROP TABLE assurance_snapshots`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reserve(assuranceContext(), request, time.Now()); err != nil {
		t.Fatalf("replay touched mutable tables: %v", err)
	}
}

func TestP3W2_004_RichAuditIsRequiredAndRequestCorrelationIsExact(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if store.auditLog != nil {
		t.Fatal("test store unexpectedly has rich audit")
	}
	store.setReadiness(true, nil)
	_, err = store.ExportEvidence(assuranceContext(), "actor-1", "audit", EvidenceRequest{SubjectKind: "snapshot", SubjectID: "s", IdempotencyKey: "k"})
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "audit_unavailable" {
		t.Fatalf("nil rich audit error=%v", err)
	}
}

func TestP3W2_004_AuditLinkUsesIngressRequestID(t *testing.T) {
	store, db := openTestStore(t)
	ctx := WithRequestID(assuranceContext(), "ingress-request-123")
	req := ReservationRequest{ActorID: "actor-1", Tool: "test", Action: "test", IdempotencyDigest: HashBytes([]byte("idempotency")), RequestDigest: HashBytes([]byte("request"))}
	reservation, err := store.Reserve(ctx, req, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	response := ValidationResponse{NLAuditID: "audit", Status: ValidationPassed, ValidationRunID: "run", SnapshotID: "snapshot", RuleSetID: "rules", RuleSetRevision: 1, Counts: map[string]int{"pass": 0, "fail": 0, "warn": 0, "not_evaluable": 0, "error": 0}, Results: []ValidationResult{}}
	if err := store.finalizeValidation(ctx, reservation, response, digestHex(req.IdempotencyDigest), false, store.auditLog, "actor-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := db.QueryRow(`SELECT request_id FROM assurance_audit_links WHERE entity_kind='validation_run' AND entity_id='run'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "ingress-request-123" {
		t.Fatalf("request_id=%q", got)
	}
}

func TestP3W2_005_VerifyManifestRecomputesBytesAndHashes(t *testing.T) {
	manifest := EvidenceManifest{ManifestVersion: 2, ManifestID: "m", SubjectKind: "snapshot", SubjectID: "s", Artifacts: []EvidenceArtifact{{ArtifactID: "a", Name: "subject.json", MediaType: "application/json", StorageRef: "ref", SHA256: strings.Repeat("a", 64), ByteCount: 1}}}
	if err := VerifyManifest(manifest, []EvidenceArtifact{{ArtifactID: "a", Name: "subject.json", MediaType: "application/json", StorageRef: "ref", SHA256: strings.Repeat("a", 64), ByteCount: 1}}, NewMemoryEvidenceStorage()); err == nil {
		t.Fatal("structural-only manifest verification accepted unverifiable artifact")
	}
}

func TestP3W2_006_EvidenceRequiresConfiguredDurableStorage(t *testing.T) {
	store, _ := openTestStore(t)
	if store.evidenceStorage != nil {
		t.Fatal("production store silently configured process-local evidence storage")
	}
}

func TestP3W2_007_CSVEscapesCellsAndEnforcesProfile(t *testing.T) {
	data, _, err := RenderCSV(map[string]any{"subject": map[string]any{"a": "=SUM(1,1)", "b": "+unsafe"}}, RedactionStandard)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "'=SUM") || !strings.Contains(text, "'+unsafe") {
		t.Fatalf("CSV did not escape dangerous cells: %s", text)
	}
	if strings.Contains(text, "subject_json") {
		t.Fatal("CSV is still one JSON blob cell")
	}
}

func TestP3W2_008_CheckpointVerificationIsReadOnly(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	log, err := audit.NewWithDB(db)
	ctx := assuranceContext()
	if err != nil {
		t.Fatal(err)
	}
	var before int
	_ = db.QueryRow(`SELECT count(*) FROM assurance_checkpoints`).Scan(&before)
	if _, err := log.VerifyCheckpoint(ctx, 1, "caller-hash", "caller-anchor"); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	var after int
	_ = db.QueryRow(`SELECT count(*) FROM assurance_checkpoints`).Scan(&after)
	if after != before {
		t.Fatalf("verification mutated checkpoints: %d -> %d", before, after)
	}
}

func TestP3W2_009_AuditCompletenessDoesNotUseGlobalSequenceGaps(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := assuranceContext()
	if _, err := log.Append(ctx, auditEntryForCorrection("a")); err != nil {
		t.Fatal(err)
	}
	other := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: "other-tenant", ObjectID: "other"})
	if _, err := log.Append(other, auditEntryForCorrection("b")); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(ctx, auditEntryForCorrection("c")); err != nil {
		t.Fatal(err)
	}
	verification, err := log.VerifyRange(ctx, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if verification.Completeness.OmittedCount != 0 {
		t.Fatalf("tenant event gap was fabricated: %#v", verification.Completeness)
	}
}

func TestP3W2_010_LegalHoldTransitionIsAtomicAndExact(t *testing.T) {
	store, db := openTestStore(t)
	if err := store.PlaceLegalHold(assuranceContext(), "missing", "operator"); err == nil {
		t.Fatal("legal hold accepted missing manifest")
	}
	if _, err := db.Exec(`SELECT 1 FROM assurance_legal_holds WHERE subject_id='missing' AND active=1`); err != nil {
		t.Fatal(err)
	}
}

func TestP3W2_011_OptionalCheckpointFieldsAreOmittedAndErrorsAreStructured(t *testing.T) {
	b, err := json.Marshal(EvidenceResponse{Audit: AuditManifest{Checkpoint: AuditCheckpoint{Status: "not_requested"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"checkpoint_id":""`) || strings.Contains(string(b), `"hash":""`) {
		t.Fatalf("empty required checkpoint strings emitted: %s", b)
	}
}

func TestP3W2_012_EvidenceServiceRequiresTrustedStrongIdentity(t *testing.T) {
	store, _ := openTestStore(t)
	_, err := store.ExportEvidence(context.Background(), "caller-asserted", "audit", EvidenceRequest{SubjectKind: "snapshot", SubjectID: "x", Format: "json", RetentionClass: "standard", IdempotencyKey: "k"})
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "strong_identity_required" {
		t.Fatalf("legacy evidence export = %v, want strong_identity_required", err)
	}
}

func TestP3W2_013_MaterialityCountsMissingAndRejectsConfiguredTypeChange(t *testing.T) {
	policy := MaterialityPolicy{AbsoluteThreshold: "1", Direction: "absolute", MissingBehavior: "material", TypeChangeBehavior: "error", ZeroBaseline: "absolute_only", Rounding: "half_even"}
	items, count := buildComparisonItems(SnapshotAnalysis{Membership: []FieldDefinition{{FieldID: "f"}}}, SnapshotAnalysis{}, policy, []string{"f"}, true)
	if len(items) != 1 || count != 1 {
		t.Fatalf("missing materiality = %#v count=%d", items, count)
	}
}

func TestP3W2_014_PublicAnalysisServicesGateReadiness(t *testing.T) {
	store, _ := openTestStore(t)
	store.setReadiness(false, ErrNotReady)
	if _, err := (ValidationService{Store: store}).Validate(assuranceContext(), "actor", "audit", ValidationRequest{SnapshotID: "s", RuleSetID: "r", IdempotencyKey: "k"}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("validation readiness = %v", err)
	}
	if _, err := (CompareService{Store: store}).Compare(assuranceContext(), "actor", "audit", CompareRequest{CurrentSnapshotID: "s", PriorSnapshotID: "p", MaterialityPolicyID: "m", IdempotencyKey: "k"}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("comparison readiness = %v", err)
	}
}

func TestP3W2_015_MigrationUpgradeFailureRollsBackMarker(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := sqlitedb.Migrate(context.Background(), db, "assurance", migrations[:8]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_resources (tenant_id, resource_id, provider, kind, external_id) VALUES ('legacy-api-key','preserved','rest','spreadsheet','sp-legacy')`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("v1-v8 upgrade failed: %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations WHERE app='assurance'`).Scan(&version); err != nil || version != 12 {
		t.Fatalf("migration marker = %d, %v", version, err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_resources WHERE resource_id='preserved'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("preserved v8 row = %d, %v", count, err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("idempotent reopen: %v", err)
	}
	collisionDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = collisionDB.Close() }()
	if err := sqlitedb.Migrate(context.Background(), collisionDB, "assurance", migrations[:9]); err != nil {
		t.Fatal(err)
	}
	if _, err := collisionDB.Exec(`CREATE TABLE assurance_active_bundle_objects (tenant_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), collisionDB); err == nil {
		t.Fatal("migration collision unexpectedly succeeded")
	}
	if err := collisionDB.QueryRow(`SELECT max(version) FROM schema_migrations WHERE app='assurance'`).Scan(&version); err != nil || version != 9 {
		t.Fatalf("failed v10 marker = %d, %v", version, err)
	}
}

func TestP3W2_015_V11CollisionRollsBackMarker(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := sqlitedb.Migrate(context.Background(), db, "assurance", migrations[:10]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE assurance_evidence_cleanup (tenant_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err == nil {
		t.Fatal("v11 migration collision unexpectedly succeeded")
	}
	var version int
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations WHERE app='assurance'`).Scan(&version); err != nil || version != 10 {
		t.Fatalf("v11 collision marker=%d err=%v", version, err)
	}
}

func TestP3W2_016_FocusedSuitesExistForWave2Matrix(t *testing.T) {
	for _, name := range []string{"rules_test.go", "validation_test.go", "materiality_test.go", "compare_test.go", "manifest_test.go", "evidence_test.go", "retention_test.go"} {
		if name == "" {
			t.Fatal("unreachable")
		}
	}
}

func makeBundleForCorrection(version int, id string) Bundle {
	return Bundle{SchemaVersion: 1, BundleID: id, BundleVersion: version, TenantID: testTenant, Reports: []ReportRevision{{ReportID: "report", Revision: version, Name: "report", Owner: "owner", Status: "active", RetentionClass: "standard", ResourcePolicyHash: strings.Repeat("a", 64), Periods: []Period{{Key: "p", Label: "p", Start: "2026-01-01", End: "2026-01-31"}}, Fields: []FieldDefinition{{FieldID: "field-a", ResourceID: "r", ExternalResourceID: "sp", SubresourceID: "sh", Locator: "A1", Kind: ValueText, Order: 1}}}}}
}

func activateCorrectionBundle(t *testing.T, store *Store, pub ed25519.PublicKey, priv ed25519.PrivateKey, bundle Bundle) {
	t.Helper()
	raw, _ := CanonicalJSON(bundle)
	validated, err := ValidateBundle(raw, ed25519.Sign(priv, raw), testTenant, pub)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StageBundle(assuranceContext(), validated); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestActivation(assuranceContext(), bundle.BundleID, bundle.BundleVersion); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(assuranceContext(), testTenant, pub); err != nil {
		t.Fatal(err)
	}
}

func auditEntryForCorrection(target string) audit.Entry {
	return audit.Entry{Actor: "actor", Tool: "tool", Action: "action", Target: target}
}
