package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/recipe"
)

// compareFieldsTool implements workiva_compare_fields.
type compareFieldsTool struct{}

// CompareFields returns the workiva_compare_fields tool.
func CompareFields() mcpserver.Tool { return compareFieldsTool{} }

const compareFieldsName = "workiva_compare_fields"

const compareFieldsDescription = `Compares report fields with the prior period, read-only. Returns current and prior values with their cells, the server-computed change and percentage change, direction, and whether the change is material for the section. Use it for any period-comparison question instead of computing numbers yourself. Nothing is staged or written.`

func (compareFieldsTool) Name() string { return compareFieldsName }

func (compareFieldsTool) Description() string { return compareFieldsDescription }

type compareFieldsInput struct {
	SectionID string   `json:"section_id"`
	Fields    []string `json:"fields,omitempty"`
}

type compareCell struct {
	SpreadsheetID string `json:"spreadsheet_id"`
	SheetID       string `json:"sheet_id"`
	Range         string `json:"range"`
}

type compareItem struct {
	prepareFact
	Missing     bool         `json:"missing"`
	CurrentCell compareCell  `json:"current_cell"`
	PriorCell   *compareCell `json:"prior_cell,omitempty"`
}

type compareFieldsOutput struct {
	// NLAuditID is filled by the server middleware.
	NLAuditID      string        `json:"nl_audit_id,omitempty"`
	SectionID      string        `json:"section_id"`
	SectionTitle   string        `json:"section_title"`
	Period         string        `json:"period"`
	PriorPeriod    string        `json:"prior_period"`
	MaterialityPct string        `json:"materiality_pct"`
	Items          []compareItem `json:"items"`
	NextStep       string        `json:"next_step"`
}

const compareFieldsNextStep = "Quote values and percentages only as given here (display forms or the values with their unit). Say whether each change is material for the section; do not calculate anything."

func compareFieldsInputSchema() map[string]any {
	return objectSchema(map[string]any{
		"section_id": boundedSchema("section ID whose fields to compare (esg-environmental or esg-social)", 128),
		"fields": map[string]any{"type": "array", "maxItems": 50,
			"items": boundedSchema("field key, or part of a field label, e.g. energy or business travel; omit for all fields", 256)},
	}, []string{"section_id"})
}

func compareFieldsOutputSchema() map[string]any {
	cell := objectSchema(map[string]any{
		"spreadsheet_id": boundedSchema("spreadsheet ID", 128),
		"sheet_id":       boundedSchema("sheet ID", 128),
		"range":          boundedSchema("cell range", 512),
	}, []string{"spreadsheet_id", "sheet_id", "range"})
	item := objectSchema(map[string]any{
		"key":          boundedSchema("field key", 256),
		"label":        boundedSchema("field label", 256),
		"unit":         optionalString("unit of the stored value", 32),
		"current":      map[string]any{"type": "string", "maxLength": 64, "description": "current value, canonical decimal string; empty when missing"},
		"prior":        optionalString("prior period value, canonical decimal string", 64),
		"change":       optionalString("absolute change, canonical decimal string", 64),
		"change_pct":   optionalString("percentage change, canonical decimal string", 64),
		"direction":    map[string]any{"type": "string", "maxLength": 8, "description": "up, down, flat or new; empty when missing"},
		"material":     map[string]any{"type": "boolean"},
		"display":      map[string]any{"type": "array", "maxItems": 16, "items": boundedSchema("approved display form", 128)},
		"missing":      map[string]any{"type": "boolean"},
		"current_cell": cell,
		"prior_cell":   cell,
	}, []string{"key", "label", "current", "direction", "material", "display", "missing", "current_cell"})
	return objectSchema(mergeProperties(commonStructuredOutputProperties(), map[string]any{
		"section_id":      boundedSchema("section ID", 128),
		"section_title":   boundedSchema("section title", 256),
		"period":          boundedSchema("reporting period", 32),
		"prior_period":    boundedSchema("prior period", 32),
		"materiality_pct": boundedSchema("materiality threshold in percent", 32),
		"items":           map[string]any{"type": "array", "maxItems": 500, "items": item},
		"next_step":       boundedSchema("what to do next", 1024),
	}), []string{"section_id", "section_title", "period", "prior_period", "materiality_pct", "items", "next_step"})
}

func (compareFieldsTool) RegisterSDK(s *mcp.Server, deps mcpserver.Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:         compareFieldsName,
		Description:  compareFieldsDescription,
		InputSchema:  compareFieldsInputSchema(),
		OutputSchema: compareFieldsOutputSchema(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in compareFieldsInput) (*mcp.CallToolResult, compareFieldsOutput, error) {
		comparer, ok := deps.Recipes.(recipe.Comparer)
		if err := requireDemoDep(ok, "Recipes"); err != nil {
			return nil, compareFieldsOutput{}, err
		}
		sections := deps.Recipes.Sections()
		known := false
		for _, section := range sections {
			known = known || section.ID == in.SectionID
		}
		if !known {
			return nil, compareFieldsOutput{}, unknownSectionError(in.SectionID, sections)
		}
		got, err := comparer.Compare(ctx, in.SectionID, in.Fields)
		if err != nil {
			return nil, compareFieldsOutput{}, fail(err, "fix the reported problem and call workiva_compare_fields again")
		}
		return nil, newCompareFieldsOutput(got), nil
	})
}

func newCompareFieldsOutput(c recipe.Comparison) compareFieldsOutput {
	out := compareFieldsOutput{
		SectionID: c.SectionID, SectionTitle: c.SectionTitle, Period: c.Period, PriorPeriod: c.PriorPeriod,
		MaterialityPct: c.MaterialityPct, Items: make([]compareItem, 0, len(c.Items)), NextStep: compareFieldsNextStep,
	}
	for _, it := range c.Items {
		item := compareItem{
			prepareFact: prepareFact{
				Key: it.Key, Label: it.Label, Unit: it.Unit, Current: it.Current, Prior: it.Prior, Change: it.Change,
				ChangePct: it.ChangePct, Direction: it.Direction, Material: it.Material, Display: append([]string{}, it.Display...),
			},
			Missing:     it.Missing,
			CurrentCell: compareCell{SpreadsheetID: it.CurrentCell.SpreadsheetID, SheetID: it.CurrentCell.SheetID, Range: it.CurrentCell.Range},
		}
		if it.PriorCell != nil {
			item.PriorCell = &compareCell{SpreadsheetID: it.PriorCell.SpreadsheetID, SheetID: it.PriorCell.SheetID, Range: it.PriorCell.Range}
		}
		out.Items = append(out.Items, item)
	}
	return out
}
