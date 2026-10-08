package relationships

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/identity"
	_ "modernc.org/sqlite"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err = assurance.NewWithDB(db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	return db
}

func activateRelationshipAllowlist(t *testing.T, db *sql.DB, tenant, actor string, resources ...string) {
	t.Helper()
	store, err := assurance.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]assurance.RelationshipAllowlistEntry, 0, len(resources))
	for index, resource := range resources {
		entries = append(entries, assurance.RelationshipAllowlistEntry{
			EntryID: fmt.Sprintf("%s-%d", actor, index+1), Revision: 1, ActorID: actor,
			Capability: assurance.RelationshipCapabilityRead, ResourceID: resource,
		})
	}
	fields := make([]assurance.FieldDefinition, 0, len(resources))
	for index, resource := range resources {
		fields = append(fields, assurance.FieldDefinition{
			FieldID: fmt.Sprintf("field-%d", index+1), ResourceID: resource,
			ExternalResourceID: fmt.Sprintf("external-%s", resource), SubresourceID: "sheet",
			Locator: "A1", Kind: assurance.ValueText, Required: true, Order: index + 1,
		})
	}
	bundle := assurance.Bundle{
		SchemaVersion: 1, BundleID: "relationship-test-bundle", BundleVersion: 1, TenantID: tenant,
		Reports: []assurance.ReportRevision{{
			ReportID: "relationship-test-report", Revision: 1, Name: "Relationship test", Owner: "operator", Status: "active",
			RetentionClass: "standard", ResourcePolicyHash: strings.Repeat("a", 64),
			Periods: []assurance.Period{{Key: "2026", Label: "2026", Start: "2026-01-01", End: "2026-12-31"}},
			Fields:  fields,
		}},
		RelationshipAllowlist: entries,
	}
	raw, err := assurance.CanonicalJSON(bundle)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := assurance.ValidateBundle(raw, ed25519.Sign(private, raw), tenant, public)
	if err != nil {
		t.Fatal(err)
	}
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: tenant, ObjectID: "operator"})
	if err := store.StageBundle(ctx, validated); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestActivation(ctx, bundle.BundleID, bundle.BundleVersion); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(ctx, tenant, public); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshRequiresCompleteExactScopeAndPreservesOnFailure(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{ResourceIDs: []string{"b", "a"}}
	complete := Discovery{Complete: true, EndOfScope: true, Nodes: []Node{{ResourceID: "a", Kind: "spreadsheet", ExternalID: "a"}, {ResourceID: "b", Kind: "spreadsheet", ExternalID: "b"}}, Edges: []Edge{{From: "a", To: "b", Relation: "derived_from", Provenance: "observed", Confidence: "high"}}}
	first, err := store.Refresh(ctx, "tenant", scope, complete, "actor", "audit")
	if err != nil || first.GraphVersion == "" {
		t.Fatalf("refresh: %#v %v", first, err)
	}
	failed, err := store.Refresh(ctx, "tenant", scope, Discovery{Complete: false, EndOfScope: false}, "actor", "audit")
	if err != nil || failed.GraphVersion != first.GraphVersion {
		t.Fatalf("incomplete refresh changed graph: %#v %v", failed, err)
	}
	providerFailed, err := store.Refresh(ctx, "tenant", scope, Discovery{Complete: true, EndOfScope: true, Error: "provider timeout"}, "actor", "audit")
	if err != nil || providerFailed.GraphVersion != first.GraphVersion {
		t.Fatalf("provider error changed graph: %#v %v", providerFailed, err)
	}
	var failedRuns int
	if err = db.QueryRow(`SELECT count(*) FROM assurance_relationship_discovery_runs WHERE tenant_id='tenant' AND status='failed' AND error_json='provider timeout' AND resulting_graph_version=prior_graph_version`).Scan(&failedRuns); err != nil || failedRuns != 1 {
		t.Fatalf("provider failure run not retained: count=%d err=%v", failedRuns, err)
	}
	activateRelationshipAllowlist(t, db, "tenant", "actor", "a", "b")
	got, err := store.Traverse(ctx, "tenant", "actor", "a", "", "both", 3, 100, 100)
	if err != nil || len(got.Edges) != 1 {
		t.Fatalf("active graph lost after incomplete refresh: %#v %v", got, err)
	}
}

func TestLineageIsBoundedCycleSafeAndDoesNotAuthorize(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	store, _ := NewStore(db)
	_, err := store.Refresh(ctx, "tenant", Scope{ResourceIDs: []string{"a", "b"}}, Discovery{Complete: true, EndOfScope: true, Nodes: []Node{{ResourceID: "a", Kind: "spreadsheet", ExternalID: "a"}, {ResourceID: "b", Kind: "spreadsheet", ExternalID: "b"}}, Edges: []Edge{{From: "a", To: "b", Relation: "derived_from", Provenance: "observed", Confidence: "high"}, {From: "b", To: "a", Relation: "derived_from", Provenance: "inferred", Confidence: "low"}}}, "actor", "audit")
	if err != nil {
		t.Fatal(err)
	}
	activateRelationshipAllowlist(t, db, "tenant", "actor", "a", "b")
	got, err := store.Traverse(ctx, "tenant", "actor", "a", "", "both", 8, 10, 10)
	if err != nil || len(got.Cycles) == 0 {
		t.Fatalf("cycle not reported: %#v %v", got, err)
	}
	if got.Authorizes("tenant", "b") {
		t.Fatal("relationship graph must never grant access")
	}
}

