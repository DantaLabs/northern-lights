package tools

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/mcpserver"
)

// listSpreadsheetsTool implements workiva_list_spreadsheets.
type listSpreadsheetsTool struct{}

// ListSpreadsheets returns the workiva_list_spreadsheets tool.
func ListSpreadsheets() mcpserver.Tool { return listSpreadsheetsTool{} }

const listSpreadsheetsDescription = `Lists the Workiva spreadsheets currently connected to this server, with their sheets.

Only spreadsheets that have been connected to the semantic mapping appear here. To connect a new spreadsheet, or to import fields from a two-column name/value sheet, use the workiva_sync_mapping tool.

Use this tool to discover which spreadsheet_id and sheet_id values to pass to workiva_read_range, or to answer questions like "which reports are available?"`

func (listSpreadsheetsTool) Name() string { return "workiva_list_spreadsheets" }

func (listSpreadsheetsTool) Description() string { return listSpreadsheetsDescription }

// listSpreadsheetsInput has no arguments; the object is required by the
// MCP protocol but stays empty.
type listSpreadsheetsInput struct{}

type listSheetsOutput struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type spreadsheetEntry struct {
	ID       string             `json:"id"`
	Name     string             `json:"name"`
	Region   string             `json:"region"`
	SyncedAt string             `json:"synced_at,omitempty"`
	Sheets   []listSheetsOutput `json:"sheets"`
}

type listSpreadsheetsOutput struct {
	Spreadsheets []spreadsheetEntry `json:"spreadsheets"`
}

func (listSpreadsheetsTool) RegisterSDK(s *mcp.Server, deps mcpserver.Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "workiva_list_spreadsheets",
		Description: listSpreadsheetsDescription,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ listSpreadsheetsInput) (*mcp.CallToolResult, listSpreadsheetsOutput, error) {
		if err := requireDeps(deps, true, false); err != nil {
			return nil, listSpreadsheetsOutput{}, err
		}
		spreadsheets, err := deps.Store.ListSpreadsheets(ctx)
		if err != nil {
			return nil, listSpreadsheetsOutput{}, fail(err, "the mapping store could not list spreadsheets")
		}
		out := make([]spreadsheetEntry, 0, len(spreadsheets))
		for _, sp := range spreadsheets {
			entry := spreadsheetEntry{
				ID:     sp.ID,
				Name:   sp.Name,
				Region: sp.Region,
				Sheets: make([]listSheetsOutput, 0, len(sp.Sheets)),
			}
			if !sp.SyncedAt.IsZero() {
				entry.SyncedAt = sp.SyncedAt.UTC().Format(time.RFC3339)
			}
			for _, sh := range sp.Sheets {
				entry.Sheets = append(entry.Sheets, listSheetsOutput{ID: sh.ID, Name: sh.Name})
			}
			out = append(out, entry)
		}
		return nil, listSpreadsheetsOutput{Spreadsheets: out}, nil
	})
}
