package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
)

type snapshotReportTool struct{}

// SnapshotReport returns the Wave 1 workiva_snapshot_report tool.
func SnapshotReport() mcpserver.Tool { return snapshotReportTool{} }

const snapshotReportDescription = `Captures an immutable, typed snapshot of an approved server-owned report revision and named period.

The server resolves all approved fields and reads Workiva directly without using the read cache. Empty field_ids selects all required fields. allow_partial defaults to false. consistency defaults to best_effort; revision_pinned fails unless the active provider proves that capability. The request never supplies authoritative report values.`

func (snapshotReportTool) Name() string        { return "workiva_snapshot_report" }
func (snapshotReportTool) Description() string { return snapshotReportDescription }

type snapshotPeriodInput struct {
	Key   string `json:"key" jsonschema:"approved period key,maximum=128"`
	Label string `json:"label,omitempty" jsonschema:"optional approved label,maximum=256"`
	Start string `json:"start,omitempty" jsonschema:"optional approved ISO date,maximum=10"`
	End   string `json:"end,omitempty" jsonschema:"optional approved ISO date,maximum=10"`
}

type snapshotReportInput struct {
	ReportID             string              `json:"report_id" jsonschema:"approved server-owned report ID,maximum=128"`
	Period               snapshotPeriodInput `json:"period" jsonschema:"exact approved named period"`
	FieldIDs             []string            `json:"field_ids,omitempty" jsonschema:"optional approved field subset,maxItems=1000"`
	AllowPartial         bool                `json:"allow_partial,omitempty" jsonschema:"seal an explicitly incomplete partial snapshot when a source fails"`
	IncludeRelationships bool                `json:"include_relationships,omitempty" jsonschema:"include already approved relationship count"`
	Consistency          string              `json:"consistency,omitempty" jsonschema:"none, best_effort, or revision_pinned"`
	RetentionClass       string              `json:"retention_class,omitempty" jsonschema:"server-approved retention class,maximum=64"`
	IdempotencyKey       string              `json:"idempotency_key" jsonschema:"single-use client idempotency key,maximum=256"`
}

func snapshotReportInputSchema() map[string]any {
	boundedString := func(description string, max int) map[string]any {
		return map[string]any{"type": "string", "description": description, "minLength": 1, "maxLength": max}
	}
	optionalString := func(description string, max int) map[string]any {
		return map[string]any{"type": "string", "description": description, "maxLength": max}
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"report_id": boundedString("approved server-owned report ID", 128),
			"period": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"key":   boundedString("approved period key", 128),
					"label": optionalString("optional approved label", 256),
					"start": optionalString("optional approved ISO date", 10),
					"end":   optionalString("optional approved ISO date", 10),
				},
				"required": []string{"key"},
			},
			"field_ids":             map[string]any{"type": "array", "maxItems": 1000, "uniqueItems": true, "items": boundedString("approved stable field ID", 128)},
			"allow_partial":         map[string]any{"type": "boolean"},
			"include_relationships": map[string]any{"type": "boolean"},
			"consistency":           boundedEnum("none", "best_effort", "revision_pinned"),
			"retention_class":       optionalString("server-approved retention class", 64),
			"idempotency_key":       boundedString("single-use client idempotency key", 256),
		},
		"required":             []string{"report_id", "period", "idempotency_key"},
		"additionalProperties": false,
	}
}

func snapshotReportOutputSchema() map[string]any {
	stringProperty := func(max int) map[string]any { return map[string]any{"type": "string", "maxLength": max} }
	typedValue := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"kind": boundedEnum("blank", "text", "integer", "number", "boolean", "date", "datetime", "currency", "percent", "error"),
			"text": stringProperty(10000), "number": stringProperty(256), "boolean": map[string]any{"type": "boolean"},
			"date": stringProperty(10), "datetime": stringProperty(64), "unit": stringProperty(64), "scale": stringProperty(32),
			"precision": map[string]any{"type": "integer"}, "percent_basis": stringProperty(16), "timezone": stringProperty(64),
			"formula": map[string]any{"type": "boolean"}, "formula_text": stringProperty(10000), "calculated": map[string]any{"type": "boolean"},
			"calculated_source": stringProperty(64), "error_code": stringProperty(128),
		},
		"required": []string{"kind", "formula"},
	}
	observation := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"observation_id": stringProperty(128), "field_id": stringProperty(128), "resource_id": stringProperty(128),
			"external_resource_id": stringProperty(512), "locator": stringProperty(512), "typed_value": typedValue,
			"provider_revision": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
				"value": stringProperty(512), "strength": boundedEnum("verified", "best_effort", "unavailable"),
			}, "required": []string{"strength"}},
			"source_fingerprint": stringProperty(64), "observed_at": stringProperty(64),
		},
		"required": []string{"observation_id", "field_id", "resource_id", "external_resource_id", "locator", "typed_value", "provider_revision", "source_fingerprint", "observed_at"},
	}
	itemError := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"field_id": stringProperty(128),
			"code":     boundedEnum("source_unavailable", "denied", "invalid", "ambiguous"),
			"message":  stringProperty(512),
		},
		"required": []string{"field_id", "code", "message"},
	}
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"nl_audit_id": stringProperty(128),
			"status":      boundedEnum("completed", "partial", "failed", "idempotency_replay", "error", "denied", "idempotency_in_progress", "idempotency_conflict"),
			"snapshot_id": stringProperty(128), "report_id": stringProperty(128), "definition_revision": boundedInteger(1, 1000000),
			"period": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
				"key": stringProperty(128), "label": stringProperty(256),
			}, "required": []string{"key", "label"}},
			"captured_at":  stringProperty(64),
			"completeness": boundedEnum("complete", "incomplete", "not_created"),
			"observations": map[string]any{"type": "array", "maxItems": 1000, "items": observation},
			"item_errors":  map[string]any{"type": "array", "maxItems": 1000, "items": itemError},
			"content_hash": stringProperty(64), "relationship_count": boundedInteger(0, 1000), "error": structuredErrorSchema(),
		},
		"required": []string{"nl_audit_id", "status"},
	}
}

