package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/content"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const contentPlacementToolName = "workiva_content_placement"

//go:embed testdata/content-placement.input.json
var contentPlacementInputSchemaRaw []byte

//go:embed testdata/content-placement.output.json
var contentPlacementOutputSchemaRaw []byte

type contentPlacementTool struct{}

func ContentPlacement() mcpserver.Tool    { return contentPlacementTool{} }
func (contentPlacementTool) Name() string { return contentPlacementToolName }
func (contentPlacementTool) Description() string {
	return "Privately ingest source bytes, stage an owned candidate, and record separate visual acknowledgement. Provider confirmation remains disabled until restored-database admission is verified."
}

func (contentPlacementTool) RegisterSDK(server *mcp.Server, deps mcpserver.Deps) {
	inputSchema := decodeContentPlacementSchema(contentPlacementInputSchemaRaw)
	outputSchema := decodeContentPlacementSchema(contentPlacementOutputSchemaRaw)
	server.AddTool(&mcp.Tool{Name: contentPlacementToolName, Description: (contentPlacementTool{}).Description(), InputSchema: inputSchema, OutputSchema: outputSchema}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return handleContentPlacement(ctx, req, deps.Content)
	})
}

func decodeContentPlacementSchema(raw []byte) map[string]any {
	var schema map[string]any
	_ = json.Unmarshal(raw, &schema)
	// The SDK's low-level handler requires object at the root; the published
	// oneOf and each closed phase definition remain the source of field rules.
	schema["type"] = "object"
	return schema
}

func handleContentPlacement(ctx context.Context, req *mcp.CallToolRequest, runtime *content.Runtime) (*mcp.CallToolResult, error) {
	auditID := mcpserver.AuditIDFromContext(ctx)
	var raw []byte
	if req != nil && req.Params != nil {
		raw = req.Params.Arguments
	}
	phase := placementPhase(raw)
	if mcpserver.IsAPIKeyRequest(ctx) {
		return contentPlacementFailure(phase, auditID, "strong_identity_required", "delegated identity is required for content placement", false, false), nil
	}
	permission, ok := placementPermission(phase)
	if !ok {
		return nil, errors.New("unsupported content placement phase")
	}
	principal, trusted := identity.PrincipalFromContext(ctx)
	if !trusted || principal.TokenType != identity.TokenTypeDelegated || principal.UsedSubjectFallback || !principal.HasPermission(permission) {
		return contentPlacementFailure(phase, auditID, "permission_denied", "trusted delegated phase permission is required", false, false), nil
	}
	if !principal.IsTrustedDelegatedActor() {
		return contentPlacementFailure(phase, auditID, "strong_identity_required", "canonical tenant and actor identity are required", false, false), nil
	}
	if auditID == "" {
		return nil, errors.New("content request audit correlation is unavailable")
	}
	if runtime == nil {
		return contentPlacementFailure(phase, auditID, "feature_disabled", "content placement runtime is unavailable", false, false), nil
	}
	request, err := content.DecodePlacementTransportRequest(raw)
	if err != nil {
		var typed *assurance.Error
		if !errors.As(err, &typed) {
			return contentPlacementFailure(phase, auditID, "invalid_request", "arguments do not match the published content phase schema", false, false), nil
		}
		return contentPlacementError(phase, auditID, err), nil
	}
	switch request.Phase {
	case "ingest":
		result, invocation, err := runtime.IngestPublicWithInvocation(ctx, raw, auditID)
		if err != nil {
			return contentPlacementError(phase, auditID, err), nil
		}
		output, err := content.ProjectIngestOutput(result, invocation)
		if err != nil {
			return contentPlacementError(phase, auditID, &assurance.Error{Code: "internal_error", Message: "sealed ingest response could not be projected"}), nil
		}
		return placementToolResult(output, false), nil
	case "stage":
		result, invocation, err := runtime.StageWithInvocation(ctx, auditID, *request.Stage)
		if err != nil {
			return contentPlacementError(phase, auditID, err), nil
		}
		output, err := content.ProjectStageOutput(result, invocation.AuditID)
		if err != nil {
			return contentPlacementError(phase, auditID, &assurance.Error{Code: "internal_error", Message: "sealed stage response could not be projected"}), nil
		}
		output.Replayed = invocation.ReplayDisposition == "sealed_replay"
		return placementToolResult(output, false), nil
	case "acknowledge":
		idem, _, err := request.Acknowledge.IdempotencyDigests()
		if err != nil {
			return contentPlacementError(phase, auditID, err), nil
		}
		result, invocation, err := runtime.AcknowledgeWithInvocation(ctx, auditID, request.Acknowledge.Request, idem)
		if err != nil {
			return contentPlacementError(phase, auditID, err), nil
		}
		output, err := content.ProjectAcknowledgeOutput(result, invocation)
		if err != nil {
			return contentPlacementError(phase, auditID, &assurance.Error{Code: "internal_error", Message: "sealed acknowledgement response could not be projected"}), nil
		}
		return placementToolResult(output, false), nil
	case "confirm":
		_, err := runtime.Confirm(ctx, auditID, *request.Confirm)
		return contentPlacementError(phase, auditID, err), nil
	case "reconcile":
		_, err := runtime.Reconcile(ctx, auditID, *request.Reconcile)
		return contentPlacementError(phase, auditID, err), nil
	default:
		return nil, errors.New("unsupported content placement phase")
	}
}

