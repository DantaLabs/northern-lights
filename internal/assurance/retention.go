package assurance

import (
	"context"
	"database/sql"
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
	if policy.RetentionClass == "" || len(policy.RetentionClass) > 64 || seconds <= 0 || policy.Status != "active" {
		return fmt.Errorf("invalid retention policy")
	}
	return nil
}

func (s *Store) ResolveRetentionPolicy(ctx context.Context, class string) (RetentionPolicy, error) {
	if class == "" {
		return RetentionPolicy{}, domainError("retention_policy_missing", "retention policy is required")
	}
	tenant := identity.StorageTenant(ctx)
	var policy RetentionPolicy
	err := s.db.QueryRowContext(ctx, `SELECT retention_class, duration_seconds, policy_json FROM assurance_retention_policies WHERE tenant_id=? AND retention_class=?`, tenant, class).Scan(&policy.RetentionClass, &policy.DurationSeconds, new(string))
	if err == sql.ErrNoRows {
		return RetentionPolicy{}, domainError("retention_policy_missing", "retention policy is not provisioned")
	}
	if err != nil {
		return RetentionPolicy{}, err
	}
	policy.Status = "active"
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
	tenant := identity.StorageTenant(ctx)
	value := 0
	if held {
		value = 1
	}
	result, err := s.db.ExecContext(ctx, `UPDATE assurance_evidence_manifests SET legal_hold=? WHERE tenant_id=? AND manifest_id=?`, value, tenant, manifestID)
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
		var subjectKind, subjectID string
		if err := s.db.QueryRowContext(ctx, `SELECT subject_kind, subject_id FROM assurance_evidence_manifests WHERE tenant_id=? AND manifest_id=?`, tenant, manifestID).Scan(&subjectKind, &subjectID); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO assurance_legal_holds (tenant_id, hold_id, subject_kind, subject_id, reason, active, actor_id, created_at) VALUES (?, ?, ?, ?, 'authorized_retention_hold', 1, ?, ?)`, tenant, uuid.NewString(), subjectKind, subjectID, actorID, formatTimestamp(time.Now().UTC())); err != nil {
			return err
		}
	} else if _, err := s.db.ExecContext(ctx, `UPDATE assurance_legal_holds SET active=0, released_at=? WHERE tenant_id=? AND subject_id=(SELECT subject_id FROM assurance_evidence_manifests WHERE tenant_id=? AND manifest_id=?) AND active=1`, formatTimestamp(time.Now().UTC()), tenant, tenant, manifestID); err != nil {
		return err
	}
	if s.auditLog != nil {
		action := "release_legal_hold"
		if held {
			action = "place_legal_hold"
		}
		_, _ = s.auditLog.Append(ctx, audit.Entry{Actor: actorID, Tool: "assurance_retention", Action: action, Target: manifestID})
	}
	return nil
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
