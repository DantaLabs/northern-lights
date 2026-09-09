package tools

import (
	"context"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

// syncMappingTool implements workiva_sync_mapping.
type syncMappingTool struct{}

// SyncMapping returns the workiva_sync_mapping tool.
func SyncMapping() mcpserver.Tool { return syncMappingTool{} }

const syncMappingDescription = `Discovers fields from a two-column "mapper" sheet and upserts them into the mapping store.

Reads the sheet live: the name column holds human-readable field labels, the value column holds the cells those labels refer to. Every row from start_row on becomes one field whose name is the label normalized to snake_case (for example "Scope 2 Energy (kWh)" becomes scope_2_energy_kwh) and whose range is the value column cell of that row. Rows with an empty name are skipped.

Use this after the reporting team edits the mapper sheet, or to bootstrap a new spreadsheet. Afterwards workiva_search_fields and workiva_get_field resolve the synced names, and workiva_update_field can write them with confirmation. The sync itself is recorded in the audit trail.`

func (syncMappingTool) Name() string { return "workiva_sync_mapping" }

func (syncMappingTool) Description() string { return syncMappingDescription }

type syncMappingInput struct {
	SpreadsheetID string `json:"spreadsheet_id" jsonschema:"Workiva spreadsheet ID, e.g. abc123"`
	SheetID       string `json:"sheet_id" jsonschema:"Workiva sheet ID within the spreadsheet"`
	NameColumn    string `json:"name_column,omitempty" jsonschema:"A1 column letter holding field names, default A"`
	ValueColumn   string `json:"value_column,omitempty" jsonschema:"A1 column letter holding the referenced values, default B"`
	StartRow      int    `json:"start_row,omitempty" jsonschema:"first 1-indexed row of data; rows above are skipped, default 2"`
}

type syncMappingOutput struct {
	SpreadsheetID string   `json:"spreadsheet_id"`
	SheetID       string   `json:"sheet_id"`
	FieldsCount   int      `json:"fields_count"`
	Fields        []string `json:"fields"`
}

func (syncMappingTool) RegisterSDK(s *mcp.Server, deps mcpserver.Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "workiva_sync_mapping",
		Description: syncMappingDescription,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in syncMappingInput) (*mcp.CallToolResult, syncMappingOutput, error) {
		if err := requireDeps(deps, true, true); err != nil {
			return nil, syncMappingOutput{}, err
		}
		if deps.Client == nil {
			return nil, syncMappingOutput{}, failMsg("Workiva client is not available", "server misconfiguration: check Workiva credentials")
		}

		actor := mcpserver.ActorFromRequest(req, mcpserver.DefaultActorHeader)
		nameCol, valueCol, startRowIdx, err := syncArgs(in)
		if err != nil {
			return nil, syncMappingOutput{}, err
		}

		// Read the full column span and skip header rows below; the API
		// has no cheap "rows with content" query.
		cellRange, err := workiva.RangeToA1(workiva.Range{
			StartRow: -1, StartCol: nameCol, StopRow: -1, StopCol: valueCol,
		})
		if err != nil {
			return nil, syncMappingOutput{}, fail(err, "the name/value columns do not form a valid span")
		}
		data, err := deps.Client.GetSheetData(ctx, in.SpreadsheetID, in.SheetID, cellRange, []string{"value"})
		if err != nil {
			return nil, syncMappingOutput{}, fail(err, "the mapper sheet could not be read from Workiva; check the spreadsheet and sheet IDs")
		}
		if data.Range == nil {
			return nil, syncMappingOutput{}, failMsg("the mapper sheet returned no range metadata", "check that the sheet ID is correct")
		}

		names, err := syncFields(ctx, deps, in, data, nameCol, valueCol, startRowIdx, actor)
		if err != nil {
			return nil, syncMappingOutput{}, err
		}

		return nil, syncMappingOutput{
			SpreadsheetID: in.SpreadsheetID,
			SheetID:       in.SheetID,
			FieldsCount:   len(names),
			Fields:        names,
		}, nil
	})
}

