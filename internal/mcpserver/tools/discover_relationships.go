package tools

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/relationships"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type discoverRelationshipsTool struct{}

func DiscoverRelationships() mcpserver.Tool    { return discoverRelationshipsTool{} }
func (discoverRelationshipsTool) Name() string { return "workiva_discover_relationships" }
func (discoverRelationshipsTool) Description() string {
	return "Read-only discovery or bounded lineage over the tenant-scoped persisted relationship graph. Provider discovery/refresh is not enabled unless a verified exact-scope provider capability is configured."
}

type relationshipInput struct {
	Operation string `json:"operation"`
	Root      *struct {
		ResourceID string `json:"resource_id"`
		Locator    string `json:"locator,omitempty"`
	} `json:"root,omitempty"`
	ResourceIDs      []string `json:"resource_ids,omitempty"`
	Direction        string   `json:"direction,omitempty"`
	MaxDepth         *int     `json:"max_depth,omitempty"`
	MaxNodes         *int     `json:"max_nodes,omitempty"`
	MaxEdges         *int     `json:"max_edges,omitempty"`
	IncludeDocuments bool     `json:"include_documents,omitempty"`
	Refresh          bool     `json:"refresh,omitempty"`
	IdempotencyKey   string   `json:"idempotency_key"`
}

func relationshipInputSchema() map[string]any {
	return decodeRelationshipSchema(relationshipInputSchemaJSON)
}
func relationshipOutputSchema() map[string]any {
	return decodeRelationshipSchema(relationshipOutputSchemaJSON)
}
func decodeRelationshipSchema(raw string) map[string]any {
	var schema map[string]any
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		panic(err)
	}
	return schema
}

