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

// headerClient sends the bearer token plus an X-NL-Actor header.
func headerClient(token, actor string) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			req.Header.Set("Authorization", "Bearer "+token)
			if actor != "" {
				req.Header.Set("X-NL-Actor", actor)
			}
			return http.DefaultTransport.RoundTrip(req)
		}),
	}
}

// callToolWithActor is callTool with a caller identity header.
func callToolWithActor(t *testing.T, deps mcpserver.Deps, tool mcpserver.Tool, actor string, args map[string]any) *mcp.CallToolResult {
	t.Helper()

	reg := mcpserver.NewRegistry()
	reg.Register(tool)

	handler, err := mcpserver.New(deps, reg, &mcpserver.Options{APIToken: "test-token"})
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "dev"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: headerClient("test-token", actor),
	}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      tool.Name(),
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	return result
}

// TestUpdateFieldActorFromHeader verifies the X-NL-Actor identity lands on
// the write audit entry instead of the hardcoded default.
func TestUpdateFieldActorFromHeader(t *testing.T) {
	var edits [][]byte
	env := newTestEnv(t, writeMock(t, &edits))
	env.deps.Cfg.RequireWriteConfirmation = false
	seedField(t, env)

	res := callToolWithActor(t, env.deps, UpdateField(), "samuel@vadian.dev", map[string]any{
		"name":  "scope2_energy_kwh",
		"value": "9999",
	})
	if res.IsError {
		t.Fatalf("write returned error: %+v", res.Content)
	}

	entries, err := env.deps.Audit.Recent(context.Background(), 10, "sp-1/sh-1/B3")
	if err != nil {
		t.Fatalf("Audit.Recent: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no audit entry for the write")
	}
	if entries[0].Actor != "samuel@vadian.dev" {
		t.Errorf("audit actor = %q, want samuel@vadian.dev", entries[0].Actor)
	}
}

// TestUpdateFieldRejectsMismatchedValue verifies a confirm token cannot be
// used to write a different value than the one previewed at staging.
func TestUpdateFieldRejectsMismatchedValue(t *testing.T) {
	var edits [][]byte
	env := newTestEnv(t, writeMock(t, &edits))
	seedField(t, env)

	stage := callTool(t, env.deps, UpdateField(), map[string]any{
		"name":  "scope2_energy_kwh",
		"value": "5678",
	})
	if stage.IsError {
		t.Fatalf("phase 1 returned error: %+v", stage.Content)
	}
	token, _ := structuredContent(t, stage)["confirm_token"].(string)
	if token == "" {
		t.Fatal("phase 1 confirm_token missing")
	}

	exec := callTool(t, env.deps, UpdateField(), map[string]any{
		"name":          "scope2_energy_kwh",
		"value":         "9999",
		"confirm_token": token,
	})
	if !exec.IsError {
		t.Fatalf("phase 2 with a different value must fail, got %+v", exec.Content)
	}
	if len(edits) != 0 {
		t.Errorf("mismatched confirm triggered %d writes, want 0", len(edits))
	}
}

// TestSanitizeActorViaHeader covers the header validation rules through the
// public middleware path: invalid identities fall back to the default actor.
func TestSanitizeActorViaHeader(t *testing.T) {
	var edits [][]byte
	env := newTestEnv(t, writeMock(t, &edits))
	env.deps.Cfg.RequireWriteConfirmation = false
	seedField(t, env)

	res := callToolWithActor(t, env.deps, UpdateField(), strings.Repeat("a", 200), map[string]any{
		"name":  "scope2_energy_kwh",
		"value": "42",
	})
	if res.IsError {
		t.Fatalf("write returned error: %+v", res.Content)
	}

	entries, err := env.deps.Audit.Recent(context.Background(), 10, "sp-1/sh-1/B3")
	if err != nil {
		t.Fatalf("Audit.Recent: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no audit entry for the write")
	}
	if entries[0].Actor != mcpserver.DefaultActor {
		t.Errorf("audit actor = %q, want fallback %q for overlong header", entries[0].Actor, mcpserver.DefaultActor)
	}
}
