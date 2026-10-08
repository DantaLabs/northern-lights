package transfer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
)

type Endpoint struct {
	ResourceID  string `json:"resource_id"`
	SheetID     string `json:"sheet_id"`
	Locator     string `json:"locator"`
	Fingerprint string `json:"fingerprint"`
}
type Intent struct {
	ID             string    `json:"transfer_id"`
	TenantID       string    `json:"tenant_id"`
	ActorID        string    `json:"actor_id"`
	Permission     string    `json:"permission"`
	RouteID        string    `json:"route_id"`
	RouteRevision  int       `json:"route_revision"`
	MappingID      string    `json:"mapping_id"`
	Source         Endpoint  `json:"source"`
	Target         Endpoint  `json:"target"`
	Before         string    `json:"before"`
	Intended       string    `json:"intended"`
	PolicyID       string    `json:"policy_id"`
	PolicyRevision int       `json:"policy_revision"`
	PolicyHash     string    `json:"policy_hash"`
	MappingHash    string    `json:"mapping_hash"`
	ExpiresAt      time.Time `json:"expires_at"`
}
type State string

const (
	StateStaged                 State = "staged"
	StateClaimed                State = "claimed"
	StateReconciliationRequired State = "reconciliation_required"
	StateMachineVerified        State = "machine_verified_visual_ack_pending"
	StateVisuallyAcknowledged   State = "visually_acknowledged"
	StateReconciledNotApplied   State = "reconciled_not_applied"
)

type Transfer struct {
	Intent                   Intent
	State                    State
	Submissions              int
	OperationReference       string
	ClaimFenceDigest         string
	TerminalFenceDigest      string
	MachineOutcome           string
	MachineOutcomeProvenance string
	VisualState              string
}
type StageResult struct {
	TransferID            string
	Token                 string
	ReplayRequiresRestage bool
}
type Claim struct {
	TenantID, ActorID, Permission, TransferID, Token, LeaseID string
	Now, LeaseUntil                                           time.Time
}
type Store struct{ db *sql.DB }

const transferExecutionLease = time.Minute

