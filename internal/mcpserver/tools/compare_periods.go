package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
)

type comparePeriodsTool struct{}

func ComparePeriods() mcpserver.Tool    { return comparePeriodsTool{} }
func (comparePeriodsTool) Name() string { return "workiva_compare_periods" }
func (comparePeriodsTool) Description() string {
	return "Compares two immutable snapshots by stable server-owned field IDs using the selected server-owned materiality policy."
}

type comparePeriodsInput struct {
	CurrentSnapshotID   string   `json:"current_snapshot_id"`
	PriorSnapshotID     string   `json:"prior_snapshot_id"`
	MaterialityPolicyID string   `json:"materiality_policy_id"`
	FieldIDs            []string `json:"field_ids,omitempty"`
	IncludeUnchanged    bool     `json:"include_unchanged"`
	RetentionClass      string   `json:"retention_class,omitempty"`
	IdempotencyKey      string   `json:"idempotency_key"`
}

func (comparePeriodsTool) RegisterSDK(server *mcp.Server, deps mcpserver.Deps) {
	mcp.AddTool(server, &mcp.Tool{Name: "workiva_compare_periods", Description: comparePeriodsTool{}.Description(), InputSchema: comparePeriodsInputSchema(), OutputSchema: comparePeriodsOutputSchema()}, func(ctx context.Context, request *mcp.CallToolRequest, input comparePeriodsInput) (*mcp.CallToolResult, assurance.ComparisonResponse, error) {
		if deps.Assurance == nil {
			return nil, assurance.ComparisonResponse{}, failMsgWithContext(ctx, "assurance store is not available", "enable and provision Phase 3 assurance")
		}
		if deps.Cfg != nil && !deps.Cfg.AssuranceEnabled {
			return nil, assurance.ComparisonResponse{}, failMsgWithContext(ctx, "assurance comparison feature is disabled", "set NL_ASSURANCE_ENABLED=true and provision a signed bundle")
		}
		if _, trusted := identityForTool(ctx); !trusted && (deps.Cfg == nil || !deps.Cfg.AssuranceLegacyAPIKeyProfile) {
			if !trusted {
				return nil, assurance.ComparisonResponse{}, failMsgWithContext(ctx, "trusted assurance identity is required", "use the explicitly labeled legacy/demo assurance profile or Entra authentication")
			}
		}
		ctx = assurance.WithRequestID(ctx, mcpserver.RequestIDFromContext(ctx))
		response, err := (assurance.CompareService{Store: deps.Assurance, Audit: deps.Audit}).Compare(ctx, mcpserver.ActorFromContextOrRequest(ctx, request, deps.ActorHeader), mcpserver.AuditIDFromContext(ctx), assurance.CompareRequest{CurrentSnapshotID: input.CurrentSnapshotID, PriorSnapshotID: input.PriorSnapshotID, MaterialityPolicyID: input.MaterialityPolicyID, FieldIDs: input.FieldIDs, IncludeUnchanged: input.IncludeUnchanged, RetentionClass: input.RetentionClass, IdempotencyKey: input.IdempotencyKey})
		if err != nil {
			return nil, assurance.ComparisonResponse{}, failWithContext(ctx, err, "verify the immutable snapshots, server-owned materiality policy, and authorization")
		}
		return nil, response, nil
	})
}

func comparePeriodsInputSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"current_snapshot_id": boundedSchema("current immutable snapshot ID", 128), "prior_snapshot_id": boundedSchema("prior immutable snapshot ID", 128), "materiality_policy_id": boundedSchema("server-owned materiality policy ID", 128), "field_ids": map[string]any{"type": "array", "maxItems": 1000, "uniqueItems": true, "items": boundedSchema("approved stable field ID", 128)}, "include_unchanged": map[string]any{"type": "boolean"}, "retention_class": map[string]any{"type": "string", "maxLength": 64}, "idempotency_key": boundedSchema("single-use idempotency key", 256)}, "required": []string{"current_snapshot_id", "prior_snapshot_id", "materiality_policy_id", "idempotency_key"}}
}
func comparePeriodsOutputSchema() map[string]any {
	item := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"comparison_item_id": boundedSchema("comparison item ID", 128), "field_id": boundedSchema("stable field ID", 128), "prior": typedValueSchema(), "current": typedValueSchema(), "absolute_delta": map[string]any{"type": "string", "maxLength": 256}, "percentage_delta": map[string]any{"type": "string", "maxLength": 256}, "comparison_status": map[string]any{"type": "string", "enum": []string{"unchanged", "changed", "added", "removed", "missing_in_current", "missing_in_prior", "not_comparable"}}, "materiality_status": map[string]any{"type": "string", "enum": []string{"material", "immaterial", "unassessed", "not_comparable"}}, "evidence_observation_ids": boundedArray("observation ID", 1000)}, "required": []string{"comparison_item_id", "field_id", "comparison_status", "materiality_status", "evidence_observation_ids"}}
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"nl_audit_id": boundedSchema("audit ID", 128), "status": map[string]any{"type": "string", "enum": []string{"completed", "incompatible", "error", "idempotency_replay"}}, "comparison_id": boundedSchema("comparison ID", 128), "current_snapshot_id": boundedSchema("current snapshot ID", 128), "prior_snapshot_id": boundedSchema("prior snapshot ID", 128), "completeness": map[string]any{"type": "string", "enum": []string{"complete", "incomplete", "not_evaluable"}}, "comparison_basis": map[string]any{"type": "string", "enum": []string{"definition_membership_union", "explicit_field_ids"}}, "materiality_policy_id": boundedSchema("materiality policy ID", 128), "materiality_revision": map[string]any{"type": "integer"}, "changes": map[string]any{"type": "array", "maxItems": 1000, "items": item}, "material_count": map[string]any{"type": "integer"}}, "required": []string{"nl_audit_id", "status", "comparison_id", "current_snapshot_id", "prior_snapshot_id", "completeness", "comparison_basis", "materiality_policy_id", "materiality_revision", "changes", "material_count"}}
}
