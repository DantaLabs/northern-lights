package assurance

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/dantalabs/northern-lights/internal/identity"
)

// TransferRoute is a bounded operator-provisioned mapping. Callers select its ID only.
type ConversionPolicy struct {
	PolicyID    string    `json:"policy_id" yaml:"policy_id"`
	Revision    int       `json:"revision" yaml:"revision"`
	Mode        string    `json:"mode" yaml:"mode"`
	SourceKind  ValueKind `json:"source_kind" yaml:"source_kind"`
	SourceUnit  string    `json:"source_unit" yaml:"source_unit"`
	TargetKind  ValueKind `json:"target_kind" yaml:"target_kind"`
	TargetUnit  string    `json:"target_unit" yaml:"target_unit"`
	ContentHash string    `json:"content_hash,omitempty" yaml:"content_hash,omitempty"`
}

func conversionPolicyContent(p ConversionPolicy) ConversionPolicy { p.ContentHash = ""; return p }
func validateConversionPolicy(p ConversionPolicy) error {
	if validateBundleString(p.PolicyID, "policy_id", 128, true) != nil || p.Revision <= 0 || p.Revision > maxRevision {
		return fmt.Errorf("policy ID and positive bounded revision are required")
	}
	if p.Mode != "typed_copy" {
		return fmt.Errorf("only typed_copy conversion is supported")
	}
	if p.SourceKind == "" || p.TargetKind == "" || p.SourceKind != p.TargetKind || p.SourceUnit != p.TargetUnit {
		return fmt.Errorf("typed_copy requires exact matching kind and unit")
	}
	return nil
}

// ResolveConversionPolicy selects only the exact policy revision in the active tenant bundle.
func (s *Store) ResolveConversionPolicy(ctx context.Context, id string, revision int) (ConversionPolicy, error) {
	if err := s.Ready(); err != nil {
		return ConversionPolicy{}, err
	}
	p, ok := identity.PrincipalFromContext(ctx)
	if !ok || p.TenantID == "" {
		return ConversionPolicy{}, domainError("strong_identity_required", "trusted tenant is required")
	}
	var raw, storedHash string
	err := s.db.QueryRowContext(ctx, `SELECT x.policy_json,x.content_hash FROM assurance_conversion_policy_revisions x JOIN assurance_active_bundle_objects a ON a.tenant_id=x.tenant_id AND a.object_kind='conversion_policy' AND a.object_id=x.policy_id AND a.object_revision=x.revision JOIN assurance_active_bundles b ON b.tenant_id=a.tenant_id AND b.singleton=1 AND b.bundle_version=a.bundle_version WHERE x.tenant_id=? AND x.policy_id=? AND x.revision=?`, p.TenantID, id, revision).Scan(&raw, &storedHash)
	if err != nil {
		return ConversionPolicy{}, domainError("conversion_policy_not_found", "exact conversion policy revision is not active")
	}
	var policy ConversionPolicy
	if json.Unmarshal([]byte(raw), &policy) != nil || policy.Revision != revision || validateConversionPolicy(policy) != nil {
		return ConversionPolicy{}, domainError("conversion_policy_integrity_failed", "active conversion policy failed integrity validation")
	}
	canonical, err := CanonicalJSON(conversionPolicyContent(policy))
	if err != nil || storedHash != policy.ContentHash || digestHex(HashBytes(canonical)) != policy.ContentHash {
		return ConversionPolicy{}, domainError("conversion_policy_integrity_failed", "active conversion policy hash verification failed")
	}
	return policy, nil
}

// ConvertTypedValue performs only bounded typed-copy; it never evaluates formulas or converts units.
func (p ConversionPolicy) ConvertTypedValue(v TypedValue) (TypedValue, error) {
	if validateConversionPolicy(p) != nil || p.Mode != "typed_copy" || v.Kind != p.SourceKind || p.SourceKind != p.TargetKind || v.Unit != p.SourceUnit || p.SourceUnit != p.TargetUnit || v.Formula || v.FormulaText != "" || v.Calculated {
		return TypedValue{}, domainError("conversion_unsupported", "value is not an exact non-formula typed copy")
	}
	return v, nil
}

