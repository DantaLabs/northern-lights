package mcpserver

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/audit"
)

// fakeWriteTool carries the name of a write tool so the audit middleware
// treats it as a mutation. It must never execute when the audit log is
// unavailable.
type fakeWriteTool struct{ called *bool }

func (f fakeWriteTool) Name() string        { return "workiva_update_field" }
func (f fakeWriteTool) Description() string { return "test stub for a write tool" }

type fakeWriteInput struct {
	Name  string `json:"name" jsonschema:"field name"`
	Value string `json:"value" jsonschema:"new value"`
}

func (f fakeWriteTool) RegisterSDK(s *mcp.Server, _ Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        f.Name(),
		Description: f.Description(),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in fakeWriteInput) (*mcp.CallToolResult, struct{}, error) {
		*f.called = true
		return &mcp.CallToolResult{}, struct{}{}, nil
	})
}

// TestUnauditedWriteRefused closes the audit database, then calls a write
// tool: the middleware must refuse the call and the tool must not execute.
func TestUnauditedWriteRefused(t *testing.T) {
	deps := testDeps(t)

	// Break the audit log: appends fail on a closed handle.
	if err := deps.Audit.Close(); err != nil {
		t.Fatalf("close audit log: %v", err)
	}

	called := false
	reg := NewRegistry()
	reg.Register(fakeWriteTool{called: &called})
	reg.Register(echoTool{})

	handler, err := New(deps, reg, &Options{APIToken: "test-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "dev"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: bearerClient("test-token"),
	}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close session: %v", err)
		}
	})

	// Write tool: refused, never executes.
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "workiva_update_field",
		Arguments: map[string]any{"name": "x", "value": "1"},
	}); err == nil {
		t.Fatal("write call succeeded despite unavailable audit log")
	}
	if called {
		t.Error("write tool executed despite audit failure")
	}

	// Read tool: still executes (the failure is logged, not fatal).
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "hi"},
	}); err != nil {
		t.Fatalf("read call should proceed with logged audit failure: %v", err)
	}
}

// TestAuditFailureOnOpenLogSanity confirms a normally-open log accepts
// appends, so the test above exercises the failure path rather than a
// broken setup.
func TestAuditFailureOnOpenLogSanity(t *testing.T) {
	deps := testDeps(t)
	if _, err := deps.Audit.Append(context.Background(), audit.Entry{Actor: "t", Tool: "t", Action: "call"}); err != nil {
		t.Fatalf("append on open log: %v", err)
	}
}
