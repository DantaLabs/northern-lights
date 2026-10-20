package changeset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

type sheetGroup struct {
	spreadsheet, sheet string
	indices            []int
	edits              []workiva.CellEdit
}

// Plan every sheet before submitting anything, so invalid coordinates cannot
// strand part of an otherwise valid approval.
func plan(items []stagedItem) ([]sheetGroup, error) {
	var groups []sheetGroup
	positions := map[[2]string]int{}
	for i, item := range items {
		r, err := boundedRange(item.Range)
		if err != nil {
			return nil, err
		}
		key := [2]string{item.SpreadsheetID, item.SheetID}
		pos, ok := positions[key]
		if !ok {
			pos = len(groups)
			positions[key] = pos
			groups = append(groups, sheetGroup{spreadsheet: key[0], sheet: key[1]})
		}
		group := &groups[pos]
		group.indices = append(group.indices, i)
		for row := r.StartRow; row <= r.StopRow; row++ {
			for col := r.StartCol; col <= r.StopCol; col++ {
				group.edits = append(group.edits, workiva.CellEdit{Row: row, Column: col, Value: item.After})
			}
		}
	}
	return groups, nil
}

func (s *service) execute(ctx context.Context, actor, id string, items []stagedItem) (Result, error) {
	groups, err := plan(items)
	if err != nil {
		return Result{}, fmt.Errorf("token consumed; restage: %w", err)
	}
	result := Result{ID: id, Status: "completed", Items: make([]ItemResult, len(items))}
	for i, item := range items {
		result.Items[i] = ItemResult{FieldName: item.FieldName, Before: item.Before, After: item.After, Status: "not_submitted"}
	}
	for _, group := range groups {
		op, delay, submitErr := s.env.Client.UpdateSheetWithRetryAfter(ctx, group.spreadsheet, group.sheet, workiva.NewLiteralEditCellsUpdate(group.edits))
		if submitErr == nil {
			_, submitErr = s.env.Client.WaitOperationWithInitialRetryAfter(ctx, op, delay)
		}
		if submitErr != nil {
			status := "reconciliation_required"
			var failed *workiva.OperationFailedError
			if errors.As(submitErr, &failed) {
				status = "failed"
			}
			var apiErr *workiva.APIError
			if errors.As(submitErr, &apiErr) {
				if op == "" {
					op = apiErr.OperationURL
				}
				if apiErr.StatusCode == 400 {
					status = "failed"
				}
			}
			for _, i := range group.indices {
				result.Items[i].Status = status
				result.Items[i].OperationURL = op
				result.Items[i].Message = submitErr.Error()
				if err := s.audit(ctx, actor, id, items[i], &result.Items[i]); err != nil {
					result.Items[i].Message += "; audit recovery required: " + err.Error()
				}
			}
			// The snapshot may now be stale even though acceptance could not be
			// proved. Invalidate it rather than guessing at the intended value.
			cacheErr := s.invalidate(ctx, group)
			if cacheErr != nil {
				for _, i := range group.indices {
					result.Items[i].Message += "; cache invalidation: " + cacheErr.Error()
				}
			}
			if status == "reconciliation_required" {
				result.Status = status
				break
			}
			if result.Status == "completed" {
				result.Status = "partial"
			}
			continue
		}
		for _, i := range group.indices {
			item := items[i]
			out := &result.Items[i]
			out.OperationURL = op
			value, cells, readErr := s.read(ctx, item)
			if readErr != nil {
				out.Status = "reconciliation_required"
				out.Message = "write completed; read-back failed: " + readErr.Error()
				if err := s.invalidate(ctx, group); err != nil {
					out.Message += "; cache invalidation: " + err.Error()
				}
				result.Status = "reconciliation_required"
			} else {
				out.Status = "completed"
				out.ReadBack = value
				for _, cell := range cells {
					if cell.Value != item.After {
						out.Status = "mismatch"
					}
				}
				if out.Status == "mismatch" && result.Status == "completed" {
					result.Status = "partial"
				}
				if err := s.env.Store.CacheCells(ctx, cells); err != nil {
					out.Message = "cache refresh failed: " + err.Error()
				}
			}
			if err := s.audit(ctx, actor, id, item, out); err != nil {
				out.Message += "; audit recovery required: " + err.Error()
				// Never return an ordinary retryable error after a completed mutation.
				out.Status = "reconciliation_required"
				result.Status = "reconciliation_required"
			}
		}
	}
	var lines []string
	for _, item := range result.Items {
		lines = append(lines, fmt.Sprintf("%s: %s -> %s (%s)", item.FieldName, item.Before, item.After, item.Status))
	}
	result.Summary = strings.Join(lines, "\n")
	return result, nil
}

func (s *service) audit(ctx context.Context, actor, id string, item stagedItem, out *ItemResult) error {
	before, err := json.Marshal(map[string]string{"change_set_id": id, "field": item.FieldName, "value": item.Before})
	if err != nil {
		return err
	}
	after, err := json.Marshal(map[string]string{"change_set_id": id, "field": item.FieldName, "value": out.ReadBack, "intended": item.After, "status": out.Status})
	if err != nil {
		return err
	}
	auditID, err := randomToken()
	if err != nil {
		return err
	}
	_, err = s.env.Audit.Append(ctx, audit.Entry{Actor: actor, Tool: "workiva_confirm_change_set", Action: "write", Target: item.SpreadsheetID + "/" + item.SheetID + "/" + item.Range, BeforeJSON: string(before), AfterJSON: string(after), WorkivaOpURL: out.OperationURL, AuditID: auditID})
	if err == nil {
		out.AuditID = auditID
	}
	return err
}

func (s *service) invalidate(ctx context.Context, group sheetGroup) error {
	_, err := s.env.DB.ExecContext(ctx, `DELETE FROM snapshots WHERE tenant_id=? AND spreadsheet_id=? AND sheet_id=?`, identity.StorageTenant(ctx), group.spreadsheet, group.sheet)
	return err
}
