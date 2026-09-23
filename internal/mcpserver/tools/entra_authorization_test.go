package tools

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

type toolTokenVerifier struct {
	principal identity.Principal
	err       error
}

func (v toolTokenVerifier) Verify(context.Context, string) (identity.Principal, error) {
	return v.principal, v.err
}

func TestEntra401And403MakeZeroWorkivaCalls(t *testing.T) {
	tests := []struct {
		name       string
		verifier   toolTokenVerifier
		tool       mcpserver.Tool
		arguments  string
		wantStatus int
	}{
		{
			name:       "invalid token",
			verifier:   toolTokenVerifier{err: errors.New("bad signature")},
			tool:       ListSpreadsheets(),
			arguments:  `{}`,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "read forbidden",
			verifier:   toolTokenVerifier{principal: toolPrincipal(identity.PermissionAuditRead)},
			tool:       ListSpreadsheets(),
			arguments:  `{}`,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "read only cannot preview",
			verifier:   toolTokenVerifier{principal: toolPrincipal(identity.PermissionWorkivaRead)},
			tool:       UpdateField(),
			arguments:  `{"name":"scope2_energy_kwh","value":"1"}`,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "preview only cannot confirm",
			verifier:   toolTokenVerifier{principal: toolPrincipal(identity.PermissionWorkivaWritePreview)},
			tool:       UpdateField(),
			arguments:  `{"name":"scope2_energy_kwh","value":"1","confirm_token":"opaque"}`,
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps, apiCalls := authorizationTestDeps(t)
			reg := mcpserver.NewRegistry()
			reg.Register(tc.tool)
			handler, err := mcpserver.New(deps, reg, &mcpserver.Options{
				AuthMode:      config.AuthModeEntra,
				APIToken:      "legacy-key-must-not-fallback",
				TokenVerifier: tc.verifier,
			})
			if err != nil {
				t.Fatal(err)
			}
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tc.tool.Name() + `","arguments":` + tc.arguments + `}}`
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Authorization", "Bearer invalid-or-unprivileged")
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", res.Code, tc.wantStatus, res.Body.String())
			}
			if calls := apiCalls.Load(); calls != 0 {
				t.Fatalf("Workiva API calls = %d, want 0", calls)
			}
		})
	}
}

type countingBackend struct{ calls *atomic.Int32 }

func (b countingBackend) called() { b.calls.Add(1) }

func (b countingBackend) ListSpreadsheets(context.Context) ([]workiva.Spreadsheet, error) {
	b.called()
	return nil, nil
}

func (b countingBackend) ListSheets(context.Context, string) ([]workiva.Sheet, error) {
	b.called()
	return nil, nil
}

func (b countingBackend) GetSheetData(context.Context, string, string, string, []string) (*workiva.SheetData, error) {
	b.called()
	return &workiva.SheetData{}, nil
}

func (b countingBackend) UpdateSheetWithRetryAfter(context.Context, string, string, workiva.SheetUpdate) (string, time.Duration, error) {
	b.called()
	return "", 0, nil
}

func (b countingBackend) WaitOperationWithInitialRetryAfter(context.Context, string, time.Duration) (string, error) {
	b.called()
	return "", nil
}

func authorizationTestDeps(t *testing.T) (mcpserver.Deps, *atomic.Int32) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "authorization.db")
	store, err := mapping.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	auditLog, err := audit.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = auditLog.Close() })
	calls := &atomic.Int32{}
	return mcpserver.Deps{
		Client: countingBackend{calls: calls},
		Store:  store,
		Audit:  auditLog,
		Cfg: &config.Config{
			Region:                   "eu",
			RequireWriteConfirmation: true,
		},
	}, calls
}

func toolPrincipal(permissions ...identity.Permission) identity.Principal {
	return identity.Principal{
		TenantID:    "11111111-1111-1111-1111-111111111111",
		ObjectID:    "22222222-2222-2222-2222-222222222222",
		Subject:     "subject-1",
		TokenType:   identity.TokenTypeDelegated,
		Permissions: permissions,
	}
}
