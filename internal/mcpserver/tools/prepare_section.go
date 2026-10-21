package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/recipe"
)

// prepareSectionTool implements workiva_prepare_section.
type prepareSectionTool struct{}

// PrepareSection returns the workiva_prepare_section tool.
func PrepareSection() mcpserver.Tool { return prepareSectionTool{} }

const prepareSectionName = "workiva_prepare_section"

const prepareSectionDescription = `Prepares one report section from its section recipe, returning findings, narrative facts, verbatim blocks and a change set preview. Call it first when a consultant asks to update a section, before staging any section text.`

func (prepareSectionTool) Name() string { return prepareSectionName }

func (prepareSectionTool) Description() string { return prepareSectionDescription }

type prepareSectionInput struct {
	SectionID string `json:"section_id"`
}

type prepareFinding struct {
	Kind     string `json:"kind"`
	Field    string `json:"field"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type prepareFact struct {
	Key       string   `json:"key"`
	Label     string   `json:"label"`
	Unit      string   `json:"unit,omitempty"`
	Current   string   `json:"current"`
	Prior     string   `json:"prior,omitempty"`
	Change    string   `json:"change,omitempty"`
	ChangePct string   `json:"change_pct,omitempty"`
	Direction string   `json:"direction"`
	Material  bool     `json:"material"`
	Display   []string `json:"display"`
}

type prepareVerbatim struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

type prepareChangeSetItem struct {
	FieldName     string `json:"field_name"`
	SpreadsheetID string `json:"spreadsheet_id"`
	SheetID       string `json:"sheet_id"`
	Range         string `json:"range"`
	Before        string `json:"before,omitempty"`
	After         string `json:"after,omitempty"`
	Source        string `json:"source,omitempty"`
}

type prepareChangeSet struct {
	ChangeSetID  string                 `json:"change_set_id"`
	ConfirmToken string                 `json:"confirm_token"`
	ExpiresAt    string                 `json:"expires_at"`
	Items        []prepareChangeSetItem `json:"items"`
}

type prepareSectionOutput struct {
	// NLAuditID is filled by the server middleware.
	NLAuditID string            `json:"nl_audit_id,omitempty"`
	SectionID string            `json:"section_id"`
	Findings  []prepareFinding  `json:"findings"`
	Facts     []prepareFact     `json:"facts"`
	Verbatim  []prepareVerbatim `json:"verbatim"`
	ChangeSet prepareChangeSet  `json:"change_set"`
	Summary   string            `json:"summary"`
	NextStep  string            `json:"next_step"`
}

func prepareSectionInputSchema() map[string]any {
	return objectSchema(map[string]any{
		"section_id": boundedSchema("section ID of the section recipe to prepare", 128),
	}, []string{"section_id"})
}

func prepareSectionOutputSchema() map[string]any {
	finding := objectSchema(map[string]any{
		"kind":     boundedSchema("finding kind", 32),
		"field":    boundedSchema("field the finding concerns", 256),
		"severity": boundedSchema("finding severity", 32),
		"message":  boundedSchema("finding message", 1024),
	}, []string{"kind", "field", "severity", "message"})
	fact := objectSchema(map[string]any{
		"key":        boundedSchema("narrative fact key", 256),
		"label":      boundedSchema("narrative fact label", 256),
		"unit":       optionalString("unit of the value", 32),
		"current":    boundedSchema("current value, canonical decimal string", 64),
		"prior":      optionalString("prior period value, canonical decimal string", 64),
		"change":     optionalString("absolute change, canonical decimal string", 64),
		"change_pct": optionalString("percentage change, canonical decimal string", 64),
		"direction":  boundedEnum("up", "down", "flat", "new"),
		"material":   map[string]any{"type": "boolean"},
		"display": map[string]any{"type": "array", "maxItems": 16,
			"items": boundedSchema("approved display form of the value, accepted by the number guard", 128)},
	}, []string{"key", "label", "current", "direction", "material", "display"})
	verbatim := objectSchema(map[string]any{
		"id":    boundedSchema("verbatim block ID", 128),
		"title": boundedSchema("verbatim block title", 256),
		"text":  boundedSchema("verbatim block text, exact", 16000),
	}, []string{"id", "title", "text"})
	item := objectSchema(map[string]any{
		"field_name":     boundedSchema("field name", 256),
		"spreadsheet_id": boundedSchema("spreadsheet ID", 128),
		"sheet_id":       boundedSchema("sheet ID", 128),
		"range":          boundedSchema("cell range", 512),
		"before":         optionalString("value before the write", 4096),
		"after":          optionalString("value after the write", 4096),
		"source":         optionalString("where the new value comes from", 256),
	}, []string{"field_name", "spreadsheet_id", "sheet_id", "range"})
	changeSet := objectSchema(map[string]any{
		"change_set_id": boundedSchema("change set ID", 128),
		"confirm_token": boundedSchema("confirmation token for the change set", 4096),
		"expires_at":    boundedSchema("expiry time, RFC 3339", 64),
		"items":         map[string]any{"type": "array", "maxItems": 500, "items": item},
	}, []string{"change_set_id", "confirm_token", "expires_at", "items"})
	return objectSchema(mergeProperties(commonStructuredOutputProperties(), map[string]any{
		"section_id": boundedSchema("section ID", 128),
		"findings":   map[string]any{"type": "array", "maxItems": 500, "items": finding},
		"facts":      map[string]any{"type": "array", "maxItems": 500, "items": fact},
		"verbatim":   map[string]any{"type": "array", "maxItems": 50, "items": verbatim},
		"change_set": changeSet,
		"summary":    optionalString("change set summary, one line per item", 262144),
		"next_step":  boundedSchema("what to do next", 1024),
	}), []string{"section_id", "findings", "facts", "verbatim", "change_set", "summary", "next_step"})
}

const prepareSectionNextStep = "Show the analyst the findings first, then the change set summary. Ask for approval, and after the analyst approves call workiva_confirm_change_set with change_set_id and confirm_token."

func (prepareSectionTool) RegisterSDK(s *mcp.Server, deps mcpserver.Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:         prepareSectionName,
		Description:  prepareSectionDescription,
		InputSchema:  prepareSectionInputSchema(),
		OutputSchema: prepareSectionOutputSchema(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in prepareSectionInput) (*mcp.CallToolResult, prepareSectionOutput, error) {
		if err := requireDemoDep(deps.Recipes != nil, "Recipes"); err != nil {
			return nil, prepareSectionOutput{}, err
		}
		actor := mcpserver.ActorFromContextOrRequest(ctx, req, deps.ActorHeader)

		sections := deps.Recipes.Sections()
		known := false
		for _, section := range sections {
			if section.ID == in.SectionID {
				known = true
				break
			}
		}
		if !known {
			return nil, prepareSectionOutput{}, unknownSectionError(in.SectionID, sections)
		}

		prepared, err := deps.Recipes.Prepare(ctx, actor, in.SectionID)
		if err != nil {
			return nil, prepareSectionOutput{}, fail(err, "fix the reported problem and call workiva_prepare_section again")
		}
		return nil, newPrepareSectionOutput(prepared), nil
	})
}

func unknownSectionError(sectionID string, sections []recipe.SectionInfo) error {
	msg := fmt.Sprintf("unknown section ID %q", sectionID)
	if len(sections) == 0 {
		return failMsg(msg, "no section recipes are loaded; check NL_DEMO_SECTIONS_DIR")
	}
	parts := make([]string, 0, len(sections))
	for _, section := range sections {
		parts = append(parts, fmt.Sprintf("%s (%s)", section.ID, section.Title))
	}
	return failMsg(msg, "available sections: "+strings.Join(parts, ", "))
}

// newPrepareSectionOutput maps a prepared section to the tool output. Every
// array is non-nil because the output schema requires arrays.
func newPrepareSectionOutput(p recipe.Prepared) prepareSectionOutput {
	out := prepareSectionOutput{
		SectionID: p.SectionID,
		Findings:  make([]prepareFinding, 0, len(p.Findings)),
		Facts:     make([]prepareFact, 0, len(p.Facts)),
		Verbatim:  make([]prepareVerbatim, 0, len(p.Verbatim)),
		ChangeSet: prepareChangeSet{
			ChangeSetID:  p.ChangeSet.ID,
			ConfirmToken: p.ChangeSet.Token,
			ExpiresAt:    p.ChangeSet.ExpiresAt.UTC().Format(time.RFC3339),
			Items:        make([]prepareChangeSetItem, 0, len(p.ChangeSet.Items)),
		},
		Summary:  p.ChangeSet.Summary,
		NextStep: prepareSectionNextStep,
	}
	for _, f := range p.Findings {
		out.Findings = append(out.Findings, prepareFinding{Kind: f.Kind, Field: f.Field, Severity: f.Severity, Message: f.Message})
	}
	for _, f := range p.Facts {
		display := make([]string, 0, len(f.Display))
		display = append(display, f.Display...)
		out.Facts = append(out.Facts, prepareFact{
			Key: f.Key, Label: f.Label, Unit: f.Unit, Current: f.Current, Prior: f.Prior,
			Change: f.Change, ChangePct: f.ChangePct, Direction: f.Direction, Material: f.Material, Display: display,
		})
	}
	for _, v := range p.Verbatim {
		out.Verbatim = append(out.Verbatim, prepareVerbatim{ID: v.ID, Title: v.Title, Text: v.Text})
	}
	for _, item := range p.ChangeSet.Items {
		out.ChangeSet.Items = append(out.ChangeSet.Items, prepareChangeSetItem{
			FieldName: item.FieldName, SpreadsheetID: item.SpreadsheetID, SheetID: item.SheetID,
			Range: item.Range, Before: item.Before, After: item.After, Source: item.Source,
		})
	}
	return out
}