type TransferRoute struct {
	RouteID                 string                   `json:"route_id" yaml:"route_id"`
	Revision                int                      `json:"revision" yaml:"revision"`
	SourceMappingID         string                   `json:"source_mapping_id" yaml:"source_mapping_id"`
	SourceResourceID        string                   `json:"source_resource_id" yaml:"source_resource_id"`
	SourceSheetID           string                   `json:"source_sheet_id" yaml:"source_sheet_id"`
	SourceLocator           string                   `json:"source_locator" yaml:"source_locator"`
	TargetResourceID        string                   `json:"target_resource_id" yaml:"target_resource_id"`
	TargetSheetID           string                   `json:"target_sheet_id" yaml:"target_sheet_id"`
	TargetLocator           string                   `json:"target_locator" yaml:"target_locator"`
	ConversionPolicyID      string                   `json:"conversion_policy_id" yaml:"conversion_policy_id"`
	ConversionPolicyVersion int                      `json:"conversion_policy_version" yaml:"conversion_policy_version"`
	Capabilities            []string                 `json:"capabilities" yaml:"capabilities"`
	ExportProfiles          []ExportProfileReference `json:"export_profiles,omitempty" yaml:"export_profiles,omitempty"`
	ContentHash             string                   `json:"content_hash,omitempty" yaml:"content_hash,omitempty"`
}

// ExportProfileReference pins a transfer export to the exact profile revision
// approved alongside the signed route, never to a later active revision.
type ExportProfileReference struct {
	ProfileID string `json:"profile_id" yaml:"profile_id"`
	Revision  int    `json:"revision" yaml:"revision"`
}

func transferRouteContent(r TransferRoute) TransferRoute { r.ContentHash = ""; return r }

var singleCellLocator = regexp.MustCompile(`^[A-Za-z0-9 _.-]{1,128}!(?:\$?[A-Z]{1,3}\$?[1-9][0-9]{0,6})$`)

func validateTransferRoute(r TransferRoute) error {
	for _, v := range []struct {
		s   string
		n   string
		max int
	}{{r.RouteID, "route_id", 128}, {r.SourceMappingID, "source_mapping_id", 128}, {r.SourceResourceID, "source_resource_id", 128}, {r.SourceSheetID, "source_sheet_id", 128}, {r.SourceLocator, "source_locator", 256}, {r.TargetResourceID, "target_resource_id", 128}, {r.TargetSheetID, "target_sheet_id", 128}, {r.TargetLocator, "target_locator", 256}, {r.ConversionPolicyID, "conversion_policy_id", 128}} {
		if err := validateBundleString(v.s, v.n, v.max, true); err != nil {
			return err
		}
	}
	if r.Revision <= 0 || r.Revision > maxRevision || r.ConversionPolicyVersion <= 0 || r.ConversionPolicyVersion > maxRevision {
		return fmt.Errorf("route and conversion policy revisions must be positive and bounded")
	}
	if !singleCellLocator.MatchString(r.TargetLocator) {
		return fmt.Errorf("target locator must identify exactly one cell on a sheet")
	}
	if len(r.Capabilities) == 0 || len(r.Capabilities) > 8 {
		return fmt.Errorf("route requires 1 through 8 capabilities")
	}
	if len(r.ExportProfiles) > maxExportProfiles {
		return fmt.Errorf("route export profile references exceed the bound")
	}
	seenProfiles := map[string]bool{}
	for _, profile := range r.ExportProfiles {
		if err := validateBundleString(profile.ProfileID, "export_profile", 128, true); err != nil || profile.Revision <= 0 || profile.Revision > maxRevision || seenProfiles[profile.ProfileID] {
			return fmt.Errorf("route export profile references must be bounded and unique")
		}
		seenProfiles[profile.ProfileID] = true
	}
	seen := map[string]bool{}
	for _, c := range r.Capabilities {
		if err := validateBundleString(c, "capability", 128, true); err != nil || seen[c] {
			return fmt.Errorf("capabilities must be bounded and unique")
		}
		seen[c] = true
	}
	return nil
}

