package assurance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
)

func TestWave2ManifestPackageHashBindsArtifactIdentityAndStorageReference(t *testing.T) {
	artifact := EvidenceArtifact{ArtifactID: "artifact-a", Name: "subject.json", MediaType: "application/json", ByteCount: 4, SHA256: digestHex(HashBytes([]byte("data"))), StorageRef: "opaque-a"}
	manifest := EvidenceManifest{ManifestID: "manifest-a", ManifestVersion: 2, SubjectKind: "snapshot", SubjectID: "snapshot-a", Artifacts: []EvidenceArtifact{artifact}}
	manifest.PackageHash = digestHex(HashBytes(packageHash(manifest.Artifacts)))

	tampered := artifact
	tampered.StorageRef = "opaque-b"
	if err := VerifyManifest(manifest, []EvidenceArtifact{tampered}); err == nil {
		t.Fatal("manifest accepted a tampered storage reference")
	}
	tampered = artifact
	tampered.ArtifactID = "artifact-b"
	if err := VerifyManifest(manifest, []EvidenceArtifact{tampered}); err == nil {
		t.Fatal("manifest accepted a tampered artifact identity")
	}
}

func TestWave2RedactionRemovesSensitivePathsAndTenantMetadataBeforeHashing(t *testing.T) {
	raw := []byte(`{"subject":"ok","filesystem_path":"/srv/private/file.json","storage_path":"/srv/private/storage","path":"/srv/private","tenant_id":"hidden-tenant","authorization":"Bearer secret","credential":"secret","idempotency_key":"raw-key","nested":{"provider_revision":"rev-1"}}`)
	redacted, err := redactCanonical(raw, RedactionStandard)
	if err != nil {
		t.Fatal(err)
	}
	text := string(redacted)
	for _, forbidden := range []string{"/srv/private", "hidden-tenant", "Bearer secret", "secret", "raw-key"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("redacted JSON contains %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, `"subject":"ok"`) {
		t.Fatalf("redaction removed the authoritative subject: %s", text)
	}
	records := collectRedactionRecords(raw, RedactionStandard)
	foundStoragePath := false
	for _, record := range records {
		if record.Field == "storage_path" {
			foundStoragePath = true
		}
	}
	if !foundStoragePath {
		t.Fatalf("redaction record omitted storage_path: %#v", records)
	}
}

func TestWave2CSVPolicyIsRepresentedInCanonicalManifest(t *testing.T) {
	manifest := EvidenceManifest{ManifestID: "manifest-csv", ManifestVersion: 2, SubjectKind: "snapshot", SubjectID: "snapshot-csv", CSV: &CSVMetadata{FormulaEscaping: "prefix_formula_values_with_apostrophe", Authoritative: false}}
	raw, err := CanonicalManifestJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "formula_escaping") || !strings.Contains(string(raw), "prefix_formula_values_with_apostrophe") {
		t.Fatalf("canonical manifest omitted CSV escape policy: %s", raw)
	}
}

func TestWave2CleanupReconciliationDeletesArtifactsWithoutReexport(t *testing.T) {
	store, db := openTestStore(t)
	storage := NewMemoryEvidenceStorage()
	ctx := assuranceContext()
	ref, err := storage.Put(ctx, "subject.json", "application/json", []byte("artifact"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_evidence_cleanup (tenant_id, cleanup_id, record_id, storage_reference, state, error, created_at, updated_at) VALUES (?, 'cleanup-1', 'record-1', ?, 'reconciliation_required', 'injected storage fault', ?, ?)`, testTenant, ref, formatTimestamp(time.Now().UTC()), formatTimestamp(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}

	result, err := store.ReconcileEvidenceCleanup(ctx, storage, time.Now().UTC(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Attempted != 1 || result.Deleted != 1 || result.Pending != 0 {
		t.Fatalf("reconciliation result=%#v", result)
	}
	var state, cleanupError string
	if err := db.QueryRow(`SELECT state, error FROM assurance_evidence_cleanup WHERE tenant_id=? AND cleanup_id=?`, testTenant, "cleanup-1").Scan(&state, &cleanupError); err != nil {
		t.Fatal(err)
	}
	if state != "deleted" || cleanupError != "" {
		t.Fatalf("cleanup state=%q error=%q", state, cleanupError)
	}
	if _, err := storage.Read(ctx, ref); !errors.Is(err, ErrEvidenceNotFound) {
		t.Fatalf("artifact remained after reconciliation: %v", err)
	}
}

func TestWave2LegalHoldPlacementIsIdempotentPerManifest(t *testing.T) {
	store, db := openTestStore(t)
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: testTenant, ObjectID: "admin", Permissions: []identity.Permission{identity.PermissionTenantAdmin}})
	if _, err := db.Exec(`INSERT INTO assurance_evidence_manifests (tenant_id, manifest_id, manifest_version, subject_kind, subject_id, manifest_hash, manifest_json, audit_integrity, audit_completeness, expiry_at, legal_hold, idempotency_digest) VALUES (?, 'manifest-idempotent-hold', 2, 'snapshot', 'subject', 'hash', '{}', '{}', 'unknown', ?, 0, 'hold')`, testTenant, time.Now().Add(time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := store.PlaceLegalHold(ctx, "manifest-idempotent-hold", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := store.PlaceLegalHold(ctx, "manifest-idempotent-hold", "admin"); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_legal_holds WHERE tenant_id=? AND manifest_id=? AND active=1`, testTenant, "manifest-idempotent-hold").Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active hold rows=%d, want 1", active)
	}
}

