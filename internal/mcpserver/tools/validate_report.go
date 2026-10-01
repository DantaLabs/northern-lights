package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
)

type validateReportTool struct{}

func ValidateReport() mcpserver.Tool    { return validateReportTool{} }
func (validateReportTool) Name() string { return "workiva_validate_report" }
func (validateReportTool) Description() string {
	return "Deterministically validates one immutable server-owned report snapshot using its approved closed rule set. The provider is never called."
}

type validateReportInput struct {
	SnapshotID     string   `json:"snapshot_id"`
	RuleSetID      string   `json:"rule_set_id"`
	RuleIDs        []string `json:"rule_ids,omitempty"`
	FailOnWarning  bool     `json:"fail_on_warning"`
	RetentionClass string   `json:"retention_class,omitempty"`
	IdempotencyKey string   `json:"idempotency_key"`
}

func (validateReportTool) RegisterSDK(server *mcp.Server, deps mcpserver.Deps) {
	mcp.AddTool(server, &mcp.Tool{Name: "workiva_validate_report", Description: validateReportTool{}.Description(), InputSchema: validateReportInputSchema(), OutputSchema: validateReportOutputSchema()}, func(ctx context.Context, request *mcp.CallToolRequest, input validateReportInput) (*mcp.CallToolResult, assurance.ValidationResponse, error) {
		if deps.Assurance == nil {
			err := failMsgWithContext(ctx, "assurance store is not available", "enable and provision Phase 3 assurance")
			return structuredToolResult(ctx, err, ""), validationErrorResponse(ctx, err), nil
		}
		if deps.Cfg != nil && !deps.Cfg.AssuranceEnabled {
			err := failMsgWithContext(ctx, "assurance validation feature is disabled", "set NL_ASSURANCE_ENABLED=true and provision a signed bundle")
			return structuredToolResult(ctx, err, ""), validationErrorResponse(ctx, err), nil
		}
		if _, trusted := identityForTool(ctx); !trusted && (deps.Cfg == nil || !deps.Cfg.AssuranceLegacyAPIKeyProfile) {
			if !trusted {
				err := &assurance.Error{Code: "strong_identity_required", Message: "trusted assurance identity is required"}
				return structuredToolResult(ctx, err, ""), validationErrorResponse(ctx, err), nil
			}
		}
		ctx = assurance.WithRequestID(ctx, mcpserver.RequestIDFromContext(ctx))
		response, err := (assurance.ValidationService{Store: deps.Assurance, Audit: deps.Audit}).Validate(ctx, mcpserver.ActorFromContextOrRequest(ctx, request, deps.ActorHeader), mcpserver.AuditIDFromContext(ctx), assurance.ValidationRequest{SnapshotID: input.SnapshotID, RuleSetID: input.RuleSetID, RuleIDs: input.RuleIDs, FailOnWarning: input.FailOnWarning, RetentionClass: input.RetentionClass, IdempotencyKey: input.IdempotencyKey})
		if err != nil {
			return structuredToolResult(ctx, err, "verify the immutable snapshot, approved rule set, and legacy/demo or Entra authorization"), validationErrorResponse(ctx, err), nil
		}
		return nil, response, nil
	})
}

func validationErrorResponse(ctx context.Context, err error) assurance.ValidationResponse {
	_, status := structuredFailure(err, mcpserver.AuditIDFromContext(ctx))
	return assurance.ValidationResponse{
		NLAuditID: mcpserver.AuditIDFromContext(ctx), Status: status,
		ValidationRunID: "not_created", SnapshotID: "not_created", RuleSetID: "not_created", RuleSetRevision: 1,
		Counts:  map[string]int{"pass": 0, "fail": 0, "warn": 0, "not_evaluable": 0, "error": 0},
		Results: []assurance.ValidationResult{},
	}
}

func validateReportInputSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"snapshot_id": boundedSchema("immutable snapshot ID", 128), "rule_set_id": boundedSchema("server-owned rule set ID", 128), "rule_ids": map[string]any{"type": "array", "maxItems": 1000, "uniqueItems": true, "items": boundedSchema("approved rule ID", 128)}, "fail_on_warning": map[string]any{"type": "boolean"}, "retention_class": map[string]any{"type": "string", "maxLength": 64}, "idempotency_key": boundedSchema("single-use idempotency key", 256)}, "required": []string{"snapshot_id", "rule_set_id", "idempotency_key"}}
}
func validateReportOutputSchema() map[string]any {
	result := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"result_id": boundedSchema("result ID", 128), "rule_id": boundedSchema("rule ID", 128), "rule_revision": boundedInteger(1, 1000000), "code": boundedEnum("required", "type", "unit", "range", "allowed_values", "reconciliation", "variance", "completeness"), "status": boundedEnum("pass", "fail", "warn", "not_evaluable", "error"), "actual": typedValueSchema(), "expected": typedValueSchema(), "absolute_tolerance": map[string]any{"type": "string", "maxLength": 256}, "relative_tolerance": map[string]any{"type": "string", "maxLength": 256}, "field_ids": boundedArray("field ID", 1000), "evidence_observation_ids": boundedArray("observation ID", 1000)}}
	result["required"] = []string{"result_id", "rule_id", "rule_revision", "code", "status", "field_ids", "evidence_observation_ids"}
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"nl_audit_id": boundedSchema("audit ID", 128), "status": boundedEnum("passed", "failed", "error", "not_evaluable", "idempotency_replay", "denied", "idempotency_in_progress", "idempotency_conflict"), "error": structuredErrorSchema(), "validation_run_id": boundedSchema("validation run ID", 128), "snapshot_id": boundedSchema("snapshot ID", 128), "rule_set_id": boundedSchema("rule set ID", 128), "rule_set_revision": boundedInteger(1, 1000000), "counts": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"pass": boundedInteger(0, 1000), "fail": boundedInteger(0, 1000), "warn": boundedInteger(0, 1000), "not_evaluable": boundedInteger(0, 1000), "error": boundedInteger(0, 1000)}, "required": []string{"pass", "fail", "warn", "not_evaluable", "error"}}, "results": map[string]any{"type": "array", "maxItems": 1000, "items": result}}, "required": []string{"nl_audit_id", "status"}}
}

func boundedSchema(description string, max int) map[string]any {
	return map[string]any{"type": "string", "description": description, "minLength": 1, "maxLength": max}
}
func boundedArray(description string, max int) map[string]any {
	return map[string]any{"type": "array", "maxItems": max, "items": boundedSchema(description, 128)}
}
func typedValueSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"kind": boundedEnum("blank", "text", "integer", "number", "boolean", "date", "datetime", "currency", "percent", "error"), "text": map[string]any{"type": "string", "maxLength": 10000}, "number": map[string]any{"type": "string", "maxLength": 256}, "boolean": map[string]any{"type": "boolean"}, "date": map[string]any{"type": "string", "maxLength": 10}, "datetime": map[string]any{"type": "string", "maxLength": 64}, "unit": map[string]any{"type": "string", "maxLength": 128}, "scale": map[string]any{"type": "string", "maxLength": 64}, "formula": map[string]any{"type": "boolean"}}, "required": []string{"kind", "formula"}}
}

// identityForTool is intentionally tiny: middleware owns permission checks;
// this helper only distinguishes trusted Entra from API-key compatibility.
func identityForTool(ctx context.Context) (string, bool) {
	if p, ok := identity.PrincipalFromContext(ctx); ok && p.TenantID != "" && p.ObjectID != "" {
		return p.AuditActor(), true
	}
	return "", false
}
