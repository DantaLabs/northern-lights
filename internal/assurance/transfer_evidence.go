package assurance

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/dantalabs/northern-lights/internal/identity"
)

// transferEvidenceIntent is deliberately independent of internal/transfer to
// avoid an import cycle and to keep the export surface explicitly allowlisted.
type transferEvidenceIntent struct {
	TransferID     string                   `json:"transfer_id"`
	TenantID       string                   `json:"tenant_id"`
	ActorID        string                   `json:"actor_id"`
	Permission     string                   `json:"permission"`
	RouteID        string                   `json:"route_id"`
	RouteRevision  int                      `json:"route_revision"`
	MappingID      string                   `json:"mapping_id"`
	Source         transferEvidenceEndpoint `json:"source"`
	Target         transferEvidenceEndpoint `json:"target"`
	Before         string                   `json:"before"`
	Intended       string                   `json:"intended"`
	PolicyID       string                   `json:"policy_id"`
	PolicyRevision int                      `json:"policy_revision"`
	PolicyHash     string                   `json:"policy_hash"`
	MappingHash    string                   `json:"mapping_hash"`
	ExpiresAt      string                   `json:"expires_at"`
}

type transferEvidenceEndpoint struct {
	ResourceID  string `json:"resource_id"`
	SheetID     string `json:"sheet_id"`
	Locator     string `json:"locator"`
	Fingerprint string `json:"fingerprint"`
}

type transferEvidenceFence struct {
	Kind      string `json:"kind"`
	Digest    string `json:"digest"`
	CreatedAt string `json:"created_at"`
}

type transferEvidenceAuditEvent struct {
	EventID     string `json:"event_id"`
	Disposition string `json:"disposition"`
	CreatedAt   string `json:"created_at"`
}

type transferEvidenceReadback struct {
	ReadbackID    string     `json:"readback_id"`
	Value         TypedValue `json:"value"`
	CacheBypassed bool       `json:"cache_bypassed"`
	ObservedAt    string     `json:"observed_at"`
}

type transferEvidenceAuditLink struct {
	LinkID    string `json:"link_id"`
	AuditID   string `json:"audit_id"`
	CreatedAt string `json:"created_at"`
}

type transferEvidenceVisual struct {
	AcknowledgementID string      `json:"acknowledgement_id"`
	ConfirmerActorID  string      `json:"confirmer_actor_id"`
	AckActorID        string      `json:"ack_actor_id"`
	Observation       string      `json:"observation"`
	UIResourceID      string      `json:"ui_resource_id,omitempty"`
	UILocator         string      `json:"ui_locator,omitempty"`
	Refreshed         bool        `json:"refreshed"`
	ObservedValue     *TypedValue `json:"observed_value,omitempty"`
	ObservedDigest    string      `json:"observed_digest,omitempty"`
	CreatedAt         string      `json:"created_at"`
}

type transferEvidenceReconciliation struct {
	ReconciliationID string `json:"reconciliation_id"`
	Reason           string `json:"reason"`
	Classification   string `json:"classification"`
	Disposition      string `json:"disposition"`
	CreatedAt        string `json:"created_at"`
}

