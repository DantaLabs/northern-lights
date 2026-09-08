package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/audit"
)

func TestDemoModeEndToEnd(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "demo.db")
	t.Setenv("NL_DEMO_MODE", "true")
	t.Setenv("NL_DB_PATH", dbPath)
	t.Setenv("NL_API_KEY", "demo")
	t.Setenv("NL_LISTEN_ADDR", "127.0.0.1:0")
	t.Setenv("NL_READ_CACHE_TTL", "0")

	cfg, handler, cleanup, err := buildServer("", "")
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	defer cleanup()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// cfg is returned for logging assertions if needed.
	_ = cfg

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "demo-test", Version: "dev"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: bearerHTTPClient("demo"),
	}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close session: %v", err)
		}
	})

	// 1. list_spreadsheets returns 2 demo spreadsheets.
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "workiva_list_spreadsheets",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("list_spreadsheets: %v", err)
	}
	content := structuredContent(t, result)
	spreadsheets := content["spreadsheets"].([]any)
	if len(spreadsheets) != 2 {
		t.Fatalf("expected 2 spreadsheets, got %d", len(spreadsheets))
	}

	// 2. search_fields returns demo fields.
	result, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "workiva_search_fields",
		Arguments: map[string]any{"query": "energy"},
	})
	if err != nil {
		t.Fatalf("search_fields: %v", err)
	}
	content = structuredContent(t, result)
	fields := content["fields"].([]any)
	if len(fields) == 0 {
		t.Fatal("expected demo fields matching energy")
	}

	// 3. get_field returns the synthetic value.
	result, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "workiva_get_field",
		Arguments: map[string]any{"name": "total_energy_consumption_kwh"},
	})
	if err != nil {
		t.Fatalf("get_field: %v", err)
	}
	content = structuredContent(t, result)
	if content["value"] != "225000" {
		t.Errorf("value = %v, want 225000", content["value"])
	}

	// 4. update_field phase 1 returns a confirm_token.
	result, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "workiva_update_field",
		Arguments: map[string]any{"name": "total_energy_consumption_kwh", "value": "230000"},
	})
	if err != nil {
		t.Fatalf("update_field phase 1: %v", err)
	}
	content = structuredContent(t, result)
	if content["status"] != "awaiting_confirmation" {
		t.Fatalf("status = %v, want awaiting_confirmation", content["status"])
	}
	confirmToken, ok := content["confirm_token"].(string)
	if !ok || confirmToken == "" {
		t.Fatal("missing confirm_token")
	}
	if content["before"] != "225000" {
		t.Errorf("before = %v, want 225000", content["before"])
	}

	// 5. update_field phase 2 writes and returns written status.
	result, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "workiva_update_field",
		Arguments: map[string]any{"name": "total_energy_consumption_kwh", "value": "230000", "confirm_token": confirmToken},
	})
	if err != nil {
		t.Fatalf("update_field phase 2: %v", err)
	}
	content = structuredContent(t, result)
	if content["status"] != "written" {
		t.Fatalf("status = %v, want written", content["status"])
	}
	if content["after"] != "230000" {
		t.Errorf("after = %v, want 230000", content["after"])
	}

	// 6. audit_trail returns entries including the seeded init entry.
	result, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "workiva_audit_trail",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("audit_trail: %v", err)
	}
	content = structuredContent(t, result)
	entries := content["entries"].([]any)
	if len(entries) == 0 {
		t.Fatal("expected audit entries")
	}

	// Verify the hash chain by reopening the audit log directly.
	log, err := audit.Open(dbPath)
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := log.Close(); err != nil {
			t.Errorf("close audit log: %v", err)
		}
	})
	if err := log.Verify(ctx); err != nil {
		t.Fatalf("audit verify: %v", err)
	}

	// 7. Verify the startup log message constant is exported in main.
	if demoStartupMessage != "DEMO MODE: no Workiva credentials required. All data is synthetic." {
		t.Error("unexpected demo startup message")
	}
}

func TestDemoModeWithoutAPIKey(t *testing.T) {
	// Unset any inherited API key.
	if err := os.Unsetenv("NL_API_KEY"); err != nil {
		t.Fatalf("unset NL_API_KEY: %v", err)
	}
	t.Setenv("NL_DEMO_MODE", "true")
	t.Setenv("NL_DB_PATH", filepath.Join(t.TempDir(), "demo2.db"))
	t.Setenv("NL_LISTEN_ADDR", "127.0.0.1:0")

	_, handler, cleanup, err := buildServer("", "")
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	defer cleanup()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// The legacy hardcoded "demo" token must be rejected: demo mode now
	// generates a random token per run (printed to the server log).
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer demo")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("read unauthorized response: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close unauthorized response: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("hardcoded demo token accepted: status = %d, want 401", resp.StatusCode)
	}
}

func bearerHTTPClient(token string) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			req.Header.Set("Authorization", "Bearer "+token)
			return http.DefaultTransport.RoundTrip(req)
		}),
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func structuredContent(t *testing.T, result *mcp.CallToolResult) map[string]any {
	t.Helper()
	content, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent is %T, want map", result.StructuredContent)
	}
	return content
}