func NewWithDB(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("transfer: database required")
	}
	if _, err := assurance.NewWithDB(db); err != nil {
		return nil, err
	}
	store := &Store{db: db}
	if err := store.QuarantineExpired(context.Background(), time.Now().UTC()); err != nil {
		return nil, err
	}
	return store, nil
}
func digest(v string) string { x := sha256.Sum256([]byte(v)); return hex.EncodeToString(x[:]) }
func confirmationDigest(token string, frozenIntent []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(token))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(frozenIntent)
	return hex.EncodeToString(h.Sum(nil))
}
func (s *Store) Stage(ctx context.Context, i Intent, token, keyDigest, requestDigest string, now time.Time) (StageResult, error) {
	if i.ExpiresAt.IsZero() || !i.ExpiresAt.After(now.UTC()) {
		return StageResult{}, errors.New("transfer: intent expiry must be in the future")
	}
	if i.ID == "" || i.TenantID == "" || i.ActorID == "" || i.Permission == "" || token == "" || keyDigest == "" || requestDigest == "" {
		return StageResult{}, errors.New("incomplete stage binding")
	}
	i.ExpiresAt = i.ExpiresAt.UTC()
	raw, e := json.Marshal(i)
	if e != nil {
		return StageResult{}, e
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return StageResult{}, e
	}
	defer func() { _ = tx.Rollback() }()
	var orphan int
	e = tx.QueryRowContext(ctx, `SELECT 1 FROM assurance_transfer_startup_quarantines WHERE tenant_id=? AND transfer_id=?`, i.TenantID, i.ID).Scan(&orphan)
	if e == nil {
		return StageResult{}, errors.New("transfer: external fence quarantine blocks transfer ID reuse")
	}
	if e != sql.ErrNoRows {
		return StageResult{}, e
	}
	var id, rd string
	e = tx.QueryRowContext(ctx, `SELECT transfer_id,request_digest FROM transfer_intents WHERE tenant_id=? AND actor_id=? AND idempotency_digest=?`, i.TenantID, i.ActorID, keyDigest).Scan(&id, &rd)
	if e == nil {
		if rd != requestDigest {
			return StageResult{}, errors.New("idempotency_conflict")
		}
		return StageResult{TransferID: id, ReplayRequiresRestage: true}, nil
	}
	if e != sql.ErrNoRows {
		return StageResult{}, e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO transfer_intents(tenant_id,transfer_id,actor_id,permission,intent_json,state,token_digest,idempotency_digest,request_digest,expires_at) VALUES(?,?,?,?,?,'staged',?,?,?,?)`, i.TenantID, i.ID, i.ActorID, i.Permission, string(raw), confirmationDigest(token, raw), keyDigest, requestDigest, i.ExpiresAt.Format(time.RFC3339Nano))
	if e != nil {
		return StageResult{}, e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO transfer_audit_events(tenant_id,transfer_id,event_id,disposition,details,created_at) VALUES(?,?,?,?,?,?)`, i.TenantID, i.ID, "stage:"+i.ID, "staged", `{"mutation_submitted":false,"token_persisted":false}`, now.UTC().Format(time.RFC3339Nano)); e != nil {
		return StageResult{}, e
	}
	if e = tx.Commit(); e != nil {
		return StageResult{}, e
	}
	return StageResult{TransferID: i.ID, Token: token}, nil
}
func (s *Store) Claim(ctx context.Context, c Claim) (bool, error) {
	if c.Token == "" || c.LeaseID == "" {
		return false, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var frozen []byte
	if err := tx.QueryRowContext(ctx, `SELECT intent_json FROM transfer_intents WHERE tenant_id=? AND transfer_id=? AND actor_id=? AND permission=? AND state='staged' AND expires_at>?`, c.TenantID, c.TransferID, c.ActorID, c.Permission, c.Now.UTC().Format(time.RFC3339Nano)).Scan(&frozen); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE transfer_intents SET state='claimed',lease_id=?,lease_expires_at=?,row_version=row_version+1 WHERE tenant_id=? AND transfer_id=? AND actor_id=? AND permission=? AND token_digest=? AND state='staged' AND expires_at>?`, c.LeaseID, c.LeaseUntil.UTC().Format(time.RFC3339Nano), c.TenantID, c.TransferID, c.ActorID, c.Permission, confirmationDigest(c.Token, frozen), c.Now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO transfer_audit_events(tenant_id,transfer_id,event_id,disposition,details,created_at) VALUES(?,?,?,?,?,?)`, c.TenantID, c.TransferID, "claim:"+c.TransferID, "claim_committed", `{"token_persisted":false}`, c.Now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
func (s *Store) QuarantineExpired(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT tenant_id,transfer_id,row_version,state,lease_id,lease_expires_at,claim_fence_digest,operation_reference FROM transfer_intents WHERE state IN ('claimed','submitting','fenced')`)
	if err != nil {
		return err
	}
	type expired struct {
		tenant, id, state, leaseID, leaseExpiry, claimFence, operation string
		version                                                        int64
	}
	var found []expired
	for rows.Next() {
		var x expired
		if err = rows.Scan(&x.tenant, &x.id, &x.version, &x.state, &x.leaseID, &x.leaseExpiry, &x.claimFence, &x.operation); err != nil {
			_ = rows.Close()
			return err
		}
		leaseUntil, parseErr := time.Parse(time.RFC3339Nano, x.leaseExpiry)
		leaseValid := x.leaseID != "" && x.leaseExpiry != "" && parseErr == nil && leaseUntil.After(now.UTC())
		if x.state == "fenced" || x.state == "submitting" {
			leaseValid = leaseValid && x.claimFence != ""
		}
		if leaseValid {
			continue
		}
		found = append(found, x)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, x := range found {
		res, e := tx.ExecContext(ctx, `UPDATE transfer_intents SET state='reconciliation_required',recovery_reason='crash_recovery',machine_outcome='write_outcome_unknown',lease_id='',lease_expires_at='',row_version=row_version+1 WHERE tenant_id=? AND transfer_id=? AND row_version=? AND state IN ('claimed','submitting','fenced')`, x.tenant, x.id, x.version)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		if n == 0 {
			continue
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO transfer_audit_events(tenant_id,transfer_id,event_id,disposition,details,created_at) VALUES(?,?,?,?,?,?)`, x.tenant, x.id, "recovery:"+x.id, "crash_recovery_quarantined", `{"mutation_resubmitted":false,"automatic_poll":false,"reconciliation_required":true}`, now.UTC().Format(time.RFC3339Nano)); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO transfer_reconciliations(tenant_id,transfer_id,reason,classification,evidence_ref,disposition,created_at) VALUES(?,?,?,?,?,?,?)`, x.tenant, x.id, "crash_recovery", "still_unknown", x.operation, "reconciliation_required", now.UTC().Format(time.RFC3339Nano)); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (s *Store) Get(ctx context.Context, tenant, id string) (Transfer, error) {
	var t Transfer
	var raw string
	e := s.db.QueryRowContext(ctx, `SELECT intent_json,state,submissions,operation_reference,claim_fence_digest,terminal_fence_digest,machine_outcome,machine_outcome_provenance,visual_state FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, id).Scan(&raw, &t.State, &t.Submissions, &t.OperationReference, &t.ClaimFenceDigest, &t.TerminalFenceDigest, &t.MachineOutcome, &t.MachineOutcomeProvenance, &t.VisualState)
	if e != nil {
		return t, e
	}
	e = json.Unmarshal([]byte(raw), &t.Intent)
	return t, e
}
func (s *Store) RecordAcknowledgement(ctx context.Context, tenant, id, actor, obs, location string, refreshed bool, value string) error {
	v := 0
	if refreshed {
		v = 1
	}
	_, e := s.db.ExecContext(ctx, `INSERT INTO transfer_visual_acknowledgements VALUES(?,?,?,?,?,?,?,?)`, tenant, id, actor, obs, location, v, value, time.Now().UTC().Format(time.RFC3339Nano))
	return e
}
func (s *Store) RecordReadback(ctx context.Context, tenant, id, readbackID, canonicalValue string, cacheBypassed bool, now time.Time) error {
	if !cacheBypassed {
		return errors.New("transfer: read-back must bypass cache")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var intended string
	var state State
	var raw string
	var operationCompleted int
	if err = tx.QueryRowContext(ctx, `SELECT intent_json,state,operation_completed FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, id).Scan(&raw, &state, &operationCompleted); err != nil {
		return err
	}
	var intent Intent
	if err = json.Unmarshal([]byte(raw), &intent); err != nil {
		return err
	}
	intended = intent.Intended
	if operationCompleted != 1 {
		return errors.New("transfer: provider operation is not known completed")
	}
	if state != StateClaimed && state != State("submitting") && state != StateReconciliationRequired {
		return errors.New("transfer: read-back not permitted in current state")
	}
	outcome := StateReconciliationRequired
	machine := "write_outcome_unknown"
	visual := "pending"
	prov := "provider_read_back"
	if canonicalValue == intended {
		outcome = StateMachineVerified
		machine = "api_verified"
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO assurance_transfer_readbacks(tenant_id,transfer_id,readback_id,typed_value_json,cache_bypassed,observed_at) VALUES(?,?,?,?,1,?)`, tenant, id, readbackID, canonicalValue, now.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE transfer_intents SET state=?,machine_outcome=?,machine_outcome_provenance=?,visual_state=?,row_version=row_version+1 WHERE tenant_id=? AND transfer_id=?`, outcome, machine, prov, visual, tenant, id); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO transfer_audit_events(tenant_id,transfer_id,event_id,disposition,details,created_at) VALUES(?,?,?,?,?,?)`, tenant, id, "readback:"+readbackID, machine, `{"cache_bypassed":true}`, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ApplyReconciliation(ctx context.Context, tenant, id, classification, readbackValue string, readbackEqual bool, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var intendedRaw string
	if err = tx.QueryRowContext(ctx, `SELECT intent_json FROM transfer_intents WHERE tenant_id=? AND transfer_id=? AND state='reconciliation_required'`, tenant, id).Scan(&intendedRaw); err != nil {
		return err
	}
	var intent Intent
	if err = json.Unmarshal([]byte(intendedRaw), &intent); err != nil {
		return err
	}
	canonicalIntended, err := assurance.CanonicalJSONBytes([]byte(intent.Intended))
	if err != nil {
		return errors.New("transfer: frozen intended value is not canonical JSON")
	}
	canonicalCaller, err := assurance.CanonicalJSONBytes([]byte(readbackValue))
	if err != nil && readbackValue != "" {
		return errors.New("transfer: reconciliation evidence is not canonical JSON")
	}
	callerValue := readbackValue
	if err == nil && readbackValue != "" {
		callerValue = string(canonicalCaller)
	}
	var next State
	var machine, prov string
	switch classification {
	case "confirmed_applied":
		var persisted string
		var cacheBypassed int
		err = tx.QueryRowContext(ctx, `SELECT typed_value_json,cache_bypassed FROM assurance_transfer_readbacks WHERE tenant_id=? AND transfer_id=? ORDER BY observed_at DESC,readback_id DESC LIMIT 1`, tenant, id).Scan(&persisted, &cacheBypassed)
		if err != nil || cacheBypassed != 1 || persisted != string(canonicalIntended) {
			return errors.New("transfer: confirmed_applied requires persisted matching uncached read-back")
		}
		if !readbackEqual || callerValue != string(canonicalIntended) {
			return errors.New("transfer: reconciliation caller value does not match persisted evidence")
		}
		next = StateMachineVerified
		machine = "api_verified"
		prov = "reconciliation"
	case "confirmed_not_applied":
		// A caller assertion or an unequal target read-back is not authoritative
		// proof after an unknown submission. Require a persisted terminal
		// provider failure observation; the read-back is supporting evidence only.
		var event string
		if err = tx.QueryRowContext(ctx, `SELECT event_json FROM assurance_transfer_poll_events WHERE tenant_id=? AND transfer_id=? ORDER BY observed_at DESC,event_id DESC LIMIT 1`, tenant, id).Scan(&event); err != nil {
			return errors.New("transfer: confirmed_not_applied requires authoritative failed operation evidence")
		}
		var operation struct {
			Status string `json:"status"`
		}
		if json.Unmarshal([]byte(event), &operation) != nil || (operation.Status != "failed" && operation.Status != "rejected" && operation.Status != "cancelled" && operation.Status != "canceled") {
			return errors.New("transfer: confirmed_not_applied requires authoritative failed operation evidence")
		}
		next = StateReconciledNotApplied
		machine = "not_applicable"
		prov = "reconciliation"
	case "conflicting_value", "still_unknown", "provider_evidence_inconsistent":
		// These dispositions intentionally remain open. They record the
		// operator/provider evidence but do not require a matching persisted
		// read-back and never close the transfer.
		next = StateReconciliationRequired
		machine = "write_outcome_unknown"
		prov = "reconciliation"
	default:
		return errors.New("transfer: invalid reconciliation classification")
	}
	res, err := tx.ExecContext(ctx, `UPDATE transfer_intents SET state=?,machine_outcome=?,machine_outcome_provenance=?,row_version=row_version+1 WHERE tenant_id=? AND transfer_id=? AND state='reconciliation_required'`, next, machine, prov, tenant, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("transfer: reconciliation requires reconciliation_required state")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO transfer_reconciliations(tenant_id,transfer_id,reason,classification,evidence_ref,disposition,created_at) VALUES(?,?,?,?,?,?,?)`, tenant, id, "operator_reconciliation", classification, readbackValue, string(next), now.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordReconciliation(ctx context.Context, tenant, id, reason, class, evidence string) error {
	_, e := s.db.ExecContext(ctx, `INSERT INTO transfer_reconciliations VALUES(?,?,?,?,?,'open',?)`, tenant, id, reason, class, evidence, time.Now().UTC().Format(time.RFC3339Nano))
	return e
}
func (s *Store) EvidenceCounts(ctx context.Context, tenant, id string) (int, int, error) {
	var a, r int
	e := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM transfer_visual_acknowledgements WHERE tenant_id=? AND transfer_id=?),(SELECT count(*) FROM transfer_reconciliations WHERE tenant_id=? AND transfer_id=?)`, tenant, id, tenant, id).Scan(&a, &r)
	return a, r, e
}

type Fence interface {
	CreateClaim(context.Context, string, []byte) (string, error)
	VerifyClaim(context.Context, string, string, ...[]byte) error
	CreateTerminal(context.Context, string, []byte) (string, error)
	VerifyTerminal(context.Context, string, string, ...[]byte) error
	Identity(string) (string, string, error)
	Durable() bool
}

// CommittedClaim reads the claim's database version and timestamp from the
// claim transaction, before any lease renewal can increment row_version.
func (s *Store) CommittedClaim(ctx context.Context, tenant, id, actor, lease string) (int64, string, error) {
	var version int64
	var claimTime string
	err := s.db.QueryRowContext(ctx, `SELECT i.row_version,e.created_at FROM transfer_intents i JOIN transfer_audit_events e ON e.tenant_id=i.tenant_id AND e.transfer_id=i.transfer_id AND e.event_id='claim:'||i.transfer_id WHERE i.tenant_id=? AND i.transfer_id=? AND i.actor_id=? AND i.lease_id=? AND i.state='claimed'`, tenant, id, actor, lease).Scan(&version, &claimTime)
	if err != nil || version < 1 || claimTime == "" {
		return 0, "", errors.Join(errors.New("transfer: committed claim evidence unavailable"), err)
	}
	return version, claimTime, nil
}

// FakeFence is an in-memory, non-durable test double. Production uses AzureBlobFence.
type FakeFence struct {
	mu                        sync.Mutex
	Claims, Terminals         map[string]string
	claimBytes, terminalBytes map[string][]byte
	Fail                      string
}

func (f *FakeFence) Durable() bool { return false }
func (f *FakeFence) Identity(tenant string) (string, string, error) {
	if tenant == "" {
		return "", "", errors.New("transfer: missing tenant")
	}
	return "test-only-environment", digest(tenant), nil
}

func (f *FakeFence) create(ctx context.Context, m map[string]string, id string, b []byte, which string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Fail == which {
		return "", errors.New(which + " fence unavailable")
	}
	d := digest(string(b))
	if old := m[id]; old != "" {
		if old != d {
			return "", errors.New("existing fence binding mismatch")
		}
		return old, nil
	}
	m[id] = d
	if which == "claim" {
		if f.claimBytes == nil {
			f.claimBytes = make(map[string][]byte)
		}
		f.claimBytes[id] = append([]byte(nil), b...)
	} else {
		if f.terminalBytes == nil {
			f.terminalBytes = make(map[string][]byte)
		}
		f.terminalBytes[id] = append([]byte(nil), b...)
	}
	return d, nil
}
func (f *FakeFence) CreateClaim(c context.Context, id string, b []byte) (string, error) {
	if f.Claims == nil {
		f.Claims = map[string]string{}
	}
	return f.create(c, f.Claims, id, b, "claim")
}
func (f *FakeFence) VerifyClaim(_ context.Context, id, d string, expected ...[]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Claims[id] != d || d == "" || (len(expected) > 0 && (len(expected) != 1 || string(f.claimBytes[id]) != string(expected[0]) || digest(string(expected[0])) != d)) {
		return errors.New("claim fence mismatch")
	}
	return nil
}
func (f *FakeFence) CreateTerminal(c context.Context, id string, b []byte) (string, error) {
	if f.Terminals == nil {
		f.Terminals = map[string]string{}
	}
	return f.create(c, f.Terminals, id, b, "terminal")
}
func (f *FakeFence) VerifyTerminal(_ context.Context, id, d string, expected ...[]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Terminals[id] != d || d == "" || (len(expected) > 0 && (len(expected) != 1 || string(f.terminalBytes[id]) != string(expected[0]) || digest(string(expected[0])) != d)) {
		return errors.New("terminal fence mismatch")
	}
	return nil
}

func (s *Store) markUnknown(ctx context.Context, tenant, id, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE transfer_intents SET state='reconciliation_required',machine_outcome='write_outcome_unknown',recovery_reason=?,lease_id='',lease_expires_at='',submissions=CASE WHEN state='submitting' AND submissions=0 THEN 1 ELSE submissions END,row_version=row_version+1 WHERE tenant_id=? AND transfer_id=? AND state IN ('claimed','fenced','submitting')`, reason, tenant, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	now := time.Now().UTC()
	if _, err = tx.ExecContext(ctx, `INSERT INTO transfer_audit_events(tenant_id,transfer_id,event_id,disposition,details,created_at) VALUES(?,?,?,?,?,?)`, tenant, id, "unknown:"+id, "write_outcome_unknown", `{"mutation_resubmitted":false,"automatic_poll":false,"reconciliation_required":true}`, now.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO transfer_reconciliations(tenant_id,transfer_id,reason,classification,evidence_ref,disposition,created_at) VALUES(?,?,?,?,?,?,?)`, tenant, id, reason, "still_unknown", "", "reconciliation_required", now.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RenewLease(ctx context.Context, tenant, id, leaseID string, now time.Time) error {
	if tenant == "" || id == "" || leaseID == "" {
		return errors.New("transfer: invalid execution lease")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE transfer_intents SET lease_expires_at=?,row_version=row_version+1 WHERE tenant_id=? AND transfer_id=? AND lease_id=? AND state IN ('claimed','fenced','submitting')`, now.UTC().Add(transferExecutionLease).Format(time.RFC3339Nano), tenant, id, leaseID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("transfer: execution lease lost")
	}
	return err
}
func (s *Store) markFenced(ctx context.Context, tenant, id, fenceDigest string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE transfer_intents SET state='fenced',claim_fence_digest=?,row_version=row_version+1 WHERE tenant_id=? AND transfer_id=? AND state='claimed'`, fenceDigest, tenant, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("transfer: claim state changed")
	}
	return err
}

func (s *Store) markFencedOwned(ctx context.Context, tenant, id, leaseID, fenceDigest string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE transfer_intents SET state='fenced',claim_fence_digest=?,row_version=row_version+1 WHERE tenant_id=? AND transfer_id=? AND lease_id=? AND state='claimed'`, fenceDigest, tenant, id, leaseID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("transfer: claim state changed")
	}
	return err
}

func (s *Store) markSubmitting(ctx context.Context, tenant, id, leaseID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE transfer_intents SET state='submitting',submission_started_at=?,lease_expires_at=?,row_version=row_version+1 WHERE tenant_id=? AND transfer_id=? AND lease_id=? AND state='fenced'`, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Add(transferExecutionLease).Format(time.RFC3339Nano), tenant, id, leaseID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("transfer: fenced state changed")
	}
	return err
}
func (s *Store) persistOperation(ctx context.Context, tenant, id, reference string) error {
	if reference == "" {
		return errors.New("transfer: empty operation reference")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE transfer_intents SET state='submitting',operation_reference=?,submissions=submissions+1,submission_started_at=CASE WHEN submission_started_at='' THEN ? ELSE submission_started_at END,row_version=row_version+1 WHERE tenant_id=? AND transfer_id=? AND state IN ('fenced','submitting') AND operation_reference='' AND submissions=0`, reference, time.Now().UTC().Format(time.RFC3339Nano), tenant, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("transfer: submission already started")
	}
	return err
}
func (s *Store) markOperationCompleted(ctx context.Context, tenant, id, reference string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE transfer_intents SET operation_completed=1,row_version=row_version+1 WHERE tenant_id=? AND transfer_id=? AND state='submitting' AND operation_reference=? AND operation_completed=0`, tenant, id, reference)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("transfer: completed operation does not match persisted submission")
	}
	return err
}
