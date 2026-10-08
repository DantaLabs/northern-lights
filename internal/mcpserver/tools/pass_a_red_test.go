package tools

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dantalabs/northern-lights/internal/identity"
)

func TestPassADiscoverHonorsMaxNodes(t *testing.T) {
	fixture := newDiscoverBehaviorFixture(t)
	payload := fixture.call(t, fixture.client(t, "actor-a-token"), map[string]any{
		"operation": "discover", "resource_ids": []any{discoverResourceA, discoverResourceZ},
		"max_nodes": 1, "idempotency_key": "pass-a-max-nodes",
	})
	nodes, _ := payload["nodes"].([]any)
	if len(nodes) > 1 || payload["status"] != "partial" || payload["completeness"] != "incomplete" {
		t.Fatalf("discover did not truthfully honor max_nodes=1: %#v", payload)
	}
	if ignored, _ := payload["ignored_fields"].([]any); len(ignored) != 0 {
		t.Fatalf("bounded max_nodes was reported ignored: %#v", ignored)
	}
}

func TestPassARefreshRequiresRefreshCapabilityBeforeSpikeGate(t *testing.T) {
	fixture := newDiscoverBehaviorFixture(t)
	fixture.principals["actor-a-token"] = identity.Principal{
		TenantID: identity.LegacyTenantID, ObjectID: "actor-a",
		Permissions: []identity.Permission{identity.PermissionAssuranceRelationshipRead},
	}
	payload := fixture.call(t, fixture.client(t, "actor-a-token"), map[string]any{
		"operation": "discover", "resource_ids": []any{discoverResourceA},
		"refresh": true, "idempotency_key": "pass-a-refresh-capability",
	})
	if payload["status"] != "denied" {
		t.Fatalf("read-only principal reached refresh spike gate: %#v", payload)
	}
}

func TestPassAHTTPRelationshipAuthorizationDenialConformsToOutput(t *testing.T) {
	fixture := newRawWave2Fixture(t)
	server, principals := startRawEntraServer(t, fixture)
	principals["relationship-denied"] = identity.Principal{
		TenantID: identity.LegacyTenantID, ObjectID: "denied",
		Permissions: []identity.Permission{identity.PermissionAssuranceSnapshot},
	}
	client := &rawMCPClient{t: t, url: server.URL + "/mcp", authToken: "relationship-denied"}
	initializeRaw(t, client)
	resp, body := client.post("tools/call", map[string]any{"name": "workiva_discover_relationships", "arguments": map[string]any{
		"operation": "lineage", "root": map[string]any{"resource_id": "resource-1"}, "idempotency_key": "pass-a-denied-shape",
	}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"operation", "completeness", "nodes", "edges", "omitted_branches", "cycle_markers", "ignored_fields", "errors", "reconciliation_required"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("authorization denial omits required relationship output field %q: %s", key, body)
		}
	}
}
