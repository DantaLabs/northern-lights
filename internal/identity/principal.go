// Package identity defines trusted caller principals and authorization policy.
package identity

import (
	"context"
	"errors"
)

// TokenType distinguishes delegated user access tokens from app-only tokens.
type TokenType string

const (
	TokenTypeDelegated   TokenType = "delegated"
	TokenTypeApplication TokenType = "application"

	// LegacyTenantID owns API-key requests and rows created before tenant
	// isolation. Entra tenant IDs are canonical UUIDs, so this value cannot
	// collide with a verified Entra tenant.
	LegacyTenantID = "legacy-api-key"
)

// Permission is one Northern Lights authorization capability.
type Permission string

const (
	PermissionWorkivaRead         Permission = "workiva.read"
	PermissionWorkivaWritePreview Permission = "workiva.write.preview"
	PermissionWorkivaWriteConfirm Permission = "workiva.write.confirm"
	PermissionMappingSync         Permission = "mapping.sync"
	PermissionAuditRead           Permission = "audit.read"
	PermissionTenantAdmin         Permission = "tenant.admin"
)

var allPermissions = []Permission{
	PermissionWorkivaRead,
	PermissionWorkivaWritePreview,
	PermissionWorkivaWriteConfirm,
	PermissionMappingSync,
	PermissionAuditRead,
	PermissionTenantAdmin,
}

// AllPermissions returns the complete permission vocabulary in stable order.
func AllPermissions() []Permission {
	return append([]Permission(nil), allPermissions...)
}

// Principal is the verified identity and authorization data attached to a
// request. DisplayLabel is mutable identity metadata and must never be used for
// authorization or audit attribution.
type Principal struct {
	TenantID            string
	ObjectID            string
	Subject             string
	Issuer              string
	AuthorizedClientID  string
	TokenType           TokenType
	Scopes              []string
	Roles               []string
	DisplayLabel        string
	Permissions         []Permission
	UsedSubjectFallback bool
}

// AuditActor returns the stable tenant/object identity. Subject fallback is
// visible in the value and is used only when validation explicitly enabled it.
func (p Principal) AuditActor() string {
	if p.TenantID == "" {
		return ""
	}
	if p.ObjectID != "" {
		return p.TenantID + "/" + p.ObjectID
	}
	if p.UsedSubjectFallback && p.Subject != "" {
		return p.TenantID + "/sub:" + p.Subject
	}
	return ""
}

// HasPermission reports whether the verified policy granted permission.
func (p Principal) HasPermission(permission Permission) bool {
	for _, granted := range p.Permissions {
		if granted == permission {
			return true
		}
	}
	return false
}

type principalContextKey struct{}

// ContextWithPrincipal returns a child context carrying an isolated copy of p.
func ContextWithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, clonePrincipal(p))
}

// PrincipalFromContext retrieves an isolated copy of the verified principal.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(Principal)
	if !ok {
		return Principal{}, false
	}
	return clonePrincipal(p), true
}

// StorageTenant returns the only tenant identifier storage layers may trust.
// Verified Entra middleware supplies Principal.TenantID; contexts without a
// principal are explicit API-key/legacy operations.
func StorageTenant(ctx context.Context) string {
	if p, ok := PrincipalFromContext(ctx); ok && p.TenantID != "" {
		return p.TenantID
	}
	return LegacyTenantID
}

func clonePrincipal(p Principal) Principal {
	p.Scopes = append([]string(nil), p.Scopes...)
	p.Roles = append([]string(nil), p.Roles...)
	p.Permissions = append([]Permission(nil), p.Permissions...)
	return p
}

// PermissionPolicy maps Entra scope and app-role values to Northern Lights
// permissions. Application permissions are the intersection of the token's
// roles and the explicitly configured client policy.
type PermissionPolicy struct {
	DelegatedScopes    map[string][]Permission
	ApplicationRoles   map[string][]Permission
	ApplicationClients map[string][]Permission
}

// PermissionsFor derives permissions from scp for delegated tokens or roles
// for application tokens. Delegated roles and application scopes are ignored.
func (p PermissionPolicy) PermissionsFor(principal Principal) ([]Permission, error) {
	switch principal.TokenType {
	case TokenTypeDelegated:
		granted := make(map[Permission]bool)
		for _, scope := range principal.Scopes {
			for _, permission := range p.DelegatedScopes[scope] {
				granted[permission] = true
			}
		}
		return orderedPermissions(granted), nil
	case TokenTypeApplication:
		clientPermissions, ok := p.ApplicationClients[principal.AuthorizedClientID]
		if !ok || principal.AuthorizedClientID == "" {
			return nil, errors.New("application client is not authorized")
		}
		allowedByClient := make(map[Permission]bool, len(clientPermissions))
		for _, permission := range clientPermissions {
			allowedByClient[permission] = true
		}
		granted := make(map[Permission]bool)
		for _, role := range principal.Roles {
			for _, permission := range p.ApplicationRoles[role] {
				if allowedByClient[permission] {
					granted[permission] = true
				}
			}
		}
		return orderedPermissions(granted), nil
	default:
		return nil, errors.New("unknown token type")
	}
}

func orderedPermissions(granted map[Permission]bool) []Permission {
	permissions := make([]Permission, 0, len(granted))
	for _, permission := range allPermissions {
		if granted[permission] {
			permissions = append(permissions, permission)
		}
	}
	return permissions
}
