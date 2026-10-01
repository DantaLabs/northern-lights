package assurance

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
)

func TestWave2ExportProfileResolutionRejectsAmbiguousActiveProfiles(t *testing.T) {
	store, db := openTestStore(t)
	snapshotBundle(t, store, []FieldDefinition{{FieldID: "field", ResourceID: "r", ExternalResourceID: "sp", SubresourceID: "sh", Locator: "A1", Kind: ValueText, Required: true, Order: 1}})
	profiles := []string{"profile-standard", "profile-extra"}
	rawProfiles, err := json.Marshal(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE assurance_report_revisions SET export_profiles_json=? WHERE tenant_id=? AND report_id='energy-report' AND revision=1`, string(rawProfiles), testTenant); err != nil {
		t.Fatal(err)
	}
	extra := ExportProfile{ProfileID: "profile-extra", Revision: 1, Status: "active", PermittedSubjects: []string{"snapshot"}, RedactionProfile: "standard", RetentionClass: "long_term", MaxRows: 100, MaxBytes: 1 << 20, DeliveryPolicy: "opaque_reference"}
	raw, err := CanonicalJSON(extra)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_export_profiles (tenant_id, profile_id, revision, status, profile_json, content_hash) VALUES (?, ?, ?, ?, ?, ?)`, testTenant, extra.ProfileID, extra.Revision, extra.Status, string(raw), digestHex(HashBytes(raw))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_active_bundle_objects (tenant_id, object_kind, object_id, object_revision, bundle_version) VALUES (?, 'export_profile', ?, 1, 1)`, testTenant, extra.ProfileID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_snapshots (tenant_id, snapshot_id, idempotency_digest, report_id, definition_revision, period_json, status, completeness, mapping_set_hash, provider_route, retention_class) VALUES (?, 'snapshot-profile', 'digest-profile', 'energy-report', 1, '{}', 'completed', 'complete', '', '', 'long_term')`, testTenant); err != nil {
		t.Fatal(err)
	}
	if _, err := store.resolveExportProfile(assuranceContext(), "snapshot", "snapshot-profile", RedactionStandard, "long_term"); err == nil || !containsDomainCode(err, "export_profile_ambiguous") {
		t.Fatalf("ambiguous profiles were accepted: %v", err)
	}
}

func TestWave2BundleRejectsDuplicateExportProfileIDs(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, sig := signedBundle(t, private, func(bundle *Bundle) {
		profile := ExportProfile{ProfileID: "duplicate-profile", Revision: 1, Status: "active", PermittedSubjects: []string{"snapshot"}, RedactionProfile: "standard", RetentionClass: "standard", MaxRows: 10, MaxBytes: 1024, DeliveryPolicy: "opaque_reference"}
		bundle.ExportProfiles = []ExportProfile{profile, profile}
		bundle.Reports[0].ExportProfiles = []string{profile.ProfileID}
	})
	if _, err := ValidateBundle(raw, sig, testTenant, public); err == nil {
		t.Fatal("duplicate export profile IDs were accepted")
	}
}
