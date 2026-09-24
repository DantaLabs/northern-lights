package identity

import (
	"context"
	"reflect"
	"testing"
)

func TestPrincipalContextAndImmutableAuditActor(t *testing.T) {
	p := Principal{
		TenantID:           "11111111-1111-1111-1111-111111111111",
		ObjectID:           "22222222-2222-2222-2222-222222222222",
		Subject:            "opaque-subject",
		Issuer:             "https://login.microsoftonline.com/11111111-1111-1111-1111-111111111111/v2.0",
		AuthorizedClientID: "33333333-3333-3333-3333-333333333333",
		TokenType:          TokenTypeDelegated,
		Scopes:             []string{"nl.read"},
		DisplayLabel:       "Mutable Name",
	}

	ctx := ContextWithPrincipal(context.Background(), p)
	got, ok := PrincipalFromContext(ctx)
	if !ok {
		t.Fatal("PrincipalFromContext did not find the principal")
	}
	if !reflect.DeepEqual(got, p) {
		t.Fatalf("principal = %#v, want %#v", got, p)
	}
	if got.AuditActor() != p.TenantID+"/"+p.ObjectID {
		t.Fatalf("AuditActor() = %q", got.AuditActor())
	}
	if _, ok := PrincipalFromContext(context.Background()); ok {
		t.Fatal("empty context unexpectedly contains a principal")
	}
}

func TestSubjectFallbackAuditActorIsExplicit(t *testing.T) {
	p := Principal{TenantID: "tenant", Subject: "subject", UsedSubjectFallback: true}
	if got := p.AuditActor(); got != "tenant/sub:subject" {
		t.Fatalf("AuditActor() = %q, want tenant/sub:subject", got)
	}
	if got := (Principal{TenantID: "tenant", Subject: "subject"}).AuditActor(); got != "" {
		t.Fatalf("implicit subject fallback actor = %q, want empty", got)
	}
}

func TestPermissionValuesAreExact(t *testing.T) {
	got := AllPermissions()
	want := []Permission{
		PermissionWorkivaRead,
		PermissionWorkivaWritePreview,
		PermissionWorkivaWriteConfirm,
		PermissionMappingSync,
		PermissionAuditRead,
		PermissionTenantAdmin,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AllPermissions() = %#v, want %#v", got, want)
	}
	wantValues := []string{
		"workiva.read",
		"workiva.write.preview",
		"workiva.write.confirm",
		"mapping.sync",
		"audit.read",
		"tenant.admin",
	}
	for i, permission := range got {
		if string(permission) != wantValues[i] {
			t.Errorf("permission %d = %q, want %q", i, permission, wantValues[i])
		}
	}
}

func TestDelegatedPermissionsComeOnlyFromScopes(t *testing.T) {
	policy := PermissionPolicy{
		DelegatedScopes: map[string][]Permission{
			"nl.all": AllPermissions(),
		},
		ApplicationRoles: map[string][]Permission{
			"nl.admin": {PermissionTenantAdmin},
		},
	}
	p := Principal{TokenType: TokenTypeDelegated, Scopes: []string{"nl.all"}, Roles: []string{"nl.admin"}}
	permissions, err := policy.PermissionsFor(p)
	if err != nil {
		t.Fatalf("PermissionsFor: %v", err)
	}
	if !reflect.DeepEqual(permissions, AllPermissions()) {
		t.Fatalf("permissions = %#v, want all six", permissions)
	}

	p.Scopes = nil
	permissions, err = policy.PermissionsFor(p)
	if err != nil {
		t.Fatalf("PermissionsFor without scopes: %v", err)
	}
	if len(permissions) != 0 {
		t.Fatalf("delegated roles granted permissions: %#v", permissions)
	}
}

func TestApplicationPermissionsRequireRoleAndClientPolicy(t *testing.T) {
	policy := PermissionPolicy{
		ApplicationRoles: map[string][]Permission{
			"nl.writer": {PermissionWorkivaRead, PermissionWorkivaWriteConfirm},
		},
		ApplicationClients: map[string][]Permission{
			"client-allowed": {PermissionWorkivaRead},
		},
	}
	p := Principal{
		TokenType:          TokenTypeApplication,
		AuthorizedClientID: "client-allowed",
		Roles:              []string{"nl.writer"},
	}
	permissions, err := policy.PermissionsFor(p)
	if err != nil {
		t.Fatalf("PermissionsFor: %v", err)
	}
	if !reflect.DeepEqual(permissions, []Permission{PermissionWorkivaRead}) {
		t.Fatalf("permissions = %#v, want intersection of role and client policy", permissions)
	}

	p.AuthorizedClientID = "client-not-configured"
	if _, err := policy.PermissionsFor(p); err == nil {
		t.Fatal("unconfigured application client was authorized")
	}
}
