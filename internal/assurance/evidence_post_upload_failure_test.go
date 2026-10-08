package assurance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
)

type postUploadFailureStorage struct {
	puts, deletes int
	deleteErr     error
}

func (s *postUploadFailureStorage) Put(context.Context, string, string, []byte) (string, error) {
	s.puts++
	return "known-opaque-object", errors.New("read-back unavailable after create")
}
func (*postUploadFailureStorage) Read(context.Context, string) ([]byte, error) {
	return nil, errors.New("unavailable")
}
func (s *postUploadFailureStorage) Delete(context.Context, string) error {
	s.deletes++
	return s.deleteErr
}

func TestExportRecordsPostUploadReferenceAndSealedReplayDoesNotRepeatStorage(t *testing.T) {
	store, db := openTestStore(t)
	snapshotBundle(t, store, []FieldDefinition{{FieldID: "amount", ResourceID: "resource", ExternalResourceID: "sp", SubresourceID: "sh", Locator: "B3", Kind: ValueNumber, Unit: "kWh", Required: true, Order: 1}})
	seedLongTermRetentionForEvidenceTest(t, db)
	reader := &scriptedReader{results: map[string]ProviderRead{"B3": {Value: ProviderValue{Value: json.Number("12")}, CacheBypassed: true}}, errors: map[string]error{}}
	snapshot, err := (SnapshotService{Store: store, Reader: reader}).Capture(assuranceContext(), "actor-1", "snapshot-audit", SnapshotRequest{ReportID: "energy-report", Period: Period{Key: "2026-Q3"}, RetentionClass: "long_term", IdempotencyKey: "snapshot-key"})
	if err != nil {
		t.Fatal(err)
	}
	storage := &postUploadFailureStorage{deleteErr: errors.New("exact object version could not be verified")}
	store.SetEvidenceStorage(storage)
	request := EvidenceRequest{SubjectKind: "snapshot", SubjectID: snapshot.SnapshotID, Format: "json", RedactionProfile: RedactionStandard, RetentionClass: "long_term", IdempotencyKey: "post-upload-failure"}
	_, firstErr := store.ExportEvidence(assuranceContext(), "ignored", "export-audit", request)
	if firstErr == nil {
		t.Fatal("post-upload verification failure unexpectedly completed")
	}
	if storage.puts != 1 || storage.deletes != 1 {
		t.Fatalf("initial storage calls puts=%d deletes=%d", storage.puts, storage.deletes)
	}
	var cleanupCount int
	var cleanupState, cleanupRef string
	if err := db.QueryRow(`SELECT count(*), max(state), max(storage_reference) FROM assurance_evidence_cleanup WHERE tenant_id=? AND storage_reference=?`, testTenant, "known-opaque-object").Scan(&cleanupCount, &cleanupState, &cleanupRef); err != nil {
		t.Fatal(err)
	}
	if cleanupCount != 1 || cleanupState != "reconciliation_required" || cleanupRef != "known-opaque-object" {
		t.Fatalf("post-upload cleanup row count=%d state=%q ref=%q", cleanupCount, cleanupState, cleanupRef)
	}
	var reservationState, responseStatus, sealedEnvelope string
	if err := db.QueryRow(`SELECT state, response_status, response_envelope FROM assurance_idempotency_records WHERE tenant_id=? AND actor_id=? AND tool='workiva_export_evidence' AND action='export' AND idempotency_digest=?`, testTenant, testTenant+"/actor-1", digestHex(DigestIdempotencyKey(request.IdempotencyKey))).Scan(&reservationState, &responseStatus, &sealedEnvelope); err != nil {
		t.Fatal(err)
	}
	if reservationState != string(ReservationStateFailed) || responseStatus != "failed" || sealedEnvelope == "" {
		t.Fatalf("failed export reservation state=%q status=%q envelope=%q", reservationState, responseStatus, sealedEnvelope)
	}
	var failure StructuredError
	if err := json.Unmarshal([]byte(sealedEnvelope), &failure); err != nil || failure.Code == "" || failure.Message == "" {
		t.Fatalf("failed export envelope is not typed: %s (%v)", sealedEnvelope, err)
	}
	_, replayErr := store.ExportEvidence(assuranceContext(), "ignored", "different-audit", request)
	var replayFailure *Error
	if !errors.As(replayErr, &replayFailure) || replayFailure.Code != "internal_error" {
		t.Fatalf("failed replay error=%v, want typed sealed internal_error (first=%v)", replayErr, firstErr)
	}
	if storage.puts != 1 || storage.deletes != 1 {
		t.Fatalf("failed replay repeated storage work: puts=%d deletes=%d", storage.puts, storage.deletes)
	}
}

func seedLongTermRetentionForEvidenceTest(t *testing.T, db *sql.DB) {
	t.Helper()
	policy := RetentionPolicy{TenantID: testTenant, RetentionClass: "long_term", Revision: 1, DurationSeconds: 3600, Status: "active"}
	raw, err := CanonicalJSON(policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_retention_policy_revisions (tenant_id, retention_class, revision, duration_seconds, policy_json, content_hash) VALUES (?, ?, ?, ?, ?, ?)`, testTenant, policy.RetentionClass, policy.Revision, policy.DurationSeconds, string(raw), digestHex(HashBytes(raw))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_active_bundle_objects (tenant_id, object_kind, object_id, object_revision, bundle_version) VALUES (?, 'retention_policy', ?, ?, 1)`, testTenant, policy.RetentionClass, policy.Revision); err != nil {
		t.Fatal(err)
	}
}
