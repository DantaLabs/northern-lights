package tools

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/demoenv"
	"github.com/dantalabs/northern-lights/internal/sectiontext"
)

// TestSectionTextStubsNotImplementedYet is replaced by the L3 lane.
func TestSectionTextStubsNotImplementedYet(t *testing.T) {
	deps := newTestEnv(t, http.NotFound).deps
	deps.SectionText = sectiontext.New(demoenv.Env{}, nil, nil)

	session := serveDemoTools(t, deps)
	calls := map[string]map[string]any{
		"workiva_stage_section_text": {
			"section_id": "environmental",
			"blocks":     []any{map[string]any{"kind": "narrative", "text": "Energy use fell."}},
		},
		"workiva_confirm_section_text": {"text_id": "tx-1", "confirm_token": "tok"},
	}
	for name, args := range calls {
		t.Run(name, func(t *testing.T) {
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if !result.IsError || !strings.Contains(resultText(result), "not implemented yet") {
				t.Fatalf("want a not implemented yet error, got %+v", result)
			}
		})
	}
}