func snapshotErrorResponse(ctx context.Context, err error) assurance.SnapshotResponse {
	_, status := structuredFailure(err, mcpserver.AuditIDFromContext(ctx))
	return assurance.SnapshotResponse{NLAuditID: mcpserver.AuditIDFromContext(ctx), Status: assurance.SnapshotStatus(status)}
}

func (snapshotReportTool) RegisterSDK(server *mcp.Server, deps mcpserver.Deps) {
	mcp.AddTool(server, &mcp.Tool{Name: "workiva_snapshot_report", Description: snapshotReportDescription, InputSchema: snapshotReportInputSchema(), OutputSchema: snapshotReportOutputSchema()},
		func(ctx context.Context, request *mcp.CallToolRequest, input snapshotReportInput) (*mcp.CallToolResult, assurance.SnapshotResponse, error) {
			if err := requireDeps(deps, true, true); err != nil {
				return structuredToolResult(ctx, err, ""), snapshotErrorResponse(ctx, err), nil
			}
			if deps.Assurance == nil {
				err := failMsgWithContext(ctx, "assurance store is not available", "enable and provision Phase 3 assurance")
				return structuredToolResult(ctx, err, ""), snapshotErrorResponse(ctx, err), nil
			}
			if deps.Cfg != nil && !deps.Cfg.AssuranceEnabled {
				err := failMsgWithContext(ctx, "assurance snapshot feature is disabled", "set NL_ASSURANCE_ENABLED=true and provision a signed bundle")
				return structuredToolResult(ctx, err, ""), snapshotErrorResponse(ctx, err), nil
			}
			if _, trusted := identityForTool(ctx); !trusted && (deps.Cfg == nil || !deps.Cfg.AssuranceLegacyAPIKeyProfile) {
				err := &assurance.Error{Code: "strong_identity_required", Message: "trusted assurance identity is required"}
				return structuredToolResult(ctx, err, ""), snapshotErrorResponse(ctx, err), nil
			}
			reader, ok := deps.Client.(assurance.SourceReader)
			if !ok {
				err := failMsgWithContext(ctx, "uncached typed provider reads are not available", "use the configured Workiva REST provider router")
				return structuredToolResult(ctx, err, ""), snapshotErrorResponse(ctx, err), nil
			}
			service := assurance.SnapshotService{
				Store:  deps.Assurance,
				Reader: reader,
				Audit:  deps.Audit,
				Authorize: func(ctx context.Context, field assurance.FieldDefinition) error {
					if !resourceAllowed(deps, field.ExternalResourceID, field.SubresourceID) {
						return fmt.Errorf("resource denied")
					}
					return requireTenantOwnedResource(ctx, deps, field.ExternalResourceID, field.SubresourceID)
				},
			}
			ctx = assurance.WithRequestID(ctx, mcpserver.RequestIDFromContext(ctx))
			response, err := service.Capture(ctx, mcpserver.ActorFromContextOrRequest(ctx, request, deps.ActorHeader),
				mcpserver.AuditIDFromContext(ctx), assurance.SnapshotRequest{
					ReportID: input.ReportID,
					Period:   assurance.Period{Key: input.Period.Key, Label: input.Period.Label, Start: input.Period.Start, End: input.Period.End},
					FieldIDs: input.FieldIDs, AllowPartial: input.AllowPartial, IncludeRelationships: input.IncludeRelationships,
					Consistency: assurance.ConsistencyMode(input.Consistency), RetentionClass: input.RetentionClass, IdempotencyKey: input.IdempotencyKey,
				})
			if err != nil {
				return structuredToolResult(ctx, err, "verify the approved report, period, fields, capability, and signed bundle"), snapshotErrorResponse(ctx, err), nil
			}
			return nil, response, nil
		})
}
