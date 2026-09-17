package tools

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/mcpserver"
)

// TestRejectedKeyNeverReachesWorkiva sends a tools/call for a live-discovery
// tool with a malformed key and checks the fake Workiva API receives
// nothing, not even a token request.
func TestRejectedKeyNeverReachesWorkiva(t *testing.T) {
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Workiva received %s %s after a 401", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	reg := mcpserver.NewRegistry()
	reg.Register(ListSpreadsheets())
	handler, err := mcpserver.New(env.deps, reg, &mcpserver.Options{APIToken: "test-token"})
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	for _, key := range []string{"Bearer Bearer test-token", "Bearer wrong", "Basic dGVzdA==", ""} {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"workiva_list_spreadsheets","arguments":{}}}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("key %q: status = %d, want 401", key, resp.StatusCode)
		}
	}
	if n := env.apiCalls.Load(); n != 0 {
		t.Fatalf("Workiva API calls = %d, want 0", n)
	}
}