func (s *Store) transferSubjectJSON(ctx context.Context, id string) ([]byte, error) {
	tenant := identity.StorageTenant(ctx)
	var row struct {
		ActorID, Permission, IntentJSON, State, OperationReference string
		OperationCompleted                                         int
		MachineOutcome, MachineProvenance, VisualState             string
		ClaimFenceDigest, TerminalFenceDigest                      string
	}
	err := s.db.QueryRowContext(ctx, `SELECT actor_id,permission,intent_json,state,operation_reference,operation_completed,machine_outcome,machine_outcome_provenance,visual_state,claim_fence_digest,terminal_fence_digest FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, id).Scan(
		&row.ActorID, &row.Permission, &row.IntentJSON, &row.State, &row.OperationReference, &row.OperationCompleted, &row.MachineOutcome, &row.MachineProvenance, &row.VisualState, &row.ClaimFenceDigest, &row.TerminalFenceDigest)
	if err != nil {
		return nil, domainError("subject_not_found", "transfer evidence subject was not found")
	}
	var intent transferEvidenceIntent
	if json.Unmarshal([]byte(row.IntentJSON), &intent) != nil || intent.TransferID != id || intent.TenantID != tenant || intent.ActorID != row.ActorID || intent.Permission != row.Permission || intent.RouteID == "" || intent.RouteRevision <= 0 || intent.MappingHash == "" {
		return nil, domainError("subject_integrity_failed", "transfer evidence intent failed integrity validation")
	}
	if err := s.verifyFrozenTransferRoute(ctx, tenant, intent); err != nil {
		return nil, err
	}
	beforeValue, err := decodeTransferEvidenceValue(intent.Before)
	if err != nil {
		return nil, err
	}
	intendedValue, err := decodeTransferEvidenceValue(intent.Intended)
	if err != nil {
		return nil, err
	}
	fences, err := loadTransferEvidenceFences(ctx, s.db, tenant, id)
	if err != nil {
		return nil, err
	}
	audits, err := loadTransferEvidenceAuditEvents(ctx, s.db, tenant, id)
	if err != nil {
		return nil, err
	}
	visuals, err := loadTransferEvidenceVisuals(ctx, s.db, tenant, id)
	if err != nil {
		return nil, err
	}
	legacyReconciliations, err := loadTransferEvidenceLegacyReconciliations(ctx, s.db, tenant, id)
	if err != nil {
		return nil, err
	}
	reconciliations, err := loadTransferEvidenceReconciliations(ctx, s.db, tenant, id)
	if err != nil {
		return nil, err
	}
	readbacks, err := loadTransferEvidenceReadbacks(ctx, s.db, tenant, id)
	if err != nil {
		return nil, err
	}
	links, err := loadTransferEvidenceAuditLinks(ctx, s.db, tenant, id)
	if err != nil {
		return nil, err
	}
	quarantines, err := loadTransferEvidenceQuarantines(ctx, s.db, tenant, id)
	if err != nil {
		return nil, err
	}
	intentExport := struct {
		TransferID     string                   `json:"transfer_id"`
		TenantID       string                   `json:"tenant_id"`
		ActorID        string                   `json:"actor_id"`
		Permission     string                   `json:"permission"`
		RouteID        string                   `json:"route_id"`
		RouteRevision  int                      `json:"route_revision"`
		MappingID      string                   `json:"mapping_id"`
		Source         transferEvidenceEndpoint `json:"source"`
		Target         transferEvidenceEndpoint `json:"target"`
		Before         TypedValue               `json:"before"`
		Intended       TypedValue               `json:"intended"`
		PolicyID       string                   `json:"policy_id"`
		PolicyRevision int                      `json:"policy_revision"`
		PolicyHash     string                   `json:"policy_hash"`
		MappingHash    string                   `json:"mapping_hash"`
		ExpiresAt      string                   `json:"expires_at"`
	}{intent.TransferID, intent.TenantID, intent.ActorID, intent.Permission, intent.RouteID, intent.RouteRevision, intent.MappingID, intent.Source, intent.Target, beforeValue, intendedValue, intent.PolicyID, intent.PolicyRevision, intent.PolicyHash, intent.MappingHash, intent.ExpiresAt}
	return CanonicalJSON(map[string]any{
		"transfer_id": id, "intent": intentExport,
		"state": row.State, "operation_reference": safeTransferOperationReference(row.OperationReference),
		"operation_completed": row.OperationCompleted != 0,
		"machine_outcome":     row.MachineOutcome, "machine_outcome_provenance": row.MachineProvenance,
		"visual_state": row.VisualState, "claim_fence_digest": row.ClaimFenceDigest, "terminal_fence_digest": row.TerminalFenceDigest,
		"external_fences": fences, "uncached_readbacks": readbacks,
		"audit_events": audits, "visual_acknowledgements": visuals,
		"reconciliations": reconciliations, "audit_links": links, "startup_quarantines": quarantines,
		"legacy_reconciliations": legacyReconciliations,
		"omissions":              []string{"confirmation tokens, idempotency/request digests, provider credentials, raw fence bodies, private filesystem paths, credential-bearing operation URLs, and unallowlisted audit details are excluded"},
	})
}

func safeTransferOperationReference(value string) string {
	if value == "" || len(value) > 2048 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\t") {
		return ""
	}
	u, err := url.Parse(value)
	if err != nil {
		return ""
	}
	if u.Scheme == "https" {
		if u.Opaque != "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, "/") || strings.Contains(u.Path, "\\") {
			return ""
		}
		for _, segment := range strings.Split(u.EscapedPath(), "/") {
			decoded, decodeErr := url.PathUnescape(segment)
			if decodeErr != nil || decoded == "." || decoded == ".." || strings.ContainsAny(decoded, "/\\") {
				return ""
			}
		}
		return value
	}
	// Some providers expose an opaque operation ID instead of a URL. Keep this
	// deliberately narrow: it must not be a path, URI, or control-bearing value.
	if u.Scheme != "" || u.Host != "" || strings.ContainsAny(value, "/\\?#:@") {
		return ""
	}
	for i, r := range value {
		alphanumeric := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		separator := i > 0 && (r == '-' || r == '_' || r == '.')
		if !alphanumeric && !separator {
			return ""
		}
	}
	if value[0] == '.' || strings.Contains(value, "..") {
		return ""
	}
	return value
}

func decodeTransferEvidenceValue(raw string) (TypedValue, error) {
	var value TypedValue
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil || decoder.Decode(new(any)) == nil || value.Kind == "" || value.Kind != ValueBlank && value.Kind != ValueText && value.Kind != ValueInteger && value.Kind != ValueNumber && value.Kind != ValueBoolean && value.Kind != ValueDate && value.Kind != ValueDateTime && value.Kind != ValueCurrency && value.Kind != ValuePercent && value.Kind != ValueError {
		return TypedValue{}, domainError("subject_integrity_failed", "transfer evidence contains an invalid typed value")
	}
	canonical, err := CanonicalJSON(value)
	if err != nil || !bytes.Equal(canonical, []byte(raw)) {
		return TypedValue{}, domainError("subject_integrity_failed", "transfer evidence typed value is not canonical")
	}
	return value, nil
}

func loadTransferEvidenceReadbacks(ctx context.Context, db *sql.DB, tenant, id string) ([]transferEvidenceReadback, error) {
	rows, err := db.QueryContext(ctx, `SELECT readback_id,typed_value_json,cache_bypassed,observed_at FROM assurance_transfer_readbacks WHERE tenant_id=? AND transfer_id=? ORDER BY observed_at,readback_id`, tenant, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []transferEvidenceReadback{}
	for rows.Next() {
		var value transferEvidenceReadback
		var raw string
		var bypassed int
		if err := rows.Scan(&value.ReadbackID, &raw, &bypassed, &value.ObservedAt); err != nil {
			return nil, err
		}
		decoded, err := decodeTransferEvidenceValue(raw)
		if err != nil {
			return nil, err
		}
		value.Value, value.CacheBypassed = decoded, bypassed == 1
		result = append(result, value)
		if len(result) > 1000 {
			return nil, fmt.Errorf("transfer read-back evidence exceeds bound")
		}
	}
	return result, rows.Err()
}

func loadTransferEvidenceAuditLinks(ctx context.Context, db *sql.DB, tenant, id string) ([]transferEvidenceAuditLink, error) {
	rows, err := db.QueryContext(ctx, `SELECT link_id,audit_id,created_at FROM assurance_audit_links WHERE tenant_id=? AND entity_kind='transfer' AND entity_id=? ORDER BY created_at,link_id`, tenant, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []transferEvidenceAuditLink{}
	for rows.Next() {
		var value transferEvidenceAuditLink
		if err := rows.Scan(&value.LinkID, &value.AuditID, &value.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, value)
		if len(result) > 1000 {
			return nil, fmt.Errorf("transfer audit links exceed bound")
		}
	}
	return result, rows.Err()
}

func loadTransferEvidenceQuarantines(ctx context.Context, db *sql.DB, tenant, id string) ([]map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT environment_digest,disposition,reason,audit_id,created_at FROM assurance_transfer_startup_quarantines WHERE tenant_id=? AND transfer_id=? ORDER BY created_at,environment_digest`, tenant, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []map[string]string{}
	for rows.Next() {
		var env, disposition, reason, auditID, created string
		if err := rows.Scan(&env, &disposition, &reason, &auditID, &created); err != nil {
			return nil, err
		}
		result = append(result, map[string]string{"environment_digest": env, "disposition": disposition, "reason": reason, "audit_id": auditID, "created_at": created})
		if len(result) > 1000 {
			return nil, fmt.Errorf("transfer quarantine evidence exceeds bound")
		}
	}
	return result, rows.Err()
}

func (s *Store) verifyFrozenTransferRoute(ctx context.Context, tenant string, intent transferEvidenceIntent) error {
	var raw, storedHash string
	err := s.db.QueryRowContext(ctx, `SELECT route_json,content_hash FROM assurance_transfer_route_revisions WHERE tenant_id=? AND route_id=? AND revision=?`, tenant, intent.RouteID, intent.RouteRevision).Scan(&raw, &storedHash)
	if err != nil {
		return domainError("transfer_route_unavailable", "the exact frozen transfer route revision is unavailable")
	}
	var route TransferRoute
	if json.Unmarshal([]byte(raw), &route) != nil || route.Revision != intent.RouteRevision || route.RouteID != intent.RouteID || route.ContentHash != intent.MappingHash || storedHash != intent.MappingHash || validateTransferRoute(route) != nil {
		return domainError("transfer_route_integrity_failed", "the exact frozen transfer route does not match the transfer intent")
	}
	canonical, err := CanonicalJSON(transferRouteContent(route))
	if err != nil || digestHex(HashBytes(canonical)) != intent.MappingHash {
		return domainError("transfer_route_integrity_failed", "the exact frozen transfer route hash is invalid")
	}
	return nil
}

func loadTransferEvidenceFences(ctx context.Context, db *sql.DB, tenant, id string) ([]transferEvidenceFence, error) {
	rows, err := db.QueryContext(ctx, `SELECT fence_kind,fence_digest,created_at FROM assurance_transfer_fences WHERE tenant_id=? AND transfer_id=? ORDER BY fence_kind`, tenant, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []transferEvidenceFence{}
	for rows.Next() {
		var value transferEvidenceFence
		if err := rows.Scan(&value.Kind, &value.Digest, &value.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, value)
		if len(result) > 4 {
			return nil, fmt.Errorf("transfer fence evidence exceeds bound")
		}
	}
	return result, rows.Err()
}

func loadTransferEvidenceAuditEvents(ctx context.Context, db *sql.DB, tenant, id string) ([]transferEvidenceAuditEvent, error) {
	rows, err := db.QueryContext(ctx, `SELECT event_id,disposition,created_at FROM transfer_audit_events WHERE tenant_id=? AND transfer_id=? ORDER BY created_at,event_id`, tenant, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []transferEvidenceAuditEvent{}
	for rows.Next() {
		var value transferEvidenceAuditEvent
		if err := rows.Scan(&value.EventID, &value.Disposition, &value.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, value)
		if len(result) > 1000 {
			return nil, fmt.Errorf("transfer audit evidence exceeds bound")
		}
	}
	return result, rows.Err()
}

func loadTransferEvidenceVisuals(ctx context.Context, db *sql.DB, tenant, id string) ([]transferEvidenceVisual, error) {
	rows, err := db.QueryContext(ctx, `SELECT acknowledgement_id,confirmer_actor_id,ack_actor_id,observation,ui_location,refreshed,observed_value_json,observed_digest,created_at FROM assurance_transfer_visual_evidence WHERE tenant_id=? AND transfer_id=? ORDER BY created_at,acknowledgement_id`, tenant, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []transferEvidenceVisual{}
	for rows.Next() {
		var value transferEvidenceVisual
		var refreshed int
		var uiLocation, observed string
		if err := rows.Scan(&value.AcknowledgementID, &value.ConfirmerActorID, &value.AckActorID, &value.Observation, &uiLocation, &refreshed, &observed, &value.ObservedDigest, &value.CreatedAt); err != nil {
			return nil, err
		}
		if uiLocation != "" {
			var location struct {
				ResourceID string `json:"resource_id"`
				Locator    string `json:"locator"`
			}
			decoder := json.NewDecoder(strings.NewReader(uiLocation))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&location) != nil || location.ResourceID == "" || location.Locator == "" {
				return nil, domainError("subject_integrity_failed", "stored visual UI location is invalid")
			}
			value.UIResourceID, value.UILocator = location.ResourceID, location.Locator
		}
		value.Refreshed = refreshed != 0
		if observed != "" {
			typed, err := decodeTransferEvidenceValue(observed)
			if err != nil {
				return nil, err
			}
			value.ObservedValue = &typed
		}
		result = append(result, value)
		if len(result) > 1000 {
			return nil, fmt.Errorf("transfer visual evidence exceeds bound")
		}
	}
	return result, rows.Err()
}

func loadTransferEvidenceLegacyReconciliations(ctx context.Context, db *sql.DB, tenant, id string) ([]transferEvidenceReconciliation, error) {
	rows, err := db.QueryContext(ctx, `SELECT reason,classification,disposition,created_at FROM transfer_reconciliations WHERE tenant_id=? AND transfer_id=? ORDER BY created_at`, tenant, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []transferEvidenceReconciliation{}
	for rows.Next() {
		var value transferEvidenceReconciliation
		if err := rows.Scan(&value.Reason, &value.Classification, &value.Disposition, &value.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, value)
		if len(result) > 1000 {
			return nil, fmt.Errorf("transfer reconciliation evidence exceeds bound")
		}
	}
	return result, rows.Err()
}

func loadTransferEvidenceReconciliations(ctx context.Context, db *sql.DB, tenant, id string) ([]transferEvidenceReconciliation, error) {
	rows, err := db.QueryContext(ctx, `SELECT reconciliation_id,reason,classification,disposition,created_at FROM assurance_transfer_reconciliation_evidence WHERE tenant_id=? AND transfer_id=? ORDER BY created_at,reconciliation_id`, tenant, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []transferEvidenceReconciliation{}
	for rows.Next() {
		var value transferEvidenceReconciliation
		if err := rows.Scan(&value.ReconciliationID, &value.Reason, &value.Classification, &value.Disposition, &value.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, value)
		if len(result) > 1000 {
			return nil, fmt.Errorf("transfer reconciliation evidence exceeds bound")
		}
	}
	return result, rows.Err()
}

func (s *Store) resolveTransferExportProfile(ctx context.Context, tenant, id string, requested RedactionProfile, retention string) (ExportProfile, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT intent_json FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, id).Scan(&raw); err != nil {
		return ExportProfile{}, domainError("subject_not_found", "transfer evidence subject was not found")
	}
	var intent transferEvidenceIntent
	if json.Unmarshal([]byte(raw), &intent) != nil || intent.TransferID != id || intent.TenantID != tenant {
		return ExportProfile{}, domainError("subject_integrity_failed", "transfer evidence intent failed integrity validation")
	}
	var routeJSON string
	if err := s.db.QueryRowContext(ctx, `SELECT route_json FROM assurance_transfer_route_revisions WHERE tenant_id=? AND route_id=? AND revision=?`, tenant, intent.RouteID, intent.RouteRevision).Scan(&routeJSON); err != nil {
		return ExportProfile{}, domainError("export_profile_not_approved", "transfer has no exact frozen export profile association")
	}
	var route TransferRoute
	if json.Unmarshal([]byte(routeJSON), &route) != nil || s.verifyFrozenTransferRoute(ctx, tenant, intent) != nil {
		return ExportProfile{}, domainError("transfer_route_integrity_failed", "frozen transfer route integrity validation failed")
	}
	if len(route.ExportProfiles) == 0 {
		return ExportProfile{}, domainError("export_profile_not_approved", "historical transfer route has no approved export profile association")
	}
	var matches []ExportProfile
	for _, reference := range route.ExportProfiles {
		var revision int
		var status, profileJSON, storedHash string
		if err := s.db.QueryRowContext(ctx, `SELECT p.revision,p.status,p.profile_json,p.content_hash FROM assurance_export_profiles p JOIN assurance_active_bundle_objects a ON a.tenant_id=p.tenant_id AND a.object_kind='export_profile' AND a.object_id=p.profile_id AND a.object_revision=p.revision JOIN assurance_active_bundles b ON b.tenant_id=a.tenant_id AND b.singleton=1 AND b.bundle_version=a.bundle_version WHERE p.tenant_id=? AND p.profile_id=? AND p.revision=? AND p.status='active'`, tenant, reference.ProfileID, reference.Revision).Scan(&revision, &status, &profileJSON, &storedHash); err != nil {
			continue
		}
		if digestHex(HashBytes([]byte(profileJSON))) != storedHash {
			return ExportProfile{}, domainError("export_profile_integrity_failed", "stored export profile hash does not match")
		}
		var profile ExportProfile
		if json.Unmarshal([]byte(profileJSON), &profile) != nil || profile.ProfileID != reference.ProfileID || profile.Revision != reference.Revision || profile.Status != "active" {
			return ExportProfile{}, domainError("export_profile_integrity_failed", "stored export profile JSON is invalid")
		}
		canonical, canonicalErr := CanonicalJSON(profile)
		if canonicalErr != nil || digestHex(HashBytes(canonical)) != storedHash {
			return ExportProfile{}, domainError("export_profile_integrity_failed", "stored export profile content hash does not match")
		}
		profile.Revision, profile.Status = revision, status
		if ValidateExportProfile(profile) != nil {
			return ExportProfile{}, domainError("export_profile_integrity_failed", "stored export profile failed schema validation")
		}
		for _, subject := range profile.PermittedSubjects {
			if subject == "transfer" && profile.RedactionProfile == string(requested) && profile.RetentionClass == retention && profile.DeliveryPolicy == "opaque_reference" {
				matches = append(matches, profile)
				break
			}
		}
	}
	if len(matches) > 1 {
		return ExportProfile{}, domainError("export_profile_ambiguous", "more than one frozen transfer profile matches the requested export")
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return ExportProfile{}, domainError("export_profile_not_approved", "requested transfer redaction, retention, or delivery policy is not approved")
}
