package tools

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

// getFieldTool implements workiva_get_field.
type getFieldTool struct{}

// GetField returns the workiva_get_field tool.
func GetField() mcpserver.Tool { return getFieldTool{} }

const getFieldDescription = `Returns the current value of one mapped field, identified by its exact field name.

Use workiva_search_fields first when you only have a natural-language reference such as "scope 2 energy"; it resolves those to field names. When the cached read is still fresh the value comes from the local snapshot cache with no Workiva API call; otherwise the field's range is read live from the API and the cache is refreshed. The response includes the field's spreadsheet, sheet, range, type, description, the value, and when it was fetched (fetched_at, RFC 3339).`

func (getFieldTool) Name() string { return "workiva_get_field" }

func (getFieldTool) Description() string { return getFieldDescription }

type getFieldInput struct {
	Name string `json:"name" jsonschema:"exact mapped field name, e.g. scope2_energy_kwh; find one with workiva_search_fields when unsure"`
}

type getFieldOutput struct {
	Name          string    `json:"name"`
	SpreadsheetID string    `json:"spreadsheet_id"`
	SheetID       string    `json:"sheet_id"`
	Range         string    `json:"range"`
	FieldType     string    `json:"field_type"`
	Description   string    `json:"description,omitempty"`
	Aliases       string    `json:"aliases,omitempty"`
	Value         string    `json:"value"`
	FetchedAt     time.Time `json:"fetched_at"`
	Source        string    `json:"source"`
}

func (getFieldTool) RegisterSDK(s *mcp.Server, deps mcpserver.Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "workiva_get_field",
		Description: getFieldDescription,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getFieldInput) (*mcp.CallToolResult, getFieldOutput, error) {
		if err := requireDeps(deps, true, false); err != nil {
			return nil, getFieldOutput{}, err
		}

		field, err := deps.Store.GetField(ctx, in.Name)
		if err != nil {
			return nil, getFieldOutput{}, fail(err, "the mapping store could not look up the field")
		}
		if field == nil {
			return nil, getFieldOutput{}, failMsg("no field named "+in.Name,
				"run workiva_search_fields with the natural-language phrase to find the right field name")
		}

		out := getFieldOutput{
			Name:          field.Name,
			SpreadsheetID: field.SpreadsheetID,
			SheetID:       field.SheetID,
			Range:         field.CellRange,
			FieldType:     field.FieldType,
			Description:   field.Description,
			Aliases:       field.Aliases,
		}

		// Fresh cache hit: serve the snapshot without an API call.
		if ttl := cacheTTL(deps); ttl > 0 {
			cells, err := deps.Store.GetCachedCells(ctx, field.SpreadsheetID, field.SheetID, ttl)
			if err != nil {
				return nil, getFieldOutput{}, fail(err, "the snapshot cache could not be read")
			}
			if value, ok := valueFromCells(cells, field.CellRange); ok {
				out.Value = value
				out.FetchedAt = cells[0].FetchedAt.UTC()
				out.Source = "cache"
				return nil, out, nil
			}
		}

		// Cold or stale cache: read the field's range live and refresh
		// the snapshot for later calls.
		if deps.Client == nil {
			return nil, getFieldOutput{}, failMsg("Workiva client is not available", "server misconfiguration: check Workiva credentials")
		}
		data, err := deps.Client.GetSheetData(ctx, field.SpreadsheetID, field.SheetID, field.CellRange,
			[]string{"cells.value", "cells.calculatedValue"})
		if err != nil {
			return nil, getFieldOutput{}, fail(err, "the field could not be read from Workiva; the spreadsheet may have been disconnected")
		}

		fetchedAt := time.Now().UTC()
		if cells := gridToCachedCells(field.SpreadsheetID, field.SheetID, data, fetchedAt); cells != nil {
			if err := deps.Store.CacheCells(ctx, cells); err != nil {
				return nil, getFieldOutput{}, fail(err, "the read succeeded but the snapshot cache could not be updated")
			}
		}

		value, ok := gridValue(data)
		if !ok {
			return nil, getFieldOutput{}, failMsg("the field range "+field.CellRange+" contains no readable cells",
				"check the mapping for "+field.Name+" with workiva_read_range")
		}
		out.Value = value
		out.FetchedAt = fetchedAt
		out.Source = "live"
		return nil, out, nil
	})
}

// gridValue flattens a fetched grid into one display value, joining cells
// row by row. The boolean is false when the grid has no cells at all.
func gridValue(data *workiva.SheetData) (string, bool) {
	if len(data.Cells) == 0 {
		return "", false
	}
	var parts []string
	for _, row := range data.Cells {
		for _, cell := range row {
			parts = append(parts, cellText(cell))
		}
	}
	return joinNonEmpty(parts), true
}

// joinNonEmpty joins cell texts with ", ", dropping empty cells so sparse
// ranges stay readable.
func joinNonEmpty(parts []string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += ", "
		}
		out += p
	}
	return out
}
