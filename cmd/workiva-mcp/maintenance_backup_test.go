package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dantalabs/northern-lights/internal/identity"
)

type backupMaintenanceVerifier struct {
	principal identity.Principal
	err       error
	calls     atomic.Int32
}

func (v *backupMaintenanceVerifier) Verify(_ context.Context, raw string) (identity.Principal, error) {
	v.calls.Add(1)
	if raw != "signed-token" {
		return identity.Principal{}, errors.New("invalid token")
	}
	return v.principal, v.err
}

func TestBackupMaintenanceEndpointRequiresVerifiedTenantAdminBeforeWork(t *testing.T) {
	for _, tc := range []struct {
		name        string
		method      string
		authority   string
		principal   identity.Principal
		verifyErr   error
		wantCode    int
		wantCalls   int32
		nilVerifier bool
	}{
		{name: "no bearer", method: http.MethodPost, wantCode: http.StatusUnauthorized},
		{name: "malformed bearer spacing", method: http.MethodPost, authority: "Bearer  signed-token", wantCode: http.StatusUnauthorized},
		{name: "wrong method", method: http.MethodGet, authority: "Bearer signed-token", wantCode: http.StatusMethodNotAllowed},
		{name: "nil verifier", method: http.MethodPost, authority: "Bearer signed-token", wantCode: http.StatusServiceUnavailable, nilVerifier: true},
		{name: "invalid signature", method: http.MethodPost, authority: "Bearer signed-token", verifyErr: errors.New("invalid"), wantCode: http.StatusUnauthorized, wantCalls: 1},
		{name: "wrong tenant", method: http.MethodPost, authority: "Bearer signed-token", principal: identity.Principal{TenantID: "other", ObjectID: "admin", Permissions: []identity.Permission{identity.PermissionTenantAdmin}}, wantCode: http.StatusForbidden, wantCalls: 1},
		{name: "empty object id", method: http.MethodPost, authority: "Bearer signed-token", principal: identity.Principal{TenantID: "tenant-a", Permissions: []identity.Permission{identity.PermissionTenantAdmin}}, wantCode: http.StatusForbidden, wantCalls: 1},
		{name: "not tenant admin", method: http.MethodPost, authority: "Bearer signed-token", principal: identity.Principal{TenantID: "tenant-a", ObjectID: "reader"}, wantCode: http.StatusForbidden, wantCalls: 1},
		{name: "trusted tenant admin", method: http.MethodPost, authority: "Bearer signed-token", principal: identity.Principal{TenantID: "tenant-a", ObjectID: "admin", Permissions: []identity.Permission{identity.PermissionTenantAdmin}}, wantCode: http.StatusOK, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verifier := &backupMaintenanceVerifier{principal: tc.principal, err: tc.verifyErr}
			var tokenVerifier identity.TokenVerifier = verifier
			if tc.nilVerifier {
				tokenVerifier = nil
			}
			var runs atomic.Int32
			handler := newBackupMaintenanceEndpoint(tokenVerifier, "tenant-a", func(context.Context, identity.Principal) (sealedBackupReceipt, error) {
				runs.Add(1)
				return sealedBackupReceipt{EnvelopeID: "opaque"}, nil
			})
			req := httptest.NewRequest(tc.method, "/maintenance/backup", strings.NewReader(`{"destination":"attacker-controlled"}`))
			if tc.authority != "" {
				req.Header.Set("Authorization", tc.authority)
			}
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			if resp.Code != tc.wantCode {
				t.Fatalf("status=%d body=%q want=%d", resp.Code, resp.Body.String(), tc.wantCode)
			}
			if got := verifier.calls.Load(); got != tc.wantCalls {
				t.Fatalf("verifier calls=%d want=%d", got, tc.wantCalls)
			}
			wantRuns := int32(0)
			if tc.name == "trusted tenant admin" {
				wantRuns = 1
			}
			if got := runs.Load(); got != wantRuns {
				t.Fatalf("backup executions=%d want=%d", got, wantRuns)
			}
			if tc.name == "trusted tenant admin" && !strings.Contains(resp.Body.String(), `"envelope_id":"opaque"`) {
				t.Fatalf("successful response omitted sealed receipt: %q", resp.Body.String())
			}
			if tc.name != "trusted tenant admin" && strings.Contains(resp.Body.String(), "attacker-controlled") {
				t.Fatalf("request body leaked in denial response: %q", resp.Body.String())
			}
		})
	}
}
