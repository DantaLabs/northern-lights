package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

// listSpreadsheetsTool implements workiva_list_spreadsheets.
type listSpreadsheetsTool struct{}

// ListSpreadsheets returns the workiva_list_spreadsheets tool.
func ListSpreadsheets() mcpserver.Tool { return listSpreadsheetsTool{} }

const listSpreadsheetsDescription = `Discovers the Workiva spreadsheets and sheets currently available to the authenticated client.

When a Workiva client is configured, this tool reads the live spreadsheet and sheet catalogs, including files that are not mapped locally. In demo or no-client mode it falls back to the local mapping store.

Use this tool to discover which spreadsheet_id and sheet_id values to pass to workiva_read_range or workiva_sync_mapping. Live discovery only identifies files and sheets; local semantic mappings are still required by workiva_search_fields, workiva_get_field, and workiva_update_field.`

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
		if deps.Client != nil {
			live, err := deps.Client.ListSpreadsheets(ctx)
			if err != nil {
				return nil, listSpreadsheetsOutput{}, fail(err, "Workiva could not list spreadsheets; check the client credentials and file:read scope")
			}
			entries, err := liveSpreadsheetEntries(ctx, deps, live)
			if err != nil {
				return nil, listSpreadsheetsOutput{}, fail(err, "Workiva could not list spreadsheet sheets; check the spreadsheet IDs and file:read scope")
			}
			return nil, listSpreadsheetsOutput{Spreadsheets: entries}, nil
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

func liveSpreadsheetEntries(ctx context.Context, deps mcpserver.Deps, spreadsheets []workiva.Spreadsheet) ([]spreadsheetEntry, error) {
	region := ""
	if deps.Cfg != nil {
		region = deps.Cfg.Region
	}
	out := make([]spreadsheetEntry, 0, len(spreadsheets))
	for _, sp := range spreadsheets {
		entry := spreadsheetEntry{
			ID:     sp.ID,
			Name:   sp.Name,
			Region: region,
			Sheets: make([]listSheetsOutput, 0),
		}
		// Sheets are fetched live for every discovered spreadsheet.
		if deps.Client != nil {
			sheets, err := deps.Client.ListSheets(ctx, sp.ID)
			if err != nil {
				return nil, fmt.Errorf("list sheets for spreadsheet %q: %w", sp.ID, err)
			}
			for _, sh := range sheets {
				entry.Sheets = append(entry.Sheets, listSheetsOutput{ID: sh.ID, Name: sh.Name})
			}
		}
		out = append(out, entry)
	}
	return out, nil
}
