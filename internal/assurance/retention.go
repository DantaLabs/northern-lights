package assurance

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
)

type RetentionPolicy struct {
	PolicyID         string        `json:"policy_id,omitempty" yaml:"policy_id,omitempty"`
	Revision         int           `json:"revision,omitempty" yaml:"revision,omitempty"`
	TenantID         string        `json:"tenant_id,omitempty" yaml:"tenant_id,omitempty"`
	RetentionClass   string        `json:"retention_class" yaml:"retention_class"`
	DurationSeconds  int64         `json:"duration_seconds" yaml:"duration_seconds"`
	Duration         time.Duration `json:"-" yaml:"-"`
	AllowDisplayText bool          `json:"allow_display_text" yaml:"allow_display_text"`
	Status           string        `json:"status" yaml:"status"`
	LegalHold        bool          `json:"legal_hold,omitempty" yaml:"legal_hold,omitempty"`
}

type RetentionDecision struct {
	Allowed   bool
	ExpiresAt time.Time
	LegalHold bool
	Reason    string
}

func ValidateRetentionPolicy(policy RetentionPolicy) error {
	seconds := policy.DurationSeconds
	if seconds <= 0 && policy.Duration > 0 {
		seconds = int64(policy.Duration / time.Second)
	}
	if policy.RetentionClass == "" || len(policy.RetentionClass) > 64 || policy.Revision <= 0 || policy.Revision > maxRevision || seconds <= 0 || policy.Status != "active" {
		return fmt.Errorf("invalid retention policy")
	}
	return nil
}

func retentionRevisionConflict(existingRevision int, existingHash, incomingHash string, incomingRevision int) bool {
	if existingRevision <= 0 {
		return false
	}
	if incomingRevision < existingRevision {
		return true
	}
	return incomingRevision == existingRevision && existingHash != incomingHash || incomingRevision > existingRevision && existingHash == incomingHash
}

func (s *Store) ResolveRetentionPolicy(ctx context.Context, class string) (RetentionPolicy, error) {
	if err := s.Ready(); err != nil {
		return RetentionPolicy{}, err
	}
	if class == "" {
		return RetentionPolicy{}, domainError("retention_policy_missing", "retention policy is required")
	}
	tenant := identity.StorageTenant(ctx)
	var policy RetentionPolicy
	var raw, storedHash string
	var revision int
	err := s.db.QueryRowContext(ctx, `SELECT p.retention_class, p.duration_seconds, p.policy_json, p.content_hash, p.revision FROM assurance_retention_policy_revisions p JOIN assurance_active_bundle_objects a ON a.tenant_id=p.tenant_id AND a.object_kind='retention_policy' AND a.object_id=p.retention_class AND a.object_revision=p.revision WHERE p.tenant_id=? AND p.retention_class=? ORDER BY p.revision DESC LIMIT 1`, tenant, class).Scan(&policy.RetentionClass, &policy.DurationSeconds, &raw, &storedHash, &revision)
	if err == sql.ErrNoRows {
		return RetentionPolicy{}, domainError("retention_policy_missing", "retention policy is not provisioned")
	}
	if err != nil {
		return RetentionPolicy{}, err
	}
	if digestHex(HashBytes([]byte(raw))) != storedHash {
		return RetentionPolicy{}, domainError("retention_policy_integrity_failed", "stored retention policy hash does not match")
	}
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return RetentionPolicy{}, domainError("retention_policy_integrity_failed", "stored retention policy JSON is invalid")
	}
	policy.RetentionClass = class
	policy.Status = "active"
	policy.Revision = revision
	if err := ValidateRetentionPolicy(policy); err != nil {
		return RetentionPolicy{}, domainError("retention_policy_integrity_failed", err.Error())
	}
	policy.Duration = time.Duration(policy.DurationSeconds) * time.Second
	return policy, nil
}

func (s *Store) ResolveRetention(ctx context.Context, class string, now time.Time) (RetentionDecision, error) {
	policy, err := s.ResolveRetentionPolicy(ctx, class)
	if err != nil {
		return RetentionDecision{}, err
	}
	return RetentionDecision{Allowed: true, ExpiresAt: retentionExpiry(now, policy), LegalHold: policy.LegalHold}, nil
}

func (s *Store) PlaceLegalHold(ctx context.Context, manifestID, actorID string) error {
	return s.setLegalHold(ctx, manifestID, actorID, true)
}

func (s *Store) ReleaseLegalHold(ctx context.Context, manifestID, actorID string) error {
	return s.setLegalHold(ctx, manifestID, actorID, false)
}

func (s *Store) setLegalHold(ctx context.Context, manifestID, actorID string, held bool) error {
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok || principal.TenantID == "" || principal.ObjectID == "" || !principal.HasPermission(identity.PermissionTenantAdmin) {
		return domainError("operator_authorization_required", "an authorized tenant operator is required")
	}
	trustedActor := principal.AuditActor()
	if trustedActor == "" {
		return domainError("audit_actor_missing", "trusted principal actor is missing")
	}
	if err := s.requireRichAudit(); err != nil {
		return err
	}
	tenant := identity.StorageTenant(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var subjectKind, subjectID string
	if err := tx.QueryRowContext(ctx, `SELECT subject_kind, subject_id FROM assurance_evidence_manifests WHERE tenant_id=? AND manifest_id=?`, tenant, manifestID).Scan(&subjectKind, &subjectID); err != nil {
		if err == sql.ErrNoRows {
			return domainError("subject_not_found", "evidence manifest was not found")
		}
		return err
	}
	value := 0
	if held {
		value = 1
	}
	result, err := tx.ExecContext(ctx, `UPDATE assurance_evidence_manifests SET legal_hold=? WHERE tenant_id=? AND manifest_id=?`, value, tenant, manifestID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return domainError("subject_not_found", "evidence manifest was not found")
	}
	if held {
		if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_legal_holds (tenant_id, hold_id, subject_kind, subject_id, manifest_id, reason, active, actor_id, created_at) VALUES (?, ?, ?, ?, ?, 'authorized_retention_hold', 1, ?, ?)`, tenant, uuid.NewString(), subjectKind, subjectID, manifestID, trustedActor, formatTimestamp(time.Now().UTC())); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE assurance_legal_holds SET active=0, released_at=? WHERE tenant_id=? AND manifest_id=? AND active=1`, formatTimestamp(time.Now().UTC()), tenant, manifestID); err != nil {
		return err
	}
	action := "release_legal_hold"
	if held {
		action = "place_legal_hold"
	}
	if _, err := s.auditLog.AppendTx(ctx, tx, audit.Entry{Actor: trustedActor, Tool: "assurance_retention", Action: action, Target: manifestID}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PurgeExpired(ctx context.Context, now time.Time) error {
	return s.PurgeExpiredEvidence(ctx, now)
}

func retentionExpiry(now time.Time, policy RetentionPolicy) time.Time {
	if policy.Duration > 0 {
		return now.UTC().Add(policy.Duration)
	}
	return now.UTC().Add(time.Duration(policy.DurationSeconds) * time.Second)
}
