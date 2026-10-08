package assurance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/dantalabs/northern-lights/internal/identity"
)

func TestAzureEvidenceExportCleanupReconcilesAmbiguousCreateWithoutReexport(t *testing.T) {
	store, db := openTestStore(t)
	snapshotBundle(t, store, []FieldDefinition{{FieldID: "amount", ResourceID: "resource", ExternalResourceID: "sp", SubresourceID: "sh", Locator: "B3", Kind: ValueNumber, Unit: "kWh", Required: true, Order: 1}})
	seedLongTermRetentionForEvidenceTest(t, db)
	reader := &scriptedReader{results: map[string]ProviderRead{"B3": {Value: ProviderValue{Value: json.Number("12")}, CacheBypassed: true}}, errors: map[string]error{}}
	snapshot, err := (SnapshotService{Store: store, Reader: reader}).Capture(assuranceContext(), "actor-1", "snapshot-audit", SnapshotRequest{ReportID: "energy-report", Period: Period{Key: "2026-Q3"}, RetentionClass: "long_term", IdempotencyKey: "snapshot-key-ambiguous-cleanup"})
	if err != nil {
		t.Fatal(err)
	}

	fake := &evidenceBlobFake{objects: make(map[string][]byte), failAfterCommit: true}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	defer server.Close()
	client, err := azblob.NewClientWithNoCredential(server.URL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	storage, err := NewAzureEvidenceStorageWithClient(context.Background(), AzureEvidenceConfig{ServiceURL: server.URL, ContainerName: "evidence-private", TenantID: testTenant, EnvironmentDigest: "sandbox-a", Timeout: time.Second, MaxArtifactBytes: 1 << 20}, client)
	if err != nil {
		t.Fatal(err)
	}
	store.SetEvidenceStorage(storage)
	request := EvidenceRequest{SubjectKind: "snapshot", SubjectID: snapshot.SnapshotID, Format: "json", RedactionProfile: RedactionStandard, RetentionClass: "long_term", IdempotencyKey: "ambiguous-create-cleanup"}
	if _, err := store.ExportEvidence(assuranceContext(), "ignored", "export-audit", request); err == nil {
		t.Fatal("committed-but-response-lost Blob upload unexpectedly completed export")
	}

	var cleanupID, cleanupRef, cleanupState string
	if err := db.QueryRow(`SELECT cleanup_id, storage_reference, state FROM assurance_evidence_cleanup WHERE tenant_id=? AND record_id=(SELECT record_id FROM assurance_idempotency_records WHERE tenant_id=? AND tool='workiva_export_evidence' AND action='export' AND idempotency_digest=? LIMIT 1)`, testTenant, testTenant, digestHex(DigestIdempotencyKey(request.IdempotencyKey))).Scan(&cleanupID, &cleanupRef, &cleanupState); err != nil {
		t.Fatalf("export did not persist cleanup ownership for its known opaque ref: %v", err)
	}
	if cleanupID == "" || cleanupRef == "" || cleanupState != "reconciliation_required" {
		t.Fatalf("cleanup row id=%q state=%q ref=%q", cleanupID, cleanupState, cleanupRef)
	}
	assertExportFailedEnvelope(t, db, request.IdempotencyKey)
	fake.mu.Lock()
	if fake.putConditions != 1 || fake.gets != 0 || fake.deletes != 0 || len(fake.objects) != 1 {
		t.Fatalf("failed exporter cleanup should leave one known object: PUT=%d GET=%d DELETE=%d objects=%d", fake.putConditions, fake.gets, fake.deletes, len(fake.objects))
	}
	fake.mu.Unlock()

	// An exporter may cause cleanup to be recorded but cannot discover and
	// resolve an unknown object version. The same tenant's admin can do so.
	exporterResult, exporterErr := store.ReconcileEvidenceCleanup(assuranceContext(), storage, time.Now().UTC(), 10)
	if exporterErr == nil || exporterResult.Attempted != 1 || exporterResult.Pending != 1 || exporterResult.Deleted != 0 {
		t.Fatalf("exporter reconciliation result=%+v err=%v; want pending admin-only resolution", exporterResult, exporterErr)
	}
	if state := cleanupStateFor(t, db, cleanupID); state != "reconciliation_required" {
		t.Fatalf("exporter-only reconciliation changed cleanup state to %q", state)
	}

	adminCtx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: testTenant, ObjectID: "cleanup-admin", Permissions: []identity.Permission{identity.PermissionTenantAdmin}})
	adminResult, err := store.ReconcileEvidenceCleanup(adminCtx, storage, time.Now().UTC(), 10)
	if err != nil || adminResult.Attempted != 1 || adminResult.Deleted != 1 || adminResult.Pending != 0 {
		t.Fatalf("tenant-admin cleanup result=%+v err=%v", adminResult, err)
	}
	if state := cleanupStateFor(t, db, cleanupID); state != "deleted" {
		t.Fatalf("confirmed conditional deletion did not seal cleanup state: %q", state)
	}
	// If an earlier attempt actually deleted the object but its cleanup-row
	// transaction did not commit, a later admin pass may resolve only the proven
	// absence (Blob 404), without attempting an unconditional delete.
	if _, err := db.Exec(`INSERT INTO assurance_evidence_cleanup (tenant_id, cleanup_id, record_id, storage_reference, state, error, created_at, updated_at) VALUES (?, 'confirmed-absent-cleanup', 'same-object', ?, 'reconciliation_required', 'simulated lost cleanup seal', ?, ?)`, testTenant, cleanupRef, formatTimestamp(time.Now().UTC()), formatTimestamp(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	absentResult, err := store.ReconcileEvidenceCleanup(adminCtx, storage, time.Now().UTC(), 10)
	if err != nil || absentResult.Attempted != 1 || absentResult.Deleted != 1 || absentResult.Pending != 0 || cleanupStateFor(t, db, "confirmed-absent-cleanup") != "deleted" {
		t.Fatalf("confirmed-absence reconciliation result=%+v err=%v", absentResult, err)
	}
	fake.mu.Lock()
	if fake.putConditions != 1 || fake.gets != 2 || fake.deletes != 1 || len(fake.objects) != 0 || fake.deleteIfMatch != `"evidence-1"` {
		fake.mu.Unlock()
		t.Fatalf("admin cleanup was not verified/no-repeat: PUT=%d GET=%d DELETE=%d objects=%d If-Match=%q", fake.putConditions, fake.gets, fake.deletes, len(fake.objects), fake.deleteIfMatch)
	}
	putCount, getCount, deleteCount := fake.putConditions, fake.gets, fake.deletes
	fake.mu.Unlock()

	_, replayErr := store.ExportEvidence(assuranceContext(), "ignored", "replay-audit", request)
	var typedErr *Error
	if !errors.As(replayErr, &typedErr) || typedErr.Code != "internal_error" {
		t.Fatalf("failed export replay=%v, want sealed internal_error", replayErr)
	}
	assertExportFailedEnvelope(t, db, request.IdempotencyKey)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.putConditions != putCount || fake.gets != getCount || fake.deletes != deleteCount || len(fake.objects) != 0 {
		t.Fatalf("sealed export replay repeated storage work: PUT=%d GET=%d DELETE=%d objects=%d", fake.putConditions, fake.gets, fake.deletes, len(fake.objects))
	}
}

func cleanupStateFor(t *testing.T, db *sql.DB, cleanupID string) string {
	t.Helper()
	var state string
	if err := db.QueryRow(`SELECT state FROM assurance_evidence_cleanup WHERE tenant_id=? AND cleanup_id=?`, testTenant, cleanupID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func assertExportFailedEnvelope(t *testing.T, db *sql.DB, key string) {
	t.Helper()
	var state, status, envelope string
	if err := db.QueryRow(`SELECT state, response_status, response_envelope FROM assurance_idempotency_records WHERE tenant_id=? AND tool='workiva_export_evidence' AND action='export' AND idempotency_digest=?`, testTenant, digestHex(DigestIdempotencyKey(key))).Scan(&state, &status, &envelope); err != nil {
		t.Fatal(err)
	}
	var failure StructuredError
	if state != string(ReservationStateFailed) || status != "failed" || envelope == "" || json.Unmarshal([]byte(envelope), &failure) != nil || failure.Code == "" {
		t.Fatalf("failed export reservation not durably sealed: state=%q status=%q envelope=%q", state, status, envelope)
	}
}
