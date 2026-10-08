package assurance

import (
	"context"
	"crypto/sha256"
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

// RequireRichAudit checks that the configured audit log shares this database
// and that its required tables are queryable. It does not verify historical
// hash-chain integrity; explicit chain verification belongs to backup/restore
// and evidence-verification paths. State-changing transfer work calls this
// availability check before any provider read. Missing or unusable audit
// schema fails closed; merely having an audit.Log value is not sufficient.
func (s *Store) RequireRichAudit(ctx context.Context) error {
	if s == nil || s.db == nil {
		return domainError("audit_unavailable", "shared rich audit is unavailable")
	}
	if err := s.requireRichAudit(); err != nil {
		return err
	}
	for _, table := range []string{"audit_log", "assurance_audit_links"} {
		var one int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM `+table+` LIMIT 1`).Scan(&one); err != nil && err != sql.ErrNoRows {
			return domainError("audit_unavailable", "shared rich audit is unavailable")
		}
	}
	return nil
}

func (s *Store) DB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
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

// BlockReadiness closes the current process's readiness gate until a fresh
// validated startup succeeds. Restore reconciliation uses this when the
// external inventory, durable quarantine, or audit append fails.
func (s *Store) BlockReadiness(err error) {
	if err == nil {
		err = ErrNotReady
	}
	s.setReadiness(false, errors.Join(ErrNotReady, err))
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

// LookupReservation classifies an existing trusted-scope key without creating
// a reservation or consulting execution-only dependencies such as rich audit.
// A miss must still use Reserve after all pre-execution checks; Reserve handles
// the race with another owner atomically.
func (s *Store) LookupReservation(ctx context.Context, request ReservationRequest) (ReservationResult, bool, error) {
	if s == nil || s.db == nil || request.ActorID == "" || request.Tool == "" || request.Action == "" || request.IdempotencyDigest == (Digest{}) || request.RequestDigest == (Digest{}) {
		return ReservationResult{}, false, fmt.Errorf("assurance: incomplete trusted reservation scope")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ReservationResult{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, found, err := loadReservationTx(ctx, tx, identity.StorageTenant(ctx), request)
	if err != nil {
		return ReservationResult{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return ReservationResult{}, false, err
	}
	return result, found, nil
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
	if failure.NLAuditID == "" {
		failure.NLAuditID = uuid.NewString()
	}
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

// ReserveAndClaimTransfer commits the confirm reservation and the token-bound
// local claim together. A reservation hit never inspects the token or transfer.
// Call only after trusted authorization, replay lookup, and rich-audit readiness.
func (s *Store) ReserveAndClaimTransfer(ctx context.Context, request ReservationRequest, transferID, permission, token, leaseID string, now time.Time) (ReservationResult, error) {
	if transferID == "" || permission == "" || token == "" || leaseID == "" {
		return ReservationResult{}, errors.New("assurance: incomplete confirmation claim")
	}
	tenant := identity.StorageTenant(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReservationResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if prior, found, err := loadReservationTx(ctx, tx, tenant, request); err != nil {
		return ReservationResult{}, err
	} else if found {
		if err := tx.Commit(); err != nil {
			return ReservationResult{}, err
		}
		return prior, nil
	}
	var frozen []byte
	var tokenDigest string
	if err := tx.QueryRowContext(ctx, `SELECT intent_json,token_digest FROM transfer_intents WHERE tenant_id=? AND transfer_id=? AND actor_id=? AND permission=? AND state='staged' AND expires_at>?`, tenant, transferID, request.ActorID, permission, formatTimestamp(now)).Scan(&frozen, &tokenDigest); err != nil {
		return ReservationResult{}, errors.New("transfer: confirmation claim unavailable")
	}
	if tokenDigest != transferConfirmationDigest(token, frozen) {
		return ReservationResult{}, errors.New("transfer: confirmation claim unavailable")
	}
	reservation, err := s.reserveTx(ctx, tx, request, now, nil)
	if err != nil {
		return ReservationResult{}, err
	}
	if reservation.Disposition != ReservationOwned {
		if err := tx.Commit(); err != nil {
			return ReservationResult{}, err
		}
		return reservation, nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE transfer_intents SET state='claimed',lease_id=?,lease_expires_at=?,row_version=row_version+1 WHERE tenant_id=? AND transfer_id=? AND actor_id=? AND permission=? AND token_digest=? AND state='staged' AND expires_at>?`, leaseID, formatTimestamp(now.Add(time.Minute)), tenant, transferID, request.ActorID, permission, tokenDigest, formatTimestamp(now))
	if err != nil {
		return ReservationResult{}, err
	}
	if err := requireOneTransition(result); err != nil {
		return ReservationResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO transfer_audit_events(tenant_id,transfer_id,event_id,disposition,details,created_at) VALUES(?,?,?,?,?,?)`, tenant, transferID, "claim:"+transferID, "claim_committed", `{"token_persisted":false}`, formatTimestamp(now)); err != nil {
		return ReservationResult{}, err
	}
	result, err = tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET execution_started=1,last_heartbeat_at=? WHERE tenant_id=? AND record_id=? AND owner_nonce=? AND state='reserved'`, formatTimestamp(now), tenant, reservation.RecordID, reservation.OwnerNonce)
	if err != nil {
		return ReservationResult{}, err
	}
	if err := requireOneTransition(result); err != nil {
		return ReservationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReservationResult{}, err
	}
	return reservation, nil
}

// WithTransferAuditID binds the server-generated MCP audit ID to the rich
// transfer evidence. It is never read from client arguments.
type transferAuditIDContextKey struct{}

func WithTransferAuditID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, transferAuditIDContextKey{}, id)
}
func transferAuditIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(transferAuditIDContextKey{}).(string)
	if id == "" {
		return uuid.NewString()
	}
	return id
}

