package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/mcpserver"
)

// confirmChangeSetTool implements workiva_confirm_change_set.
type confirmChangeSetTool struct{}

// ConfirmChangeSet returns the workiva_confirm_change_set tool.
func ConfirmChangeSet() mcpserver.Tool { return confirmChangeSetTool{} }

const confirmChangeSetName = "workiva_confirm_change_set"

const confirmChangeSetDescription = `Writes every field in a previewed change set after the person approved it, using the change set ID and confirmation token. Call it only after workiva_prepare_section returned a change set and the person explicitly approved the preview. Report how many fields completed and name any that did not, grouped by category rather than one line per field. Never retry an item whose status is mismatch or reconciliation_required; tell the person to check the spreadsheet.`

func (confirmChangeSetTool) Name() string { return confirmChangeSetName }

func (confirmChangeSetTool) Description() string { return confirmChangeSetDescription }

type confirmChangeSetInput struct {
	ChangeSetID  string `json:"change_set_id"`
	ConfirmToken string `json:"confirm_token"`
}

type confirmChangeSetItem struct {
	FieldName    string `json:"field_name"`
	Status       string `json:"status"`
	ReadBack     string `json:"read_back,omitempty"`
	AuditID      string `json:"audit_id,omitempty"`
	OperationURL string `json:"operation_url,omitempty"`
	Before       string `json:"before"`
	After        string `json:"after"`
	Message      string `json:"message,omitempty"`
}

type confirmChangeSetOutput struct {
	// NLAuditID is filled by the server middleware.
	NLAuditID   string                 `json:"nl_audit_id,omitempty"`
	ChangeSetID string                 `json:"change_set_id"`
	Status      string                 `json:"status"`
	Items       []confirmChangeSetItem `json:"items"`
	Summary     string                 `json:"summary"`
}

func confirmChangeSetInputSchema() map[string]any {
	return objectSchema(map[string]any{
		"change_set_id": boundedSchema("change set ID from workiva_prepare_section", 128),
		"confirm_token": boundedSchema("confirmation token from workiva_prepare_section", 4096),
	}, []string{"change_set_id", "confirm_token"})
}

func confirmChangeSetOutputSchema() map[string]any {
	item := objectSchema(map[string]any{
		"before":        optionalString("value before approval", 4096),
		"after":         optionalString("approved value", 4096),
		"message":       optionalString("reconciliation detail", 4096),
		"field_name":    boundedSchema("field name", 256),
		"status":        boundedSchema("write status", 32),
		"read_back":     optionalString("value read back after the write", 4096),
		"audit_id":      optionalString("audit ID of the write", 128),
		"operation_url": optionalString("Workiva operation URL", 2048),
	}, []string{"field_name", "status"})
	return objectSchema(mergeProperties(commonStructuredOutputProperties(), map[string]any{
		"change_set_id": boundedSchema("change set ID", 128),
		"status":        boundedSchema("change set status", 32),
		"items":         map[string]any{"type": "array", "maxItems": 500, "items": item},
		"summary":       optionalString("markdown summary of the writes", 262144),
	}), []string{"change_set_id", "status", "items", "summary"})
}

func (confirmChangeSetTool) RegisterSDK(s *mcp.Server, deps mcpserver.Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:         confirmChangeSetName,
		Description:  confirmChangeSetDescription,
		InputSchema:  confirmChangeSetInputSchema(),
		OutputSchema: confirmChangeSetOutputSchema(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in confirmChangeSetInput) (*mcp.CallToolResult, confirmChangeSetOutput, error) {
		if err := requireDemoDep(deps.ChangeSets != nil, "ChangeSets"); err != nil {
			return nil, confirmChangeSetOutput{}, err
		}
		actor := mcpserver.ActorFromContextOrRequest(ctx, req, deps.ActorHeader)
		result, err := deps.ChangeSets.Confirm(ctx, actor, in.ChangeSetID, in.ConfirmToken)
		if err != nil {
			return nil, confirmChangeSetOutput{}, failWithContext(ctx, err, "review the confirmation error; reconcile uncertain writes before staging again")
		}
		out := confirmChangeSetOutput{NLAuditID: mcpserver.AuditIDFromContext(ctx), ChangeSetID: result.ID, Status: result.Status, Summary: result.Summary, Items: make([]confirmChangeSetItem, 0, len(result.Items))}
		for _, item := range result.Items {
			out.Items = append(out.Items, confirmChangeSetItem{FieldName: item.FieldName, Status: item.Status, ReadBack: item.ReadBack, AuditID: item.AuditID, OperationURL: item.OperationURL, Before: item.Before, After: item.After, Message: item.Message})
		}
		return nil, out, nil
	})
}
