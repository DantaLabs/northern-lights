package tools

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/mcpserver"
)

// searchFieldsTool implements workiva_search_fields.
type searchFieldsTool struct{}

// SearchFields returns the workiva_search_fields tool.
func SearchFields() mcpserver.Tool { return searchFieldsTool{} }

const searchFieldsDescription = `Searches the semantic field mapping for fields matching a natural-language query.

Call this tool FIRST whenever the user refers to a report value in plain language, such as "scope 2 energy", "water consumption", or "turnover". It resolves those references to mapped field names with their exact spreadsheet, sheet, and cell range, which you then pass to workiva_get_field for the value or to write tools.

Matches come from field names and their aliases. Results include the field's range, description, and, when a fresh cached read exists, the current value. If nothing matches, try rephrasing or list connected spreadsheets with workiva_list_spreadsheets.`

func (searchFieldsTool) Name() string { return "workiva_search_fields" }

func (searchFieldsTool) Description() string { return searchFieldsDescription }

type searchFieldsInput struct {
	Query string `json:"query" jsonschema:"natural-language search terms, e.g. scope 2 energy"`
}

type searchFieldsEntry struct {
	Name          string `json:"name"`
	SpreadsheetID string `json:"spreadsheet_id"`
	SheetID       string `json:"sheet_id"`
	Range         string `json:"range"`
	FieldType     string `json:"field_type"`
	Description   string `json:"description,omitempty"`
	CachedValue   string `json:"cached_value,omitempty"`
	CachedAt      string `json:"cached_at,omitempty"`
}

type searchFieldsOutput struct {
	Fields []searchFieldsEntry `json:"fields"`
}

func (searchFieldsTool) RegisterSDK(s *mcp.Server, deps mcpserver.Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "workiva_search_fields",
		Description: searchFieldsDescription,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in searchFieldsInput) (*mcp.CallToolResult, searchFieldsOutput, error) {
		if err := requireDeps(deps, true, false); err != nil {
			return nil, searchFieldsOutput{}, err
		}
		if in.Query == "" {
			return nil, searchFieldsOutput{}, failMsg("query must not be empty", "pass the natural-language phrase the user used, such as scope 2 energy")
		}

		fields, err := deps.Store.SearchFields(ctx, in.Query)
		if err != nil {
			return nil, searchFieldsOutput{}, fail(err, "the mapping store could not search fields")
		}

		ttl := cacheTTL(deps)
		entries := make([]searchFieldsEntry, 0, len(fields))
		for _, f := range fields {
			entry := searchFieldsEntry{
				Name:          f.Name,
				SpreadsheetID: f.SpreadsheetID,
				SheetID:       f.SheetID,
				Range:         f.CellRange,
				FieldType:     f.FieldType,
				Description:   f.Description,
			}
			if ttl > 0 {
				if cells, err := deps.Store.GetCachedCells(ctx, f.SpreadsheetID, f.SheetID, ttl); err == nil {
					if value, ok := valueFromCells(cells, f.CellRange); ok {
						entry.CachedValue = value
						entry.CachedAt = cells[0].FetchedAt.UTC().Format(time.RFC3339)
					}
				}
			}
			entries = append(entries, entry)
		}
		return nil, searchFieldsOutput{Fields: entries}, nil
	})
}
