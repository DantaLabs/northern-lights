package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/mcpserver"
)

// listAllTools serves All() and returns what tools/list gives a client.
func listAllTools(t *testing.T) []*mcp.Tool {
	t.Helper()
	reg := mcpserver.NewRegistry()
	for _, tool := range All() {
		reg.Register(tool)
	}
	handler, err := mcpserver.New(newTestEnv(t, http.NotFound).deps, reg, &mcpserver.Options{APIToken: "test-token"})
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ctx := context.Background()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "dev"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: bearerHTTPClient("test-token")}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	return list.Tools
}

// TestToolSchemasAreCopilotStudioCompatible fails on the schema shapes
// Copilot Studio cannot consume: $ref at any depth, an array-valued type,
// and a numeric exclusiveMinimum/exclusiveMaximum. Enums are reported as
// warnings because Copilot Studio treats them as plain strings.
func TestToolSchemasAreCopilotStudioCompatible(t *testing.T) {
	tools := listAllTools(t)
	if len(tools) != 11 {
		t.Fatalf("tools/list returned %d tools, want 11", len(tools))
	}
	for _, tool := range tools {
		schema, ok := tool.InputSchema.(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Fatalf("%s: inputSchema is %T %v, want an object schema", tool.Name, tool.InputSchema, tool.InputSchema)
		}
		walkSchema(t, tool.Name+".inputSchema", schema)
		if tool.Name == "workiva_snapshot_report" || tool.Name == "workiva_validate_report" || tool.Name == "workiva_compare_periods" || tool.Name == "workiva_export_evidence" {
			output, ok := tool.OutputSchema.(map[string]any)
			if !ok || output["type"] != "object" {
				t.Fatalf("%s: outputSchema is %T %v, want an object schema", tool.Name, tool.OutputSchema, tool.OutputSchema)
			}
			walkSchema(t, tool.Name+".outputSchema", output)
		}
	}
}

func TestWave2ToolNamesReserveExactFinalContract(t *testing.T) {
	want := []string{"workiva_list_spreadsheets", "workiva_read_range", "workiva_search_fields", "workiva_get_field", "workiva_update_field", "workiva_sync_mapping", "workiva_audit_trail", "workiva_snapshot_report", "workiva_validate_report", "workiva_compare_periods", "workiva_export_evidence"}
	tools := All()
	if len(tools) != len(want) {
		t.Fatalf("All() returned %d tools, want %d", len(tools), len(want))
	}
	for index, tool := range tools {
		if tool.Name() != want[index] {
			t.Fatalf("tool[%d] = %q, want %q", index, tool.Name(), want[index])
		}
	}
}

func TestWave3FinalToolNamesAreContractFixtureOnly(t *testing.T) {
	final := append([]string{}, []string{"workiva_list_spreadsheets", "workiva_read_range", "workiva_search_fields", "workiva_get_field", "workiva_update_field", "workiva_sync_mapping", "workiva_audit_trail", "workiva_snapshot_report", "workiva_validate_report", "workiva_compare_periods", "workiva_export_evidence"}...)
	final = append(final, "workiva_discover_relationships", "workiva_transfer_value")
	if len(final) != 13 || final[11] != "workiva_discover_relationships" || final[12] != "workiva_transfer_value" {
		t.Fatalf("final-name fixture = %v", final)
	}
	if len(All()) != 11 {
		t.Fatalf("Wave 3 fixture must not register tools; All() returned %d", len(All()))
	}
}

func walkSchema(t *testing.T, path string, node any) {
	t.Helper()
	switch v := node.(type) {
	case map[string]any:
		for key, child := range v {
			at := path + "." + key
			switch key {
			case "$ref":
				t.Errorf("%s: $ref is hidden by Copilot Studio", at)
			case "type":
				if _, isArray := child.([]any); isArray {
					t.Errorf("%s: array-valued type cuts off the schema in Copilot Studio: %v", at, child)
				}
			case "exclusiveMinimum", "exclusiveMaximum":
				if _, isBool := child.(bool); !isBool {
					t.Errorf("%s: numeric %s throws FormatException in Copilot Studio: %v", at, key, child)
				}
			case "enum":
				t.Logf("warning: %s: enum %v is treated as a plain string by Copilot Studio", at, child)
			}
			walkSchema(t, at, child)
		}
	case []any:
		for i, child := range v {
			walkSchema(t, fmt.Sprintf("%s[%d]", path, i), child)
		}
	}
}
