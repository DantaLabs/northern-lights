package tools

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/demoenv"
	"github.com/dantalabs/northern-lights/internal/recipe"
)

// TestPrepareSectionStubNotImplementedYet is replaced by the L2 lane.
func TestPrepareSectionStubNotImplementedYet(t *testing.T) {
	t.Setenv("NL_DEMO_SECTIONS_DIR", t.TempDir())
	deps := newTestEnv(t, http.NotFound).deps
	recipes, facts, err := recipe.New(demoenv.Env{}, nil)
	if err != nil {
		t.Fatalf("recipe.New: %v", err)
	}
	deps.Recipes, deps.Facts = recipes, facts

	session := serveDemoTools(t, deps)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "workiva_prepare_section", Arguments: map[string]any{"section_id": "environmental"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError || !strings.Contains(resultText(result), "not implemented yet") {
		t.Fatalf("want a not implemented yet error, got %+v", result)
	}
}
