package assurance

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func relationshipAllowlistEntry(id string, revision int, actor, resource string) RelationshipAllowlistEntry {
	return RelationshipAllowlistEntry{
		EntryID:    id,
		Revision:   revision,
		ActorID:    actor,
		Capability: RelationshipCapabilityRead,
		ResourceID: resource,
	}
}

func signedRelationshipBundle(t *testing.T, private ed25519.PrivateKey, bundleID string, bundleVersion int, entries []RelationshipAllowlistEntry) ([]byte, []byte) {
	t.Helper()
	return signedBundle(t, private, func(bundle *Bundle) {
		bundle.BundleID = bundleID
		bundle.BundleVersion = bundleVersion
		bundle.RelationshipAllowlist = entries
	})
}

func validateStageActivateRelationshipBundle(t *testing.T, store *Store, public ed25519.PublicKey, raw, signature []byte) {
	t.Helper()
	validated, err := ValidateBundle(raw, signature, testTenant, public)
	if err != nil {
		t.Fatalf("ValidateBundle: %v", err)
	}
	ctx := assuranceContext()
	if err := store.StageBundle(ctx, validated); err != nil {
		t.Fatalf("StageBundle: %v", err)
	}
	if err := store.RequestActivation(ctx, validated.Bundle.BundleID, validated.Bundle.BundleVersion); err != nil {
		t.Fatalf("RequestActivation: %v", err)
	}
	if err := store.Bootstrap(ctx, testTenant, public); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
}

func TestRelationshipAllowlistSuccessiveCompleteBundlesPreserveHistoryAndOmitActiveEntries(t *testing.T) {
	store, db := openTestStore(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := relationshipAllowlistEntry("root-read", 1, "actor-a", "root")
	omitted := relationshipAllowlistEntry("secret-read", 1, "actor-a", "secret")
	raw, signature := signedRelationshipBundle(t, private, "relationships-1", 1, []RelationshipAllowlistEntry{root, omitted})
	validateStageActivateRelationshipBundle(t, store, public, raw, signature)

	raw, signature = signedRelationshipBundle(t, private, "relationships-2", 2, []RelationshipAllowlistEntry{root})
	validateStageActivateRelationshipBundle(t, store, public, raw, signature)

	var history, rootActive, omittedActive int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_relationship_allowlist_revisions WHERE tenant_id=?`, testTenant).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM assurance_active_bundle_objects WHERE tenant_id=? AND object_kind='relationship_allowlist' AND object_id='root-read' AND object_revision=1`, testTenant).Scan(&rootActive); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM assurance_active_bundle_objects WHERE tenant_id=? AND object_kind='relationship_allowlist' AND object_id='secret-read'`, testTenant).Scan(&omittedActive); err != nil {
		t.Fatal(err)
	}
	if history != 2 || rootActive != 1 || omittedActive != 0 {
		t.Fatalf("successive complete bundle state: history=%d root_active=%d omitted_active=%d", history, rootActive, omittedActive)
	}
	var rootRows int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_relationship_allowlist_revisions WHERE tenant_id=? AND entry_id='root-read' AND revision=1`, testTenant).Scan(&rootRows); err != nil {
		t.Fatal(err)
	}
	if rootRows != 1 {
		t.Fatalf("unchanged allowlist entry was not idempotent: rows=%d", rootRows)
	}
}

