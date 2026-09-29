package tools

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	_ "modernc.org/sqlite"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

func provisionSnapshotTestBundle(t *testing.T, store *assurance.Store) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bundle := assurance.Bundle{
		SchemaVersion: 1, BundleID: "wave1", BundleVersion: 1, TenantID: "legacy-api-key",
		Reports: []assurance.ReportRevision{{
			ReportID: "energy", Revision: 1, Name: "Energy", Owner: "reporting", Status: "active", RetentionClass: "standard",
			ResourcePolicyHash: strings.Repeat("a", 64),
			Periods:            []assurance.Period{{Key: "2026-Q3", Label: "Q3 2026", Start: "2026-07-01", End: "2026-09-30"}},
			Fields:             []assurance.FieldDefinition{{FieldID: "scope2", ResourceID: "r1", ExternalResourceID: "sp-1", SubresourceID: "sh-1", Locator: "B3", Kind: assurance.ValueNumber, Unit: "kWh", Scale: "ones", Required: true, Order: 1}},
		}},
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := assurance.CanonicalJSONBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := assurance.ValidateBundle(raw, ed25519.Sign(private, canonical), "legacy-api-key", public)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StageBundle(context.Background(), validated); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestActivation(context.Background(), "wave1", 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(context.Background(), "legacy-api-key", public); err != nil {
		t.Fatal(err)
	}
}

func TestWave1C008C009SnapshotToolUsesApprovedUncachedSourceAndReplays(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/spreadsheets/sp-1/sheets/sh-1/sheetdata" || r.URL.Query().Get("$cellrange") != "B3" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":{"range":{"startRow":2,"startColumn":1,"stopRow":2,"stopColumn":1},"cells":[[{"value":24683.00}]]}}`)
	}))
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store, err := assurance.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	provisionSnapshotTestBundle(t, store)
	env.deps.Assurance = store
	env.deps.Cfg.AssuranceEnabled = true
	env.deps.Client = workivaprovider.NewRouter(env.deps.Client)
	args := map[string]any{
		"report_id": "energy", "period": map[string]any{"key": "2026-Q3"},
		"consistency": "best_effort", "idempotency_key": "snapshot-tool-key",
	}
	first := callTool(t, env.deps, SnapshotReport(), args)
	if first.IsError {
		t.Fatalf("snapshot call failed: %#v", first.Content)
	}
	firstBody := structuredContent(t, first)
	if firstBody["status"] != "completed" || firstBody["snapshot_id"] == "" || firstBody["completeness"] != "complete" {
		t.Fatalf("snapshot response = %#v", firstBody)
	}
	encoded, err := json.Marshal(first.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(":null")) {
		t.Fatalf("snapshot response emitted JSON null: %s", encoded)
	}
	callsAfterFirst := env.apiCalls.Load()
	replay := callTool(t, env.deps, SnapshotReport(), args)
	if replay.IsError {
		t.Fatalf("snapshot replay failed: %#v", replay.Content)
	}
	if body := structuredContent(t, replay); body["status"] != "idempotency_replay" || body["snapshot_id"] != firstBody["snapshot_id"] {
		t.Fatalf("replay response = %#v", body)
	}
	if got := env.apiCalls.Load(); got != callsAfterFirst {
		t.Fatalf("replay made provider calls: before=%d after=%d", callsAfterFirst, got)
	}
}

func callToolAllowError(t *testing.T, deps mcpserver.Deps, tool mcpserver.Tool, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	reg := mcpserver.NewRegistry()
	reg.Register(tool)
	handler, err := mcpserver.New(deps, reg, &mcpserver.Options{APIToken: "test-token"})
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "dev"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: bearerHTTPClient("test-token"),
	}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool.Name(), Arguments: args})
}

func TestWave1SnapshotAuditFailureFailsClosedBeforeProviderReads(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("provider request reached after audit failure: %s %s", r.Method, r.URL.Path)
	}))
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store, err := assurance.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	provisionSnapshotTestBundle(t, store)
	env.deps.Assurance = store
	env.deps.Cfg.AssuranceEnabled = true
	env.deps.Client = workivaprovider.NewRouter(env.deps.Client)
	if err := env.deps.Audit.Close(); err != nil {
		t.Fatal(err)
	}

	result, callErr := callToolAllowError(t, env.deps, SnapshotReport(), map[string]any{
		"report_id": "energy", "period": map[string]any{"key": "2026-Q3"},
		"idempotency_key": "audit-failure-key",
	})
	if callErr == nil && (result == nil || !result.IsError) {
		t.Fatalf("snapshot succeeded after audit append failure: result=%#v", result)
	}
	var snapshots, sealed int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_snapshots`).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM assurance_idempotency_records WHERE state='sealed'`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if snapshots != 0 || sealed != 0 {
		t.Fatalf("audit failure left persisted state: snapshots=%d sealed_reservations=%d", snapshots, sealed)
	}
}

func TestWave1C064SnapshotSchemaIsClosedBoundedAndDefaultsBestEffort(t *testing.T) {
	tools := listAllTools(t)
	var snapshotSchema map[string]any
	for _, tool := range tools {
		if tool.Name == "workiva_snapshot_report" {
			snapshotSchema, _ = tool.InputSchema.(map[string]any)
		}
	}
	if snapshotSchema == nil || snapshotSchema["additionalProperties"] != false {
		t.Fatalf("snapshot schema = %#v", snapshotSchema)
	}
	properties := snapshotSchema["properties"].(map[string]any)
	fields := properties["field_ids"].(map[string]any)
	if fields["type"] != "array" || fields["maxItems"] != float64(1000) && fields["maxItems"] != 1000 {
		t.Fatalf("field_ids schema = %#v", fields)
	}
	period := properties["period"].(map[string]any)
	if period["additionalProperties"] != false {
		t.Fatalf("period schema is not closed: %#v", period)
	}
}
