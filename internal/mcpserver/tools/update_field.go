package tools

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

// pendingWriteTTL bounds the lifetime of a staged write confirmation
// token. Declared as a variable so tests can shorten it.
var pendingWriteTTL = 5 * time.Minute

// updateFieldTool implements workiva_update_field.
type updateFieldTool struct{}

// UpdateField returns the workiva_update_field tool.
func UpdateField() mcpserver.Tool { return updateFieldTool{} }

const updateFieldDescription = `Writes a new value to one mapped field, identified by its exact field name.

This tool is two-phase by default so a human approves every write before it reaches Workiva (EU AI Act Art. 14 human oversight):

1. Call with just name and value. Nothing is written. The response carries the current value (before), a preview of the new value (after_preview), and a confirm_token that is valid for 5 minutes and single use.
2. Show the before/after preview to the user. When they approve, call the tool again with the same name, value, and the confirm_token. Only then is the batched editCells write sent to Workiva, and the mutation is recorded in the audit trail with before and after values plus the Workiva operation URL.

The confirmation requirement can be disabled by the server operator (require_write_confirmation: false); when disabled a call without confirm_token writes immediately, still fully audited.

Values are written in Ones scale regardless of the cell display format. After a successful write the snapshot cache is refreshed so subsequent reads return the new value.`

func (updateFieldTool) Name() string { return "workiva_update_field" }

func (updateFieldTool) Description() string { return updateFieldDescription }

type updateFieldInput struct {
	Name         string `json:"name" jsonschema:"exact mapped field name, e.g. scope2_energy_kwh; find one with workiva_search_fields when unsure"`
	Value        string `json:"value" jsonschema:"new value for the field, written in Ones scale"`
	ConfirmToken string `json:"confirm_token,omitempty" jsonschema:"token from a previous staging call; present it to execute the staged write once"`
}

type updateFieldOutput struct {
	Status        string `json:"status"`
	ConfirmToken  string `json:"confirm_token,omitempty"`
	Field         string `json:"field"`
	SpreadsheetID string `json:"spreadsheet_id"`
	SheetID       string `json:"sheet_id"`
	Range         string `json:"range"`
	Before        string `json:"before"`
	AfterPreview  string `json:"after_preview,omitempty"`
	After         string `json:"after,omitempty"`
	WorkivaOpURL  string `json:"workiva_op_url,omitempty"`
	Message       string `json:"message,omitempty"`
}

func (updateFieldTool) RegisterSDK(s *mcp.Server, deps mcpserver.Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "workiva_update_field",
		Description: updateFieldDescription,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in updateFieldInput) (*mcp.CallToolResult, updateFieldOutput, error) {
		if err := requireDeps(deps, true, true); err != nil {
			return nil, updateFieldOutput{}, err
		}

		field, err := resolveField(ctx, deps, in.Name)
		if err != nil {
			return nil, updateFieldOutput{}, err
		}

		// Phase 2: a confirm token was presented.
		if in.ConfirmToken != "" {
			return executeConfirmedWrite(ctx, deps, field, in)
		}

		// Single-phase write is allowed only when the operator disabled
		// the confirmation requirement.
		if !confirmationRequired(deps) {
			return executeWrite(ctx, deps, field, in.Value)
		}

		// Phase 1: stage the write and return a confirmation token.
		return stageWrite(ctx, deps, field, in.Value)
	})
}

// resolveField looks up the field by exact name and wraps every failure
// mode in the tool error envelope.
func resolveField(ctx context.Context, deps mcpserver.Deps, name string) (*mapping.Field, error) {
	field, err := deps.Store.GetField(ctx, name)
	if err != nil {
		return nil, fail(err, "the mapping store could not look up the field")
	}
	if field == nil {
		return nil, failMsg("no field named "+name,
			"run workiva_search_fields with the natural-language phrase to find the right field name")
	}
	return field, nil
}

// confirmationRequired reports whether staged confirmation is active. A
// missing config means confirmation stays on: this is the safe default.
func confirmationRequired(deps mcpserver.Deps) bool {
	return deps.Cfg == nil || deps.Cfg.RequireWriteConfirmation
}