func placementPhase(raw []byte) string {
	var phase struct {
		Phase string `json:"phase"`
	}
	_ = json.Unmarshal(raw, &phase)
	return phase.Phase
}

func placementPermission(phase string) (identity.Permission, bool) {
	switch phase {
	case "ingest":
		return identity.PermissionContentIngest, true
	case "stage":
		return identity.PermissionContentStage, true
	case "confirm":
		return identity.PermissionContentConfirm, true
	case "acknowledge":
		return identity.PermissionContentAcknowledge, true
	case "reconcile":
		return identity.PermissionContentReconcile, true
	default:
		return "", false
	}
}

func contentPlacementError(phase, auditID string, err error) *mcp.CallToolResult {
	var typed *assurance.Error
	if errors.As(err, &typed) {
		code := typed.Code
		message := placementErrorMessage(code)
		if code == "capability_unverified" {
			code = "feature_disabled"
			message = "capability_unverified: the requested content capability is not admitted; no provider write was submitted"
		}
		if !knownPlacementErrorCode(code) {
			code = "internal_error"
			message = "operation failed; use nl_audit_id for investigation"
		}
		return contentPlacementFailure(phase, auditID, code, message, typed.Retryable, typed.ReconciliationRequired)
	}
	return contentPlacementFailure(phase, auditID, "internal_error", "operation failed; use nl_audit_id for investigation", false, false)
}

func knownPlacementErrorCode(code string) bool {
	switch code {
	case "invalid_request", "request_too_large", "source_too_large", "invalid_utf8", "invalid_base64", "unsupported_media_type", "unsupported_input", "missing_required_field", "invalid_phase", "invalid_value", "feature_disabled", "strong_identity_required", "permission_denied", "not_found_or_forbidden", "idempotency_conflict", "idempotency_in_progress", "replay_requires_restage", "artifact_hash_mismatch", "destination_not_approved", "destination_ambiguous", "resource_access_denied", "policy_blocked", "unsupported_conversion", "stale_preview", "confirmation_expired", "confirmation_invalid", "already_submitted", "result_uncertain", "reconciliation_required", "provider_unavailable", "audit_unavailable", "storage_unavailable", "internal_error":
		return true
	default:
		return false
	}
}

func contentPlacementFailure(phase, auditID, code, message string, retryable, reconciliation bool) *mcp.CallToolResult {
	output := content.PublicPlacementOutput{Phase: phase, Status: placementStatus(code), NLAuditID: auditID, Replayed: false, NoMutationSubmitted: true, ReconciliationRequired: reconciliation, Result: nil, Errors: []content.PublicPlacementError{{Code: code, Message: message, Retryable: retryable}}}
	return placementToolResult(output, true)
}

func placementStatus(code string) string {
	switch code {
	case "permission_denied", "strong_identity_required", "forbidden":
		return "denied"
	case "invalid_request", "invalid_phase", "request_too_large", "source_too_large", "invalid_utf8", "invalid_base64", "unsupported_media_type", "unsupported_input", "missing_required_field", "invalid_value":
		return "invalid"
	case "idempotency_conflict":
		return "conflict"
	case "idempotency_in_progress":
		return "pending"
	case "feature_disabled", "provider_unavailable", "content_policy_unavailable":
		return "unavailable"
	case "result_uncertain", "reconciliation_required", "already_submitted":
		return "uncertain"
	default:
		return "error"
	}
}

func placementErrorMessage(code string) string {
	switch code {
	case "permission_denied", "forbidden":
		return "trusted delegated phase permission is required"
	case "strong_identity_required":
		return "canonical delegated tenant and actor identity are required"
	case "idempotency_conflict":
		return "idempotency key conflicts with a prior request"
	case "idempotency_in_progress":
		return "an operation with this idempotency key is still in progress"
	case "feature_disabled":
		return "the requested content capability is unavailable"
	case "provider_unavailable":
		return "the required provider capability is unavailable"
	case "audit_unavailable":
		return "rich audit storage is unavailable"
	case "storage_unavailable":
		return "content storage is unavailable"
	case "policy_blocked", "content_policy_unavailable":
		return "the configured signed policy does not permit this operation"
	case "not_found_or_forbidden":
		return "the requested owned content is unavailable"
	case "artifact_hash_mismatch":
		return "stored content integrity validation failed"
	case "confirmation_expired":
		return "the confirmation has expired"
	case "confirmation_invalid":
		return "the confirmation is invalid"
	case "result_uncertain", "reconciliation_required", "already_submitted":
		return "the result is uncertain; do not repeat the operation"
	default:
		return "operation failed; use nl_audit_id for investigation"
	}
}

func placementToolResult(output content.PublicPlacementOutput, isError bool) *mcp.CallToolResult {
	encoded, _ := json.Marshal(output)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}, StructuredContent: output, IsError: isError}
}
