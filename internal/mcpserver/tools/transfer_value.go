package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/google/uuid"
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
			defer scrubTransferInput(in)
			input, ignored, validationErr := validateTransferContract(in)
			phase := transferOutputPhase(input.Phase)
			if validationErr != nil {
				return nil, transferContractOutput(ctx, phase, "error", validationErr, ignored), nil
			}

			// API-key identity is caller-asserted and cannot authorize the
			// high-impact phases. Entra authorization is performed by the
			// transport middleware before this handler; this check remains a
			// transport-independent defense for direct tool invocation.
			if input.Phase == "confirm" || input.Phase == "acknowledge" || input.Phase == "reconcile" {
				principal, trusted := identity.PrincipalFromContext(ctx)
				if mcpserver.IsAPIKeyRequest(ctx) || !trusted || principal.AuditActor() == "" {
					err := transferContractIssue("strong_identity_required", "", "trusted Entra identity is required for this transfer phase")
					return nil, transferContractOutput(ctx, phase, "denied", err, ignored), nil
				}
			}

			if input.Phase == "bulk_stage" || input.Phase == "bulk_confirm" {
				err := transferContractIssue("feature_disabled", "", "bulk transfer is explicitly disabled; no provider call or mutation was performed")
				return nil, transferContractOutput(ctx, phase, "feature_disabled", err, ignored), nil
			}

			// Only explicit dependency injection enables the bounded single-transfer
			// fixture path. Production main deliberately constructs no service.
			if deps.Transfer == nil {
				err := transferContractIssue("transfer_service_unavailable", "", "transfer service is unavailable because no verified durable fence is configured; no transfer action was performed")
				return nil, transferContractOutput(ctx, phase, "unavailable", err, ignored), nil
			}
			principal, trusted := identity.PrincipalFromContext(ctx)
			permission := identity.PermissionWorkivaWritePreview
			switch input.Phase {
			case "confirm":
				permission = identity.PermissionWorkivaWriteConfirm
			case "acknowledge":
				permission = identity.PermissionWorkivaVisualAck
			case "reconcile":
				permission = identity.PermissionReconciliationManage
			}
			if !trusted || principal.TenantID == "" || principal.AuditActor() == "" || !principal.HasPermission(permission) || mcpserver.IsAPIKeyRequest(ctx) {
				err := transferContractIssue("permission_denied", "", "trusted Entra capability required")
				return nil, transferContractOutput(ctx, phase, "denied", err, ignored), nil
			}
			return nil, executeSingleTransfer(ctx, deps, input, ignored), nil
		})
}

func transferOutputPhase(phase string) string {
	switch phase {
	case "stage", "confirm", "acknowledge", "reconcile", "bulk_stage", "bulk_confirm":
		return phase
	default:
		return "unknown"
	}
}

func transferAuditID(ctx context.Context) string {
	if id := mcpserver.AuditIDFromContext(ctx); id != "" {
		return id
	}
	return uuid.NewString()
}

func transferContractOutput(ctx context.Context, phase, status string, err error, ignored []string) map[string]any {
	if ignored == nil {
		ignored = []string{}
	}
	issue := transferContractError{Code: "internal_error", Msg: "transfer request failed"}
	if err != nil {
		var typed transferContractError
		if errors.As(err, &typed) {
			issue = typed
		} else {
			issue.Msg = err.Error()
		}
	}
	auditID := transferAuditID(ctx)
	errorObject := map[string]any{
		"code": issue.Code, "message": issue.Msg, "retryable": false,
		"reconciliation_required": false, "nl_audit_id": auditID,
	}
	if issue.Field != "" {
		errorObject["field"] = issue.Field
	}
	return map[string]any{
		"nl_audit_id": auditID, "phase": phase, "status": status,
		"no_mutation_submitted": true, "reconciliation_required": false,
		"ignored_fields": ignored, "errors": []any{errorObject}, "error": errorObject,
	}
}

func scrubTransferInput(in map[string]any) {
	if in == nil {
		return
	}
	delete(in, "confirmation_token")
	delete(in, "idempotency_key")
}
