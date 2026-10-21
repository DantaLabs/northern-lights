package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type demoAuthTool struct {
	name   string
	called *bool
}

func (f demoAuthTool) Name() string        { return f.name }
func (f demoAuthTool) Description() string { return "test demo authorization" }
func (f demoAuthTool) RegisterSDK(s *mcp.Server, _ Deps) {
	mcp.AddTool(s, &mcp.Tool{Name: f.Name(), Description: f.Description()},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
			*f.called = true
			return &mcp.CallToolResult{}, struct{}{}, nil
		})
}

func TestDemoToolsEntraAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name       string
		permission identity.Permission
	}{
		{"workiva_prepare_section", identity.PermissionWorkivaWritePreview},
		{"workiva_stage_section_text", identity.PermissionWorkivaWritePreview},
		{"workiva_confirm_change_set", identity.PermissionWorkivaWriteConfirm},
		{"workiva_confirm_section_text", identity.PermissionWorkivaWriteConfirm},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !writeTools[tc.name] {
				t.Fatal("demo tool must be audited as a write")
			}
			for _, permission := range []identity.Permission{tc.permission, identity.PermissionWorkivaRead} {
				t.Run(string(permission), func(t *testing.T) {
					called := false
					reg := NewRegistry()
					reg.Register(demoAuthTool{name: tc.name, called: &called})
					handler, err := New(testDeps(t), reg, &Options{AuthMode: config.AuthModeEntra, TokenVerifier: &fakeTokenVerifier{principal: delegatedPrincipal(permission)}})
					if err != nil {
						t.Fatal(err)
					}
					req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, tc.name)))
					req.Header.Set("Authorization", "Bearer valid-token")
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Accept", "application/json, text/event-stream")
					res := httptest.NewRecorder()
					handler.ServeHTTP(res, req)
					allowed := permission == tc.permission
					wantStatus := http.StatusForbidden
					if allowed {
						wantStatus = http.StatusOK
					}
					if res.Code != wantStatus || called != allowed {
						t.Fatalf("status=%d called=%v, want status=%d called=%v: %s", res.Code, called, wantStatus, allowed, res.Body.String())
					}
				})
			}
		})
	}
}
