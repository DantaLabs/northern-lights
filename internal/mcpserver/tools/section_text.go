package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/sectiontext"
)

const (
	stageSectionTextName   = "workiva_stage_section_text"
	confirmSectionTextName = "workiva_confirm_section_text"
)

const stageSectionTextDescription = `Stages the narrative blocks and verbatim blocks for a section in the report document and returns a preview, after the number guard checks every number. Call it after workiva_prepare_section, once the narrative blocks are drafted from the narrative facts. For Copilot, prefer blocks_json: construct a JSON array string from your drafted narrative and the exact prepared verbatim blocks, for example [{"kind":"narrative","text":"Draft from prepared facts."},{"kind":"verbatim","verbatim_id":"policy","text":"Exact prepared policy text."}]. Supply exactly one of blocks_json or blocks. Copy every verbatim ID and text from prepare; never rewrite or invent them. Before staging, ask the person for the cause of any material change the facts do not explain. Give each narrative block a short note with your reasoning (why this change leads, where the cause came from, what you left out); the server saves it as a Workiva bookmark named "NL AGENT: ..." on that paragraph, so reviewers see the reasoning beside the text without it entering the report. Show the preview with narrative as a quote, each note in italics under its paragraph and verbatim blocks labelled, and ask before writing. If the server refuses a block, explain the reason in one sentence and fix only what it names; never reword a number into words to get past the number guard.`

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
	Note       string `json:"note,omitempty"`
}

type stageSectionTextInput struct {
	SectionID  string          `json:"section_id"`
	Blocks     json.RawMessage `json:"blocks,omitempty"`
	BlocksJSON json.RawMessage `json:"blocks_json,omitempty"`
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
	AgentNotes       string `json:"agent_notes,omitempty"`
}

func sectionTextBlockSchema() map[string]any {
	return objectSchema(map[string]any{
		"kind":        boundedEnum("narrative", "verbatim"),
		"text":        optionalString("block text", 4000),
		"verbatim_id": optionalString("verbatim block ID, required for verbatim blocks", 128),
		"note":        optionalString("narrative blocks only: one line of reasoning for reviewers, saved as a Workiva bookmark named \"NL AGENT: ...\" on the paragraph, never as report text", 250),
	}, []string{"kind", "text"})
}

func stageSectionTextInputSchema() map[string]any {
	return objectSchema(map[string]any{
		"section_id":  boundedSchema("section ID", 128),
		"blocks":      sectionTextBlocksSchema(),
		"blocks_json": optionalString("Preferred for Copilot: JSON array string of drafted blocks; supply this or blocks, never both. Each block requires kind and text; verbatim blocks also require the exact prepared verbatim_id and text. Maximum 20 blocks, 4000 characters per text.", 100000),
	}, []string{"section_id"})
}

func sectionTextBlocksSchema() map[string]any {
	return map[string]any{"type": "array", "minItems": 1, "maxItems": 20, "items": sectionTextBlockSchema()}
}

func (in stageSectionTextInput) decodeBlocks(schema *jsonschema.Resolved) ([]sectionTextBlock, error) {
	// RawMessage preserves property presence, including an explicitly supplied null.
	if (in.Blocks == nil) == (in.BlocksJSON == nil) {
		return nil, failMsg("supply exactly one of blocks or blocks_json", "construct the blocks from the prepared facts and verbatim text")
	}
	encoded := in.Blocks
	if in.BlocksJSON != nil {
		var value string
		if string(in.BlocksJSON) == "null" {
			return nil, failMsg("blocks_json must be a JSON array string", "supply the drafted blocks as JSON")
		}
		if err := json.Unmarshal(in.BlocksJSON, &value); err != nil {
			return nil, fmt.Errorf("blocks_json must be a JSON array string: %w", err)
		}
		encoded = []byte(value)
	}
	var value any
	// Unmarshal requires exactly one JSON value and rejects trailing values.
	if err := json.Unmarshal(encoded, &value); err != nil {
		return nil, fmt.Errorf("invalid section text blocks JSON: %w", err)
	}
	if err := schema.Validate(value); err != nil {
		return nil, fmt.Errorf("invalid section text blocks: %w", err)
	}
	var blocks []sectionTextBlock
	if err := json.Unmarshal(encoded, &blocks); err != nil {
		return nil, fmt.Errorf("invalid section text blocks: %w", err)
	}
	return blocks, nil
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
		"agent_notes":        optionalString("NL AGENT bookmarks: completed, failed, or empty when there were no notes; a failed note never undoes the written text", 32),
	}), []string{"section_id", "document_id", "workiva_section_id", "status"})
}

func (stageSectionTextTool) RegisterSDK(s *mcp.Server, deps mcpserver.Deps) {
	// Resolve the same bounded schema advertised for array clients, so JSON string
	// clients cannot bypass required properties, types, lengths or extra-key checks.
	encodedSchema, err := json.Marshal(sectionTextBlocksSchema())
	if err != nil {
		panic(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(encodedSchema, &schema); err != nil {
		panic(err)
	}
	blocksSchema, err := schema.Resolve(nil)
	if err != nil {
		panic(err)
	}
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
		inputBlocks, err := in.decodeBlocks(blocksSchema)
		if err != nil {
			return nil, stageSectionTextOutput{}, err
		}
		blocks := make([]sectiontext.Block, len(inputBlocks))
		for i, b := range inputBlocks {
			blocks[i] = sectiontext.Block{Kind: b.Kind, Text: b.Text, VerbatimID: b.VerbatimID, Note: b.Note}
		}
		preview, err := deps.SectionText.Stage(ctx, actor, in.SectionID, blocks)
		if err != nil {
			return nil, stageSectionTextOutput{}, err
		}
		outBlocks := make([]sectionTextBlock, len(preview.Blocks))
		var summary strings.Builder
		for i, b := range preview.Blocks {
			outBlocks[i] = sectionTextBlock{Kind: b.Kind, Text: b.Text, VerbatimID: b.VerbatimID, Note: b.Note}
			if i > 0 {
				summary.WriteString("\n\n")
			}
			summary.WriteString("[" + b.Kind + "]\n" + b.Text)
			if b.Note != "" {
				summary.WriteString("\n[note] " + sectiontext.NotePrefix + b.Note)
			}
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
		return nil, confirmSectionTextOutput{SectionID: result.SectionID, DocumentID: result.DocumentID, WorkivaSectionID: result.WorkivaSectionID, AuditID: result.AuditID, Status: result.Status, AgentNotes: result.Notes}, nil
	})
}
