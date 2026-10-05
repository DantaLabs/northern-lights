package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type transferValueTool struct{}

func TransferValue() mcpserver.Tool    { return transferValueTool{} }
func (transferValueTool) Name() string { return "workiva_transfer_value" }
func (transferValueTool) Description() string {
	return "Stages, confirms, acknowledges, or reconciles a typed value transfer. Bulk phases are disabled. Single-transfer phases require a configured transfer service backed by a verified durable fence."
}

//go:embed testdata/mcp-schemas/workiva_transfer_value.input.json
var transferInputSchemaJSON []byte

//go:embed testdata/mcp-schemas/workiva_transfer_value.output.json
var transferOutputSchemaJSON []byte

func transferSchema(which string) map[string]any {
	b := transferInputSchemaJSON
	if which == "output" {
		b = transferOutputSchemaJSON
	}
	var schema map[string]any
	if err := json.Unmarshal(b, &schema); err != nil {
		panic(fmt.Sprintf("decode transfer %s schema: %v", which, err))
	}
	return schema
}

func (transferValueTool) RegisterSDK(s *mcp.Server, deps mcpserver.Deps) {
	mcp.AddTool(s, &mcp.Tool{Name: "workiva_transfer_value", Description: (transferValueTool{}).Description(), InputSchema: transferSchema("input"), OutputSchema: transferSchema("output")},
		func(ctx context.Context, _ *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, map[string]any, error) {
			phase, _ := in["phase"].(string)
			status := "unavailable"
			code := "transfer_service_unavailable"
			message := "transfer service is unavailable because no verified durable fence is configured; no transfer action was performed"
			switch phase {
			case "bulk_stage", "bulk_confirm":
				status, code, message = "feature_disabled", "feature_disabled", "bulk transfer is explicitly disabled; no provider call or mutation was performed"
			case "stage", "confirm", "acknowledge", "reconcile":
				// Deliberately do not call the service in this schema-only step.
				if deps.Transfer != nil {
					code = "transfer_phase_unavailable"
					message = "transfer phase execution is not enabled yet; no transfer action was performed"
				}
			default:
				status, code, message = "error", "invalid_phase", "phase is not supported"
			}
			return nil, map[string]any{"phase": phase, "nl_audit_id": mcpserver.AuditIDFromContext(ctx), "status": status, "no_mutation_submitted": true, "reconciliation_required": false, "ignored_fields": []string{}, "errors": []any{map[string]any{"code": code, "message": message, "retryable": false}}}, nil
		})
}
