package assurance

import (
	"context"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
)

func TestLegalHoldReleaseIsBoundToExactManifest(t *testing.T) {
	store, db := openTestStore(t)
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: testTenant, ObjectID: "admin", Permissions: []identity.Permission{identity.PermissionTenantAdmin}})
	for _, id := range []string{"manifest-a", "manifest-b"} {
		if _, err := db.Exec(`INSERT INTO assurance_evidence_manifests (tenant_id, manifest_id, manifest_version, subject_kind, subject_id, manifest_hash, manifest_json, audit_integrity, audit_completeness, expiry_at, legal_hold, idempotency_digest) VALUES (?, ?, 2, 'snapshot', 'same-subject', 'hash', '{}', '{}', 'unknown', ?, 0, ?)`, testTenant, id, time.Now().Add(time.Hour).Format(time.RFC3339Nano), id); err != nil {
			t.Fatal(err)
		}
		if err := store.PlaceLegalHold(ctx, id, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ReleaseLegalHold(ctx, "manifest-a", "admin"); err != nil {
		t.Fatal(err)
	}
	var heldA, heldB, activeB int
	if err := db.QueryRow(`SELECT legal_hold FROM assurance_evidence_manifests WHERE tenant_id=? AND manifest_id='manifest-a'`, testTenant).Scan(&heldA); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT legal_hold FROM assurance_evidence_manifests WHERE tenant_id=? AND manifest_id='manifest-b'`, testTenant).Scan(&heldB); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM assurance_legal_holds WHERE tenant_id=? AND manifest_id='manifest-b' AND active=1`, testTenant).Scan(&activeB); err != nil {
		t.Fatal(err)
	}
	if heldA != 0 || heldB != 1 || activeB != 1 {
		t.Fatalf("hold state a=%d b=%d active_b=%d", heldA, heldB, activeB)
	}
}

func TestPurgeTombstoneRetryIsUniquelyKeyedByManifest(t *testing.T) {
	store, db := openTestStore(t)
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: testTenant, ObjectID: "admin", Permissions: []identity.Permission{identity.PermissionTenantAdmin}})
	if _, err := db.Exec(`INSERT INTO assurance_evidence_manifests (tenant_id, manifest_id, manifest_version, subject_kind, subject_id, manifest_hash, manifest_json, audit_integrity, audit_completeness, expiry_at, legal_hold, idempotency_digest) VALUES (?, 'manifest-retry', 2, 'snapshot', 'subject', 'hash', '{}', '{}', 'unknown', ?, 0, 'retry')`, testTenant, time.Now().Add(-time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_evidence_tombstones (tenant_id, tombstone_id, subject_kind, subject_id, manifest_id, reason, purged_at, state) VALUES (?, 'first-id', 'snapshot', 'subject', 'manifest-retry', 'retention_pending', ?, 'pending')`, testTenant, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	_ = store.PurgeExpired(ctx, time.Now().UTC())
	if _, err := db.Exec(`INSERT OR IGNORE INTO assurance_evidence_tombstones (tenant_id, tombstone_id, subject_kind, subject_id, manifest_id, reason, purged_at) VALUES (?, 'second-id', 'snapshot', 'subject', 'manifest-retry', 'retention_pending', ?)`, testTenant, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_evidence_tombstones WHERE tenant_id=? AND manifest_id=?`, testTenant, "manifest-retry").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("tombstone count=%d, want one", count)
	}
}

func TestPurgeAuditFailureLeavesReconciliationStateWithoutBlocking(t *testing.T) {
	store, db := openTestStore(t)
	storage := NewMemoryEvidenceStorage()
	store.SetEvidenceStorage(storage)
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: testTenant, ObjectID: "admin", Permissions: []identity.Permission{identity.PermissionTenantAdmin}})
	ref, err := storage.Put(ctx, "subject.json", "application/json", []byte(`{"subject":"value"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_evidence_manifests (tenant_id, manifest_id, manifest_version, subject_kind, subject_id, manifest_hash, manifest_json, audit_integrity, audit_completeness, expiry_at, legal_hold, idempotency_digest) VALUES (?, 'manifest-audit-failure', 2, 'snapshot', 'subject', 'hash', '{}', '{}', 'unknown', ?, 0, 'audit-failure')`, testTenant, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_evidence_artifacts (tenant_id, artifact_id, manifest_id, artifact_hash, size_bytes, media_type, storage_reference) VALUES (?, 'artifact-audit-failure', 'manifest-audit-failure', 'hash', 20, 'application/json', ?)`, testTenant, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	var expired int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_evidence_manifests WHERE tenant_id=? AND expiry_at<>'' AND expiry_at<=? AND legal_hold=0`, testTenant, time.Now().UTC().Format(time.RFC3339Nano)).Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if expired != 1 {
		t.Fatalf("expired=%d, want 1", expired)
	}
	if err := store.PurgeExpired(ctx, time.Now().UTC()); err == nil {
		t.Fatal("purge succeeded without an audit append")
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM assurance_evidence_tombstones WHERE tenant_id=? AND manifest_id=?`, testTenant, "manifest-audit-failure").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "reconciliation_required" {
		t.Fatalf("tombstone state=%q, want reconciliation_required", state)
	}
}

func TestLegalHoldUsesTrustedPrincipalActor(t *testing.T) {
	store, db := openTestStore(t)
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: testTenant, ObjectID: "admin", Permissions: []identity.Permission{identity.PermissionTenantAdmin}})
	if _, err := db.Exec(`INSERT INTO assurance_evidence_manifests (tenant_id, manifest_id, manifest_version, subject_kind, subject_id, manifest_hash, manifest_json, audit_integrity, audit_completeness, expiry_at, legal_hold, idempotency_digest) VALUES (?, 'manifest-trusted-actor', 2, 'snapshot', 'subject', 'hash', '{}', '{}', 'unknown', ?, 0, 'trusted-actor')`, testTenant, time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := store.PlaceLegalHold(ctx, "manifest-trusted-actor", "spoofed-actor"); err != nil {
		t.Fatal(err)
	}
	var holdActor, auditActor string
	if err := db.QueryRow(`SELECT actor_id FROM assurance_legal_holds WHERE tenant_id=? AND manifest_id=?`, testTenant, "manifest-trusted-actor").Scan(&holdActor); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT actor FROM audit_log WHERE tenant_id=? AND target=? ORDER BY seq DESC LIMIT 1`, testTenant, "manifest-trusted-actor").Scan(&auditActor); err != nil {
		t.Fatal(err)
	}
	if holdActor != testTenant+"/admin" || auditActor != testTenant+"/admin" {
		t.Fatalf("trusted actors hold=%q audit=%q", holdActor, auditActor)
	}
}
