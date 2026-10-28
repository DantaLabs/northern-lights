package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/mcpserver/tools"
)

var demoToolNames = []string{
	"workiva_prepare_section",
	"workiva_confirm_change_set",
	"workiva_stage_section_text",
	"workiva_confirm_section_text",
	"workiva_compare_fields",
	"workiva_revert_change_set",
}

// demoSession builds the server with the demo environment, serves it and
// returns a connected client session.
func demoSession(t *testing.T, pipeline bool, mappingsPath string) *mcp.ClientSession {
	t.Helper()
	t.Setenv("NL_DEMO_MODE", "true")
	t.Setenv("NL_DB_PATH", filepath.Join(t.TempDir(), "demo.db"))
	t.Setenv("NL_API_KEY", "demo")
	t.Setenv("NL_LISTEN_ADDR", "127.0.0.1:0")
	t.Setenv("NL_READ_CACHE_TTL", "0")
	if pipeline {
		t.Setenv("NL_DEMO_PIPELINE", "true")
		t.Setenv("NL_DEMO_SECTIONS_DIR", t.TempDir())
	} else {
		t.Setenv("NL_DEMO_PIPELINE", "")
		if err := os.Unsetenv("NL_DEMO_PIPELINE"); err != nil {
			t.Fatalf("unset NL_DEMO_PIPELINE: %v", err)
		}
	}
	_, handler, cleanup, err := buildServer("", mappingsPath)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(cleanup)
	return connectDemo(t, handler)
}

func listToolNames(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func allToolNames() []string {
	var names []string
	for _, tool := range tools.All() {
		names = append(names, tool.Name())
	}
	return names
}

func equalNames(t *testing.T, got, want []string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tool names mismatch\n got (%d): %v\nwant (%d): %v", len(got), got, len(want), want)
	}
}

func TestDemoPipelineFlagOffKeepsThirteenTools(t *testing.T) {
	session := demoSession(t, false, "")
	got := listToolNames(t, session)
	if len(got) != 13 {
		t.Fatalf("got %d tools, want 13: %v", len(got), got)
	}
	equalNames(t, got, allToolNames())
}

func TestDemoPipelineFlagOnAddsFourTools(t *testing.T) {
	session := demoSession(t, true, "")
	got := listToolNames(t, session)
	if len(got) != 19 {
		t.Fatalf("got %d tools, want 19: %v", len(got), got)
	}
	equalNames(t, got, append(allToolNames(), demoToolNames...))
}

func TestDemoPipelineServicesReachTools(t *testing.T) {
	session := demoSession(t, true, "")
	calls := map[string]map[string]any{
		"workiva_prepare_section":      {"section_id": "esg-environmental"},
		"workiva_confirm_change_set":   {"change_set_id": "cs-1", "confirm_token": "tok"},
		"workiva_stage_section_text":   {"section_id": "esg-environmental", "blocks": []any{map[string]any{"kind": "narrative", "text": "Energy use fell."}}},
		"workiva_confirm_section_text": {"text_id": "st-1", "confirm_token": "tok"},
		"workiva_compare_fields":       {"section_id": "esg-environmental"},
		"workiva_revert_change_set":    {"change_set_id": "cs-1"},
	}
	for name, args := range calls {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: transport error: %v", name, err)
		}
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok && strings.Contains(tc.Text, "is not available") {
				t.Errorf("%s: service missing from Deps: %s", name, tc.Text)
			}
		}
	}
}

const extraMappingsYAML = `region: eu
spreadsheets:
  - id: "demo-esg-test"
    name: "ESG test"
    sheets:
      - id: "demo-esg-sheet"
        name: "Emissions"
        fields:
          - { name: esg_test_scope1_tco2e, range: "B2", aliases: "esg test scope 1", field_type: number, description: "Test scope 1 emissions" }
`

func searchFieldNames(t *testing.T, session *mcp.ClientSession, query string) []string {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "workiva_search_fields",
		Arguments: map[string]any{"query": query},
	})
	if err != nil {
		t.Fatalf("search_fields %q: %v", query, err)
	}
	var names []string
	for _, f := range structuredContent(t, res)["fields"].([]any) {
		names = append(names, f.(map[string]any)["name"].(string))
	}
	return names
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestDemoModeLoadsMappingsFile(t *testing.T) {
	yamlPath := filepath.Join(t.TempDir(), "extra.yaml")
	if err := os.WriteFile(yamlPath, []byte(extraMappingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	session := demoSession(t, false, yamlPath)
	if got := searchFieldNames(t, session, "esg test scope 1"); !contains(got, "esg_test_scope1_tco2e") {
		t.Errorf("extra field not found: %v", got)
	}
	if got := searchFieldNames(t, session, "energy"); !contains(got, "total_energy_consumption_kwh") {
		t.Errorf("built-in demo field not found: %v", got)
	}
}

func TestDemoModeWithoutMappingsLoadsBuiltInOnly(t *testing.T) {
	session := demoSession(t, false, "")
	if got := searchFieldNames(t, session, "energy"); !contains(got, "total_energy_consumption_kwh") {
		t.Errorf("built-in demo field not found: %v", got)
	}
	if got := searchFieldNames(t, session, "esg test scope 1"); contains(got, "esg_test_scope1_tco2e") {
		t.Errorf("extra field unexpectedly present: %v", got)
	}
}

func TestDemoModeMissingMappingsFileFailsStartup(t *testing.T) {
	t.Setenv("NL_DEMO_MODE", "true")
	t.Setenv("NL_DB_PATH", filepath.Join(t.TempDir(), "demo.db"))
	t.Setenv("NL_API_KEY", "demo")
	t.Setenv("NL_LISTEN_ADDR", "127.0.0.1:0")
	t.Setenv("NL_READ_CACHE_TTL", "0")
	_, _, cleanup, err := buildServer("", filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil {
		cleanup()
		t.Fatal("buildServer succeeded, want error")
	}
	if !strings.Contains(err.Error(), "load demo extra mappings") {
		t.Fatalf("error = %v, want load demo extra mappings", err)
	}
}

func connectDemo(t *testing.T, handler http.Handler) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "demo-pipeline-test", Version: "dev"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: bearerHTTPClient("demo"),
	}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}
