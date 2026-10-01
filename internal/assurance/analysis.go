package assurance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/dantalabs/northern-lights/internal/identity"
)

// SnapshotAnalysis is the immutable local input used by validation and
// comparison. Membership is loaded from the historical report revision, never
// inferred from whichever observations happened to be present.
type SnapshotAnalysis struct {
	Response            SnapshotResponse
	ReportID            string
	Revision            int
	Membership          []FieldDefinition
	MaterialityPolicyID string
}

func (s *Store) ResolveRuleSet(ctx context.Context, id string, revision int) (RuleSet, error) {
	if err := s.Ready(); err != nil {
		return RuleSet{}, err
	}
	tenant := identity.StorageTenant(ctx)
	query := `SELECT r.revision, r.status, r.content_hash, r.definition_json FROM assurance_rule_sets r JOIN assurance_active_bundle_objects a ON a.tenant_id=r.tenant_id AND a.object_kind='rule_set' AND a.object_id=r.rule_set_id AND a.object_revision=r.revision WHERE r.tenant_id=? AND r.rule_set_id=? AND r.status='active'`
	args := []any{tenant, id}
	if revision > 0 {
		query += ` AND revision=?`
		args = append(args, revision)
	}
	query += ` ORDER BY revision DESC LIMIT 1`
	var set RuleSet
	var definition string
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&set.Revision, &set.Status, &set.ContentHash, &definition)
	if errors.Is(err, sql.ErrNoRows) {
		return RuleSet{}, domainError("rule_set_not_found", "approved rule set was not found")
	}
	if err != nil {
		return RuleSet{}, err
	}
	if err := json.Unmarshal([]byte(definition), &set); err != nil {
		return RuleSet{}, fmt.Errorf("assurance: decode rule set: %w", err)
	}
	set.RuleSetID = id
	return set, nil
}

func (s *Store) ResolveMaterialityPolicy(ctx context.Context, id string, revision int) (MaterialityPolicy, error) {
	if err := s.Ready(); err != nil {
		return MaterialityPolicy{}, err
	}
	tenant := identity.StorageTenant(ctx)
	query := `SELECT p.revision, p.policy_json FROM assurance_materiality_policies p JOIN assurance_active_bundle_objects a ON a.tenant_id=p.tenant_id AND a.object_kind='materiality_policy' AND a.object_id=p.policy_id AND a.object_revision=p.revision WHERE p.tenant_id=? AND p.policy_id=? AND p.status='active'`
	args := []any{tenant, id}
	if revision > 0 {
		query += ` AND revision=?`
		args = append(args, revision)
	}
	query += ` ORDER BY revision DESC LIMIT 1`
	var policy MaterialityPolicy
	var raw string
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&policy.Revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return MaterialityPolicy{}, domainError("materiality_policy_not_found", "approved materiality policy was not found")
	}
	if err != nil {
		return MaterialityPolicy{}, err
	}
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return MaterialityPolicy{}, fmt.Errorf("assurance: decode materiality policy: %w", err)
	}
	policy.PolicyID = id
	return policy, nil
}

func (s *Store) LoadReportRevision(ctx context.Context, reportID string, revision int) (ReportRevision, error) {
	tenant := identity.StorageTenant(ctx)
	var report ReportRevision
	var periodsJSON, exportsJSON string
	err := s.db.QueryRowContext(ctx, `SELECT report_id, revision, name, description, owner, status, retention_class, resource_policy_hash, rule_set_id, materiality_policy_id, export_profiles_json, effective_from, effective_to, periods_json, content_hash FROM assurance_report_revisions WHERE tenant_id=? AND report_id=? AND revision=?`, tenant, reportID, revision).Scan(&report.ReportID, &report.Revision, &report.Name, &report.Description, &report.Owner, &report.Status, &report.RetentionClass, &report.ResourcePolicyHash, &report.RuleSetID, &report.MaterialityPolicyID, &exportsJSON, &report.EffectiveFrom, &report.EffectiveTo, &periodsJSON, &report.ContentHash)
	if errors.Is(err, sql.ErrNoRows) {
		return ReportRevision{}, domainError("report_revision_not_found", "report definition revision was not found")
	}
	if err != nil {
		return ReportRevision{}, err
	}
	if err := json.Unmarshal([]byte(periodsJSON), &report.Periods); err != nil {
		return ReportRevision{}, err
	}
	if err := json.Unmarshal([]byte(exportsJSON), &report.ExportProfiles); err != nil {
		return ReportRevision{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT field_id, resource_id, external_resource_id, subresource_id, locator, value_kind, unit, scale, precision_value, percent_basis, timezone, required, field_order, mapping_revision FROM assurance_report_fields WHERE tenant_id=? AND report_id=? AND revision=? ORDER BY field_order`, tenant, reportID, revision)
	if err != nil {
		return ReportRevision{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var field FieldDefinition
		if err := rows.Scan(&field.FieldID, &field.ResourceID, &field.ExternalResourceID, &field.SubresourceID, &field.Locator, &field.Kind, &field.Unit, &field.Scale, &field.Precision, &field.PercentBasis, &field.Timezone, &field.Required, &field.Order, &field.MappingRevision); err != nil {
			return ReportRevision{}, err
		}
		report.Fields = append(report.Fields, field)
	}
	if err := rows.Err(); err != nil {
		return ReportRevision{}, err
	}
	return report, nil
}

func (s *Store) SnapshotForAnalysis(ctx context.Context, id string) (SnapshotAnalysis, error) {
	if err := s.Ready(); err != nil {
		return SnapshotAnalysis{}, err
	}
	tenant := identity.StorageTenant(ctx)
	var reportID string
	var revision int
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT report_id, definition_revision, status FROM assurance_snapshots WHERE tenant_id=? AND snapshot_id=?`, tenant, id).Scan(&reportID, &revision, &status)
	if errors.Is(err, sql.ErrNoRows) || status == "running" || status == "failed" || status == "expired" {
		return SnapshotAnalysis{}, domainError("snapshot_not_found", "immutable snapshot was not found")
	}
	if err != nil {
		return SnapshotAnalysis{}, err
	}
	response, err := s.Snapshot(ctx, id)
	if err != nil {
		return SnapshotAnalysis{}, err
	}
	report, err := s.LoadReportRevision(ctx, reportID, revision)
	if err != nil {
		return SnapshotAnalysis{}, err
	}
	return SnapshotAnalysis{Response: response, ReportID: reportID, Revision: revision, Membership: append([]FieldDefinition(nil), report.Fields...), MaterialityPolicyID: report.MaterialityPolicyID}, nil
}

func membershipByField(fields []FieldDefinition) map[string]FieldDefinition {
	result := make(map[string]FieldDefinition, len(fields))
	for _, field := range fields {
		result[field.FieldID] = field
	}
	return result
}

func sortedFieldIDs(values map[string]FieldDefinition) []string {
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
