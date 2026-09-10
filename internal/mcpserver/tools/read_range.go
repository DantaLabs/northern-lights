package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

// readRangeTool implements workiva_read_range.
type readRangeTool struct{}

// ReadRange returns the workiva_read_range tool.
func ReadRange() mcpserver.Tool { return readRangeTool{} }

const readRangeDescription = `Reads a range of cells from a Workiva sheet and returns the values as a grid of rows.

Args:
- spreadsheet_id: the spreadsheet ID, found via workiva_list_spreadsheets.
- sheet_id: the sheet ID within that spreadsheet.
- range: A1 notation such as "B3" or "B3:D10".

Every call fetches live data from the Workiva API and refreshes the read cache, so recent values are also available to workiva_get_field and workiva_search_fields. Formula cells return their calculated value. Values are returned in Ones scale, matching the Workiva API.`

func (readRangeTool) Name() string { return "workiva_read_range" }

func (readRangeTool) Description() string { return readRangeDescription }

type readRangeInput struct {
	SpreadsheetID string `json:"spreadsheet_id" jsonschema:"the spreadsheet ID, e.g. from workiva_list_spreadsheets"`
	SheetID       string `json:"sheet_id" jsonschema:"the sheet ID within the spreadsheet"`
	Range         string `json:"range" jsonschema:"A1 notation of the cells to read, e.g. B3 or B3:D10"`
}

type readRangeOutput struct {
	SpreadsheetID string     `json:"spreadsheet_id"`
	SheetID       string     `json:"sheet_id"`
	Range         string     `json:"range"`
	Rows          [][]string `json:"rows"`
	FetchedAt     time.Time  `json:"fetched_at"`
}

func (readRangeTool) RegisterSDK(s *mcp.Server, deps mcpserver.Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "workiva_read_range",
		Description: readRangeDescription,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in readRangeInput) (*mcp.CallToolResult, readRangeOutput, error) {
		if err := requireDeps(deps, true, true); err != nil {
			return nil, readRangeOutput{}, err
		}
		if deps.Client == nil {
			return nil, readRangeOutput{}, failMsg("Workiva client is not available", "server misconfiguration: check Workiva credentials")
		}
		if _, err := workiva.A1ToRange(in.Range); err != nil {
			return nil, readRangeOutput{}, fail(err, "use A1 notation such as B3 or B3:D10")
		}

		data, err := deps.Client.GetSheetData(ctx, in.SpreadsheetID, in.SheetID, in.Range,
			[]string{"cells.value", "cells.calculatedValue"})
		if err != nil {
			return nil, readRangeOutput{}, fail(err, "verify spreadsheet_id and sheet_id with workiva_list_spreadsheets")
		}

		fetchedAt := time.Now().UTC()
		rows := make([][]string, 0, len(data.Cells))
		for _, row := range data.Cells {
			outRow := make([]string, 0, len(row))
			for _, cell := range row {
				outRow = append(outRow, cellText(cell))
			}
			rows = append(rows, outRow)
		}

		if cells := gridToCachedCells(in.SpreadsheetID, in.SheetID, data, fetchedAt); cells != nil {
			if err := deps.Store.CacheCells(ctx, cells); err != nil {
				return nil, readRangeOutput{}, fail(err, "the read succeeded but the snapshot cache could not be updated")
			}
		}

		target := fmt.Sprintf("%s/%s/%s", in.SpreadsheetID, in.SheetID, in.Range)
		if _, err := deps.Audit.Append(ctx, audit.Entry{
			Tool:   "workiva_read_range",
			Action: "read",
			Target: target,
		}); err != nil {
			return nil, readRangeOutput{}, fail(err, "the read succeeded but could not be audited")
		}

		return nil, readRangeOutput{
			SpreadsheetID: in.SpreadsheetID,
			SheetID:       in.SheetID,
			Range:         in.Range,
			Rows:          rows,
			FetchedAt:     fetchedAt,
		}, nil
	})
}