// TransferAuditIDFromContext exposes the server-generated audit correlation to
// the transfer service without exposing the context key or accepting caller
// supplied identity data.
func TransferAuditIDFromContext(ctx context.Context) string { return transferAuditIDFromContext(ctx) }

// TransferStageRecord is the storage-only projection needed to finish a
// transfer stage. Token is transient input; only its digest is persisted.
type TransferStageRecord struct {
	TransferID        string
	ActorID           string
	Permission        string
	IntentJSON        string
	Token             string
	IdempotencyDigest string
	RequestDigest     string
	ExpiresAt         time.Time
}

// FinalizeTransferStage atomically creates the local transfer, its private
// compatibility event, the rich audit row/link, and the token-free replay
// envelope. The provider is never called from this transaction.
func (s *Store) FinalizeTransferStage(ctx context.Context, reservation ReservationResult, record TransferStageRecord, envelope []byte, now time.Time) error {
	if err := s.RequireRichAudit(ctx); err != nil {
		return err
	}
	if record.TransferID == "" || record.ActorID == "" || record.Permission == "" || record.IntentJSON == "" || record.Token == "" || record.ExpiresAt.IsZero() {
		return errors.New("assurance: incomplete transfer stage record")
	}
	canonicalEnvelope, err := CanonicalJSONBytes(envelope)
	if err != nil {
		return err
	}
	tenant := identity.StorageTenant(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	actor, err := reservationActor(ctx, tx, reservation)
	if err != nil {
		return err
	}
	tokenDigest := transferConfirmationDigest(record.Token, []byte(record.IntentJSON))
	if _, err = tx.ExecContext(ctx, `INSERT INTO transfer_intents
	 (tenant_id,transfer_id,actor_id,permission,intent_json,state,token_digest,idempotency_digest,request_digest,expires_at)
	 VALUES(?,?,?,?,?,'staged',?,?,?,?)`, tenant, record.TransferID, record.ActorID, record.Permission,
		record.IntentJSON, tokenDigest, record.IdempotencyDigest, record.RequestDigest, formatTimestamp(record.ExpiresAt)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO transfer_audit_events
	 (tenant_id,transfer_id,event_id,disposition,details,created_at) VALUES(?,?,?,?,?,?)`, tenant, record.TransferID,
		"stage:"+record.TransferID, "staged", `{"mutation_submitted":false,"token_persisted":false}`, formatTimestamp(now)); err != nil {
		return err
	}
	auditID := transferAuditIDFromContext(ctx)
	if _, err = s.auditLog.AppendTx(ctx, tx, audit.Entry{Actor: actor, Tool: "workiva_transfer_value", Action: "stage", Target: record.TransferID, AfterJSON: string(canonicalEnvelope), AuditID: auditID}); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO assurance_audit_links
	 (tenant_id,link_id,entity_kind,entity_id,audit_id,request_id,correlation_id,created_at) VALUES(?,?,?,?,?,?,?,?)`, tenant,
		uuid.NewString(), "transfer", record.TransferID, auditID, RequestIDFromContext(ctx), reservation.CorrelationID, formatTimestamp(now)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO assurance_audit_links
	 (tenant_id,link_id,entity_kind,entity_id,audit_id,request_id,correlation_id,created_at) VALUES(?,?,?,?,?,?,?,?)`, tenant,
		uuid.NewString(), "idempotency_record", reservation.RecordID, auditID, RequestIDFromContext(ctx), reservation.CorrelationID, formatTimestamp(now)); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET state='sealed',response_envelope=?,response_hash=?,response_status='staged',entity_reference=?,audit_id=?,terminal_at=?,lease_expires_at=?
	 WHERE tenant_id=? AND record_id=? AND state='reserved' AND owner_nonce=?`, string(canonicalEnvelope), digestHex(HashBytes(canonicalEnvelope)), record.TransferID, auditID,
		formatTimestamp(now), formatTimestamp(now), tenant, reservation.RecordID, reservation.OwnerNonce)
	if err != nil {
		return err
	}
	if err := requireOneTransition(result); err != nil {
		return err
	}
	return tx.Commit()
}

// TransferCompletionRequest is the provider-completed, local evidence bundle
// committed by FinalizeTransferCompletion. TypedReadbackJSON is the canonical
// typed value obtained from an uncached provider read.
type TransferCompletionRequest struct {
	TransferID          string
	OperationReference  string
	TerminalFenceDigest string
	ReadbackID          string
	TypedReadbackJSON   string
	ReplayEnvelope      []byte
	Now                 time.Time
}

// FinalizeTransferCompletion persists the known-completed operation evidence,
// uncached typed read-back, rich audit link, machine outcome, and confirm
// replay envelope in one local transaction. api_verified is derived from the
// persisted read-back row and frozen intent, never from a caller boolean alone.
func (s *Store) FinalizeTransferCompletion(ctx context.Context, reservation ReservationResult, request TransferCompletionRequest) error {
	if err := s.RequireRichAudit(ctx); err != nil {
		return err
	}
	if request.TransferID == "" || request.OperationReference == "" || request.ReadbackID == "" || request.TypedReadbackJSON == "" || request.TerminalFenceDigest == "" {
		return errors.New("assurance: incomplete transfer completion evidence")
	}
	if request.ReplayEnvelope == nil {
		return errors.New("assurance: transfer completion replay envelope is required")
	}
	canonicalReadback, err := CanonicalJSONBytes([]byte(request.TypedReadbackJSON))
	if err != nil {
		return err
	}
	canonicalEnvelope, err := CanonicalJSONBytes(request.ReplayEnvelope)
	if err != nil {
		return err
	}
	tenant := identity.StorageTenant(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var intentJSON, state, operation string
	var operationCompleted int
	if err = tx.QueryRowContext(ctx, `SELECT intent_json,state,operation_reference,operation_completed FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, request.TransferID).Scan(&intentJSON, &state, &operation, &operationCompleted); err != nil {
		return err
	}
	if state != "submitting" || operation != request.OperationReference || operationCompleted != 0 {
		return errors.New("assurance: transfer is not awaiting first completion finalization")
	}
	var intent struct {
		Intended string `json:"intended"`
	}
	if err := json.Unmarshal([]byte(intentJSON), &intent); err != nil || intent.Intended == "" {
		return errors.New("assurance: frozen transfer intent is invalid")
	}
	canonicalIntended, err := CanonicalJSONBytes([]byte(intent.Intended))
	if err != nil {
		return errors.New("assurance: frozen intended value is not canonical JSON")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO assurance_transfer_poll_events
	 (tenant_id,transfer_id,event_id,event_json,observed_at) VALUES(?,?,?,?,?)`, tenant, request.TransferID, uuid.NewString(),
		string(mustCanonical(map[string]any{"operation_reference": request.OperationReference, "status": "completed", "provider_completion_observed": true})), formatTimestamp(request.Now)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO assurance_transfer_readbacks
	 (tenant_id,transfer_id,readback_id,typed_value_json,cache_bypassed,observed_at) VALUES(?,?,?,?,1,?)`, tenant, request.TransferID,
		request.ReadbackID, string(canonicalReadback), formatTimestamp(request.Now)); err != nil {
		return err
	}
	var persisted string
	var cacheBypassed int
	if err = tx.QueryRowContext(ctx, `SELECT typed_value_json,cache_bypassed FROM assurance_transfer_readbacks WHERE tenant_id=? AND transfer_id=? AND readback_id=?`, tenant, request.TransferID, request.ReadbackID).Scan(&persisted, &cacheBypassed); err != nil {
		return err
	}
	if cacheBypassed != 1 || persisted != string(canonicalIntended) {
		return errors.New("assurance: persisted uncached read-back does not equal frozen intent")
	}
	completion, err := tx.ExecContext(ctx, `UPDATE transfer_intents SET operation_completed=1,state='machine_verified_visual_ack_pending',terminal_fence_digest=?,machine_outcome='api_verified',machine_outcome_provenance='provider_read_back',visual_state='pending',row_version=row_version+1 WHERE tenant_id=? AND transfer_id=? AND state='submitting' AND operation_reference=? AND operation_completed=0`, request.TerminalFenceDigest, tenant, request.TransferID, request.OperationReference)
	if err != nil {
		return err
	}
	if err := requireOneTransition(completion); err != nil {
		return err
	}
	auditID := transferAuditIDFromContext(ctx)
	actor, err := reservationActor(ctx, tx, reservation)
	if err != nil {
		return err
	}
	if _, err = s.auditLog.AppendTx(ctx, tx, audit.Entry{Actor: actor, Tool: "workiva_transfer_value", Action: "confirm", Target: request.TransferID, WorkivaOpURL: request.OperationReference, AfterJSON: string(canonicalEnvelope), AuditID: auditID}); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO transfer_audit_events
	 (tenant_id,transfer_id,event_id,disposition,details,created_at) VALUES(?,?,?,?,?,?)`, tenant, request.TransferID,
		"terminal:"+request.TransferID, "api_verified", `{"terminal_fence_verified":true,"readback_persisted":true,"operation_completed":true}`, formatTimestamp(request.Now)); err != nil {
		return err
	}
	for _, entity := range []struct{ kind, id string }{{"transfer", request.TransferID}, {"idempotency_record", reservation.RecordID}} {
		if _, err = tx.ExecContext(ctx, `INSERT INTO assurance_audit_links
		 (tenant_id,link_id,entity_kind,entity_id,audit_id,request_id,correlation_id,created_at) VALUES(?,?,?,?,?,?,?,?)`, tenant,
			uuid.NewString(), entity.kind, entity.id, auditID, RequestIDFromContext(ctx), reservation.CorrelationID, formatTimestamp(request.Now)); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET state='sealed',response_envelope=?,response_hash=?,response_status='machine_verified_visual_ack_pending',entity_reference=?,audit_id=?,terminal_at=?,lease_expires_at=?
	 WHERE tenant_id=? AND record_id=? AND state='reserved' AND owner_nonce=?`, string(canonicalEnvelope), digestHex(HashBytes(canonicalEnvelope)), request.TransferID, auditID,
		formatTimestamp(request.Now), formatTimestamp(request.Now), tenant, reservation.RecordID, reservation.OwnerNonce)
	if err != nil {
		return err
	}
	if err := requireOneTransition(result); err != nil {
		return err
	}
	return tx.Commit()
}

// TransferActionResult is the redacted, immutable projection used by the
// acknowledge and reconcile replay envelopes. It deliberately contains no
// confirmation token, idempotency key, request body, or mutable entity blob.
type TransferActionResult struct {
	AuditID                      string `json:"nl_audit_id"`
	Status                       string `json:"status"`
	TransferID                   string `json:"transfer_id"`
	State                        string `json:"state,omitempty"`
	MachineOutcome               string `json:"machine_outcome,omitempty"`
	MachineOutcomeProvenance     string `json:"machine_outcome_provenance,omitempty"`
	VisualState                  string `json:"visual_state,omitempty"`
	OperationReference           string `json:"operation_reference,omitempty"`
	ReadbackID                   string `json:"readback_id,omitempty"`
	ReadbackJSON                 string `json:"readback_json,omitempty"`
	ReadbackCacheBypassed        bool   `json:"readback_cache_bypassed,omitempty"`
	TargetResourceID             string `json:"target_resource_id,omitempty"`
	TargetLocator                string `json:"target_locator,omitempty"`
	ReconciliationClassification string `json:"reconciliation_classification,omitempty"`
	ReconciliationRequired       bool   `json:"reconciliation_required"`
	NoMutationSubmitted          bool   `json:"no_mutation_submitted"`
	Terminal                     bool   `json:"terminal,omitempty"`
	ErrorCode                    string `json:"error_code,omitempty"`
	ErrorMessage                 string `json:"error_message,omitempty"`
}

type TransferVisualAcknowledgementRequest struct {
	TransferID        string
	AckActorID        string
	Observation       string
	UILocationJSON    string
	Note              string
	Refreshed         bool
	RefreshOverride   string
	ObservedValueJSON string
	ObservedDigest    string
	Now               time.Time
}

type TransferReconciliationRequest struct {
	TransferID            string
	Action                string
	Classification        string
	Note                  string
	OperationEventJSON    string
	ReadbackID            string
	TypedReadbackJSON     string
	ReadbackCacheBypassed bool
	Now                   time.Time
}

type transferRowProjection struct {
	IntentJSON, State, OperationReference, MachineOutcome, MachineProvenance, VisualState string
	OperationCompleted                                                                    int
}

func loadTransferRowTx(ctx context.Context, tx *sql.Tx, tenant, transferID string) (transferRowProjection, error) {
	var row transferRowProjection
	err := tx.QueryRowContext(ctx, `SELECT intent_json,state,operation_reference,machine_outcome,machine_outcome_provenance,visual_state,operation_completed FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, transferID).Scan(
		&row.IntentJSON, &row.State, &row.OperationReference, &row.MachineOutcome, &row.MachineProvenance, &row.VisualState, &row.OperationCompleted)
	return row, err
}

type transferIntentEndpoints struct {
	Intended string `json:"intended"`
	ActorID  string `json:"actor_id"`
	Target   struct {
		ResourceID string `json:"resource_id"`
		Locator    string `json:"locator"`
	} `json:"target"`
}

func transferActionResultFromRow(auditID, transferID string, row transferRowProjection) (TransferActionResult, transferIntentEndpoints, error) {
	var intent transferIntentEndpoints
	if err := json.Unmarshal([]byte(row.IntentJSON), &intent); err != nil {
		return TransferActionResult{}, intent, err
	}
	return TransferActionResult{
		AuditID: auditID, TransferID: transferID, State: row.State,
		MachineOutcome: row.MachineOutcome, MachineOutcomeProvenance: row.MachineProvenance,
		VisualState: row.VisualState, OperationReference: row.OperationReference,
		TargetResourceID: intent.Target.ResourceID, TargetLocator: intent.Target.Locator,
		NoMutationSubmitted: true,
	}, intent, nil
}

func (s *Store) sealTransferActionTx(ctx context.Context, tx *sql.Tx, reservation ReservationResult, result TransferActionResult, now time.Time) error {
	envelope, err := CanonicalJSON(result)
	if err != nil {
		return err
	}
	auditID := result.AuditID
	actor, tool, action, correlationID := "", "", "", reservation.CorrelationID
	tenant := identity.StorageTenant(ctx)
	if err := tx.QueryRowContext(ctx, `SELECT actor_id,tool,action,correlation_id FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=? AND owner_nonce=? AND state='reserved'`, tenant, reservation.RecordID, reservation.OwnerNonce).Scan(&actor, &tool, &action, &correlationID); err != nil {
		return err
	}
	if _, err := s.auditLog.AppendTx(ctx, tx, audit.Entry{Actor: actor, Tool: tool, Action: action, Target: result.TransferID, WorkivaOpURL: result.OperationReference, AfterJSON: string(envelope), AuditID: auditID}); err != nil {
		return err
	}
	for _, entity := range []struct{ kind, id string }{{"transfer", result.TransferID}, {"idempotency_record", reservation.RecordID}} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_audit_links (tenant_id,link_id,entity_kind,entity_id,audit_id,request_id,correlation_id,created_at) VALUES(?,?,?,?,?,?,?,?)`, tenant, uuid.NewString(), entity.kind, entity.id, auditID, RequestIDFromContext(ctx), correlationID, formatTimestamp(now)); err != nil {
			return err
		}
	}
	sealed, err := tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET state='sealed',response_envelope=?,response_hash=?,response_status=?,entity_reference=?,audit_id=?,terminal_at=?,lease_expires_at=? WHERE tenant_id=? AND record_id=? AND state='reserved' AND owner_nonce=?`, string(envelope), digestHex(HashBytes(envelope)), result.Status, result.TransferID, auditID, formatTimestamp(now), formatTimestamp(now), tenant, reservation.RecordID, reservation.OwnerNonce)
	if err != nil {
		return err
	}
	return requireOneTransition(sealed)
}

// FinalizeTransferVisualAcknowledgement records independent visual evidence,
// rich audit, the resulting local CAS, and the sealed replay envelope in one
// SQLite transaction. It never calls a provider.
func (s *Store) FinalizeTransferVisualAcknowledgement(ctx context.Context, reservation ReservationResult, request TransferVisualAcknowledgementRequest) (TransferActionResult, error) {
	if err := s.requireRichAudit(); err != nil {
		return TransferActionResult{}, err
	}
	if request.TransferID == "" || request.AckActorID == "" || request.Observation == "" || (request.UILocationJSON == "" && (request.Observation == "matches" || request.Observation == "mismatch")) {
		return TransferActionResult{}, errors.New("assurance: incomplete visual acknowledgement")
	}
	if request.Now.IsZero() {
		request.Now = time.Now().UTC()
	}
	observed := ""
	if request.ObservedValueJSON != "" {
		canonical, err := CanonicalJSONBytes([]byte(request.ObservedValueJSON))
		if err != nil {
			return TransferActionResult{}, err
		}
		observed = string(canonical)
	}
	tenant := identity.StorageTenant(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TransferActionResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := loadTransferRowTx(ctx, tx, tenant, request.TransferID)
	if err != nil {
		return TransferActionResult{}, err
	}
	auditID := transferAuditIDFromContext(ctx)
	result, intent, err := transferActionResultFromRow(auditID, request.TransferID, row)
	if err != nil {
		return TransferActionResult{}, err
	}
	var persistedReadback string
	if err := tx.QueryRowContext(ctx, `SELECT typed_value_json FROM assurance_transfer_readbacks WHERE tenant_id=? AND transfer_id=? ORDER BY observed_at DESC,readback_id DESC LIMIT 1`, tenant, request.TransferID).Scan(&persistedReadback); err != nil && err != sql.ErrNoRows {
		return TransferActionResult{}, err
	}
	result.NoMutationSubmitted = true
	result.Status = "ack_rejected"
	result.ErrorCode = "ack_rejected"
	result.ErrorMessage = "visual acknowledgement was recorded without changing the machine outcome"
	result.VisualState = row.VisualState
	result.ReconciliationRequired = false
	if row.State != "machine_verified_visual_ack_pending" || row.MachineOutcome != "api_verified" {
		result.ErrorMessage = "visual acknowledgement is not allowed for the current transfer state"
	} else {
		switch request.Observation {
		case "matches":
			if !request.Refreshed && request.RefreshOverride == "" {
				result.ErrorMessage = "a refreshed visual match or an approved refresh override is required"
			} else if persistedReadback == "" || (observed != "" && observed != persistedReadback) {
				result.Status = "reconciliation_required"
				result.ErrorCode = "visual_observation_conflict"
				result.ErrorMessage = "visual observation conflicts with persisted machine read-back"
				result.ReconciliationRequired = true
				result.ReconciliationClassification = "visual_observation_conflict"
				result.VisualState = "mismatch"
				transition, transitionErr := tx.ExecContext(ctx, `UPDATE transfer_intents SET state='reconciliation_required',visual_state='mismatch',row_version=row_version+1 WHERE tenant_id=? AND transfer_id=? AND state='machine_verified_visual_ack_pending'`, tenant, request.TransferID)
				if transitionErr != nil {
					return TransferActionResult{}, transitionErr
				}
				if transitionErr = requireOneTransition(transition); transitionErr != nil {
					return TransferActionResult{}, transitionErr
				}
			} else {
				result.Status = "visually_acknowledged"
				result.ErrorCode, result.ErrorMessage = "", ""
				result.VisualState = "visually_acknowledged"
				transition, transitionErr := tx.ExecContext(ctx, `UPDATE transfer_intents SET state='visually_acknowledged',visual_state='visually_acknowledged',row_version=row_version+1 WHERE tenant_id=? AND transfer_id=? AND state='machine_verified_visual_ack_pending'`, tenant, request.TransferID)
				if transitionErr != nil {
					return TransferActionResult{}, transitionErr
				}
				if transitionErr = requireOneTransition(transition); transitionErr != nil {
					return TransferActionResult{}, transitionErr
				}
			}
		case "mismatch", "not_visible":
			result.Status = "reconciliation_required"
			result.ReconciliationRequired = true
			result.ReconciliationClassification = "visual_disagreement"
			if request.Observation == "not_visible" {
				result.ReconciliationClassification = "visual_not_visible"
			} else if observed != "" && observed == persistedReadback {
				result.ReconciliationClassification = "visual_observation_conflict"
				result.ErrorCode = "visual_observation_conflict"
			}
			if result.ErrorCode == "" {
				result.ErrorCode = "visual_disagreement"
			}
			result.ErrorMessage = "visual evidence disagrees with or cannot verify the machine result"
			result.VisualState = request.Observation
			transition, transitionErr := tx.ExecContext(ctx, `UPDATE transfer_intents SET state='reconciliation_required',visual_state=?,row_version=row_version+1 WHERE tenant_id=? AND transfer_id=? AND state='machine_verified_visual_ack_pending'`, request.Observation, tenant, request.TransferID)
			if transitionErr != nil {
				return TransferActionResult{}, transitionErr
			}
			if transitionErr = requireOneTransition(transition); transitionErr != nil {
				return TransferActionResult{}, transitionErr
			}
		case "not_checked":
			result.Status = "machine_verified_visual_ack_pending"
			result.ErrorCode, result.ErrorMessage = "", ""
			result.VisualState = "not_checked"
			transition, transitionErr := tx.ExecContext(ctx, `UPDATE transfer_intents SET visual_state='not_checked',row_version=row_version+1 WHERE tenant_id=? AND transfer_id=? AND state='machine_verified_visual_ack_pending'`, tenant, request.TransferID)
			if transitionErr != nil {
				return TransferActionResult{}, transitionErr
			}
			if transitionErr = requireOneTransition(transition); transitionErr != nil {
				return TransferActionResult{}, transitionErr
			}
		}
	}
	ackID := uuid.NewString()
	refreshed := 0
	if request.Refreshed {
		refreshed = 1
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO transfer_visual_acknowledgements(tenant_id,transfer_id,ack_actor_id,observation,ui_location,refreshed,observed_value,created_at) VALUES(?,?,?,?,?,?,?,?)`, tenant, request.TransferID, request.AckActorID, request.Observation, request.UILocationJSON, refreshed, observed, formatTimestamp(request.Now)); err != nil {
		return TransferActionResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO assurance_transfer_visual_evidence(tenant_id,transfer_id,acknowledgement_id,confirmer_actor_id,ack_actor_id,observation,ui_location,refreshed,observed_value_json,observed_digest,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, tenant, request.TransferID, ackID, intent.ActorID, request.AckActorID, request.Observation, request.UILocationJSON, refreshed, observed, request.ObservedDigest, formatTimestamp(request.Now)); err != nil {
		return TransferActionResult{}, err
	}
	details := mustCanonical(map[string]any{"observation": request.Observation, "refreshed": request.Refreshed, "note": request.Note, "visual_evidence_persisted": true, "machine_outcome_unchanged": true})
	if _, err = tx.ExecContext(ctx, `INSERT INTO transfer_audit_events(tenant_id,transfer_id,event_id,disposition,details,created_at) VALUES(?,?,?,?,?,?)`, tenant, request.TransferID, "visual:"+ackID, result.Status, string(details), formatTimestamp(request.Now)); err != nil {
		return TransferActionResult{}, err
	}
	if result.ReconciliationRequired {
		if _, err = tx.ExecContext(ctx, `INSERT INTO transfer_reconciliations(tenant_id,transfer_id,reason,classification,evidence_ref,disposition,created_at) VALUES(?,?,?,?,?,?,?)`, tenant, request.TransferID, "visual_observation", result.ReconciliationClassification, ackID, "reconciliation_required", formatTimestamp(request.Now)); err != nil {
			return TransferActionResult{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO assurance_transfer_reconciliation_evidence(tenant_id,transfer_id,reconciliation_id,reason,classification,evidence_ref,disposition,created_at) VALUES(?,?,?,?,?,?,?,?)`, tenant, request.TransferID, uuid.NewString(), "visual_observation", result.ReconciliationClassification, ackID, "reconciliation_required", formatTimestamp(request.Now)); err != nil {
			return TransferActionResult{}, err
		}
	}
	if err = s.sealTransferActionTx(ctx, tx, reservation, result, request.Now.UTC()); err != nil {
		return TransferActionResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return TransferActionResult{}, err
	}
	return result, nil
}

// FinalizeTransferReconciliation persists only read-only provider evidence or
// a classification CAS. It never submits a provider mutation.
func (s *Store) FinalizeTransferReconciliation(ctx context.Context, reservation ReservationResult, request TransferReconciliationRequest) (TransferActionResult, error) {
	if err := s.requireRichAudit(); err != nil {
		return TransferActionResult{}, err
	}
	if request.TransferID == "" || request.Action == "" {
		return TransferActionResult{}, errors.New("assurance: incomplete reconciliation")
	}
	if request.Now.IsZero() {
		request.Now = time.Now().UTC()
	}
	tenant := identity.StorageTenant(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TransferActionResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := loadTransferRowTx(ctx, tx, tenant, request.TransferID)
	if err != nil {
		return TransferActionResult{}, err
	}
	result, intent, err := transferActionResultFromRow(transferAuditIDFromContext(ctx), request.TransferID, row)
	if err != nil {
		return TransferActionResult{}, err
	}
	result.NoMutationSubmitted = true
	// Evidence acquisition is only valid within the unresolved episode.
	if row.State != "reconciliation_required" {
		return TransferActionResult{}, errors.New("reconciliation requires reconciliation_required state")
	}
	result.Status = row.State
	result.ReconciliationRequired = row.State == "reconciliation_required"
	if request.OperationEventJSON != "" {
		canonical, e := CanonicalJSONBytes([]byte(request.OperationEventJSON))
		if e != nil {
			return TransferActionResult{}, e
		}
		request.OperationEventJSON = string(canonical)
		if _, err = tx.ExecContext(ctx, `INSERT INTO assurance_transfer_poll_events(tenant_id,transfer_id,event_id,event_json,observed_at) VALUES(?,?,?,?,?)`, tenant, request.TransferID, uuid.NewString(), request.OperationEventJSON, formatTimestamp(request.Now)); err != nil {
			return TransferActionResult{}, err
		}
	}
	if request.TypedReadbackJSON != "" {
		canonical, e := CanonicalJSONBytes([]byte(request.TypedReadbackJSON))
		if e != nil {
			return TransferActionResult{}, e
		}
		if !request.ReadbackCacheBypassed {
			return TransferActionResult{}, errors.New("assurance: reconciliation read-back must bypass cache")
		}
		if request.Action != "read_back" || request.ReadbackID == "" {
			return TransferActionResult{}, errors.New("assurance: read-back must belong to a fresh read_back action")
		}
		request.TypedReadbackJSON = string(canonical)
		inserted, insertErr := tx.ExecContext(ctx, `INSERT INTO assurance_transfer_readbacks(tenant_id,transfer_id,readback_id,typed_value_json,cache_bypassed,observed_at) VALUES(?,?,?,?,1,?)`, tenant, request.TransferID, request.ReadbackID, request.TypedReadbackJSON, formatTimestamp(request.Now))
		if insertErr != nil {
			return TransferActionResult{}, insertErr
		}
		readRowID, idErr := inserted.LastInsertId()
		if idErr != nil {
			return TransferActionResult{}, idErr
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO assurance_reconciliation_read_actions(tenant_id,transfer_id,readback_id,readback_rowid,reconciliation_epoch) SELECT ?,?,?,?,reconciliation_epoch FROM transfer_intents WHERE tenant_id=? AND transfer_id=? AND state='reconciliation_required'`, tenant, request.TransferID, request.ReadbackID, readRowID, tenant, request.TransferID); err != nil {
			return TransferActionResult{}, err
		}
		result.ReadbackID, result.ReadbackJSON, result.ReadbackCacheBypassed = request.ReadbackID, request.TypedReadbackJSON, true
	}
	if request.Action == "classify" || request.Action == "close" {
		if row.State != "reconciliation_required" {
			return TransferActionResult{}, errors.New("reconciliation requires reconciliation_required state")
		}
		var intended string
		if err = tx.QueryRowContext(ctx, `SELECT json_extract(intent_json,'$.intended') FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, request.TransferID).Scan(&intended); err != nil {
			return TransferActionResult{}, err
		}
		canonicalIntended, e := CanonicalJSONBytes([]byte(intended))
		if e != nil {
			return TransferActionResult{}, e
		}
		var next, machine, provenance string
		switch request.Classification {
		case "confirmed_applied":
			// A pre-uncertainty or replayed row cannot prove this episode. SQLite
			// rowid orders persisted observations independently of caller clocks;
			// the joined action and epoch bind the latest read to this CAS.
			var persisted, verifiedReadbackID string
			var cache int
			if e = tx.QueryRowContext(ctx, `SELECT rb.readback_id,rb.typed_value_json,rb.cache_bypassed
 FROM assurance_transfer_readbacks rb
 JOIN assurance_reconciliation_read_actions action ON action.tenant_id=rb.tenant_id AND action.transfer_id=rb.transfer_id AND action.readback_id=rb.readback_id AND action.readback_rowid=rb.rowid
 JOIN transfer_intents intent ON intent.tenant_id=rb.tenant_id AND intent.transfer_id=rb.transfer_id AND intent.reconciliation_epoch=action.reconciliation_epoch
 WHERE rb.tenant_id=? AND rb.transfer_id=?
 AND rb.rowid=(SELECT MAX(rowid) FROM assurance_transfer_readbacks WHERE tenant_id=? AND transfer_id=?)`, tenant, request.TransferID, tenant, request.TransferID).Scan(&verifiedReadbackID, &persisted, &cache); e != nil || cache != 1 || persisted != string(canonicalIntended) {
				return TransferActionResult{}, errors.New("confirmed_applied requires latest fresh uncached reconciliation read-back for this uncertainty episode")
			}
			request.ReadbackID = verifiedReadbackID
			result.ReadbackID, result.ReadbackJSON, result.ReadbackCacheBypassed = verifiedReadbackID, persisted, true
			next, machine, provenance = "machine_verified_visual_ack_pending", "api_verified", "reconciliation"
		case "confirmed_not_applied":
			// A generic operation status (including failed/rejected/cancelled)
			// does not establish that no cells were changed. No verified Workiva
			// non-acceptance/no-effect contract is available here yet.
			return TransferActionResult{}, errors.New("capability_unverified: provider non-acceptance/no-effect evidence is unavailable; reconciliation_required")
		default:
			if request.Action == "close" {
				return TransferActionResult{}, errors.New("close requires a terminal reconciliation classification")
			}
			// An open classification cannot overwrite immutable machine evidence.
			next, machine, provenance = "reconciliation_required", row.MachineOutcome, row.MachineProvenance
		}
		res, e := tx.ExecContext(ctx, `UPDATE transfer_intents SET state=?,machine_outcome=?,machine_outcome_provenance=?,visual_state=?,row_version=row_version+1 WHERE tenant_id=? AND transfer_id=? AND state='reconciliation_required'`, next, machine, provenance, func() string {
			if next == "machine_verified_visual_ack_pending" {
				return "pending"
			}
			return row.VisualState
		}(), tenant, request.TransferID)
		if e != nil {
			return TransferActionResult{}, e
		}
		if e = requireOneTransition(res); e != nil {
			return TransferActionResult{}, e
		}
		result.State, result.Status, result.MachineOutcome, result.MachineOutcomeProvenance, result.VisualState = next, next, machine, provenance, func() string {
			if next == "machine_verified_visual_ack_pending" {
				return "pending"
			}
			return row.VisualState
		}()
		result.ReconciliationRequired = next == "reconciliation_required"
		result.ReconciliationClassification = request.Classification
		result.Terminal = next == "reconciled_not_applied"
		if _, err = tx.ExecContext(ctx, `INSERT INTO transfer_reconciliations(tenant_id,transfer_id,reason,classification,evidence_ref,disposition,created_at) VALUES(?,?,?,?,?,?,?)`, tenant, request.TransferID, "operator_reconciliation", request.Classification, request.ReadbackID, next, formatTimestamp(request.Now)); err != nil {
			return TransferActionResult{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO assurance_transfer_reconciliation_evidence(tenant_id,transfer_id,reconciliation_id,reason,classification,evidence_ref,disposition,created_at) VALUES(?,?,?,?,?,?,?,?)`, tenant, request.TransferID, uuid.NewString(), "operator_reconciliation", request.Classification, request.ReadbackID, next, formatTimestamp(request.Now)); err != nil {
			return TransferActionResult{}, err
		}
	}
	details := mustCanonical(map[string]any{"action": request.Action, "classification": request.Classification, "note": request.Note, "provider_mutation_submitted": false, "evidence_persisted": true})
	if _, err = tx.ExecContext(ctx, `INSERT INTO transfer_audit_events(tenant_id,transfer_id,event_id,disposition,details,created_at) VALUES(?,?,?,?,?,?)`, tenant, request.TransferID, "reconcile:"+uuid.NewString(), result.Status, string(details), formatTimestamp(request.Now)); err != nil {
		return TransferActionResult{}, err
	}
	if err = s.sealTransferActionTx(ctx, tx, reservation, result, request.Now.UTC()); err != nil {
		return TransferActionResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return TransferActionResult{}, err
	}
	_ = intent
	return result, nil
}

func transferConfirmationDigest(token string, frozenIntent []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(token))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(frozenIntent)
	return hex.EncodeToString(h.Sum(nil))
}

func mustCanonical(v any) []byte {
	raw, err := CanonicalJSON(v)
	if err != nil {
		return []byte(`{}`)
	}
	return raw
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
// RunJanitor is retained for isolated stores and tests. Production uses
// RunJanitorGated so periodic SQLite writes participate in backup admission.
func (s *Store) RunJanitor(ctx context.Context, interval time.Duration) {
	s.RunJanitorGated(ctx, interval, nil)
}

func (s *Store) RunJanitorGated(ctx context.Context, interval time.Duration, gate *DrainGate) {
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
			release, err := gate.EnterWrite(ctx)
			if err != nil {
				continue // backup has closed admission, or shutdown is underway
			}
			_, _ = s.ExpireStaleReservations(ctx, now.UTC())
			release()
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
