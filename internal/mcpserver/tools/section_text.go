package tools

import (
	"context"
	"github.com/dantalabs/northern-lights/internal/sectiontext"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/mcpserver"
)

const (
	stageSectionTextName   = "workiva_stage_section_text"
	confirmSectionTextName = "workiva_confirm_section_text"
)

const stageSectionTextDescription = `Stages the narrative blocks and verbatim blocks for a section in the report document and returns a preview, after the number guard checks every number. Call it after workiva_prepare_section, once the narrative blocks are drafted from the narrative facts.`

const confirmSectionTextDescription = `Writes previously staged section text into the report document after the person approved the preview. Call it only after workiva_stage_section_text returned a preview and the person explicitly approved it.`

// stageSectionTextTool implements workiva_stage_section_text.
type stageSectionTextTool struct{}

// StageSectionText returns the workiva_stage_section_text tool.
func StageSectionText() mcpserver.Tool { return stageSectionTextTool{} }

// confirmSectionTextTool implements workiva_confirm_section_text.
type confirmSectionTextTool struct{}

// ConfirmSectionText returns the workiva_confirm_section_text tool.
func ConfirmSectionText() mcpserver.Tool { return confirmSectionTextTool{} }

func (stageSectionTextTool) Name() string        { return stageSectionTextName }
func (stageSectionTextTool) Description() string { return stageSectionTextDescription }

func (confirmSectionTextTool) Name() string        { return confirmSectionTextName }
func (confirmSectionTextTool) Description() string { return confirmSectionTextDescription }

type sectionTextBlock struct {
	Kind       string `json:"kind"`
	Text       string `json:"text"`
	VerbatimID string `json:"verbatim_id,omitempty"`
}

type stageSectionTextInput struct {
	SectionID string             `json:"section_id"`
	Blocks    []sectionTextBlock `json:"blocks"`
}

type stageSectionTextOutput struct {
	// NLAuditID is filled by the server middleware.
	NLAuditID    string             `json:"nl_audit_id,omitempty"`
	TextID       string             `json:"text_id"`
	ConfirmToken string             `json:"confirm_token"`
	SectionID    string             `json:"section_id"`
	SectionTitle string             `json:"section_title"`
	Blocks       []sectionTextBlock `json:"blocks"`
	ExpiresAt    string             `json:"expires_at"`
	Summary      string             `json:"summary"`
}

type confirmSectionTextInput struct {
	TextID       string `json:"text_id"`
	ConfirmToken string `json:"confirm_token"`
}

type confirmSectionTextOutput struct {
	// NLAuditID is filled by the server middleware.
	NLAuditID        string `json:"nl_audit_id,omitempty"`
	Status           string `json:"status"`
	SectionID        string `json:"section_id"`
	DocumentID       string `json:"document_id"`
	WorkivaSectionID string `json:"workiva_section_id"`
	AuditID          string `json:"audit_id,omitempty"`
}

func sectionTextBlockSchema() map[string]any {
	return objectSchema(map[string]any{
		"kind":        boundedEnum("narrative", "verbatim"),
		"text":        optionalString("block text", 4000),
		"verbatim_id": optionalString("verbatim block ID, required for verbatim blocks", 128),
	}, []string{"kind", "text"})
}

func stageSectionTextInputSchema() map[string]any {
	return objectSchema(map[string]any{
		"section_id": boundedSchema("section ID", 128),
		"blocks":     map[string]any{"type": "array", "minItems": 1, "maxItems": 20, "items": sectionTextBlockSchema()},
	}, []string{"section_id", "blocks"})
}

func stageSectionTextOutputSchema() map[string]any {
	return objectSchema(mergeProperties(commonStructuredOutputProperties(), map[string]any{
		"text_id":       boundedSchema("staged text ID", 128),
		"confirm_token": boundedSchema("confirmation token for the staged text", 4096),
		"section_id":    boundedSchema("section ID", 128),
		"section_title": boundedSchema("section title", 256),
		"blocks":        map[string]any{"type": "array", "minItems": 1, "maxItems": 20, "items": sectionTextBlockSchema()},
		"expires_at":    boundedSchema("expiry time, RFC 3339", 64),
		"summary":       optionalString("markdown preview of the staged text", 90000),
	}), []string{"text_id", "confirm_token", "section_id", "section_title", "blocks", "expires_at", "summary"})
}

