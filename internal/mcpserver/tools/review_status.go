package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/sectiontext"
)

// reviewStatusTool implements workiva_review_status.
type reviewStatusTool struct{}

// ReviewStatus returns the workiva_review_status tool.
func ReviewStatus() mcpserver.Tool { return reviewStatusTool{} }

const reviewStatusName = "workiva_review_status"

const reviewStatusDescription = `Read-only. Shows the Workiva review tasks on a written report section (esg-environmental or esg-social): each task's status, link, and the reviewer's approve or reject responses with their comments. Use it when the analyst asks whether a section was approved or what the reviewer said. Approvals happen in Workiva, never in chat.`

func (reviewStatusTool) Name() string        { return reviewStatusName }
func (reviewStatusTool) Description() string { return reviewStatusDescription }

type reviewStatusInput struct {
	SectionID string `json:"section_id"`
}

type reviewApproval struct {
	Action  string `json:"action"`
	Comment string `json:"comment,omitempty"`
	User    string `json:"user,omitempty"`
	At      string `json:"at,omitempty"`
}

type reviewTask struct {
	ID        string           `json:"id"`
	Title     string           `json:"title"`
	Status    string           `json:"status"`
	URL       string           `json:"url,omitempty"`
	Approvals []reviewApproval `json:"approvals"`
}

type reviewStatusOutput struct {
	// NLAuditID is filled by the server middleware.
	NLAuditID        string       `json:"nl_audit_id,omitempty"`
	SectionID        string       `json:"section_id"`
	WorkivaSectionID string       `json:"workiva_section_id"`
	Tasks            []reviewTask `json:"tasks"`
}

func (reviewStatusTool) RegisterSDK(s *mcp.Server, deps mcpserver.Deps) {
	approval := objectSchema(map[string]any{
		"action":  boundedSchema("reviewer response, e.g. APPROVE or REJECT", 64),
		"comment": optionalString("reviewer comment", 4000),
		"user":    optionalString("Workiva user ID of the reviewer", 128),
		"at":      optionalString("response time", 64),
	}, []string{"action"})
	task := objectSchema(map[string]any{
		"id":        boundedSchema("task ID", 256),
		"title":     boundedSchema("task title", 512),
		"status":    boundedSchema("task status", 64),
		"url":       optionalString("link to the task in Workiva", 2048),
		"approvals": map[string]any{"type": "array", "maxItems": 50, "items": approval},
	}, []string{"id", "title", "status", "approvals"})
	mcp.AddTool(s, &mcp.Tool{
		Name:        reviewStatusName,
		Description: reviewStatusDescription,
		InputSchema: objectSchema(map[string]any{
			"section_id": boundedSchema("section ID (esg-environmental or esg-social)", 128),
		}, []string{"section_id"}),
		OutputSchema: objectSchema(mergeProperties(commonStructuredOutputProperties(), map[string]any{
			"section_id":         boundedSchema("section ID", 128),
			"workiva_section_id": boundedSchema("Workiva section ID", 128),
			"tasks":              map[string]any{"type": "array", "maxItems": 50, "items": task},
		}), []string{"section_id", "workiva_section_id", "tasks"}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in reviewStatusInput) (*mcp.CallToolResult, reviewStatusOutput, error) {
		reviewer, ok := deps.SectionText.(sectiontext.Reviewer)
		if err := requireDemoDep(ok, "SectionText"); err != nil {
			return nil, reviewStatusOutput{}, err
		}
		workivaID, tasks, err := reviewer.ReviewStatus(ctx, in.SectionID)
		if err != nil {
			return nil, reviewStatusOutput{}, fail(err, "write the section first, or check the section ID")
		}
		out := reviewStatusOutput{SectionID: in.SectionID, WorkivaSectionID: workivaID, Tasks: make([]reviewTask, 0, len(tasks))}
		for _, t := range tasks {
			rt := reviewTask{ID: t.ID, Title: t.Title, Status: t.Status, URL: t.URL, Approvals: []reviewApproval{}}
			for _, a := range t.Approvals {
				rt.Approvals = append(rt.Approvals, reviewApproval(a))
			}
			out.Tasks = append(out.Tasks, rt)
		}
		return nil, out, nil
	})
}
