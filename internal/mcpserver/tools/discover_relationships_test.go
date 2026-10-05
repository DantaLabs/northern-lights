package tools

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/relationships"
)

const (
	discoverResourceA = "discover-resource-a"
	discoverResourceZ = "discover-resource-z"
)

type discoverBehaviorFixture struct {
	rawWave2Fixture
	graph      *relationships.Store
	serverURL  string
	principals map[string]identity.Principal
	schema     map[string]any
}

func newDiscoverBehaviorFixture(t *testing.T) discoverBehaviorFixture {
	t.Helper()
	fixture := newRawWave2Fixture(t)
	activateDiscoverBehaviorBundle(t, fixture.store)
	graph, err := relationships.NewStore(fixture.db)
	if err != nil {
		t.Fatal(err)
	}
	fixture.env.deps.Relationships = graph
	approved, err := fixture.store.ApprovedRelationshipResources(context.Background(), identity.LegacyTenantID, "actor-a", []string{discoverResourceA, discoverResourceZ})
	if err != nil || len(approved) != 2 {
		t.Fatalf("signed discover fixture is not active: approved=%#v err=%v", approved, err)
	}
	server, principals := startRawEntraServer(t, fixture)
	principals["actor-b-token"] = identity.Principal{
		TenantID:    identity.LegacyTenantID,
		ObjectID:    "actor-b",
		Permissions: identity.AllPermissions(),
	}
	principals["other-tenant-token"] = identity.Principal{
		TenantID:    "other-tenant",
		ObjectID:    "actor-a",
		Permissions: identity.AllPermissions(),
	}
	client := &rawMCPClient{t: t, url: server.URL + "/mcp", authToken: "actor-a-token"}
	initializeRaw(t, client)
	schema := rawToolSchemas(t, client)["workiva_discover_relationships"]
	if schema == nil {
		t.Fatal("workiva_discover_relationships output schema not advertised")
	}
	return discoverBehaviorFixture{
		rawWave2Fixture: fixture,
		graph:           graph,
		serverURL:       server.URL,
		principals:      principals,
		schema:          schema,
	}
}

func activateDiscoverBehaviorBundle(t *testing.T, store *assurance.Store) {
	t.Helper()
	const tenant = identity.LegacyTenantID
	bundle := assurance.Bundle{
		SchemaVersion: 1,
		BundleID:      "bundle-discover-behavior",
		BundleVersion: 2,
		TenantID:      tenant,
		Reports: []assurance.ReportRevision{{
			ReportID:           "discover-report",
			Revision:           1,
			Name:               "Discover behavior",
			Owner:              "operator",
			Status:             "active",
			RetentionClass:     "standard",
			ResourcePolicyHash: strings.Repeat("d", 64),
			Periods: []assurance.Period{{
				Key: "2026", Label: "2026", Start: "2026-01-01", End: "2026-12-31",
			}},
			Fields: []assurance.FieldDefinition{
				{FieldID: "discover-field-z", ResourceID: discoverResourceZ, ExternalResourceID: "spreadsheet-z", SubresourceID: "sheet-z", Locator: "Z1", Kind: assurance.ValueText, Required: true, Order: 1},
				{FieldID: "discover-field-a", ResourceID: discoverResourceA, ExternalResourceID: "spreadsheet-a", SubresourceID: "sheet-a", Locator: "A1", Kind: assurance.ValueText, Required: true, Order: 2},
			},
		}},
		RelationshipAllowlist: []assurance.RelationshipAllowlistEntry{
			{EntryID: "discover-actor-a-resource-a", Revision: 1, ActorID: "actor-a", Capability: assurance.RelationshipCapabilityRead, ResourceID: discoverResourceA},
			{EntryID: "discover-actor-a-resource-z", Revision: 1, ActorID: "actor-a", Capability: assurance.RelationshipCapabilityRead, ResourceID: discoverResourceZ},
		},
	}
	raw, err := assurance.CanonicalJSON(bundle)
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := assurance.ValidateBundle(raw, ed25519.Sign(private, raw), tenant, public)
	if err != nil {
		t.Fatalf("ValidateBundle: %v", err)
	}
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{
		TenantID: tenant, ObjectID: "fixture-admin", Permissions: identity.AllPermissions(),
	})
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

func (f discoverBehaviorFixture) client(t *testing.T, token string) *rawMCPClient {
	t.Helper()
	client := &rawMCPClient{t: t, url: f.serverURL + "/mcp", authToken: token}
	initializeRaw(t, client)
	return client
}

