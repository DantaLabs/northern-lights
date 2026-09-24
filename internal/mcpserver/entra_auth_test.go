package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/identity"
)

type fakeTokenVerifier struct {
	principal  identity.Principal
	principals map[string]identity.Principal
	err        error
	calls      int
}

func TestAPIKeyAuthCompatibilityRemainsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		header  string
		allowed bool
	}{
		{name: "bearer", header: "Bearer phase-one-key", allowed: true},
		{name: "case insensitive bearer", header: "bEaReR phase-one-key", allowed: true},
		{name: "raw key", header: "phase-one-key", allowed: true},
		{name: "wrong", header: "Bearer wrong"},
		{name: "double bearer", header: "Bearer Bearer phase-one-key"},
		{name: "other scheme", header: "Basic cGhhc2Utb25lLWtleQ=="},
		{name: "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			handler := bearerAuthHandler("phase-one-key", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				called = true
			}))
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			req.Header.Set("Authorization", tc.header)
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if called != tc.allowed {
				t.Fatalf("downstream called=%v, want %v", called, tc.allowed)
			}
			if !tc.allowed && res.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", res.Code)
			}
			if !tc.allowed && res.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", res.Header().Get("Cache-Control"))
			}
		})
	}
}

func (v *fakeTokenVerifier) Verify(_ context.Context, raw string) (identity.Principal, error) {
	v.calls++
	if v.principals != nil {
		principal, ok := v.principals[raw]
		if !ok {
			return identity.Principal{}, errors.New("bad token")
		}
		return principal, nil
	}
	return v.principal, v.err
}

func TestNewEntraModeRequiresVerifierNotAPIKey(t *testing.T) {
	if _, err := New(Deps{}, NewRegistry(), &Options{AuthMode: config.AuthModeEntra}); err == nil {
		t.Fatal("entra mode without verifier succeeded")
	}
	verifier := &fakeTokenVerifier{principal: delegatedPrincipal(identity.PermissionWorkivaRead)}
	if _, err := New(Deps{}, NewRegistry(), &Options{AuthMode: config.AuthModeEntra, TokenVerifier: verifier}); err != nil {
		t.Fatalf("entra mode with verifier: %v", err)
	}
}

func TestEntraAuthenticationRequiresBearerAndNeverFallsBackToAPIKey(t *testing.T) {
	verifier := &fakeTokenVerifier{err: errors.New("bad token")}
	handler, err := New(testDeps(t), NewRegistry(), &Options{
		AuthMode:      config.AuthModeEntra,
		APIToken:      "legacy-key",
		TokenVerifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"legacy-key", "Bearer legacy-key", "Bearer Bearer legacy-key", ""} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
		req.Header.Set("Authorization", header)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("header %q status = %d, want 401", header, res.Code)
		}
		if got := res.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("header %q Cache-Control = %q, want no-store", header, got)
		}
	}
	if verifier.calls != 1 {
		t.Fatalf("verifier calls = %d, want 1 only for the well-formed bearer", verifier.calls)
	}
}

func TestEntraHealthEndpointsStayAnonymous(t *testing.T) {
	verifier := &fakeTokenVerifier{err: errors.New("must not be called")}
	handler, err := New(testDeps(t), NewRegistry(), &Options{AuthMode: config.AuthModeEntra, TokenVerifier: verifier})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, res.Code)
		}
		body := res.Body.String()
		for _, forbidden := range []string{"tenant", "authority", "audience", "entra"} {
			if strings.Contains(strings.ToLower(body), forbidden) {
				t.Fatalf("%s disclosed identity configuration: %q", path, body)
			}
		}
	}
	if verifier.calls != 0 {
		t.Fatalf("health endpoints invoked token verifier %d times", verifier.calls)
	}
}