func confirmSectionTextInputSchema() map[string]any {
	return objectSchema(map[string]any{
		"text_id":       boundedSchema("staged text ID from workiva_stage_section_text", 128),
		"confirm_token": boundedSchema("confirmation token from workiva_stage_section_text", 4096),
	}, []string{"text_id", "confirm_token"})
}

func confirmSectionTextOutputSchema() map[string]any {
	return objectSchema(mergeProperties(commonStructuredOutputProperties(), map[string]any{
		"section_id":         boundedSchema("section ID", 128),
		"document_id":        boundedSchema("report document ID", 128),
		"workiva_section_id": optionalString("Workiva section ID; empty if section creation is uncertain", 128),
		"audit_id":           optionalString("audit ID of the write", 128),
		"status":             boundedEnum("completed", "mismatch", "reconciliation_required"),
	}), []string{"section_id", "document_id", "workiva_section_id", "status"})
}

func (stageSectionTextTool) RegisterSDK(s *mcp.Server, deps mcpserver.Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:         stageSectionTextName,
		Description:  stageSectionTextDescription,
		InputSchema:  stageSectionTextInputSchema(),
		OutputSchema: stageSectionTextOutputSchema(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in stageSectionTextInput) (*mcp.CallToolResult, stageSectionTextOutput, error) {
		if err := requireDemoDep(deps.SectionText != nil, "SectionText"); err != nil {
			return nil, stageSectionTextOutput{}, err
		}
		actor := mcpserver.ActorFromContextOrRequest(ctx, req, deps.ActorHeader)
		if actor == "" || actor == mcpserver.DefaultActor {
			return nil, stageSectionTextOutput{}, failMsg("section text requires an authenticated actor", "authenticate and prepare the section again")
		}
		blocks := make([]sectiontext.Block, len(in.Blocks))
		for i, b := range in.Blocks {
			blocks[i] = sectiontext.Block{Kind: b.Kind, Text: b.Text, VerbatimID: b.VerbatimID}
		}
		preview, err := deps.SectionText.Stage(ctx, actor, in.SectionID, blocks)
		if err != nil {
			return nil, stageSectionTextOutput{}, err
		}
		outBlocks := make([]sectionTextBlock, len(preview.Blocks))
		var summary strings.Builder
		for i, b := range preview.Blocks {
			outBlocks[i] = sectionTextBlock{Kind: b.Kind, Text: b.Text, VerbatimID: b.VerbatimID}
			if i > 0 {
				summary.WriteString("\n\n")
			}
			summary.WriteString("[" + b.Kind + "]\n" + b.Text)
		}
		return nil, stageSectionTextOutput{TextID: preview.ID, ConfirmToken: preview.Token, SectionID: preview.SectionID, SectionTitle: preview.SectionTitle, Blocks: outBlocks, ExpiresAt: preview.ExpiresAt.Format(time.RFC3339), Summary: summary.String()}, nil
	})
}

func (confirmSectionTextTool) RegisterSDK(s *mcp.Server, deps mcpserver.Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:         confirmSectionTextName,
		Description:  confirmSectionTextDescription,
		InputSchema:  confirmSectionTextInputSchema(),
		OutputSchema: confirmSectionTextOutputSchema(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in confirmSectionTextInput) (*mcp.CallToolResult, confirmSectionTextOutput, error) {
		if err := requireDemoDep(deps.SectionText != nil, "SectionText"); err != nil {
			return nil, confirmSectionTextOutput{}, err
		}
		actor := mcpserver.ActorFromContextOrRequest(ctx, req, deps.ActorHeader)
		if actor == "" || actor == mcpserver.DefaultActor {
			return nil, confirmSectionTextOutput{}, failMsg("section text requires an authenticated actor", "authenticate and prepare the section again")
		}
		result, err := deps.SectionText.Confirm(ctx, actor, in.TextID, in.ConfirmToken)
		if err != nil {
			return nil, confirmSectionTextOutput{}, err
		}
		return nil, confirmSectionTextOutput{SectionID: result.SectionID, DocumentID: result.DocumentID, WorkivaSectionID: result.WorkivaSectionID, AuditID: result.AuditID, Status: result.Status}, nil
	})
}
