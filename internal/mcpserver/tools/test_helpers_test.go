package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

// testEnv bundles the dependencies for one tool test, with the Workiva
// client pointed at an httptest server. apiCalls counts non-token API
// requests so cache tests can assert no live read happened.
type testEnv struct {
	deps     mcpserver.Deps
	apiCalls *atomic.Int32
}

func newTestEnv(t *testing.T, handler http.HandlerFunc) testEnv {
	t.Helper()

	var apiCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/iam/v1/oauth2/token" {
			apiCalls.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}

	store, err := mapping.Open(":memory:")
	if err != nil {
		t.Fatalf("mapping.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	log, err := audit.Open(":memory:")
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	t.Cleanup(func() { log.Close() })

	tokens := workiva.NewTokenProvider(u, "test-client", "test-secret", "file:read", srv.Client())
	client := workiva.NewClient(u, tokens, nil, srv.Client())

	return testEnv{
		deps: mcpserver.Deps{
			Client: client,
			Store:  store,
			Audit:  log,
			Cfg:    &config.Config{Region: "eu", ReadCacheTTL: 30 * time.Second},
		},
		apiCalls: &apiCalls,
	}
}

// callTool serves the given tool over streamable HTTP, connects an SDK
// client, and invokes it once with the given arguments.
func callTool(t *testing.T, deps mcpserver.Deps, tool mcpserver.Tool, args map[string]any) *mcp.CallToolResult {
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
		HTTPClient: bearerHTTPClient("test-token"),
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

// structuredContent returns the result payload as a generic map.
func structuredContent(t *testing.T, result *mcp.CallToolResult) map[string]any {
	t.Helper()
	content, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent is %T, want map", result.StructuredContent)
	}
	return content
}

func bearerHTTPClient(token string) *http.Client {
	return &http.Client{
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

// tokenHandler serves OAuth token requests and delegates everything else.
func tokenHandler(t *testing.T, next http.HandlerFunc) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/iam/v1/oauth2/token" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"tok-1","expires_in":3600}`))
			return
		}
		next(w, r)
	}
}

// seedField inserts a spreadsheet, sheet, and field so tools have
// something to resolve.
func seedField(t *testing.T, env testEnv) {
	t.Helper()
	ctx := context.Background()
	if err := env.deps.Store.UpsertSpreadsheet(ctx, mapping.Spreadsheet{
		ID: "sp-1", Name: "VSME 2026", Region: "eu",
	}); err != nil {
		t.Fatalf("UpsertSpreadsheet: %v", err)
	}
	if err := env.deps.Store.UpsertSheet(ctx, mapping.Sheet{
		ID: "sh-1", SpreadsheetID: "sp-1", Name: "B3 Energy",
	}); err != nil {
		t.Fatalf("UpsertSheet: %v", err)
	}
	if _, err := env.deps.Store.UpsertField(ctx, mapping.Field{
		SpreadsheetID: "sp-1", SheetID: "sh-1",
		Name:        "scope2_energy_kwh",
		Aliases:     "scope 2 energy,scope 2 electricity",
		CellRange:   "B3",
		FieldType:   "number",
		Description: "Scope 2 energy consumption in kWh",
	}); err != nil {
		t.Fatalf("UpsertField: %v", err)
	}
}
