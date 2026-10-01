package assurance

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
)

const (
	reservationLease        = 60 * time.Second
	reservationRetryAfterMS = 1000
	defaultRecordRetention  = 24 * time.Hour
)

// Store owns assurance state on the application's shared SQLite handle. The
// caller retains database ownership.
type Store struct {
	db              *sql.DB
	evidenceStorage EvidenceStorage
	auditLog        *audit.Log

	readinessMu  sync.RWMutex
	ready        bool
	readinessErr error
}

// NewWithDB installs all assurance migrations and wraps the shared handle.
func NewWithDB(db *sql.DB) (*Store, error) {
	if err := Migrate(context.Background(), db); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// SetEvidenceStorage installs an explicitly selected delivery adapter. The
// constructor never supplies process-local storage implicitly; callers may wire
// the memory adapter only for tests or an explicit demo profile, while a
// production deployment must provide a durable opaque-reference adapter.
func (s *Store) SetEvidenceStorage(storage EvidenceStorage) {
	if storage != nil {
		s.evidenceStorage = storage
	}
}

// SetAuditLog connects rich evidence links to the shared audit chain.
func (s *Store) SetAuditLog(log *audit.Log) { s.auditLog = log }

func (s *Store) requireRichAudit(logs ...*audit.Log) error {
	if s == nil {
		return domainError("audit_unavailable", "shared rich audit is required")
	}
	log := s.auditLog
	if len(logs) == 1 && logs[0] != nil {
		log = logs[0]
	}
	if log == nil || !log.SharesDB(s.db) {
		return domainError("audit_unavailable", "shared rich audit is required")
	}
	return nil
}

// Close is a no-op because Store never owns the shared database handle.
func (s *Store) Close() error { return nil }

// Ping verifies the shared handle and assurance migration marker.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.db == nil {
		return ErrNotReady
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations WHERE app='assurance'`).Scan(&version); err != nil {
		return fmt.Errorf("assurance: ping: %w", err)
	}
	if version != len(migrations) {
		return fmt.Errorf("%w: assurance schema version %d", ErrNotReady, version)
	}
	return nil
}

// Ready reports whether a fully validated active bundle may be served.
func (s *Store) Ready() error {
	s.readinessMu.RLock()
	defer s.readinessMu.RUnlock()
	if s.ready {
		return nil
	}
	if s.readinessErr != nil {
		return s.readinessErr
	}
	return ErrNotReady
}

func (s *Store) setReadiness(ready bool, err error) {
	s.readinessMu.Lock()
	s.ready = ready
	s.readinessErr = err
	s.readinessMu.Unlock()
}

// ReservationState is the immutable-terminal reservation lifecycle.
type ReservationState string

const (
	ReservationStateReserved ReservationState = "reserved"
	ReservationStateSealed   ReservationState = "sealed"
	ReservationStateFailed   ReservationState = "failed"
	ReservationStateExpired  ReservationState = "expired"
)

// ReservationDisposition tells a caller whether it owns execution.
type ReservationDisposition string

const (
	ReservationOwned      ReservationDisposition = "owned"
	ReservationInProgress ReservationDisposition = "in_progress"
	ReservationReplay     ReservationDisposition = "replay"
	ReservationConflict   ReservationDisposition = "conflict"
)

// ReservationRequest contains trusted scope and digests only.
type ReservationRequest struct {
	ActorID           string
	Tool              string
	Action            string
	IdempotencyDigest Digest
	RequestDigest     Digest
	RetentionClass    string
}

// ReservationResult contains no client secret and no recoverable derivative.
type ReservationResult struct {
	Disposition     ReservationDisposition
	State           ReservationState
	RecordID        string
	OwnerNonce      string
	CorrelationID   string
	EntityReference string
	Envelope        []byte
	RetryAfterMS    int
}

func digestHex(digest Digest) string { return hex.EncodeToString(digest[:]) }

// Reserve atomically creates or classifies one trusted-scope reservation.
func (s *Store) Reserve(ctx context.Context, request ReservationRequest, now time.Time) (ReservationResult, error) {
	return s.reserveTx(ctx, nil, request, now, nil)
}

type snapshotStart struct {
	SnapshotID         string
	ReportID           string
	DefinitionRevision int
	PeriodJSON         string
	MappingSetHash     string
	ProviderRoute      string
	RetentionClass     string
	AuditID            string
}

func (s *Store) reserveTx(ctx context.Context, existingTx *sql.Tx, request ReservationRequest, now time.Time, start *snapshotStart) (ReservationResult, error) {
	if s == nil || s.db == nil {
		return ReservationResult{}, ErrNotReady
	}
	if request.ActorID == "" || request.Tool == "" || request.Action == "" || request.IdempotencyDigest == (Digest{}) || request.RequestDigest == (Digest{}) {
		return ReservationResult{}, fmt.Errorf("assurance: incomplete trusted reservation scope")
	}
	tenant := identity.StorageTenant(ctx)
	now = now.UTC()
	retention := request.RetentionClass
	if retention == "" {
		retention = "standard"
	}

	tx := existingTx
	ownTx := tx == nil
	var err error
	if ownTx {
		tx, err = s.db.BeginTx(ctx, nil)
		if err != nil {
			return ReservationResult{}, fmt.Errorf("assurance: reserve begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
	}

	result, found, err := loadReservationTx(ctx, tx, tenant, request)
	if err != nil {
		return ReservationResult{}, err
	}
	if found {
		if ownTx {
			if err := tx.Commit(); err != nil {
				return ReservationResult{}, fmt.Errorf("assurance: reserve commit existing: %w", err)
			}
		}
		return result, nil
	}

	recordID := uuid.NewString()
	ownerNonce := uuid.NewString()
	correlationID := uuid.NewString()
	_, err = tx.ExecContext(ctx, `INSERT INTO assurance_idempotency_records
 (tenant_id, record_id, actor_id, tool, action, idempotency_digest, request_digest, state, owner_nonce,
  lease_expires_at, last_heartbeat_at, execution_started, correlation_id, created_at, expires_at, retention_class)
 VALUES (?, ?, ?, ?, ?, ?, ?, 'reserved', ?, ?, ?, 0, ?, ?, ?, ?)`,
		tenant, recordID, request.ActorID, request.Tool, request.Action, digestHex(request.IdempotencyDigest), digestHex(request.RequestDigest),
		ownerNonce, formatTimestamp(now.Add(reservationLease)), formatTimestamp(now), correlationID, formatTimestamp(now),
		formatTimestamp(now.Add(defaultRecordRetention)), retention)
	if err != nil {
		// A concurrent unique-key winner is deterministically reloaded.
		if isConstraintError(err) {
			result, found, loadErr := loadReservationTx(ctx, tx, tenant, request)
			if loadErr != nil {
				return ReservationResult{}, loadErr
			}
			if found {
				if ownTx {
					if commitErr := tx.Commit(); commitErr != nil {
						return ReservationResult{}, fmt.Errorf("assurance: reserve commit winner: %w", commitErr)
					}
				}
				return result, nil
			}
		}
		return ReservationResult{}, fmt.Errorf("assurance: reserve insert: %w", err)
	}
	if start != nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO assurance_snapshots
 (tenant_id, snapshot_id, idempotency_digest, report_id, definition_revision, period_json, status, completeness,
  mapping_set_hash, provider_route, retention_class, audit_id)
 VALUES (?, ?, ?, ?, ?, ?, 'running', 'not_created', ?, ?, ?, ?)`,
			tenant, start.SnapshotID, digestHex(request.IdempotencyDigest), start.ReportID, start.DefinitionRevision, start.PeriodJSON,
			start.MappingSetHash, start.ProviderRoute, start.RetentionClass, start.AuditID)
		if err != nil {
			return ReservationResult{}, fmt.Errorf("assurance: start snapshot: %w", err)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET entity_reference=? WHERE tenant_id=? AND record_id=?`, start.SnapshotID, tenant, recordID); err != nil {
			return ReservationResult{}, fmt.Errorf("assurance: bind snapshot reservation: %w", err)
		}
	}
	if ownTx {
		if err := tx.Commit(); err != nil {
			return ReservationResult{}, fmt.Errorf("assurance: reserve commit: %w", err)
		}
	}
	return ReservationResult{Disposition: ReservationOwned, State: ReservationStateReserved, RecordID: recordID, OwnerNonce: ownerNonce, CorrelationID: correlationID, EntityReference: func() string {
		if start != nil {
			return start.SnapshotID
		}
		return ""
	}()}, nil
}

func loadReservationTx(ctx context.Context, tx *sql.Tx, tenant string, request ReservationRequest) (ReservationResult, bool, error) {
	var (
		result        ReservationResult
		state         string
		requestDigest string
		envelope      string
	)
	err := tx.QueryRowContext(ctx, `SELECT record_id, state, owner_nonce, correlation_id, entity_reference, request_digest, response_envelope
 FROM assurance_idempotency_records WHERE tenant_id=? AND actor_id=? AND tool=? AND action=? AND idempotency_digest=?`,
		tenant, request.ActorID, request.Tool, request.Action, digestHex(request.IdempotencyDigest)).Scan(
		&result.RecordID, &state, &result.OwnerNonce, &result.CorrelationID, &result.EntityReference, &requestDigest, &envelope)
	if errors.Is(err, sql.ErrNoRows) {
		return ReservationResult{}, false, nil
	}
	if err != nil {
		return ReservationResult{}, false, fmt.Errorf("assurance: reserve lookup: %w", err)
	}
	result.State = ReservationState(state)
	if requestDigest != digestHex(request.RequestDigest) {
		result.Disposition = ReservationConflict
		result.OwnerNonce = ""
		return result, true, nil
	}
	if result.State == ReservationStateReserved {
		result.Disposition = ReservationInProgress
		result.OwnerNonce = ""
		result.RetryAfterMS = reservationRetryAfterMS
		return result, true, nil
	}
	result.Disposition = ReservationReplay
	result.OwnerNonce = ""
	result.Envelope = []byte(envelope)
	return result, true, nil
}

// SealReservation commits an immutable replay envelope.
func (s *Store) SealReservation(ctx context.Context, recordID, ownerNonce string, envelope []byte, status, entityReference, auditID string, now time.Time) error {
	canonical, err := CanonicalJSONBytes(envelope)
	if err != nil {
		return fmt.Errorf("assurance: seal response envelope: %w", err)
	}
	tenant := identity.StorageTenant(ctx)
	result, err := s.db.ExecContext(ctx, `UPDATE assurance_idempotency_records SET state='sealed', response_envelope=?, response_hash=?,
 response_status=?, entity_reference=?, audit_id=?, terminal_at=?, lease_expires_at=?
 WHERE tenant_id=? AND record_id=? AND state='reserved' AND owner_nonce=?`, string(canonical), digestHex(HashBytes(canonical)), status,
		entityReference, auditID, formatTimestamp(now), formatTimestamp(now), tenant, recordID, ownerNonce)
	if err != nil {
		return fmt.Errorf("assurance: seal reservation: %w", err)
	}
	return requireOneTransition(result)
}

func (s *Store) sealTerminalOutcome(ctx context.Context, reservation ReservationResult, envelope []byte, status, entityReference, auditID string, now time.Time) error {
	if err := s.requireRichAudit(); err != nil {
		return err
	}
	canonical, err := CanonicalJSONBytes(envelope)
	if err != nil {
		return err
	}
	tenant := identity.StorageTenant(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var actor, tool, action string
	if err := tx.QueryRowContext(ctx, `SELECT actor_id, tool, action FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=? AND owner_nonce=? AND state='reserved'`, tenant, reservation.RecordID, reservation.OwnerNonce).Scan(&actor, &tool, &action); err != nil {
		return err
	}
	if _, err := s.auditLog.AppendTx(ctx, tx, audit.Entry{Actor: actor, Tool: tool, Action: action + ".terminal", Target: entityReference, AfterJSON: string(canonical), AuditID: auditID}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_audit_links (tenant_id, link_id, entity_kind, entity_id, audit_id, request_id, correlation_id, created_at) VALUES (?, ?, 'idempotency_record', ?, ?, ?, ?, ?)`, tenant, uuid.NewString(), reservation.RecordID, auditID, RequestIDFromContext(ctx), reservation.CorrelationID, formatTimestamp(now)); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET state='sealed', response_envelope=?, response_hash=?, response_status=?, entity_reference=?, audit_id=?, terminal_at=?, lease_expires_at=? WHERE tenant_id=? AND record_id=? AND state='reserved' AND owner_nonce=?`, string(canonical), digestHex(HashBytes(canonical)), status, entityReference, auditID, formatTimestamp(now), formatTimestamp(now), tenant, reservation.RecordID, reservation.OwnerNonce)
	if err != nil {
		return err
	}
	if err := requireOneTransition(result); err != nil {
		return err
	}
	return tx.Commit()
}

// FailReservation stores a typed terminal error envelope.
func (s *Store) FailReservation(ctx context.Context, recordID, ownerNonce string, failure StructuredError, now time.Time) error {
	envelope, err := CanonicalJSON(failure)
	if err != nil {
		return err
	}
	tenant := identity.StorageTenant(ctx)
	if err := s.requireRichAudit(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("assurance: fail reservation begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var actor, tool, action, correlationID, entityReference string
	if err := tx.QueryRowContext(ctx, `SELECT actor_id, tool, action, correlation_id, entity_reference FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=? AND state='reserved' AND owner_nonce=?`, tenant, recordID, ownerNonce).Scan(&actor, &tool, &action, &correlationID, &entityReference); err != nil {
		if err == sql.ErrNoRows {
			return ErrInvalidTransition
		}
		return err
	}
	if _, err := s.auditLog.AppendTx(ctx, tx, audit.Entry{Actor: actor, Tool: tool, Action: action + ".failed", Target: recordID, AfterJSON: string(envelope), AuditID: failure.NLAuditID}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_audit_links (tenant_id, link_id, entity_kind, entity_id, audit_id, request_id, correlation_id, created_at) VALUES (?, ?, 'idempotency_record', ?, ?, ?, ?, ?)`, tenant, uuid.NewString(), recordID, failure.NLAuditID, RequestIDFromContext(ctx), correlationID, formatTimestamp(now)); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET state='failed', response_envelope=?, response_hash=?,
	 response_status='failed', audit_id=?, terminal_at=?, lease_expires_at=?
	 WHERE tenant_id=? AND record_id=? AND state='reserved' AND owner_nonce=?`, string(envelope), digestHex(HashBytes(envelope)),
		failure.NLAuditID, formatTimestamp(now), formatTimestamp(now), tenant, recordID, ownerNonce)
	if err != nil {
		return fmt.Errorf("assurance: fail reservation: %w", err)
	}
	if err := requireOneTransition(result); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkExecutionStarted prevents an abandoned reservation from being classified
// as safely unstarted.
func (s *Store) MarkExecutionStarted(ctx context.Context, recordID, ownerNonce string, now time.Time) error {
	tenant := identity.StorageTenant(ctx)
	result, err := s.db.ExecContext(ctx, `UPDATE assurance_idempotency_records SET execution_started=1, last_heartbeat_at=?, lease_expires_at=?
 WHERE tenant_id=? AND record_id=? AND state='reserved' AND owner_nonce=?`, formatTimestamp(now), formatTimestamp(now.Add(reservationLease)), tenant, recordID, ownerNonce)
	if err != nil {
		return fmt.Errorf("assurance: mark execution: %w", err)
	}
	return requireOneTransition(result)
}

// RenewReservation extends a live reservation lease only for its owner nonce.
func (s *Store) RenewReservation(ctx context.Context, recordID, ownerNonce string, now time.Time) error {
	if recordID == "" || ownerNonce == "" {
		return ErrInvalidTransition
	}
	tenant := identity.StorageTenant(ctx)
	result, err := s.db.ExecContext(ctx, `UPDATE assurance_idempotency_records SET last_heartbeat_at=?, lease_expires_at=?
 WHERE tenant_id=? AND record_id=? AND state='reserved' AND owner_nonce=?`, formatTimestamp(now), formatTimestamp(now.Add(reservationLease)), tenant, recordID, ownerNonce)
	if err != nil {
		return fmt.Errorf("assurance: renew reservation: %w", err)
	}
	return requireOneTransition(result)
}

// ExpireStaleReservations terminalizes dead leases without reopening any key.
func (s *Store) ExpireStaleReservations(ctx context.Context, now time.Time) (int64, error) {
	tenant := identity.StorageTenant(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT record_id, execution_started, entity_reference FROM assurance_idempotency_records
 WHERE tenant_id=? AND state='reserved' AND lease_expires_at < ?`, tenant, formatTimestamp(now))
	if err != nil {
		return 0, err
	}
	type stale struct {
		id              string
		started         bool
		entityReference string
	}
	var records []stale
	for rows.Next() {
		var record stale
		if err := rows.Scan(&record.id, &record.started, &record.entityReference); err != nil {
			_ = rows.Close()
			return 0, err
		}
		records = append(records, record)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	var changed int64
	for _, record := range records {
		code := "idempotency_reservation_expired"
		message := "reservation owner lease expired before execution"
		state := "expired"
		reconciliation := false
		if record.started {
			code = "idempotency_execution_unknown"
			message = "reservation owner lease expired after execution may have started"
			state = "failed"
			reconciliation = true
		}
		envelope, _ := CanonicalJSON(StructuredError{Code: code, Message: message, Retryable: false, ReconciliationRequired: reconciliation})
		result, err := tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET state=?, response_envelope=?, response_hash=?,
 response_status='failed', terminal_at=?, lease_expires_at=? WHERE tenant_id=? AND record_id=? AND state='reserved'`,
			state, string(envelope), digestHex(HashBytes(envelope)), formatTimestamp(now), formatTimestamp(now), tenant, record.id)
		if err != nil {
			return 0, err
		}
		n, _ := result.RowsAffected()
		if n == 1 && record.entityReference != "" {
			if snapshotResult, updateErr := tx.ExecContext(ctx, `UPDATE assurance_snapshots SET status='failed', completeness='not_created', captured_at=?
 WHERE tenant_id=? AND snapshot_id=? AND status='running'`, formatTimestamp(now), tenant, record.entityReference); updateErr != nil {
				return 0, updateErr
			} else if snapshotRows, rowsErr := snapshotResult.RowsAffected(); rowsErr != nil {
				return 0, rowsErr
			} else if snapshotRows == 1 {
				if _, insertErr := tx.ExecContext(ctx, `INSERT INTO assurance_snapshot_failures
 (tenant_id, snapshot_id, failure_id, field_id, code, message, created_at) VALUES (?, ?, ?, '', ?, ?, ?)`, tenant, record.entityReference, uuid.NewString(), code, message, formatTimestamp(now)); insertErr != nil {
					return 0, insertErr
				}
			}
		}
		changed += n
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changed, nil
}

// RunJanitor expires dead reservations until ctx is canceled. It never
// re-submits provider work and is safe to run alongside capture requests.
func (s *Store) RunJanitor(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			_, _ = s.ExpireStaleReservations(ctx, now.UTC())
		}
	}
}
func (s *Store) failRunningSnapshot(ctx context.Context, snapshotID, code, message string, now time.Time) error {
	if snapshotID == "" {
		return nil
	}
	tenant := identity.StorageTenant(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE assurance_snapshots SET status='failed', completeness='not_created', captured_at=?
 WHERE tenant_id=? AND snapshot_id=? AND status='running'`, formatTimestamp(now), tenant, snapshotID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 1 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_snapshot_failures
 (tenant_id, snapshot_id, failure_id, field_id, code, message, created_at) VALUES (?, ?, ?, '', ?, ?, ?)`, tenant, snapshotID, uuid.NewString(), code, message, formatTimestamp(now)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// requireOneTransition enforces owner-nonce compare-and-set transitions.
func requireOneTransition(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrInvalidTransition
	}
	return nil
}

func reservationActor(ctx context.Context, tx *sql.Tx, reservation ReservationResult) (string, error) {
	var actor string
	err := tx.QueryRowContext(ctx, `SELECT actor_id FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=? AND owner_nonce=?`, identity.StorageTenant(ctx), reservation.RecordID, reservation.OwnerNonce).Scan(&actor)
	if err != nil {
		return "", err
	}
	if actor == "" {
		return "", domainError("audit_actor_missing", "trusted reservation actor is missing")
	}
	return actor, nil
}

func isConstraintError(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return contains(text, "constraint failed") || contains(text, "UNIQUE constraint")
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}

func formatTimestamp(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func marshalCanonical(v any) (string, error) {
	value, err := CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	return string(value), nil
}

func decodeEnvelope[T any](raw []byte) (T, error) {
	var result T
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, err
	}
	return result, nil
}