const relationshipInputSchemaJSON = `{"type":"object","additionalProperties":false,"properties":{"operation":{"type":"string","minLength":1,"maxLength":16,"description":"Required server vocabulary: discover or lineage. Conditional rules are server-enforced."},"root":{"type":"object","additionalProperties":false,"properties":{"resource_id":{"type":"string","minLength":1,"maxLength":128},"locator":{"type":"string","minLength":1,"maxLength":512}},"required":["resource_id"]},"resource_ids":{"type":"array","minItems":1,"maxItems":1000,"items":{"type":"string","minLength":1,"maxLength":128}},"direction":{"type":"string","minLength":1,"maxLength":16,"description":"Lineage vocabulary: upstream, downstream, or both. Default both. Ignored for discover."},"max_depth":{"type":"integer","minimum":0,"maximum":8,"description":"Lineage traversal depth. Default 3; absolute server cap 8."},"max_nodes":{"type":"integer","minimum":1,"maximum":1000,"description":"Traversal/node response bound. Default and absolute server cap 1000."},"max_edges":{"type":"integer","minimum":1,"maximum":5000,"description":"Traversal/edge response bound. Default and absolute server cap 5000."},"include_documents":{"type":"boolean","description":"Default false; only an approved read-only document capability may honor true."},"refresh":{"type":"boolean","description":"Default false. False is explicitly read-only; true requests a complete exact-scope refresh and requires the refresh capability."},"idempotency_key":{"type":"string","minLength":1,"maxLength":256,"description":"Required after authentication; scoped to trusted tenant, actor, tool, and operation."}},"required":["operation","idempotency_key"]}`
const relationshipOutputSchemaJSON = `{"type":"object","additionalProperties":false,"properties":{"nl_audit_id":{"type":"string","minLength":1,"maxLength":128},"operation":{"type":"string","minLength":1,"maxLength":16,"description":"Echoes discover or lineage."},"status":{"type":"string","minLength":1,"maxLength":64,"description":"completed, partial, unsupported, spike_required, denied, error, idempotency_in_progress, idempotency_replay, or idempotency_conflict."},"completeness":{"type":"string","minLength":1,"maxLength":32,"description":"complete, incomplete, or not_created."},"discovery_run_id":{"type":"string","minLength":1,"maxLength":128},"graph_version":{"type":"string","minLength":1,"maxLength":128},"scope_digest":{"type":"string","minLength":1,"maxLength":128},"provider_end_of_scope":{"type":"boolean"},"nodes":{"type":"array","maxItems":1000,"items":{"type":"object","additionalProperties":false,"properties":{"resource_id":{"type":"string","minLength":1,"maxLength":128},"kind":{"type":"string","minLength":1,"maxLength":64},"external_id":{"type":"string","minLength":1,"maxLength":512},"locator":{"type":"string","minLength":1,"maxLength":512},"name":{"type":"string","maxLength":512},"provider":{"type":"string","minLength":1,"maxLength":128},"provider_revision":{"type":"string","maxLength":256},"provenance":{"type":"string","minLength":1,"maxLength":32,"description":"observed, provider_declared, operator, or inferred."},"confidence":{"type":"string","minLength":1,"maxLength":16,"description":"high, medium, low, or unavailable."}},"required":["resource_id","kind","external_id","provenance","confidence"]}},"edges":{"type":"array","maxItems":5000,"items":{"type":"object","additionalProperties":false,"properties":{"edge_id":{"type":"string","minLength":1,"maxLength":128},"from_resource_id":{"type":"string","minLength":1,"maxLength":128},"to_resource_id":{"type":"string","minLength":1,"maxLength":128},"relation":{"type":"string","minLength":1,"maxLength":64},"provenance":{"type":"string","minLength":1,"maxLength":32},"confidence":{"type":"string","minLength":1,"maxLength":16},"evidence_reference_ids":{"type":"array","maxItems":100,"items":{"type":"string","minLength":1,"maxLength":128}},"first_seen":{"type":"string","minLength":1,"maxLength":64},"last_seen":{"type":"string","minLength":1,"maxLength":64}},"required":["edge_id","from_resource_id","to_resource_id","relation","provenance","confidence"]}},"omitted_branches":{"type":"array","maxItems":1000,"items":{"type":"object","additionalProperties":false,"properties":{"resource_id":{"type":"string","minLength":1,"maxLength":128},"reason":{"type":"string","minLength":1,"maxLength":256},"cap_reached":{"type":"boolean"}},"required":["reason"]}},"cycle_markers":{"type":"array","maxItems":1000,"items":{"type":"object","additionalProperties":false,"properties":{"resource_id":{"type":"string","minLength":1,"maxLength":128},"path_digest":{"type":"string","minLength":1,"maxLength":128}},"required":["path_digest"]}},"ignored_fields":{"type":"array","maxItems":32,"items":{"type":"string","minLength":1,"maxLength":128}},"errors":{"type":"array","maxItems":32,"items":{"type":"object","additionalProperties":false,"properties":{"code":{"type":"string","minLength":1,"maxLength":128},"message":{"type":"string","minLength":1,"maxLength":2048},"retryable":{"type":"boolean"},"field":{"type":"string","maxLength":128}},"required":["code","message","retryable"]}},"error":{"type":"object","additionalProperties":false,"properties":{"code":{"type":"string","minLength":1,"maxLength":128},"message":{"type":"string","minLength":1,"maxLength":2048},"retryable":{"type":"boolean"},"field":{"type":"string","maxLength":128}},"required":["code","message","retryable"]},"retry_after_ms":{"type":"integer","minimum":0,"maximum":60000},"reconciliation_required":{"type":"boolean"},"next_action":{"type":"string","maxLength":256}},"required":["nl_audit_id","operation","status","completeness","nodes","edges","omitted_branches","cycle_markers","ignored_fields","errors","reconciliation_required"]}`

func ignoredRelationshipFields(in relationshipInput, fields ...string) []any {
	ignored := make([]any, 0, len(fields))
	for _, field := range fields {
		switch field {
		case "direction":
			if in.Direction != "" {
				ignored = append(ignored, field)
			}
		case "max_depth":
			if in.MaxDepth != nil {
				ignored = append(ignored, field)
			}
		case "max_nodes":
			if in.MaxNodes != nil {
				ignored = append(ignored, field)
			}
		case "max_edges":
			if in.MaxEdges != nil {
				ignored = append(ignored, field)
			}
		}
	}
	return ignored
}