func TestRelationshipAllowlistHistoryRowsAreDatabaseImmutable(t *testing.T) {
	store, db := openTestStore(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, signature := signedRelationshipBundle(t, private, "relationships-1", 1, []RelationshipAllowlistEntry{
		relationshipAllowlistEntry("root-read", 1, "actor-a", "root"),
	})
	validateStageActivateRelationshipBundle(t, store, public, raw, signature)
	if _, err := db.Exec(`UPDATE assurance_relationship_allowlist_revisions SET resource_id='changed' WHERE tenant_id=? AND entry_id='root-read'`, testTenant); err == nil {
		t.Fatal("relationship allowlist history update succeeded")
	}
	if _, err := db.Exec(`DELETE FROM assurance_relationship_allowlist_revisions WHERE tenant_id=? AND entry_id='root-read'`, testTenant); err == nil {
		t.Fatal("relationship allowlist history delete succeeded")
	}
}

func TestRelationshipAllowlistRequiresSignedTenantBoundBundle(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, signature := signedRelationshipBundle(t, private, "relationships-1", 1, []RelationshipAllowlistEntry{
		relationshipAllowlistEntry("root-read", 1, "actor-a", "root"),
	})
	if _, err := ValidateBundle(raw, nil, testTenant, public); err == nil {
		t.Fatal("unsigned relationship allowlist bundle accepted")
	}
	if _, err := ValidateBundle(raw, signature, "another-tenant", public); err == nil {
		t.Fatal("cross-tenant relationship allowlist bundle accepted")
	}
	if _, err := ValidateBundle(raw, signature, testTenant, public); err != nil {
		t.Fatalf("signed tenant-bound relationship allowlist bundle rejected: %v", err)
	}
}

func TestRelationshipAllowlistRejectsDowngradeAndChangedBytesAtReusedRevision(t *testing.T) {
	t.Run("downgrade", func(t *testing.T) {
		store, _ := openTestStore(t)
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		raw, signature := signedRelationshipBundle(t, private, "relationships-1", 1, []RelationshipAllowlistEntry{
			relationshipAllowlistEntry("root-read", 2, "actor-a", "root"),
		})
		validateStageActivateRelationshipBundle(t, store, public, raw, signature)
		raw, signature = signedRelationshipBundle(t, private, "relationships-2", 2, []RelationshipAllowlistEntry{
			relationshipAllowlistEntry("root-read", 1, "actor-a", "root"),
		})
		validated, err := ValidateBundle(raw, signature, testTenant, public)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.StageBundle(assuranceContext(), validated); err == nil {
			t.Fatal("relationship allowlist revision downgrade accepted")
		}
	})

	t.Run("reused revision changed bytes", func(t *testing.T) {
		store, _ := openTestStore(t)
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		raw, signature := signedRelationshipBundle(t, private, "relationships-1", 1, []RelationshipAllowlistEntry{
			relationshipAllowlistEntry("root-read", 1, "actor-a", "root"),
		})
		validateStageActivateRelationshipBundle(t, store, public, raw, signature)
		raw, signature = signedRelationshipBundle(t, private, "relationships-2", 2, []RelationshipAllowlistEntry{
			relationshipAllowlistEntry("root-read", 1, "actor-a", "changed-resource"),
		})
		validated, err := ValidateBundle(raw, signature, testTenant, public)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.StageBundle(assuranceContext(), validated); err == nil {
			t.Fatal("relationship allowlist reused revision with changed bytes accepted")
		}
	})
}

func TestRelationshipAllowlistBootstrapRejectsSignedDowngradeCandidate(t *testing.T) {
	store, db := openTestStore(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, signature := signedRelationshipBundle(t, private, "relationships-1", 1, []RelationshipAllowlistEntry{
		relationshipAllowlistEntry("root-read", 2, "actor-a", "root"),
	})
	validateStageActivateRelationshipBundle(t, store, public, raw, signature)

	raw, signature = signedRelationshipBundle(t, private, "relationships-2", 2, []RelationshipAllowlistEntry{
		relationshipAllowlistEntry("root-read", 1, "actor-a", "root"),
	})
	validated, err := ValidateBundle(raw, signature, testTenant, public)
	if err != nil {
		t.Fatal(err)
	}
	now := formatTimestamp(time.Now())
	if _, err := db.Exec(`INSERT INTO assurance_bundle_candidates
 (tenant_id,bundle_id,bundle_version,schema_version,bundle_json,signature_hex,content_hash,activation_requested,staged_at,requested_at)
 VALUES (?,?,?,?,?,?,?,?,?,?)`, testTenant, validated.Bundle.BundleID, validated.Bundle.BundleVersion,
		validated.Bundle.SchemaVersion, string(validated.CanonicalJSON), hex.EncodeToString(validated.Signature), validated.ContentHash, 1, now, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(assuranceContext(), testTenant, public); !errors.Is(err, ErrNotReady) {
		t.Fatalf("signed relationship downgrade bootstrap = %v, want ErrNotReady", err)
	}
	var activeBundleVersion, activeEntryRevision int
	if err := db.QueryRow(`SELECT bundle_version FROM assurance_active_bundles WHERE tenant_id=?`, testTenant).Scan(&activeBundleVersion); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT object_revision FROM assurance_active_bundle_objects WHERE tenant_id=? AND object_kind='relationship_allowlist' AND object_id='root-read'`, testTenant).Scan(&activeEntryRevision); err != nil {
		t.Fatal(err)
	}
	if activeBundleVersion != 1 || activeEntryRevision != 2 {
		t.Fatalf("signed downgrade changed active state: bundle=%d entry_revision=%d", activeBundleVersion, activeEntryRevision)
	}
}

func TestRelationshipAllowlistInvalidCandidateFailsReadinessWithoutActivation(t *testing.T) {
	store, db := openTestStore(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, signature := signedRelationshipBundle(t, private, "relationships-1", 1, []RelationshipAllowlistEntry{
		relationshipAllowlistEntry("root-read", 1, "actor-a", "root"),
	})
	validateStageActivateRelationshipBundle(t, store, public, raw, signature)

	raw, signature = signedRelationshipBundle(t, private, "relationships-2", 2, []RelationshipAllowlistEntry{
		relationshipAllowlistEntry("root-read", 2, "actor-a", "root"),
	})
	validated, err := ValidateBundle(raw, signature, testTenant, public)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StageBundle(assuranceContext(), validated); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestActivation(assuranceContext(), "relationships-2", 2); err != nil {
		t.Fatal(err)
	}
	var candidate string
	if err := db.QueryRow(`SELECT bundle_json FROM assurance_bundle_candidates WHERE tenant_id=? AND bundle_id='relationships-2'`, testTenant).Scan(&candidate); err != nil {
		t.Fatal(err)
	}
	candidate = strings.Replace(candidate, `"resource_id":"root"`, `"resource_id":"tampered"`, 1)
	if _, err := db.Exec(`UPDATE assurance_bundle_candidates SET bundle_json=? WHERE tenant_id=? AND bundle_id='relationships-2'`, candidate, testTenant); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(assuranceContext(), testTenant, public); !errors.Is(err, ErrNotReady) {
		t.Fatalf("tampered relationship candidate bootstrap = %v, want ErrNotReady", err)
	}
	if err := store.Ready(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("tampered relationship candidate readiness = %v, want ErrNotReady", err)
	}
	var activeVersion, revisionTwo int
	if err := db.QueryRow(`SELECT bundle_version FROM assurance_active_bundles WHERE tenant_id=?`, testTenant).Scan(&activeVersion); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM assurance_relationship_allowlist_revisions WHERE tenant_id=? AND entry_id='root-read' AND revision=2`, testTenant).Scan(&revisionTwo); err != nil {
		t.Fatal(err)
	}
	if activeVersion != 1 || revisionTwo != 0 {
		t.Fatalf("invalid relationship candidate partially activated: active_version=%d revision_two=%d", activeVersion, revisionTwo)
	}
}

func TestRelationshipAllowlistActivationFailureRollsBackAtomically(t *testing.T) {
	store, db := openTestStore(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := relationshipAllowlistEntry("root-read", 1, "actor-a", "root")
	raw, signature := signedRelationshipBundle(t, private, "relationships-1", 1, []RelationshipAllowlistEntry{root})
	validateStageActivateRelationshipBundle(t, store, public, raw, signature)
	if _, err := db.Exec(`CREATE TRIGGER fail_allowlist_activation BEFORE INSERT ON assurance_relationship_allowlist_revisions WHEN NEW.entry_id='fail-read' BEGIN SELECT RAISE(ABORT,'injected allowlist failure'); END`); err != nil {
		t.Fatal(err)
	}
	raw, signature = signedRelationshipBundle(t, private, "relationships-2", 2, []RelationshipAllowlistEntry{
		root,
		relationshipAllowlistEntry("fail-read", 1, "actor-a", "failure"),
	})
	validated, err := ValidateBundle(raw, signature, testTenant, public)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StageBundle(assuranceContext(), validated); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestActivation(assuranceContext(), "relationships-2", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(assuranceContext(), testTenant, public); !errors.Is(err, ErrNotReady) {
		t.Fatalf("injected relationship activation failure = %v, want ErrNotReady", err)
	}
	var activeVersion, oldActive, failedHistory int
	if err := db.QueryRow(`SELECT bundle_version FROM assurance_active_bundles WHERE tenant_id=?`, testTenant).Scan(&activeVersion); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM assurance_active_bundle_objects WHERE tenant_id=? AND object_kind='relationship_allowlist' AND object_id='root-read' AND object_revision=1`, testTenant).Scan(&oldActive); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM assurance_relationship_allowlist_revisions WHERE tenant_id=? AND entry_id='fail-read'`, testTenant).Scan(&failedHistory); err != nil {
		t.Fatal(err)
	}
	if activeVersion != 1 || oldActive != 1 || failedHistory != 0 {
		t.Fatalf("relationship activation did not roll back: active_version=%d old_active=%d failed_history=%d", activeVersion, oldActive, failedHistory)
	}
}
