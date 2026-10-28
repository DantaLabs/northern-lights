package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/mcpserver"
)

// serveDemoTools serves Demo() over streamable HTTP and returns a connected
// client session.
func serveDemoTools(t *testing.T, deps mcpserver.Deps) *mcp.ClientSession {
	t.Helper()
	reg := mcpserver.NewRegistry()
	for _, tool := range Demo() {
		reg.Register(tool)
	}
	handler, err := mcpserver.New(deps, reg, &mcpserver.Options{APIToken: "test-token"})
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "dev"}, nil).Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: bearerHTTPClient("test-token")}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

var demoToolNames = []string{
	"workiva_prepare_section",
	"workiva_confirm_change_set",
	"workiva_stage_section_text",
	"workiva_confirm_section_text",
	"workiva_compare_fields",
	"workiva_revert_change_set",
	"workiva_review_status",
}

func TestDemoToolsOrder(t *testing.T) {
	demo := Demo()
	if len(demo) != len(demoToolNames) {
		t.Fatalf("Demo() returned %d tools, want %d", len(demo), len(demoToolNames))
	}
	for i, tool := range demo {
		if tool.Name() != demoToolNames[i] {
			t.Errorf("Demo()[%d] = %q, want %q", i, tool.Name(), demoToolNames[i])
		}
		if tool.Description() == "" {
			t.Errorf("%s has an empty description", tool.Name())
		}
	}

	reg := mcpserver.NewRegistry()
	for _, tool := range All() {
		reg.Register(tool)
	}
	for _, tool := range demo {
		reg.Register(tool)
	}
	names := reg.Names()
	if len(names) != 20 {
		t.Fatalf("registry has %d names, want 20: %v", len(names), names)
	}
	for i, want := range demoToolNames {
		if got := names[13+i]; got != want {
			t.Errorf("names[%d] = %q, want %q", 13+i, got, want)
		}
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "dev"}, nil)
	if err := reg.Bind(server, newTestEnv(t, http.NotFound).deps); err != nil {
		t.Fatalf("Bind: %v", err)
	}
}

func TestDemoToolSchemasAreClosedAndBounded(t *testing.T) {
	session := serveDemoTools(t, newTestEnv(t, http.NotFound).deps)
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(list.Tools) != len(demoToolNames) {
		t.Fatalf("listed %d tools, want %d", len(list.Tools), len(demoToolNames))
	}
	for _, tool := range list.Tools {
		for label, schema := range map[string]any{"input": tool.InputSchema, "output": tool.OutputSchema} {
			path := tool.Name + "." + label
			object, ok := schema.(map[string]any)
			if !ok || object["type"] != "object" {
				t.Errorf("%s: schema is not an object: %v", path, schema)
				continue
			}
			verifyClosedBoundedSchema(t, path, schema)
			walkSchema(t, path, schema)
		}
	}
}

func TestDemoToolsNameMissingDependency(t *testing.T) {
	session := serveDemoTools(t, newTestEnv(t, http.NotFound).deps)
	block := map[string]any{"kind": "narrative", "text": "Energy use fell."}
	calls := []struct {
		tool string
		args map[string]any
		dep  string
	}{
		{"workiva_prepare_section", map[string]any{"section_id": "environmental"}, "Recipes"},
		{"workiva_confirm_change_set", map[string]any{"change_set_id": "cs-1", "confirm_token": "tok"}, "ChangeSets"},
		{"workiva_stage_section_text", map[string]any{"section_id": "environmental", "blocks": []any{block}}, "SectionText"},
		{"workiva_confirm_section_text", map[string]any{"text_id": "tx-1", "confirm_token": "tok"}, "SectionText"},
		{"workiva_compare_fields", map[string]any{"section_id": "environmental"}, "Recipes"},
	}
	for _, c := range calls {
		t.Run(c.tool, func(t *testing.T) {
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: c.tool, Arguments: c.args})
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if !result.IsError {
				t.Fatalf("expected an error result, got %+v", result)
			}
			text := resultText(result)
			if !strings.Contains(text, "is not available") || !strings.Contains(text, c.dep) {
				t.Errorf("error text %q does not name missing %s", text, c.dep)
			}
		})
	}
}

// resultText joins the text content of a tool result.
func resultText(result *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
