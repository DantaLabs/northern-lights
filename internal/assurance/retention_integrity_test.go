package assurance

import "testing"

func TestResolveRetentionPolicyFailsClosedOnCorruptJSONOrHash(t *testing.T) {
	store, db := openTestStore(t)
	store.setReadiness(true, nil)
	validHash := digestHex(HashBytes([]byte(`{"retention_class":"standard","duration_seconds":60,"status":"active"}`)))
	if _, err := db.Exec(`INSERT INTO assurance_retention_policy_revisions (tenant_id, retention_class, revision, duration_seconds, policy_json, content_hash) VALUES (?, 'standard', 2, 60, ?, ?)`, testTenant, `{"retention_class":"standard","duration_seconds":60,"status":"active"}`, validHash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_active_bundle_objects (tenant_id, object_kind, object_id, object_revision, bundle_version) VALUES (?, 'retention_policy', 'standard', 2, 2)`, testTenant); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveRetentionPolicy(assuranceContext(), "standard"); err != nil {
		t.Fatalf("valid retention policy rejected: %v", err)
	}
	if _, err := db.Exec(`UPDATE assurance_retention_policy_revisions SET policy_json='{' WHERE tenant_id=? AND retention_class='standard' AND revision=2`, testTenant); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveRetentionPolicy(assuranceContext(), "standard"); err == nil {
		t.Fatal("corrupt retention JSON was accepted")
	}
}

func TestRetentionRevisionRejectsDowngradeAndContentReuse(t *testing.T) {
	if !retentionRevisionConflict(2, "hash-v2", "hash-v1", 1) {
		t.Fatal("retention revision downgrade was accepted")
	}
	if !retentionRevisionConflict(2, "hash-v2", "hash-v2", 3) {
		t.Fatal("retention content was reused under a new revision")
	}
	if retentionRevisionConflict(2, "hash-v2", "hash-v3", 3) {
		t.Fatal("new retention content at a higher revision was rejected")
	}
}