func TestWave2CleanupReconciliationIsBoundedAndTenantScoped(t *testing.T) {
	store, db := openTestStore(t)
	storage := NewMemoryEvidenceStorage()
	ctx := assuranceContext()
	for i := 0; i < 2; i++ {
		if _, err := db.Exec(`INSERT INTO assurance_evidence_cleanup (tenant_id, cleanup_id, record_id, storage_reference, state, error, created_at, updated_at) VALUES (?, ?, ?, ?, 'reconciliation_required', '', ?, ?)`, testTenant, "cleanup-"+string(rune('a'+i)), "record", "missing-ref", formatTimestamp(time.Now().UTC()), formatTimestamp(time.Now().UTC())); err != nil {
			t.Fatal(err)
		}
	}
	other := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: "other-tenant", ObjectID: "other"})
	if _, err := db.Exec(`INSERT INTO assurance_evidence_cleanup (tenant_id, cleanup_id, record_id, storage_reference, state, error, created_at, updated_at) VALUES ('other-tenant', 'other-cleanup', 'other-record', 'missing-ref', 'reconciliation_required', '', ?, ?)`, formatTimestamp(time.Now().UTC()), formatTimestamp(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	result, err := store.ReconcileEvidenceCleanup(ctx, storage, time.Now().UTC(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Attempted != 1 {
		t.Fatalf("bounded result=%#v", result)
	}
	var otherState string
	if err := db.QueryRow(`SELECT state FROM assurance_evidence_cleanup WHERE tenant_id=? AND cleanup_id=?`, "other-tenant", "other-cleanup").Scan(&otherState); err != nil {
		t.Fatal(err)
	}
	if otherState != "reconciliation_required" {
		t.Fatalf("other tenant cleanup was touched: %q", otherState)
	}
	_ = other
}

func TestWave2AmbiguousCommitPreservesArtifactsForReconciliation(t *testing.T) {
	store, db := openTestStore(t)
	storage := NewMemoryEvidenceStorage()
	ctx := assuranceContext()
	ref, err := storage.Put(ctx, "subject.json", "application/json", []byte("committed-or-unknown"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupEvidenceRefs(ctx, store, storage, []string{ref}, "ambiguous-record", ErrEvidenceCommitAmbiguous)
	var state string
	if err := db.QueryRow(`SELECT state FROM assurance_evidence_cleanup WHERE tenant_id=? AND record_id=?`, testTenant, "ambiguous-record").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "reconciliation_required" {
		t.Fatalf("ambiguous cleanup state=%q", state)
	}
	if _, err := storage.Read(ctx, ref); err != nil {
		t.Fatalf("ambiguous artifact was deleted: %v", err)
	}
	if _, err := store.ReconcileEvidenceCleanup(ctx, storage, time.Now().UTC(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Read(ctx, ref); !errors.Is(err, ErrEvidenceNotFound) {
		t.Fatalf("reconciled artifact still exists: %v", err)
	}
}

func TestWave2ExportRejectsMultipleStorageAdaptersBeforeReservation(t *testing.T) {
	store, db := openTestStore(t)
	store.setReadiness(true, nil)
	_, err := store.ExportEvidence(assuranceContext(), "actor-1", "audit", EvidenceRequest{SubjectKind: "snapshot", SubjectID: "subject", Format: "json", RedactionProfile: RedactionStandard, RetentionClass: "long_term", IdempotencyKey: "two-storages"}, NewMemoryEvidenceStorage(), NewMemoryEvidenceStorage())
	if err == nil || !containsDomainCode(err, "invalid_request") {
		t.Fatalf("multiple storage adapters were accepted: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_idempotency_records`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalid storage configuration created %d reservation(s)", count)
	}
}