func (f discoverBehaviorFixture) call(t *testing.T, client *rawMCPClient, args map[string]any) map[string]any {
	t.Helper()
	result := rawCall(t, client, "workiva_discover_relationships", args)
	payload := assertRawPayload(t, "workiva_discover_relationships", f.schema, result, false)
	if calls := f.env.apiCalls.Load(); calls != 0 {
		t.Fatalf("discover emitted payload after %d provider calls, want zero: %#v", calls, payload)
	}
	return payload
}

func TestDiscoverRelationshipsSignedApprovedResourcesAreReadOnlyOperatorFacts(t *testing.T) {
	fixture := newDiscoverBehaviorFixture(t)
	ctx := context.Background()
	_, err := fixture.graph.Refresh(ctx, identity.LegacyTenantID,
		relationships.Scope{ResourceIDs: []string{discoverResourceA}},
		relationships.Discovery{
			Complete: true, EndOfScope: true,
			Nodes: []relationships.Node{
				{ResourceID: discoverResourceA, Kind: "spreadsheet", ExternalID: "spreadsheet-a", Provenance: "observed", Confidence: "high"},
				{ResourceID: discoverResourceZ, Kind: "spreadsheet", ExternalID: "spreadsheet-z", Provenance: "observed", Confidence: "high"},
			},
			Edges: []relationships.Edge{{ID: "stable-edge", From: discoverResourceA, To: discoverResourceZ, Relation: "derived_from", Provenance: "observed", Confidence: "high"}},
		}, "actor-a", "seed-audit")
	if err != nil {
		t.Fatal(err)
	}
	var beforeVersion string
	var beforeEdges, beforeRuns int
	if err := fixture.db.QueryRow(`SELECT graph_version FROM assurance_relationship_graph_state WHERE tenant_id=?`, identity.LegacyTenantID).Scan(&beforeVersion); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.QueryRow(`SELECT count(*) FROM assurance_relationship_edges WHERE tenant_id=? AND active=1`, identity.LegacyTenantID).Scan(&beforeEdges); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.QueryRow(`SELECT count(*) FROM assurance_relationship_discovery_runs WHERE tenant_id=?`, identity.LegacyTenantID).Scan(&beforeRuns); err != nil {
		t.Fatal(err)
	}

	payload := fixture.call(t, fixture.client(t, "actor-a-token"), map[string]any{
		"operation":       "discover",
		"resource_ids":    []any{discoverResourceA, discoverResourceZ},
		"refresh":         false,
		"idempotency_key": "discover-read-only",
	})
	if payload["status"] != "completed" || payload["completeness"] != "complete" {
		t.Fatalf("discover status = %#v", payload)
	}
	nodes, ok := payload["nodes"].([]any)
	if !ok || len(nodes) != 2 {
		t.Fatalf("discover nodes = %#v, want two", payload["nodes"])
	}
	for _, raw := range nodes {
		node := raw.(map[string]any)
		if node["provenance"] != "operator" || node["confidence"] != "high" {
			t.Fatalf("signed report node has untruthful provenance: %#v", node)
		}
		if _, present := node["provider"]; present {
			t.Fatalf("operator-derived node fabricated provider provenance: %#v", node)
		}
		if _, present := node["provider_revision"]; present {
			t.Fatalf("operator-derived node fabricated provider revision: %#v", node)
		}
	}
	edges, ok := payload["edges"].([]any)
	if !ok || len(edges) != 0 {
		t.Fatalf("read-only approved-resource discovery fabricated graph edges: %#v", payload["edges"])
	}

	var afterVersion string
	var afterEdges, afterRuns int
	if err := fixture.db.QueryRow(`SELECT graph_version FROM assurance_relationship_graph_state WHERE tenant_id=?`, identity.LegacyTenantID).Scan(&afterVersion); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.QueryRow(`SELECT count(*) FROM assurance_relationship_edges WHERE tenant_id=? AND active=1`, identity.LegacyTenantID).Scan(&afterEdges); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.QueryRow(`SELECT count(*) FROM assurance_relationship_discovery_runs WHERE tenant_id=?`, identity.LegacyTenantID).Scan(&afterRuns); err != nil {
		t.Fatal(err)
	}
	if afterVersion != beforeVersion || afterEdges != beforeEdges || afterRuns != beforeRuns {
		t.Fatalf("refresh=false mutated graph: version %s->%s active_edges %d->%d runs %d->%d", beforeVersion, afterVersion, beforeEdges, afterEdges, beforeRuns, afterRuns)
	}
}

