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
	if len(tools) != 7 {
		t.Fatalf("tools/list returned %d tools, want 7", len(tools))
	}
	for _, tool := range tools {
		schema, ok := tool.InputSchema.(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Fatalf("%s: inputSchema is %T %v, want an object schema", tool.Name, tool.InputSchema, tool.InputSchema)
		}
		walkSchema(t, tool.Name+".inputSchema", schema)
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