// syncArgs validates and normalizes the input arguments.
func syncArgs(in syncMappingInput) (nameCol, valueCol, startRowIdx int, err error) {
	nameCol, err = columnIndex(in.NameColumn, "A")
	if err != nil {
		return 0, 0, 0, err
	}
	valueCol, err = columnIndex(in.ValueColumn, "B")
	if err != nil {
		return 0, 0, 0, err
	}
	if nameCol == valueCol {
		return 0, 0, 0, failMsg("name_column and value_column must differ", "use the default two-column layout A/B")
	}
	startRow := in.StartRow
	if startRow == 0 {
		startRow = 2
	}
	if startRow < 1 {
		return 0, 0, 0, failMsg("start_row must be 1 or greater", "row 1 usually holds the header")
	}
	return nameCol, valueCol, startRow - 1, nil
}

// columnIndex parses a column letter argument into a zero-based index,
// falling back to def when the argument is empty.
func columnIndex(letters, def string) (int, error) {
	if letters == "" {
		letters = def
	}
	rng, err := workiva.A1ToRange(strings.ToUpper(strings.TrimSpace(letters)) + "1")
	if err != nil {
		return 0, failMsg("invalid column "+letters, "use a plain column letter such as A or B")
	}
	return rng.StartCol, nil
}

// syncFields upserts one field per data row and audits the sync. It
// returns the upserted field names in sheet order.
func syncFields(ctx context.Context, deps mcpserver.Deps, in syncMappingInput, data *workiva.SheetData, nameCol, valueCol, startRowIdx int, actor string) ([]string, error) {
	// Seed the spreadsheet and sheet rows so the mapping store stays
	// internally consistent for later listing.
	region := "eu"
	if deps.Cfg != nil && deps.Cfg.Region != "" {
		region = deps.Cfg.Region
	}
	if err := deps.Store.UpsertSpreadsheet(ctx, mapping.Spreadsheet{ID: in.SpreadsheetID, Region: region}); err != nil {
		return nil, fail(err, "the spreadsheet row could not be recorded in the mapping store")
	}
	if err := deps.Store.UpsertSheet(ctx, mapping.Sheet{ID: in.SheetID, SpreadsheetID: in.SpreadsheetID}); err != nil {
		return nil, fail(err, "the sheet row could not be recorded in the mapping store")
	}

	valueLetters, err := workiva.RangeToA1(workiva.Range{
		StartRow: 0, StartCol: valueCol, StopRow: 0, StopCol: valueCol,
	})
	if err != nil {
		return nil, fail(err, "the value column is not representable in A1 notation")
	}
	// A single-cell range like "B1": strip the trailing row number.
	valueLetters = strings.TrimRight(valueLetters, "0123456789")

	var names []string
	for rowIdx, row := range data.Cells {
		sheetRow := data.Range.StartRow + rowIdx
		if sheetRow < startRowIdx {
			continue
		}
		name := ""
		if nameCol-data.Range.StartCol >= 0 && nameCol-data.Range.StartCol < len(row) {
			name = normalizeFieldName(cellText(row[nameCol-data.Range.StartCol]))
		}
		if name == "" {
			continue
		}
		field := mapping.Field{
			SpreadsheetID: in.SpreadsheetID,
			SheetID:       in.SheetID,
			Name:          name,
			CellRange:     valueLetters + strconv.Itoa(sheetRow+1),
		}
		if _, err := deps.Store.UpsertField(ctx, field); err != nil {
			return nil, fail(err, "field "+name+" could not be stored")
		}
		names = append(names, name)
	}

	if _, err := deps.Audit.Append(ctx, audit.Entry{
		Actor:     actor,
		Tool:      "workiva_sync_mapping",
		Action:    "sync",
		Target:    in.SpreadsheetID + "/" + in.SheetID,
		AfterJSON: `{"fields_count":` + strconv.Itoa(len(names)) + `}`,
	}); err != nil {
		return nil, fail(err, "the sync could not be recorded in the audit trail")
	}
	return names, nil
}

// normalizeFieldName converts a human-readable label into the
// snake_case form used as a field name: lowercased, every run of
// non-alphanumeric characters becomes one underscore, leading and
// trailing underscores trimmed.
func normalizeFieldName(label string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(label) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if b.Len() > 0 && !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.TrimRight(b.String(), "_")
}