// MatchTransferRoute resolves an exact signed route from the caller's selectors.
// The caller can narrow a route but cannot supply the write destination or value.
// Ambiguous matches fail closed. ResolveTransferRoute performs active-bundle,
// integrity, tenant and capability checks for each candidate.
func (s *Store) MatchTransferRoute(ctx context.Context, mappingID, sourceResourceID, sourceLocator, targetResourceID, targetLocator, policyID string) (TransferRoute, error) {
	p, ok := identity.PrincipalFromContext(ctx)
	if !ok || p.TenantID == "" || p.AuditActor() == "" || !p.HasPermission(identity.PermissionWorkivaWritePreview) {
		return TransferRoute{}, domainError("transfer_route_denied", "trusted preview capability required")
	}
	if targetResourceID == "" || targetLocator == "" || (mappingID == "" && (sourceResourceID == "" || sourceLocator == "")) {
		return TransferRoute{}, domainError("transfer_route_denied", "exact approved source and target selectors required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT x.route_id FROM assurance_transfer_route_revisions x JOIN assurance_active_bundle_objects a ON a.tenant_id=x.tenant_id AND a.object_kind='transfer_route' AND a.object_id=x.route_id AND a.object_revision=x.revision JOIN assurance_active_bundles b ON b.tenant_id=a.tenant_id AND b.singleton=1 AND b.bundle_version=a.bundle_version WHERE x.tenant_id=?`, p.TenantID)
	if err != nil {
		return TransferRoute{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return TransferRoute{}, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return TransferRoute{}, err
	}
	var match TransferRoute
	for _, id := range ids {
		r, err := s.ResolveTransferRoute(ctx, id)
		if err != nil {
			return TransferRoute{}, err
		}
		if r.TargetResourceID != targetResourceID || r.TargetLocator != targetLocator ||
			(mappingID != "" && r.SourceMappingID != mappingID) ||
			(mappingID == "" && (r.SourceResourceID != sourceResourceID || r.SourceLocator != sourceLocator)) ||
			(policyID != "" && r.ConversionPolicyID != policyID) {
			continue
		}
		if match.RouteID != "" {
			return TransferRoute{}, domainError("transfer_route_ambiguous", "multiple approved routes match")
		}
		match = r
	}
	if match.RouteID == "" {
		return TransferRoute{}, domainError("transfer_route_not_found", "no approved route matches exact selectors")
	}
	return match, nil
}

// ResolveTransferRoute requires a trusted tenant and actor, an active bundle pointer,
// the exact active route revision, and every allowlisted capability.
func (s *Store) ResolveTransferRoute(ctx context.Context, routeID string) (TransferRoute, error) {
	if err := s.Ready(); err != nil {
		return TransferRoute{}, err
	}
	p, ok := identity.PrincipalFromContext(ctx)
	if !ok || p.TenantID == "" || p.AuditActor() == "" {
		return TransferRoute{}, domainError("strong_identity_required", "trusted tenant and actor are required")
	}
	var r TransferRoute
	var raw, storedHash string
	var rev int
	err := s.db.QueryRowContext(ctx, `SELECT x.route_json,x.revision,x.content_hash FROM assurance_transfer_route_revisions x JOIN assurance_active_bundle_objects a ON a.tenant_id=x.tenant_id AND a.object_kind='transfer_route' AND a.object_id=x.route_id AND a.object_revision=x.revision JOIN assurance_active_bundles b ON b.tenant_id=a.tenant_id AND b.singleton=1 AND b.bundle_version=a.bundle_version WHERE x.tenant_id=? AND x.route_id=?`, p.TenantID, routeID).Scan(&raw, &rev, &storedHash)
	if err != nil {
		return TransferRoute{}, domainError("transfer_route_not_found", "active transfer route was not found")
	}
	if err = json.Unmarshal([]byte(raw), &r); err != nil || r.Revision != rev || validateTransferRoute(r) != nil {
		return TransferRoute{}, domainError("transfer_route_integrity_failed", "active transfer route failed integrity validation")
	}
	canonical, hashErr := CanonicalJSON(transferRouteContent(r))
	if hashErr != nil || storedHash != r.ContentHash || digestHex(HashBytes(canonical)) != r.ContentHash {
		return TransferRoute{}, domainError("transfer_route_integrity_failed", "active transfer route content hash verification failed")
	}
	if _, err := s.ResolveConversionPolicy(ctx, r.ConversionPolicyID, r.ConversionPolicyVersion); err != nil {
		return TransferRoute{}, err
	}
	for _, c := range r.Capabilities {
		if !p.HasPermission(identity.Permission(c)) {
			return TransferRoute{}, domainError("transfer_route_denied", "required route capability is not granted")
		}
	}
	return r, nil
}
