package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
)

// auditTrailTool implements workiva_audit_trail.
type auditTrailTool struct{}

// AuditTrail returns the workiva_audit_trail tool.
func AuditTrail() mcpserver.Tool { return auditTrailTool{} }

const auditTrailDescription = `Returns recent entries of the hash-chained audit trail, newest first.

Use this to answer "who changed what and when": every read, sync, and write through this server is one entry carrying the tool, the action, the target (spreadsheet/sheet/range), the before and after values for writes, the Workiva operation URL, and the timestamp. Filter by target to trace one cell, for example target "ss-1/sh-1/B3".

The full chain can be integrity-verified by the operator (workiva-mcp audit verify): every entry hashes the previous entry, so any tampering with a stored row breaks the chain at that sequence number. This tool exposes the entries; verification stays an operator-side command so clients cannot rewrite history through the MCP surface.`

func (auditTrailTool) Name() string { return "workiva_audit_trail" }

func (auditTrailTool) Description() string { return auditTrailDescription }

const (
	auditTrailDefaultLimit = 20
	auditTrailMaxLimit     = 100
)

type auditTrailInput struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum number of entries to return, default 20, capped at 100"`
	Target string `json:"target,omitempty" jsonschema:"only return entries for this exact target, e.g. sp-1/sh-1/B3"`
}

type auditTrailOutput struct {
	Count   int           `json:"count"`
	Entries []audit.Entry `json:"entries"`
}

func (auditTrailTool) RegisterSDK(s *mcp.Server, deps mcpserver.Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "workiva_audit_trail",
		Description: auditTrailDescription,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in auditTrailInput) (*mcp.CallToolResult, auditTrailOutput, error) {
		if err := requireDeps(deps, false, true); err != nil {
			return nil, auditTrailOutput{}, err
		}

		limit := in.Limit
		if limit <= 0 {
			limit = auditTrailDefaultLimit
		}
		if limit > auditTrailMaxLimit {
			limit = auditTrailMaxLimit
		}

		entries, err := deps.Audit.Recent(ctx, limit, in.Target)
		if err != nil {
			return nil, auditTrailOutput{}, fail(err, "the audit trail could not be read")
		}
		return nil, auditTrailOutput{Count: len(entries), Entries: entries}, nil
	})
}
