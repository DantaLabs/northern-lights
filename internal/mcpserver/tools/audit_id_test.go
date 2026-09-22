package tools

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
)

// allToolsMock serves enough of the Workiva API for every tool to succeed.
func allToolsMock(t *testing.T) http.HandlerFunc {
	t.Helper()
	const mapperBody = `{"data":{"range":{"startRow":1,"startColumn":0,"stopRow":1,"stopColumn":1},"cells":[[{"value":"Scope 2 Energy (kWh)"},{"value":"1234"}]]}}`
	return tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body string
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/spreadsheets":
			body = `{"data":[{"id":"sp-1","name":"VSME 2026"}]}`
		case r.Method == http.MethodGet && r.URL.Path == "/spreadsheets/sp-1/sheets":
			body = `{"data":[{"id":"sh-1","name":"B3 Energy","index":0}]}`
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sheetdata"):
			body = sheetdataBody
			if !strings.Contains(r.URL.Query().Get("$fields"), "calculatedValue") {
				body = mapperBody
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/update"):
			w.WriteHeader(http.StatusAccepted)
			body = `{"operationLocation": "/operations/op-1"}`
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/operations/"):
			body = `{"id": "op-1", "status": "completed", "resourceUrl": "/spreadsheets/sp-1/sheets/sh-1"}`
		default:
			http.NotFound(w, r)
			return
		}
		if _, err := fmt.Fprint(w, body); err != nil {
			t.Errorf("write mock response: %v", err)
		}
	})
}

// TestEveryToolReturnsMatchingAuditID calls all 7 tools and checks each
// result carries a non-empty nl_audit_id, in the structured content and the
// first text block, that matches an audit record. The confirmed write must
// also carry the Workiva operation on a record with that same ID.
func TestEveryToolReturnsMatchingAuditID(t *testing.T) {
	env := newTestEnv(t, allToolsMock(t))
	seedField(t, env)

	stage := callTool(t, env.deps, UpdateField(), map[string]any{"name": "scope2_energy_kwh", "value": "9999"})
	token, _ := structuredContent(t, stage)["confirm_token"].(string)
	if token == "" {
		t.Fatalf("staging returned no confirm_token: %+v", stage.Content)
	}

	calls := []struct {
		tool   mcpserver.Tool
		args   map[string]any
		wantOp bool
		res    *mcp.CallToolResult
	}{
		{tool: ListSpreadsheets(), args: map[string]any{}},
		{tool: ReadRange(), args: map[string]any{"spreadsheet_id": "sp-1", "sheet_id": "sh-1", "range": "B3"}},
		{tool: SearchFields(), args: map[string]any{"query": "scope"}},
		{tool: GetField(), args: map[string]any{"name": "scope2_energy_kwh"}},
		{tool: UpdateField(), args: map[string]any{"name": "scope2_energy_kwh", "value": "9999", "confirm_token": token}, wantOp: true},
		{tool: SyncMapping(), args: map[string]any{"spreadsheet_id": "sp-1", "sheet_id": "sh-1"}},
		{tool: AuditTrail(), args: map[string]any{}},
	}
	if len(calls)+1 != 8 { // 7 tools; update_field is called twice
		t.Fatalf("test covers %d tools, want 7", len(calls))
	}
	results := []*mcp.CallToolResult{stage}
	for i := range calls {
		res := callTool(t, env.deps, calls[i].tool, calls[i].args)
		if res.IsError {
			t.Fatalf("%s returned error: %+v", calls[i].tool.Name(), res.Content)
		}
		calls[i].res = res
		results = append(results, res)
	}

	entries := exportAudit(t, env.deps.Audit)
	byID := map[string][]audit.Entry{}
	for _, e := range entries {
		byID[e.AuditID] = append(byID[e.AuditID], e)
	}
	for i, res := range results {
		id, _ := structuredContent(t, res)["nl_audit_id"].(string)
		if id == "" {
			t.Fatalf("result %d has no nl_audit_id: %+v", i, res.StructuredContent)
		}
		text, ok := res.Content[0].(*mcp.TextContent)
		if !ok || !strings.Contains(text.Text, id) {
			t.Fatalf("result %d first content block lacks %s: %+v", i, id, res.Content[0])
		}
		if len(byID[id]) == 0 {
			t.Fatalf("result %d nl_audit_id %s has no audit record", i, id)
		}
	}
	for _, c := range calls {
		if !c.wantOp {
			continue
		}
		id, _ := structuredContent(t, c.res)["nl_audit_id"].(string)
		found := false
		for _, e := range byID[id] {
			if e.WorkivaOpURL != "" && e.Action == "write" {
				found = true
			}
		}
		if !found {
			t.Fatalf("confirmed write %s has no audit record with a Workiva operation: %+v", id, byID[id])
		}
	}
	if err := env.deps.Audit.Verify(t.Context()); err != nil {
		t.Fatalf("audit chain: %v", err)
	}
}
