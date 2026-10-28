package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/changeset"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
)

// revertChangeSetTool implements workiva_revert_change_set.
type revertChangeSetTool struct{}

// RevertChangeSet returns the workiva_revert_change_set tool.
func RevertChangeSet() mcpserver.Tool { return revertChangeSetTool{} }

const revertChangeSetName = "workiva_revert_change_set"

const revertChangeSetDescription = `Stages an undo of a change set you confirmed earlier: every cell it wrote goes back to the value it had before. Find the change_set_id in workiva_audit_trail (before_json of workiva_confirm_change_set entries). Nothing is written yet: show the analyst which fields go back to which values, ask for approval, then call workiva_confirm_change_set with the returned change_set_id and confirm_token. Only your own completed writes can be reverted.`

func (revertChangeSetTool) Name() string        { return revertChangeSetName }
func (revertChangeSetTool) Description() string { return revertChangeSetDescription }

type revertChangeSetInput struct {
	ChangeSetID string `json:"change_set_id"`
}

type revertChangeSetOutput struct {
	// NLAuditID is filled by the server middleware.
	NLAuditID string           `json:"nl_audit_id,omitempty"`
	ChangeSet prepareChangeSet `json:"change_set"`
	Summary   string           `json:"summary"`
}

func (revertChangeSetTool) RegisterSDK(s *mcp.Server, deps mcpserver.Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        revertChangeSetName,
		Description: revertChangeSetDescription,
		InputSchema: objectSchema(map[string]any{
			"change_set_id": boundedSchema("ID of the confirmed change set to undo", 128),
		}, []string{"change_set_id"}),
		OutputSchema: objectSchema(mergeProperties(commonStructuredOutputProperties(), map[string]any{
			"change_set": changeSetPreviewSchema(),
			"summary":    boundedSchema("one line per field: before -> after", 200000),
		}), []string{"change_set", "summary"}),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in revertChangeSetInput) (*mcp.CallToolResult, revertChangeSetOutput, error) {
		reverter, ok := deps.ChangeSets.(changeset.Reverter)
		if err := requireDemoDep(ok, "ChangeSets"); err != nil {
			return nil, revertChangeSetOutput{}, err
		}
		actor := mcpserver.ActorFromContextOrRequest(ctx, req, deps.ActorHeader)
		if actor == "" || actor == mcpserver.DefaultActor {
			return nil, revertChangeSetOutput{}, failMsg("reverting requires an authenticated actor", "sign in and try again")
		}
		p, err := reverter.StageRevert(ctx, actor, in.ChangeSetID)
		if err != nil {
			return nil, revertChangeSetOutput{}, fail(err, "check the change_set_id in workiva_audit_trail")
		}
		return nil, revertChangeSetOutput{ChangeSet: newPrepareChangeSet(p), Summary: p.Summary}, nil
	})
}