func TestRefreshExactScopeAndCompleteSuccessOnly(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	seed := func(scope Scope, from, to string) string {
		r, e := store.Refresh(ctx, "tenant", scope, Discovery{Complete: true, EndOfScope: true, Nodes: []Node{{ResourceID: from, Kind: "spreadsheet", ExternalID: from}, {ResourceID: to, Kind: "spreadsheet", ExternalID: to}}, Edges: []Edge{{ID: "edge-" + from, From: from, To: to, Relation: "derived_from", Provenance: "observed", Confidence: "high"}}}, "actor", "audit")
		if e != nil {
			t.Fatal(e)
		}
		return r.GraphVersion
	}
	v1 := seed(Scope{ResourceIDs: []string{"a"}}, "a", "x")
	v2 := seed(Scope{ResourceIDs: []string{"b"}}, "b", "y")
	if v1 == v2 {
		t.Fatalf("graph version did not advance: %q", v2)
	}
	r, err := store.Refresh(ctx, "tenant", Scope{ResourceIDs: []string{"a"}}, Discovery{Complete: true, EndOfScope: true}, "actor", "audit")
	if err != nil {
		t.Fatal(err)
	}
	var activeA, activeB int
	if err = db.QueryRow(`SELECT count(*) FROM assurance_relationship_edges WHERE tenant_id='tenant' AND source_resource_id='a' AND active=1`).Scan(&activeA); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM assurance_relationship_edges WHERE tenant_id='tenant' AND source_resource_id='b' AND active=1`).Scan(&activeB); err != nil {
		t.Fatal(err)
	}
	if activeA != 0 || activeB != 1 {
		t.Fatalf("scope refresh leaked: A=%d B=%d", activeA, activeB)
	}
	if r.GraphVersion == v2 {
		t.Fatalf("successful refresh must advance graph version: %s", r.GraphVersion)
	}
}

func TestRefreshFaultAfterInactivationRollsBackAtomically(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	store, _ := NewStore(db)
	scope := Scope{ResourceIDs: []string{"a"}}
	seed := Discovery{Complete: true, EndOfScope: true, Nodes: []Node{{ResourceID: "a", Kind: "spreadsheet", ExternalID: "a"}, {ResourceID: "b", Kind: "spreadsheet", ExternalID: "b"}}, Edges: []Edge{{ID: "stable", From: "a", To: "b", Relation: "derived_from", Provenance: "observed", Confidence: "high"}}}
	before, err := store.Refresh(ctx, "tenant", scope, seed, "actor", "audit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TRIGGER fail_relationship_insert BEFORE INSERT ON assurance_relationship_edges WHEN NEW.edge_id='broken' BEGIN SELECT RAISE(ABORT,'injected edge failure'); END`); err != nil {
		t.Fatal(err)
	}
	_, err = store.Refresh(ctx, "tenant", scope, Discovery{Complete: true, EndOfScope: true, Edges: []Edge{{ID: "broken", From: "a", To: "b", Relation: "derived_from", Provenance: "observed", Confidence: "high"}}}, "actor", "audit")
	if err == nil {
		t.Fatal("expected injected persistence fault")
	}
	var active int
	if err = db.QueryRow(`SELECT count(*) FROM assurance_relationship_edges WHERE tenant_id='tenant' AND edge_id='stable' AND active=1`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	var version string
	if err = db.QueryRow(`SELECT graph_version FROM assurance_relationship_graph_state WHERE tenant_id='tenant'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if active != 1 || version != before.GraphVersion {
		t.Fatalf("refresh partially committed: active=%d version=%s prior=%s", active, version, before.GraphVersion)
	}
}

func TestRefreshPersistsRunAndOptimisticVersion(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	store, _ := NewStore(db)
	_, err := store.Refresh(ctx, "tenant", Scope{ResourceIDs: []string{"a"}}, Discovery{Complete: true, EndOfScope: true}, "actor", "audit")
	if err != nil {
		t.Fatal(err)
	}
	var runs int
	if err = db.QueryRow(`SELECT count(*) FROM assurance_relationship_discovery_runs WHERE tenant_id='tenant' AND status='completed' AND completeness='complete' AND resulting_graph_version>prior_graph_version`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("completed run metadata not persisted: %d", runs)
	}
	if _, err = db.Exec(`UPDATE assurance_relationship_graph_state SET graph_version=99 WHERE tenant_id='tenant'`); err != nil {
		t.Fatalf("optimistic graph version state missing: %v", err)
	}
}

func TestAllowlistFailsClosedForRootEdgeAndTenant(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	store, _ := NewStore(db)
	_, err := store.Refresh(ctx, "tenant-a", Scope{ResourceIDs: []string{"root"}}, Discovery{Complete: true, EndOfScope: true, Nodes: []Node{{ResourceID: "root", Kind: "spreadsheet", ExternalID: "root"}, {ResourceID: "secret", Kind: "spreadsheet", ExternalID: "secret"}}, Edges: []Edge{{ID: "edge", From: "root", To: "secret", Relation: "derived_from", Provenance: "observed"}}}, "operator", "audit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Traverse(ctx, "tenant-a", "actor", "root", "", "both", 3, 100, 100); err == nil {
		t.Fatal("unallowlisted root was returned")
	}
	activateRelationshipAllowlist(t, db, "tenant-a", "actor", "root")
	got, err := store.Traverse(ctx, "tenant-a", "actor", "root", "", "both", 3, 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Nodes) != 1 || len(got.Edges) != 0 || len(got.Omitted) != 1 || got.Omitted[0] != "secret:denied" {
		t.Fatalf("denied edge endpoint leaked or branch not marked: %#v", got)
	}
	if _, err = store.Traverse(ctx, "tenant-b", "actor", "root", "", "both", 3, 100, 100); err == nil {
		t.Fatal("cross-tenant graph access succeeded")
	}
}

func TestRelationshipGraphNeverGrantsAndAllowlistIsActorScoped(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	store, _ := NewStore(db)
	_, err := store.Refresh(ctx, "tenant-a", Scope{ResourceIDs: []string{"r"}}, Discovery{Complete: true, EndOfScope: true, Nodes: []Node{{ResourceID: "r", Kind: "spreadsheet", ExternalID: "r"}}}, "operator", "audit")
	if err != nil {
		t.Fatal(err)
	}
	activateRelationshipAllowlist(t, db, "tenant-a", "actor-a", "r")
	if _, err = store.Traverse(ctx, "tenant-a", "actor-b", "r", "", "both", 1, 10, 10); err == nil {
		t.Fatal("another actor inherited an allowlist")
	}
}

func TestTraverseFailsWhenSignedBundleStateIsNotReady(t *testing.T) {
	db := testDB(t)
	store, err := NewStore(db, func() error { return assurance.ErrNotReady })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Traverse(context.Background(), "tenant", "actor", "root", "", "both", 1, 10, 10); !errors.Is(err, assurance.ErrNotReady) {
		t.Fatalf("Traverse readiness error = %v, want ErrNotReady", err)
	}
}

func TestTraversePropagatesResourceLookupDatabaseErrorsAtDepthZero(t *testing.T) {
	db := testDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	activateRelationshipAllowlist(t, db, "tenant", "actor", "root")
	if _, err := db.Exec(`DROP TABLE assurance_resources`); err != nil {
		t.Fatal(err)
	}
	got, err := store.Traverse(context.Background(), "tenant", "actor", "root", "", "both", 0, 10, 10)
	if err == nil {
		t.Fatal("Traverse swallowed the missing assurance_resources table error")
	}
	if len(got.Nodes) != 0 || len(got.Edges) != 0 || len(got.Cycles) != 0 || len(got.Omitted) != 0 {
		t.Fatalf("database error exposed partial graph: %#v", got)
	}
}

func TestTraverseAllowsMissingResourceRow(t *testing.T) {
	db := testDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	activateRelationshipAllowlist(t, db, "tenant", "actor", "root")
	if _, err := db.Exec(`DELETE FROM assurance_resources WHERE tenant_id=? AND resource_id=?`, "tenant", "root"); err != nil {
		t.Fatal(err)
	}
	got, err := store.Traverse(context.Background(), "tenant", "actor", "root", "", "both", 0, 10, 10)
	if err != nil {
		t.Fatalf("missing resource row should remain an empty graph result: %v", err)
	}
	if len(got.Nodes) != 0 || len(got.Edges) != 0 {
		t.Fatalf("missing resource row unexpectedly produced graph data: %#v", got)
	}
}

func TestLegacyMutableAllowlistRowsNeverGrantRelationshipAccess(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	store, _ := NewStore(db)
	if _, err := store.Refresh(ctx, "tenant", Scope{ResourceIDs: []string{"root"}}, Discovery{Complete: true, EndOfScope: true, Nodes: []Node{{ResourceID: "root", Kind: "spreadsheet", ExternalID: "root"}}}, "operator", "audit"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_relationship_resource_allowlist
 (tenant_id,actor_id,capability,resource_id,status,policy_version,provisioned_by,provisioned_at)
 VALUES ('tenant','actor','assurance.relationship.read','root','active','legacy','legacy-operator','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Traverse(ctx, "tenant", "actor", "root", "", "both", 1, 10, 10); err == nil {
		t.Fatal("unsigned mutable v15 allowlist row granted relationship access")
	}
}

func TestScopeCanonicalization(t *testing.T) {
	a, _ := CanonicalScope(Scope{ResourceIDs: []string{"b", "a"}})
	b, _ := CanonicalScope(Scope{ResourceIDs: []string{"a", "b"}})
	if a.Digest != b.Digest || a.JSON != b.JSON {
		t.Fatalf("scope is not canonical: %#v %#v", a, b)
	}
}