func (discoverRelationshipsTool) RegisterSDK(s *mcp.Server, d mcpserver.Deps) {
	mcp.AddTool(s, &mcp.Tool{Name: "workiva_discover_relationships", Description: (discoverRelationshipsTool{}).Description(), InputSchema: relationshipInputSchema(), OutputSchema: relationshipOutputSchema()}, func(ctx context.Context, req *mcp.CallToolRequest, in relationshipInput) (*mcp.CallToolResult, map[string]any, error) {
		out := map[string]any{"nl_audit_id": mcpserver.AuditIDFromContext(ctx), "operation": in.Operation, "status": "error", "completeness": "not_created", "nodes": []any{}, "edges": []any{}, "omitted_branches": []any{}, "cycle_markers": []any{}, "ignored_fields": []any{}, "errors": []any{}, "reconciliation_required": false}
		fail := func(code, msg, field string) (*mcp.CallToolResult, map[string]any, error) {
			if out["status"] != "denied" && out["status"] != "spike_required" {
				out["status"] = "error"
			}
			entry := map[string]any{"code": code, "message": msg, "retryable": false}
			if field != "" {
				entry["field"] = field
			}
			out["errors"] = []any{entry}
			out["error"] = entry
			return nil, out, nil
		}
		p, ok := identity.PrincipalFromContext(ctx)
		if !ok || !p.HasPermission(identity.PermissionAssuranceRelationshipRead) {
			out["status"] = "denied"
			return fail("permission_denied", "trusted relationship read authorization required", "")
		}
		if in.Refresh && !p.HasPermission(identity.PermissionAssuranceRelationshipRefresh) {
			out["status"] = "denied"
			return fail("permission_denied", "trusted relationship refresh authorization required", "refresh")
		}
		if d.Relationships == nil {
			out["status"] = "spike_required"
			return fail("spike_required", "relationship graph store unavailable", "")
		}
		if in.Refresh {
			out["status"] = "spike_required"
			return fail("spike_required", "trusted Entra provider refresh capability is not configured", "refresh")
		}
		if in.IncludeDocuments {
			out["status"] = "spike_required"
			return fail("spike_required", "document capability has not passed its provider spike", "include_documents")
		}
		if in.MaxDepth != nil && (*in.MaxDepth < 0 || *in.MaxDepth > 8) {
			return fail("invalid_bounds", "max_depth must be between 0 and 8", "max_depth")
		}
		if in.MaxNodes != nil && (*in.MaxNodes < 1 || *in.MaxNodes > 1000) {
			return fail("invalid_bounds", "max_nodes must be between 1 and 1000", "max_nodes")
		}
		if in.MaxEdges != nil && (*in.MaxEdges < 1 || *in.MaxEdges > 5000) {
			return fail("invalid_bounds", "max_edges must be between 1 and 5000", "max_edges")
		}
		if in.Operation == "discover" {
			out["ignored_fields"] = ignoredRelationshipFields(in, "direction", "max_depth")
			if in.Root != nil || len(in.ResourceIDs) == 0 {
				return fail("invalid_selector", "discover requires resource_ids and forbids root", "resource_ids")
			}
			resourceIDs := append([]string(nil), in.ResourceIDs...)
			sort.Strings(resourceIDs)
			canonicalScope, err := relationships.CanonicalScope(relationships.Scope{ResourceIDs: resourceIDs})
			if err != nil {
				out["status"] = "denied"
				return fail("resource_denied", "requested resources are not in active signed report definitions and actor allowlist", "resource_ids")
			}
			out["scope_digest"] = canonicalScope.Digest
			if in.Refresh {
				out["status"] = "spike_required"
				return fail("spike_required", "refresh mutation is deferred; Wave 3 discover is read-only", "refresh")
			}
			if d.Assurance == nil {
				return fail("assurance_unavailable", "signed relationship authorization is unavailable", "")
			}
			approved, err := d.Assurance.ApprovedRelationshipResources(ctx, p.TenantID, p.ObjectID, resourceIDs)
			if err != nil {
				out["status"] = "denied"
				return fail("resource_denied", "requested resources are not in active signed report definitions and actor allowlist", "resource_ids")
			}
			nodes := make([]relationships.Node, 0, len(approved))
			maxNodes := len(approved)
			if in.MaxNodes != nil && *in.MaxNodes < maxNodes {
				maxNodes = *in.MaxNodes
			}
			for _, n := range approved[:maxNodes] {
				nodes = append(nodes, relationships.Node{ResourceID: n.ResourceID, Kind: n.Kind, ExternalID: n.ExternalID, Provenance: n.Provenance, Confidence: n.Confidence})
			}
			if maxNodes < len(approved) {
				for _, n := range approved[maxNodes:] {
					out["omitted_branches"] = append(out["omitted_branches"].([]any), map[string]any{"resource_id": n.ResourceID, "reason": "max_nodes_reached", "cap_reached": true})
				}
				out["status"] = "partial"
				out["completeness"] = "incomplete"
			} else {
				out["status"] = "completed"
				out["completeness"] = "complete"
			}
			out["nodes"] = nodes
			out["edges"] = []relationships.Edge{}
			return nil, out, nil
		}
		if in.Operation != "lineage" || in.Root == nil || in.Root.ResourceID == "" || len(in.ResourceIDs) > 0 {
			return fail("invalid_selector", "lineage requires root.resource_id and forbids resource_ids", "operation")
		}
		direction := in.Direction
		if direction == "" {
			direction = "both"
		}
		depth, nodes, edges := 3, 1000, 5000
		if in.MaxDepth != nil {
			depth = *in.MaxDepth
		}
		if in.MaxNodes != nil {
			nodes = *in.MaxNodes
		}
		if in.MaxEdges != nil {
			edges = *in.MaxEdges
		}
		result, err := d.Relationships.Traverse(ctx, p.TenantID, p.ObjectID, in.Root.ResourceID, in.Root.Locator, direction, depth, nodes, edges)
		if err != nil {
			if strings.Contains(err.Error(), "denied by resource allowlist") {
				out["status"] = "denied"
				return fail("resource_denied", "lineage root is not allowlisted for this actor", "root")
			}
			return fail("invalid_scope", err.Error(), "root")
		}
		out["status"] = "completed"
		out["completeness"] = "complete"
		if result.GraphVersion != "" {
			out["graph_version"] = result.GraphVersion
		}
		for i := range result.Nodes {
			if result.Nodes[i].Confidence == "" {
				result.Nodes[i].Confidence = "unavailable"
			}
		}
		for i := range result.Edges {
			if result.Edges[i].Confidence == "" {
				result.Edges[i].Confidence = "unavailable"
			}
		}
		out["nodes"] = result.Nodes
		out["edges"] = result.Edges
		cycles := []any{}
		for _, id := range result.Cycles {
			cycles = append(cycles, map[string]any{"resource_id": id, "path_digest": id})
		}
		out["cycle_markers"] = cycles
		for _, id := range result.Omitted {
			reason, resourceID, capReached := "traversal_bound_reached", id, true
			if strings.HasSuffix(id, ":denied") {
				reason, resourceID, capReached = "resource_not_allowlisted", strings.TrimSuffix(id, ":denied"), false
			}
			out["omitted_branches"] = append(out["omitted_branches"].([]any), map[string]any{"resource_id": resourceID, "reason": reason, "cap_reached": capReached})
		}
		if len(result.Omitted) > 0 {
			out["status"] = "partial"
			out["completeness"] = "incomplete"
		}
		return nil, out, nil
	})
}
