package tools

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/mcpserver"
)

// rawMCPClient speaks JSON-RPC over plain HTTP so the response
// Content-Type can be inspected, which the SDK client hides.
type rawMCPClient struct {
	t       *testing.T
	url     string
	session string
	nextID  int
}

// post sends one JSON-RPC message with the Accept header Copilot Studio
// and the SDK send, keeps the Mcp-Session-Id, and returns the response.
func (c *rawMCPClient) post(method string, params any) (*http.Response, []byte) {
	c.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	if !strings.HasPrefix(method, "notifications/") {
		c.nextID++
		msg["id"] = c.nextID
	}
	body, err := json.Marshal(msg)
	if err != nil {
		c.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, c.url, strings.NewReader(string(body)))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set(mcpserver.ActorHeader, "raw-tester@example.com")
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		c.t.Fatalf("%s: read body: %v", method, err)
	}
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		c.session = id
	}
	return resp, data
}

// call sends tools/call, requires application/json, and returns the
// structured content of a non-error result.
func (c *rawMCPClient) call(name string, args map[string]any) map[string]any {
	c.t.Helper()
	resp, body := c.post("tools/call", map[string]any{"name": name, "arguments": args})
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("%s: status %d: %s", name, resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		c.t.Fatalf("%s: Content-Type = %q, want application/json (SSE is not supported by Quick Tunnels)", name, ct)
	}
	var rpc struct {
		Result struct {
			IsError           bool           `json:"isError"`
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
		Error *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil {
		c.t.Fatalf("%s: response is not JSON-RPC: %v: %s", name, err, body)
	}
	if rpc.Error != nil || rpc.Result.IsError {
		c.t.Fatalf("%s: tool failed: %s", name, body)
	}
	return rpc.Result.StructuredContent
}

// TestPostMCPAnswersJSONForEveryTool drives initialize, tools/list and all 7
// tools (including the two-phase write with its operation poll) over raw
// HTTP and requires application/json on each response, then checks GET
// /mcp is refused with 405.
func TestPostMCPAnswersJSONForEveryTool(t *testing.T) {
	env := newTestEnv(t, allToolsMock(t))
	seedField(t, env)
	reg := mcpserver.NewRegistry()
	for _, tool := range All() {
		reg.Register(tool)
	}
	handler, err := mcpserver.New(env.deps, reg, &mcpserver.Options{APIToken: "test-token"})
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := &rawMCPClient{t: t, url: srv.URL + "/mcp"}

	resp, body := c.post("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "raw", "version": "dev"},
	})
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("initialize: status %d Content-Type %q: %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if resp, _ := c.post("notifications/initialized", nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notifications/initialized: status %d, want 202", resp.StatusCode)
	}

	resp, body = c.post("tools/list", map[string]any{})
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("tools/list: Content-Type = %q, want application/json", ct)
	}
	var list struct {
		Result struct {
			Tools []struct{ Name string } `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &list); err != nil || len(list.Result.Tools) != 7 {
		t.Fatalf("tools/list: %d tools (err %v): %s", len(list.Result.Tools), err, body)
	}

	c.call("workiva_list_spreadsheets", map[string]any{})
	c.call("workiva_read_range", map[string]any{"spreadsheet_id": "sp-1", "sheet_id": "sh-1", "range": "B3"})
	c.call("workiva_search_fields", map[string]any{"query": "scope"})
	c.call("workiva_get_field", map[string]any{"name": "scope2_energy_kwh"})
	staged := c.call("workiva_update_field", map[string]any{"name": "scope2_energy_kwh", "value": "9999"})
	token, _ := staged["confirm_token"].(string)
	if staged["status"] != "awaiting_confirmation" || token == "" {
		t.Fatalf("preview = %v", staged)
	}
	written := c.call("workiva_update_field", map[string]any{"name": "scope2_energy_kwh", "value": "9999", "confirm_token": token})
	if written["status"] != "written" || written["workiva_op_url"] == "" {
		t.Fatalf("confirm+poll = %v", written)
	}
	c.call("workiva_sync_mapping", map[string]any{"spreadsheet_id": "sp-1", "sheet_id": "sh-1"})
	c.call("workiva_audit_trail", map[string]any{})

	get, err := http.NewRequest(http.MethodGet, srv.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	get.Header.Set("Accept", "text/event-stream")
	get.Header.Set("Authorization", "Bearer test-token")
	get.Header.Set("Mcp-Session-Id", c.session)
	getResp, err := http.DefaultClient.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	_ = getResp.Body.Close()
	if getResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp: status %d, want 405", getResp.StatusCode)
	}
	if !strings.Contains(getResp.Header.Get("Allow"), "POST") {
		t.Fatalf("GET /mcp: Allow = %q, want to include POST", getResp.Header.Get("Allow"))
	}
}