func TestToolPermissionMappingCoversSevenTools(t *testing.T) {
	tests := []struct {
		name                 string
		arguments            json.RawMessage
		confirmationRequired bool
		want                 identity.Permission
	}{
		{name: "workiva_list_spreadsheets", want: identity.PermissionWorkivaRead},
		{name: "workiva_read_range", want: identity.PermissionWorkivaRead},
		{name: "workiva_search_fields", want: identity.PermissionWorkivaRead},
		{name: "workiva_get_field", want: identity.PermissionWorkivaRead},
		{name: "workiva_update_field", arguments: json.RawMessage(`{"name":"field","value":"1"}`), confirmationRequired: true, want: identity.PermissionWorkivaWritePreview},
		{name: "workiva_update_field", arguments: json.RawMessage(`{"name":"field","value":"1","confirm_token":"token"}`), confirmationRequired: true, want: identity.PermissionWorkivaWriteConfirm},
		{name: "workiva_update_field", arguments: json.RawMessage(`{"name":"field","value":"1"}`), confirmationRequired: false, want: identity.PermissionWorkivaWriteConfirm},
		{name: "workiva_sync_mapping", want: identity.PermissionMappingSync},
		{name: "workiva_audit_trail", want: identity.PermissionAuditRead},
	}
	for _, tc := range tests {
		t.Run(tc.name+"/"+string(tc.want), func(t *testing.T) {
			got, err := requiredPermission(tc.name, tc.arguments, tc.confirmationRequired)
			if err != nil {
				t.Fatalf("requiredPermission: %v", err)
			}
			if got != tc.want {
				t.Fatalf("permission = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestForbiddenToolCallReturns403BeforeExecutionAndIsAudited(t *testing.T) {
	deps := testDeps(t)
	principal := delegatedPrincipal(identity.PermissionWorkivaRead)
	ctx := identity.ContextWithPrincipal(context.Background(), principal)
	called := false
	handler := authorizationHTTPHandler(deps.Audit, true, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"workiva_update_field","arguments":{"name":"x","value":"1","confirm_token":"secret-confirm"}}}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer secret-access-token")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.Code)
	}
	if got := res.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if called {
		t.Fatal("forbidden request reached MCP execution")
	}
	entries := exportEntries(t, deps.Audit)
	if len(entries) != 1 || entries[0].Action != "authorization_denied" || entries[0].Actor != principal.AuditActor() {
		t.Fatalf("denial audit entries = %#v", entries)
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-access-token", "secret-confirm"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("authorization audit leaked %q: %s", secret, encoded)
		}
	}
}

func TestAuthorizationHTTPHandlerRejectsOversizedBodyWithoutExecution(t *testing.T) {
	principal := delegatedPrincipal(identity.PermissionWorkivaRead)
	ctx := identity.ContextWithPrincipal(context.Background(), principal)
	called := false
	handler := authorizationHTTPHandler(nil, true, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(strings.Repeat("x", maxPeekBody+1))).WithContext(ctx)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", res.Code)
	}
	if called {
		t.Fatal("oversized request reached MCP/Workiva execution")
	}
	if got, want := res.Body.String(), "request body exceeds 1048576 bytes\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if got := res.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestAuthorizationHTTPHandlerPreservesSmallAllowedBodyExactly(t *testing.T) {
	const body = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"workiva_list_spreadsheets","arguments":{}}}`
	principal := delegatedPrincipal(identity.PermissionWorkivaRead)
	ctx := identity.ContextWithPrincipal(context.Background(), principal)
	var downstreamBody string
	handler := authorizationHTTPHandler(nil, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read downstream body: %v", err)
		}
		downstreamBody = string(data)
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)).WithContext(ctx)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", res.Code)
	}
	if downstreamBody != body {
		t.Fatalf("downstream body = %q, want exact %q", downstreamBody, body)
	}
}

func TestActorSpoofHeadersCannotChangeEntraAuditActor(t *testing.T) {
	deps := testDeps(t)
	p := delegatedPrincipal(identity.PermissionWorkivaRead)
	ctx := identity.ContextWithPrincipal(context.Background(), p)
	req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
		Params: &mcp.CallToolParamsRaw{Name: "echo", Arguments: json.RawMessage(`{"message":"x"}`)},
		Extra: &mcp.RequestExtra{Header: http.Header{
			ActorHeader:        []string{"attacker@example.com"},
			DefaultActorHeader: []string{"other@example.com"},
		}},
	}
	called := false
	handler := auditMiddleware(deps.Audit, DefaultActorHeader)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		called = true
		return &mcp.CallToolResult{}, nil
	})
	if _, err := handler(ctx, "tools/call", req); err != nil {
		t.Fatalf("audit middleware: %v", err)
	}
	if !called {
		t.Fatal("allowed call did not execute")
	}
	entries := exportEntries(t, deps.Audit)
	if len(entries) != 1 || entries[0].Actor != p.AuditActor() {
		t.Fatalf("audit actor entries = %#v, want %q", entries, p.AuditActor())
	}
}

type fakeBuiltinReadTool struct{ called *bool }

func (f fakeBuiltinReadTool) Name() string        { return "workiva_list_spreadsheets" }
func (f fakeBuiltinReadTool) Description() string { return "test builtin read" }
func (f fakeBuiltinReadTool) RegisterSDK(s *mcp.Server, _ Deps) {
	mcp.AddTool(s, &mcp.Tool{Name: f.Name(), Description: f.Description()},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
			*f.called = true
			return &mcp.CallToolResult{}, struct{}{}, nil
		})
}

func TestEntraStreamableHTTPIsStatelessAndCarriesPrincipalPerRequest(t *testing.T) {
	deps := testDeps(t)
	principal := delegatedPrincipal(identity.PermissionWorkivaRead)
	verifier := &fakeTokenVerifier{principal: principal}
	called := false
	reg := NewRegistry()
	reg.Register(fakeBuiltinReadTool{called: &called})
	handler, err := New(deps, reg, &Options{AuthMode: config.AuthModeEntra, TokenVerifier: verifier})
	if err != nil {
		t.Fatal(err)
	}

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer valid-token")
		req.Header.Set(ActorHeader, "attacker@example.com")
		req.Header.Set(DefaultActorHeader, "other@example.com")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}

	init := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"dev"}}}`)
	if init.Code != http.StatusOK {
		t.Fatalf("initialize status = %d: %s", init.Code, init.Body.String())
	}
	if session := init.Header().Get("Mcp-Session-Id"); session != "" {
		t.Fatalf("stateless initialize issued Mcp-Session-Id %q", session)
	}
	if initialized := post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); initialized.Code != http.StatusAccepted {
		t.Fatalf("initialized status = %d: %s", initialized.Code, initialized.Body.String())
	}
	call := post(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"workiva_list_spreadsheets","arguments":{}}}`)
	if call.Code != http.StatusOK {
		t.Fatalf("tools/call status = %d: %s", call.Code, call.Body.String())
	}
	if !called {
		t.Fatal("authorized tool was not called")
	}
	entries := exportEntries(t, deps.Audit)
	if len(entries) != 1 || entries[0].Actor != principal.AuditActor() {
		t.Fatalf("audit entries = %#v, want trusted actor %q", entries, principal.AuditActor())
	}
}

func TestEntraForeignSessionIDCannotGrantStateOrBypassAuthorization(t *testing.T) {
	deps := testDeps(t)
	reader := delegatedPrincipal(identity.PermissionWorkivaRead)
	reader.ObjectID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	denied := delegatedPrincipal()
	denied.ObjectID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	verifier := &fakeTokenVerifier{principals: map[string]identity.Principal{
		"reader-token": reader,
		"denied-token": denied,
	}}
	callCount := 0
	reg := NewRegistry()
	reg.Register(fakeCountingBuiltinReadTool{callCount: &callCount})
	handler, err := New(deps, reg, &Options{AuthMode: config.AuthModeEntra, TokenVerifier: verifier})
	if err != nil {
		t.Fatal(err)
	}

	post := func(token string) *httptest.ResponseRecorder {
		body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"workiva_list_spreadsheets","arguments":{}}}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Mcp-Session-Id", "foreign-or-stale-session")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}

	if res := post("reader-token"); res.Code != http.StatusOK {
		t.Fatalf("authorized call with stale session status = %d: %s", res.Code, res.Body.String())
	}
	if callCount != 1 {
		t.Fatalf("authorized call count = %d, want 1", callCount)
	}
	if res := post("denied-token"); res.Code != http.StatusForbidden {
		t.Fatalf("denied call with foreign session status = %d, want 403: %s", res.Code, res.Body.String())
	} else if got := res.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("denied call Cache-Control = %q, want no-store", got)
	}
	if callCount != 1 {
		t.Fatalf("foreign session bypassed authorization; call count = %d", callCount)
	}

	entries := exportEntries(t, deps.Audit)
	if len(entries) != 2 || entries[0].Actor != reader.AuditActor() || entries[1].Actor != denied.AuditActor() || entries[1].Action != "authorization_denied" {
		t.Fatalf("audit entries = %#v", entries)
	}
}

type fakeCountingBuiltinReadTool struct{ callCount *int }

func (f fakeCountingBuiltinReadTool) Name() string        { return "workiva_list_spreadsheets" }
func (f fakeCountingBuiltinReadTool) Description() string { return "test builtin read" }
func (f fakeCountingBuiltinReadTool) RegisterSDK(s *mcp.Server, _ Deps) {
	mcp.AddTool(s, &mcp.Tool{Name: f.Name(), Description: f.Description()},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
			(*f.callCount)++
			return &mcp.CallToolResult{}, struct{}{}, nil
		})
}

func TestAPIKeyModeRemainsStateful(t *testing.T) {
	handler, err := New(testDeps(t), NewRegistry(), &Options{AuthMode: config.AuthModeAPIKey, APIToken: "phase-one-key"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"dev"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer phase-one-key")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("initialize status = %d: %s", res.Code, res.Body.String())
	}
	if session := res.Header().Get("Mcp-Session-Id"); session == "" {
		t.Fatal("API-key initialize did not issue Mcp-Session-Id")
	}
}

func delegatedPrincipal(permissions ...identity.Permission) identity.Principal {
	return identity.Principal{
		TenantID:    "11111111-1111-1111-1111-111111111111",
		ObjectID:    "22222222-2222-2222-2222-222222222222",
		Subject:     "subject-1",
		Issuer:      "https://login.microsoftonline.com/11111111-1111-1111-1111-111111111111/v2.0",
		TokenType:   identity.TokenTypeDelegated,
		Permissions: permissions,
	}
}
