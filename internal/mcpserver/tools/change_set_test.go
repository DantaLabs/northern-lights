package tools

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/changeset"
	"github.com/dantalabs/northern-lights/internal/demoenv"
)

// TestConfirmChangeSetStubNotImplementedYet is replaced by the L1 lane.
func TestConfirmChangeSetStubNotImplementedYet(t *testing.T) {
	deps := newTestEnv(t, http.NotFound).deps
	deps.ChangeSets = changeset.New(demoenv.Env{})

	session := serveDemoTools(t, deps)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "workiva_confirm_change_set", Arguments: map[string]any{"change_set_id": "cs-1", "confirm_token": "tok"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError || !strings.Contains(resultText(result), "not implemented yet") {
		t.Fatalf("want a not implemented yet error, got %+v", result)
	}
}
