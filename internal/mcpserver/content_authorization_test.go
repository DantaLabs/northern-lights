package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestContentPlacementPhasePermissionsMatchSignedCapabilityVocabulary(t *testing.T) {
	tests := []struct {
		phase string
		want  identity.Permission
	}{
		{"ingest", identity.PermissionContentIngest},
		{"stage", identity.PermissionContentStage},
		{"confirm", identity.PermissionContentConfirm},
		{"acknowledge", identity.PermissionContentAcknowledge},
		{"reconcile", identity.PermissionContentReconcile},
	}
	for _, tc := range tests {
		t.Run(tc.phase, func(t *testing.T) {
			got, err := requiredPermission("workiva_content_placement", json.RawMessage(`{"phase":"`+tc.phase+`"}`), true)
			if err != nil {
				t.Fatalf("requiredPermission: %v", err)
			}
			if got != tc.want || string(got) != string(tc.want) {
				t.Fatalf("permission = %q, want signed capability %q", got, tc.want)
			}
		})
	}
	if _, err := requiredPermission("workiva_content_placement", json.RawMessage(`{"phase":"unknown"}`), true); err == nil {
		t.Fatal("unknown content phase was authorized")
	}
}

func TestContentPlacementHTTPRejectsApplicationAndSubjectFallbackBeforeDispatch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		principal identity.Principal
	}{
		{name: "application token", principal: identity.Principal{TenantID: "11111111-1111-1111-1111-111111111111", ObjectID: "22222222-2222-2222-2222-222222222222", TokenType: identity.TokenTypeApplication, Permissions: []identity.Permission{identity.PermissionContentIngest}}},
		{name: "subject fallback", principal: identity.Principal{TenantID: "11111111-1111-1111-1111-111111111111", ObjectID: "22222222-2222-2222-2222-222222222222", TokenType: identity.TokenTypeDelegated, UsedSubjectFallback: true, Permissions: []identity.Permission{identity.PermissionContentIngest}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := testDeps(t)
			called := false
			handler := authorizationHTTPHandler(deps.Audit, true, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"workiva_content_placement","arguments":{"phase":"ingest","name":"PRIVATE PASTE","source_text":"PRIVATE PASTE"}}}`))
			req = req.WithContext(identity.ContextWithPrincipal(req.Context(), tc.principal))
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", res.Code, res.Body.String())
			}
			if called {
				t.Fatal("untrusted content request reached dispatch")
			}
			var response map[string]any
			if err := json.Unmarshal(res.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response["status"] != "denied" || response["error"].(map[string]any)["code"] != "strong_identity_required" {
				t.Fatalf("denial does not identify missing trusted delegated identity: %#v", response)
			}
			entries := exportEntries(t, deps.Audit)
			if len(entries) != 1 || entries[0].Target != "ingest" {
				t.Fatalf("content denial audit is not phase-only: %#v", entries)
			}
			encoded, _ := json.Marshal(entries)
			if strings.Contains(string(encoded), "PRIVATE PASTE") {
				t.Fatalf("content denial audit leaked source: %s", encoded)
			}
		})
	}
}

func TestContentAuditUsesServiceOwnedSealedResultAndDoesNotRecordRawArguments(t *testing.T) {
	deps := testDeps(t)
	p := delegatedPrincipal(identity.PermissionContentIngest)
	req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
		Params: &mcp.CallToolParamsRaw{Name: "workiva_content_placement", Arguments: json.RawMessage(`{"phase":"ingest","name":"PRIVATE PASTE","source_bytes_base64":"U0VDUkVU"}`)},
	}
	handler := auditMiddleware(deps.Audit, DefaultActorHeader)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.CallToolResult{StructuredContent: map[string]any{"nl_audit_id": "sealed-service-audit-id", "status": "accepted"}}, nil
	})
	ctx := identity.ContextWithPrincipal(context.Background(), p)
	result, err := handler(ctx, "tools/call", req)
	if err != nil {
		t.Fatal(err)
	}
	callResult := result.(*mcp.CallToolResult)
	encoded, err := json.Marshal(callResult.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var content map[string]any
	if err := json.Unmarshal(encoded, &content); err != nil {
		t.Fatal(err)
	}
	if content["nl_audit_id"] != "sealed-service-audit-id" {
		t.Fatalf("generic audit middleware overwrote the sealed service audit ID: %#v", content)
	}
	if entries := exportEntries(t, deps.Audit); len(entries) != 0 {
		t.Fatalf("generic audit middleware duplicated service-owned audit: %#v", entries)
	}
	if !writeTools["workiva_content_placement"] {
		t.Fatal("content materialization is missing from write/audit-required tool classification")
	}
}

func TestContentAPIKeyHTTPGuardAuditsOnlySafePhaseAndNeverDispatches(t *testing.T) {
	deps := testDeps(t)
	called := false
	handler := contentAPIKeyAuthorizationHTTPHandler(deps.Audit, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"workiva_content_placement","arguments":{"phase":"ingest","name":"PRIVATE PASTE","source_text":"PRIVATE PASTE","source_bytes_base64":"U0VDUkVU"}}}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusForbidden || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("API-key content status/cache = %d/%q; body=%s", res.Code, res.Header().Get("Cache-Control"), res.Body.String())
	}
	if called {
		t.Fatal("API-key content request reached SDK dispatch")
	}
	entries := exportEntries(t, deps.Audit)
	if len(entries) != 1 || entries[0].Action != "authorization_denied" || entries[0].Target != "ingest" || entries[0].Actor != DefaultActor {
		t.Fatalf("API-key denial audit = %#v", entries)
	}
	encoded, _ := json.Marshal(entries)
	for _, secret := range []string{"PRIVATE PASTE", "U0VDUkVU"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("API-key denial audit leaked content %q: %s", secret, encoded)
		}
	}
}

func TestContentAPIKeyHTTPGuardBoundsWholeEnvelope(t *testing.T) {
	called := false
	handler := contentAPIKeyAuthorizationHTTPHandler(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(strings.Repeat("x", maxPeekBody+1)))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusRequestEntityTooLarge || called {
		t.Fatalf("oversized content envelope status/called = %d/%v", res.Code, called)
	}
	if got, want := res.Body.String(), "request body exceeds 1048576 bytes\n"; got != want {
		t.Fatalf("oversized response = %q, want %q", got, want)
	}
}

type contentAuthorizationProbeInput struct {
	Phase      string `json:"phase"`
	Name       string `json:"name"`
	SourceText string `json:"source_text"`
}

type contentAuthorizationProbe struct{ calls *int }

func (p contentAuthorizationProbe) Name() string        { return "workiva_content_placement" }
func (p contentAuthorizationProbe) Description() string { return "test-only content dispatch probe" }
func (p contentAuthorizationProbe) RegisterSDK(server *mcp.Server, _ Deps) {
	mcp.AddTool(server, &mcp.Tool{Name: p.Name(), Description: p.Description()}, func(context.Context, *mcp.CallToolRequest, contentAuthorizationProbeInput) (*mcp.CallToolResult, map[string]any, error) {
		*p.calls++
		return &mcp.CallToolResult{}, map[string]any{"status": "accepted"}, nil
	})
}

func TestContentAPIKeyDeniedThroughAuthenticatedMCPClientBeforeToolDispatch(t *testing.T) {
	deps := testDeps(t)
	calls := 0
	registry := NewRegistry()
	registry.Register(contentAuthorizationProbe{calls: &calls})
	handler, err := New(deps, registry, &Options{APIToken: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "content-auth-test", Version: "test"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: bearerClient("test-token")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close MCP session: %v", err)
		}
	})
	_, err = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "workiva_content_placement", Arguments: map[string]any{"phase": "ingest", "name": "PRIVATE PASTE", "source_text": "PRIVATE PASTE"}})
	if err == nil {
		t.Fatal("API-key content request succeeded")
	}
	if calls != 0 {
		t.Fatalf("API-key request reached materializing tool %d times", calls)
	}
	entries := exportEntries(t, deps.Audit)
	if len(entries) != 1 || entries[0].Action != "authorization_denied" || entries[0].Target != "ingest" {
		t.Fatalf("authenticated API-key denial audit = %#v", entries)
	}
}

func TestContentPOSTAdmissionUsesReadinessWhenContentToolIsConfigured(t *testing.T) {
	deps := testDeps(t)
	deps.Assurance = nil
	deps.DrainGate = assurance.NewDrainGate()
	calls := 0
	registry := NewRegistry()
	registry.Register(contentAuthorizationProbe{calls: &calls})
	handler, err := New(deps, registry, &Options{APIToken: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"workiva_content_placement","arguments":{"phase":"ingest"}}}`))
	req.Header.Set("Authorization", "Bearer test-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusServiceUnavailable || !strings.Contains(res.Body.String(), "content admission unavailable") {
		t.Fatalf("unready content POST status/body = %d/%q", res.Code, res.Body.String())
	}
	if calls != 0 {
		t.Fatalf("unready content reached materializing tool %d times", calls)
	}
}