func TestDiscoverRelationshipsCanonicalScopeSortsAndRejectsDuplicates(t *testing.T) {
	fixture := newDiscoverBehaviorFixture(t)
	client := fixture.client(t, "actor-a-token")
	unsorted := fixture.call(t, client, map[string]any{
		"operation":       "discover",
		"resource_ids":    []any{discoverResourceZ, discoverResourceA},
		"idempotency_key": "discover-unsorted",
	})
	sortedPayload := fixture.call(t, client, map[string]any{
		"operation":       "discover",
		"resource_ids":    []any{discoverResourceA, discoverResourceZ},
		"idempotency_key": "discover-sorted",
	})
	unsortedDigest, ok := unsorted["scope_digest"].(string)
	if !ok || unsortedDigest == "" || sortedPayload["scope_digest"] != unsortedDigest {
		t.Fatalf("equivalent resource scopes have different or missing digest: unsorted=%#v sorted=%#v", unsorted["scope_digest"], sortedPayload["scope_digest"])
	}
	for label, payload := range map[string]map[string]any{"unsorted": unsorted, "sorted": sortedPayload} {
		nodes := payload["nodes"].([]any)
		ids := make([]string, 0, len(nodes))
		for _, raw := range nodes {
			ids = append(ids, raw.(map[string]any)["resource_id"].(string))
		}
		if !sort.StringsAreSorted(ids) || !reflect.DeepEqual(ids, []string{discoverResourceA, discoverResourceZ}) {
			t.Fatalf("%s scope returned noncanonical node order: %v", label, ids)
		}
	}

	duplicate := fixture.call(t, client, map[string]any{
		"operation":       "discover",
		"resource_ids":    []any{discoverResourceA, discoverResourceA},
		"idempotency_key": "discover-duplicate",
	})
	if duplicate["status"] != "denied" || duplicate["completeness"] != "not_created" {
		t.Fatalf("duplicate canonical selector was not denied: %#v", duplicate)
	}
	assertGenericDiscoverDenial(t, duplicate)
}

func TestDiscoverRelationshipsDenialsDoNotLeakExistence(t *testing.T) {
	fixture := newDiscoverBehaviorFixture(t)
	cases := []struct {
		name       string
		token      string
		resourceID string
	}{
		{name: "unknown", token: "actor-a-token", resourceID: "does-not-exist"},
		{name: "unallowlisted", token: "actor-b-token", resourceID: discoverResourceA},
		{name: "other-tenant", token: "other-tenant-token", resourceID: discoverResourceA},
	}
	var baseline map[string]any
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := fixture.call(t, fixture.client(t, tc.token), map[string]any{
				"operation":       "discover",
				"resource_ids":    []any{tc.resourceID},
				"idempotency_key": "discover-denied-" + tc.name,
			})
			assertGenericDiscoverDenial(t, payload)
			normalized := clonePayload(t, payload)
			delete(normalized, "nl_audit_id")
			// scope_digest is a deterministic digest of the caller's selector. It
			// differs when the requested ID differs, but says nothing about whether
			// that ID exists. Compare the authorization result independently.
			delete(normalized, "scope_digest")
			if baseline == nil {
				baseline = normalized
				return
			}
			if !reflect.DeepEqual(normalized, baseline) {
				t.Fatalf("denial leaks selector class\n got: %#v\nwant: %#v", normalized, baseline)
			}
		})
	}
}

func TestDiscoverRelationshipsIncludeDocumentsRequiresSpike(t *testing.T) {
	fixture := newDiscoverBehaviorFixture(t)
	payload := fixture.call(t, fixture.client(t, "actor-a-token"), map[string]any{
		"operation":         "discover",
		"resource_ids":      []any{discoverResourceA},
		"include_documents": true,
		"refresh":           false,
		"idempotency_key":   "discover-documents-spike",
	})
	if payload["status"] != "spike_required" || payload["completeness"] != "not_created" {
		t.Fatalf("include_documents response = %#v", payload)
	}
	errorObject := payload["error"].(map[string]any)
	if errorObject["code"] != "spike_required" || errorObject["field"] != "include_documents" {
		t.Fatalf("include_documents error = %#v", errorObject)
	}
}

func assertGenericDiscoverDenial(t *testing.T, payload map[string]any) {
	t.Helper()
	if payload["status"] != "denied" || payload["completeness"] != "not_created" {
		t.Fatalf("discover denial status = %#v", payload)
	}
	if nodes, ok := payload["nodes"].([]any); !ok || len(nodes) != 0 {
		t.Fatalf("discover denial leaked nodes: %#v", payload["nodes"])
	}
	if edges, ok := payload["edges"].([]any); !ok || len(edges) != 0 {
		t.Fatalf("discover denial leaked edges: %#v", payload["edges"])
	}
	errorObject, ok := payload["error"].(map[string]any)
	if !ok || errorObject["code"] != "resource_denied" || errorObject["message"] != "requested resources are not in active signed report definitions and actor allowlist" || errorObject["field"] != "resource_ids" {
		t.Fatalf("discover denial is not generic: %#v", payload["error"])
	}
}

func clonePayload(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var cloned map[string]any
	if err := json.Unmarshal(raw, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}