// stageWrite is phase 1: read the current value live, persist a pending
// write, and return the confirmation token with a before/after preview.
func stageWrite(ctx context.Context, deps mcpserver.Deps, field *mapping.Field, value string) (*mcp.CallToolResult, updateFieldOutput, error) {
	before, err := readFieldValue(ctx, deps, field)
	if err != nil {
		return nil, updateFieldOutput{}, err
	}

	token := uuid.NewString()
	if err := deps.Store.CreatePendingWrite(ctx, mapping.PendingWrite{
		Token:     token,
		FieldID:   field.ID,
		FieldName: field.Name,
		Value:     value,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		return nil, updateFieldOutput{}, fail(err, "the write could not be staged for confirmation")
	}

	return nil, updateFieldOutput{
		Status:        "awaiting_confirmation",
		ConfirmToken:  token,
		Field:         field.Name,
		SpreadsheetID: field.SpreadsheetID,
		SheetID:       field.SheetID,
		Range:         field.CellRange,
		Before:        before,
		AfterPreview:  value,
		Message: "re-call workiva_update_field with the same name, value, and confirm_token " +
			"within 5 minutes to execute this write after human approval",
	}, nil
}

// executeConfirmedWrite is phase 2: consume the single-use token and run
// the staged write.
func executeConfirmedWrite(ctx context.Context, deps mcpserver.Deps, field *mapping.Field, in updateFieldInput) (*mcp.CallToolResult, updateFieldOutput, error) {
	pending, err := deps.Store.ConsumePendingWrite(ctx, in.ConfirmToken, pendingWriteTTL)
	if err == nil && pending == nil {
		return nil, updateFieldOutput{}, failMsg("unknown or already used confirm_token "+in.ConfirmToken,
			"stage the write again by calling workiva_update_field without confirm_token")
	}
	if errors.Is(err, mapping.ErrPendingWriteExpired) {
		return nil, updateFieldOutput{}, failMsg("confirm_token expired (older than 5 minutes)",
			"stage the write again by calling workiva_update_field without confirm_token")
	}
	if err != nil {
		return nil, updateFieldOutput{}, fail(err, "the confirmation token could not be validated")
	}

	// The staged write targets the field it was created for; a mismatch
	// means the caller shuffled arguments between the two calls.
	if pending.FieldName != in.Name {
		return nil, updateFieldOutput{}, failMsg(
			"confirm_token was staged for field "+pending.FieldName+", not "+in.Name,
			"re-call with name "+pending.FieldName+" and value "+pending.Value+", or stage a new write")
	}

	return executeWrite(ctx, deps, field, pending.Value)
}

// executeWrite sends the batched editCells update, polls the async
// operation, audits the mutation, and refreshes the snapshot cache for
// single-cell fields.
func executeWrite(ctx context.Context, deps mcpserver.Deps, field *mapping.Field, value string) (*mcp.CallToolResult, updateFieldOutput, error) {
	rng, err := workiva.A1ToRange(field.CellRange)
	if err != nil {
		return nil, updateFieldOutput{}, fail(err, "the field's mapped range is not valid A1 notation")
	}

	before, err := readFieldValue(ctx, deps, field)
	if err != nil {
		return nil, updateFieldOutput{}, err
	}

	if deps.Client == nil {
		return nil, updateFieldOutput{}, failMsg("Workiva client is not available", "server misconfiguration: check Workiva credentials")
	}
	opURL, err := deps.Client.UpdateSheet(ctx, field.SpreadsheetID, field.SheetID,
		workiva.NewEditCellsUpdate([]workiva.CellEdit{{Range: rng, Value: value}}))
	if err != nil {
		return nil, updateFieldOutput{}, fail(err, "Workiva rejected the write; check the value and the field mapping")
	}
	if _, err := deps.Client.WaitOperation(ctx, opURL); err != nil {
		return nil, updateFieldOutput{}, fail(err, "the write operation did not complete; check Workiva file history before retrying")
	}

	if err := auditWrite(ctx, deps, field, before, value, opURL); err != nil {
		return nil, updateFieldOutput{}, err
	}
	refreshCacheAfterWrite(ctx, deps, field, rng, value)

	return nil, updateFieldOutput{
		Status:        "written",
		Field:         field.Name,
		SpreadsheetID: field.SpreadsheetID,
		SheetID:       field.SheetID,
		Range:         field.CellRange,
		Before:        before,
		After:         value,
		WorkivaOpURL:  opURL,
	}, nil
}

// readFieldValue fetches the field's current display value live from
// Workiva. An empty range reads as "".
func readFieldValue(ctx context.Context, deps mcpserver.Deps, field *mapping.Field) (string, error) {
	if deps.Client == nil {
		return "", failMsg("Workiva client is not available", "server misconfiguration: check Workiva credentials")
	}
	data, err := deps.Client.GetSheetData(ctx, field.SpreadsheetID, field.SheetID, field.CellRange,
		[]string{"value", "calculatedValue"})
	if err != nil {
		return "", fail(err, "the current value could not be read from Workiva; the spreadsheet may have been disconnected")
	}
	value, _ := gridValue(data)
	return value, nil
}

// auditWrite records the mutation in the hash-chained audit log.
func auditWrite(ctx context.Context, deps mcpserver.Deps, field *mapping.Field, before, after, opURL string) error {
	target := field.SpreadsheetID + "/" + field.SheetID + "/" + field.CellRange
	b, _ := json.Marshal(map[string]string{"value": before})
	a, _ := json.Marshal(map[string]string{"value": after})
	if _, err := deps.Audit.Append(ctx, audit.Entry{
		Actor:        "copilot",
		Tool:         "workiva_update_field",
		Action:       "write",
		Target:       target,
		BeforeJSON:   string(b),
		AfterJSON:    string(a),
		WorkivaOpURL: opURL,
	}); err != nil {
		return fail(err, "the write succeeded but could not be recorded in the audit trail")
	}
	return nil
}

// refreshCacheAfterWrite updates the snapshot cache for single-cell
// fields so a subsequent workiva_get_field does not serve the stale
// pre-write value.
func refreshCacheAfterWrite(ctx context.Context, deps mcpserver.Deps, field *mapping.Field, rng workiva.Range, value string) {
	if rng.StartRow != rng.StopRow || rng.StartCol != rng.StopCol {
		return
	}
	ref, err := workiva.RangeToA1(rng)
	if err != nil {
		return
	}
	_ = deps.Store.CacheCells(ctx, []mapping.CellValue{{
		SpreadsheetID: field.SpreadsheetID,
		SheetID:       field.SheetID,
		Cell:          ref,
		Value:         value,
		FetchedAt:     time.Now().UTC(),
	}})
}
