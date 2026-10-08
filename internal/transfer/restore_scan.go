package transfer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
)

// FenceScanRequest is the immutable scope passed to a durable fence inventory.
// The inventory must return only objects at or before HighWater.CapturedAt.
type FenceScanRequest struct {
	TenantID          string
	TenantDigest      string
	EnvironmentDigest string
	HighWater         assurance.BackupFenceHighWater
}

// FenceObject is the small, provider-neutral projection needed to join a
// restored transfer database to its external create-only fences.
type FenceObject struct {
	Name              string
	ObjectName        string
	TenantDigest      string
	EnvironmentDigest string
	TransferID        string
	Kind              string
	FenceKind         string
	Digest            string
	FenceDigest       string
	Body              []byte
	LastModified      time.Time
}

// FenceInventory lists the private fence namespace through a caller-owned,
// bounded provider adapter. It has no create, update, or delete operation.
type FenceInventory interface {
	Scan(context.Context, FenceScanRequest) ([]FenceObject, error)
}

type RestoreScanOptions struct {
	TenantID          string
	EnvironmentDigest string
	HighWater         assurance.BackupFenceHighWater
}

type RestoreScanResult struct {
	Findings    int
	Quarantined int
	Ready       bool
}

type restoreFenceRecord struct {
	transferID, actorID, permission  string
	state                            State
	intent                           Intent
	idempotencyDigest, requestDigest string
	claimDigest, terminalDigest      string
	operationReference               string
	operationCompleted               bool
	rowVersion                       int64
	claimTime                        string
	readback                         string
	readbackCacheBypassed            bool
}

type restoreFenceFinding struct {
	transferID string
	reason     string
	evidence   map[string]any
}

type normalizedFenceObject struct {
	name, objectName, transferID, kind, digest string
	body                                       []byte
	validation                                 []string
}

// ScanAndJoinRestore performs the external-fence join that local database
// restore verification cannot prove. Every finding is persisted before the
// caller may treat the restored process as ready.
func ScanAndJoinRestore(ctx context.Context, store *Store, assuranceStore *assurance.Store, auditLog *audit.Log, inventory FenceInventory, opts RestoreScanOptions) (RestoreScanResult, error) {
	result := RestoreScanResult{}
	if ctx == nil {
		return result, errors.New("transfer: restore scan context is required")
	}
	if store == nil || store.db == nil || assuranceStore == nil || auditLog == nil || inventory == nil {
		return blockRestoreScan(assuranceStore, errors.New("transfer: restore scan dependencies are required"))
	}
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok || principal.TenantID == "" || principal.TenantID != opts.TenantID {
		return blockRestoreScan(assuranceStore, errors.New("transfer: restore scan requires the expected tenant principal"))
	}
	if opts.EnvironmentDigest == "" || opts.HighWater.EnvironmentDigest != opts.EnvironmentDigest || opts.HighWater.TenantDigest != digest(opts.TenantID) || opts.HighWater.Authority != "azure_blob_last_modified" {
		return blockRestoreScan(assuranceStore, errors.New("transfer: restore scan high-water binding is invalid"))
	}
	highWater, err := time.Parse(time.RFC3339Nano, opts.HighWater.CapturedAt)
	if err != nil {
		return blockRestoreScan(assuranceStore, fmt.Errorf("transfer: restore scan high-water time: %w", err))
	}
	if !auditLog.SharesDB(store.db) || assuranceStore.DB() != store.db {
		return blockRestoreScan(assuranceStore, errors.New("transfer: restore scan requires shared database and audit log"))
	}

	objects, err := inventory.Scan(ctx, FenceScanRequest{
		TenantID: opts.TenantID, TenantDigest: opts.HighWater.TenantDigest,
		EnvironmentDigest: opts.EnvironmentDigest, HighWater: opts.HighWater,
	})
	if err != nil {
		return blockRestoreScan(assuranceStore, fmt.Errorf("transfer: scan external fences: %w", err))
	}
	joined, err := normalizeRestoreFenceObjects(objects, opts, highWater)
	if err != nil {
		return blockRestoreScan(assuranceStore, err)
	}

	rows, err := store.db.QueryContext(ctx, `SELECT transfer_id,state,actor_id,permission,intent_json,idempotency_digest,request_digest,claim_fence_digest,terminal_fence_digest,operation_reference,operation_completed,row_version FROM transfer_intents WHERE tenant_id=?`, opts.TenantID)
	if err != nil {
		return blockRestoreScan(assuranceStore, fmt.Errorf("transfer: scan restored transfers: %w", err))
	}
	local := make(map[string]restoreFenceRecord)
	findings := make(map[string]restoreFenceFinding)
	addFinding := func(id, reason string, evidence map[string]any) {
		if id == "" {
			return
		}
		if prior, found := findings[id]; found {
			if prior.reason != reason && !strings.Contains(";"+prior.reason+";", ";"+reason+";") {
				prior.reason += ";" + reason
			}
			if prior.evidence == nil {
				prior.evidence = evidence
			}
			findings[id] = prior
			return
		}
		findings[id] = restoreFenceFinding{transferID: id, reason: reason, evidence: evidence}
	}
	for rows.Next() {
		var record restoreFenceRecord
		var intentJSON string
		var operationCompleted int
		if err := rows.Scan(&record.transferID, &record.state, &record.actorID, &record.permission, &intentJSON, &record.idempotencyDigest, &record.requestDigest, &record.claimDigest, &record.terminalDigest, &record.operationReference, &operationCompleted, &record.rowVersion); err != nil {
			_ = rows.Close()
			return blockRestoreScan(assuranceStore, fmt.Errorf("transfer: scan restored transfer row: %w", err))
		}
		record.operationCompleted = operationCompleted == 1
		if err := json.Unmarshal([]byte(intentJSON), &record.intent); err != nil {
			addFinding(record.transferID, "invalid_restored_intent", map[string]any{"error": err.Error()})
		}
		local[record.transferID] = record
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return blockRestoreScan(assuranceStore, fmt.Errorf("transfer: scan restored transfers: %w", err))
	}
	if err := rows.Close(); err != nil {
		return blockRestoreScan(assuranceStore, fmt.Errorf("transfer: close restored transfer scan: %w", err))
	}

	for id, record := range local {
		if err := store.db.QueryRowContext(ctx, `SELECT created_at FROM transfer_audit_events WHERE tenant_id=? AND transfer_id=? AND event_id=?`, opts.TenantID, id, "claim:"+id).Scan(&record.claimTime); err != nil && err != sql.ErrNoRows {
			return blockRestoreScan(assuranceStore, fmt.Errorf("transfer: scan claim evidence for %s: %w", id, err))
		}
		var cache int
		readbackErr := store.db.QueryRowContext(ctx, `SELECT typed_value_json,cache_bypassed FROM assurance_transfer_readbacks WHERE tenant_id=? AND transfer_id=? ORDER BY rowid DESC LIMIT 1`, opts.TenantID, id).Scan(&record.readback, &cache)
		if readbackErr == nil {
			record.readbackCacheBypassed = cache == 1
		} else if readbackErr != sql.ErrNoRows {
			return blockRestoreScan(assuranceStore, fmt.Errorf("transfer: scan read-back evidence for %s: %w", id, readbackErr))
		}
		local[id] = record
	}

	for _, object := range joined {
		if len(object.validation) != 0 {
			addFinding(object.transferID, "invalid_external_fence:"+strings.Join(object.validation, ","), map[string]any{"kind": object.kind, "object": object.name, "digest": object.digest})
		}
	}

	localIDs := make([]string, 0, len(local))
	for id := range local {
		localIDs = append(localIDs, id)
	}
	sort.Strings(localIDs)
	for _, id := range localIDs {
		record := local[id]
		claim, claimFound := joined[id+"\x00claim"]
		terminal, terminalFound := joined[id+"\x00terminal"]
		if record.state == StateStaged {
			if record.claimDigest != "" || record.terminalDigest != "" || claimFound || terminalFound {
				addFinding(id, "staged_row_has_restore_fence", map[string]any{"state": record.state, "claim": claimFound, "terminal": terminalFound})
			}
			continue
		}
		if record.claimDigest == "" || !claimFound || claim.digest != record.claimDigest {
			addFinding(id, "missing_or_mismatched_claim_fence", map[string]any{"state": record.state, "claim_fence_digest": record.claimDigest, "external_claim": claimFound})
		} else {
			reservation, err := loadConfirmReservationEvidence(ctx, store.db, opts.TenantID, id)
			if err != nil {
				return blockRestoreScan(assuranceStore, fmt.Errorf("transfer: scan confirm reservation for %s: %w", id, err))
			}
			if reason := validateRestoredClaim(record, claim, opts, reservation); reason != "" {
				addFinding(id, reason, map[string]any{"state": record.state, "claim_fence_digest": record.claimDigest, "object": claim.name})
			}
		}

		if record.state == StateClaimed || record.state == State("fenced") || record.state == State("submitting") {
			addFinding(id, "provider_outcome_unknown", map[string]any{"state": record.state, "claim": claimFound, "operation_reference": record.operationReference})
		}
		if record.state == StateReconciliationRequired && claimFound && !terminalFound {
			addFinding(id, "unresolved_claim_without_terminal_fence", map[string]any{"state": record.state, "claim": true, "operation_reference": record.operationReference})
		}
		if terminalFound && !isRestoreTerminalState(record.state) {
			addFinding(id, "terminal_before_local_persistence", map[string]any{"state": record.state, "terminal_digest": terminal.digest})
		}
		terminalExpected := isRestoreTerminalState(record.state) || record.terminalDigest != ""
		if terminalExpected {
			if record.terminalDigest == "" || !terminalFound || terminal.digest != record.terminalDigest {
				addFinding(id, "missing_or_mismatched_terminal_fence", map[string]any{"state": record.state, "terminal_fence_digest": record.terminalDigest, "external_terminal": terminalFound})
			} else if reason := validateRestoredTerminal(record, claim, terminal, opts); reason != "" {
				addFinding(id, reason, map[string]any{"state": record.state, "terminal_fence_digest": record.terminalDigest, "object": terminal.name})
			}
		} else if terminalFound {
			if reason := validateRestoredTerminal(record, claim, terminal, opts); reason != "" {
				addFinding(id, reason, map[string]any{"state": record.state, "object": terminal.name})
			}
			if record.operationReference == "" || !record.operationCompleted || record.readback == "" || !record.readbackCacheBypassed {
				addFinding(id, "nonterminal_row_missing_operation_or_readback", map[string]any{"state": record.state, "operation_reference": record.operationReference, "operation_completed": record.operationCompleted, "readback_present": record.readback != "", "readback_cache_bypassed": record.readbackCacheBypassed})
			}
		}
	}
	joinedKeys := make([]string, 0, len(joined))
	for key := range joined {
		joinedKeys = append(joinedKeys, key)
	}
	sort.Strings(joinedKeys)
	for _, key := range joinedKeys {
		object := joined[key]
		if _, found := local[object.transferID]; !found {
			addFinding(object.transferID, "fence_orphan", map[string]any{"kind": object.kind, "digest": object.digest, "object": object.name, "join_key": key})
		}
	}

	findingIDs := make([]string, 0, len(findings))
	for id := range findings {
		findingIDs = append(findingIDs, id)
	}
	sort.Strings(findingIDs)
	for _, id := range findingIDs {
		finding := findings[id]
		if err := persistRestoreQuarantine(ctx, store.db, auditLog, opts, principal, finding); err != nil {
			return blockRestoreScan(assuranceStore, err)
		}
	}
	result.Findings = len(findings)
	result.Quarantined = len(findings)
	var outstanding int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM assurance_transfer_startup_quarantines WHERE tenant_id=? AND environment_digest=? AND disposition='quarantined'`, opts.TenantID, opts.EnvironmentDigest).Scan(&outstanding); err != nil {
		return blockRestoreScan(assuranceStore, fmt.Errorf("transfer: check durable quarantine: %w", err))
	}
	result.Ready = result.Findings == 0 && outstanding == 0
	if !result.Ready {
		assuranceStore.BlockReadiness(fmt.Errorf("transfer: restore fence join has %d finding(s) and %d open quarantine(s)", result.Findings, outstanding))
	}
	return result, nil
}

func loadConfirmReservationEvidence(ctx context.Context, db *sql.DB, tenant, transferID string) (confirmReservationEvidence, error) {
	evidence := confirmReservationEvidence{}
	rows, err := db.QueryContext(ctx, `SELECT actor_id,idempotency_digest,request_digest,state,response_status
	 FROM assurance_idempotency_records WHERE tenant_id=? AND entity_reference=? AND tool='workiva_transfer_value' AND action='confirm'`, tenant, transferID)
	if err != nil {
		return evidence, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var current confirmReservationEvidence
		if err := rows.Scan(&current.actorID, &current.idempotencyDigest, &current.requestDigest, &current.state, &current.responseStatus); err != nil {
			return evidence, err
		}
		evidence.count++
		if evidence.count == 1 {
			evidence.actorID = current.actorID
			evidence.idempotencyDigest = current.idempotencyDigest
			evidence.requestDigest = current.requestDigest
			evidence.state = current.state
			evidence.responseStatus = current.responseStatus
		}
	}
	if err := rows.Err(); err != nil {
		return evidence, err
	}
	return evidence, rows.Close()
}

func normalizeRestoreFenceObjects(objects []FenceObject, opts RestoreScanOptions, highWater time.Time) (map[string]normalizedFenceObject, error) {
	joined := make(map[string]normalizedFenceObject, len(objects))
	for _, object := range objects {
		name := object.Name
		if name == "" {
			name = object.ObjectName
		}
		id := object.TransferID
		kind := strings.ToLower(object.Kind)
		if kind == "" {
			kind = strings.ToLower(object.FenceKind)
		}
		if (id == "" || kind == "") && name != "" {
			parsedID, parsedKind := parseFenceObjectName(name)
			if id == "" {
				id = parsedID
			}
			if kind == "" {
				kind = parsedKind
			}
		}
		d := object.Digest
		if d == "" {
			d = object.FenceDigest
		}
		if id == "" || !validAzureFenceSegment(id) || (kind != "claim" && kind != "terminal") || d == "" {
			return nil, fmt.Errorf("transfer: invalid external fence object %q", name)
		}
		wantName := "env/" + opts.EnvironmentDigest + "/tenant/" + opts.HighWater.TenantDigest + "/fences/" + id + "/" + kind + ".json"
		if name != wantName || object.ObjectName != "" && object.ObjectName != name || object.TransferID != "" && object.TransferID != id || object.Kind != "" && object.Kind != kind || object.FenceDigest != "" && object.FenceDigest != d {
			return nil, fmt.Errorf("transfer: external fence path or identity mismatch for %q", name)
		}
		if object.TenantDigest != opts.HighWater.TenantDigest {
			return nil, fmt.Errorf("transfer: external fence tenant binding mismatch for %s", id)
		}
		if object.EnvironmentDigest != opts.EnvironmentDigest {
			return nil, fmt.Errorf("transfer: external fence environment binding mismatch for %s", id)
		}
		if object.LastModified.IsZero() || object.LastModified.After(highWater) {
			return nil, fmt.Errorf("transfer: external fence %s has invalid restore high-water", id)
		}
		if len(object.Body) == 0 || !bytes.Equal([]byte(d), []byte(sha256Hex(object.Body))) || canonicalFenceBody(object.Body, kind, id, opts.EnvironmentDigest, opts.HighWater.TenantDigest) != nil {
			return nil, fmt.Errorf("transfer: external fence %s has unverified bytes or digest", id)
		}
		key := id + "\x00" + kind
		if _, found := joined[key]; found {
			return nil, fmt.Errorf("transfer: duplicate external fence for %s", id)
		}
		joined[key] = normalizedFenceObject{name: name, objectName: name, transferID: id, kind: kind, digest: d, body: append([]byte(nil), object.Body...)}
	}
	return joined, nil
}

func parseFenceObjectName(name string) (string, string) {
	name = strings.TrimSuffix(name, ".json")
	parts := strings.Split(strings.Trim(name, "/"), "/")
	if len(parts) < 2 {
		return "", ""
	}
	kind := parts[len(parts)-1]
	if kind != "claim" && kind != "terminal" {
		return "", ""
	}
	return parts[len(parts)-2], kind
}

func persistRestoreQuarantine(ctx context.Context, db *sql.DB, auditLog *audit.Log, opts RestoreScanOptions, principal identity.Principal, finding restoreFenceFinding) error {
	evidence := map[string]any{"tenant_id": opts.TenantID, "environment_digest": opts.EnvironmentDigest, "transfer_id": finding.transferID, "reason": finding.reason, "evidence": finding.evidence}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("transfer: encode startup quarantine: %w", err)
	}
	auditID := "startup-quarantine:" + opts.EnvironmentDigest + ":" + finding.transferID
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("transfer: begin startup quarantine: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var existing int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM assurance_transfer_startup_quarantines WHERE tenant_id=? AND environment_digest=? AND transfer_id=?`, opts.TenantID, opts.EnvironmentDigest, finding.transferID).Scan(&existing)
	if err == nil {
		return tx.Commit()
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("transfer: check startup quarantine: %w", err)
	}

	_, err = tx.ExecContext(ctx, `UPDATE transfer_intents SET state='reconciliation_required',machine_outcome='write_outcome_unknown',machine_outcome_provenance='restore_fence_join',recovery_reason=?,lease_id='',lease_expires_at='',row_version=row_version+1 WHERE tenant_id=? AND transfer_id=? AND state!='reconciliation_required'`, finding.reason, opts.TenantID, finding.transferID)
	if err != nil {
		return fmt.Errorf("transfer: quarantine restored transfer: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO transfer_reconciliations(tenant_id,transfer_id,reason,classification,evidence_ref,disposition,created_at) VALUES(?,?,?,?,?,?,?)`, opts.TenantID, finding.transferID, finding.reason, "still_unknown", auditID, "reconciliation_required", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("transfer: record restore reconciliation: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO transfer_audit_events(tenant_id,transfer_id,event_id,disposition,details,created_at) VALUES(?,?,?,?,?,?)`, opts.TenantID, finding.transferID, auditID, "startup_quarantine", string(evidenceJSON), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("transfer: record transfer quarantine: %w", err)
	}
	if _, err = auditLog.AppendTx(identity.ContextWithPrincipal(ctx, principal), tx, audit.Entry{Actor: principal.AuditActor(), Tool: "transfer_restore", Action: "startup_quarantine", Target: finding.transferID, AfterJSON: string(evidenceJSON), AuditID: auditID}); err != nil {
		return fmt.Errorf("transfer: append startup quarantine audit: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO assurance_transfer_startup_quarantines(tenant_id,environment_digest,transfer_id,disposition,reason,evidence_json,audit_id,created_at) VALUES(?,?,?,?,?,?,?,?)`, opts.TenantID, opts.EnvironmentDigest, finding.transferID, "quarantined", finding.reason, string(evidenceJSON), auditID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("transfer: persist startup quarantine: %w", err)
	}
	return tx.Commit()
}

func blockRestoreScan(store *assurance.Store, err error) (RestoreScanResult, error) {
	if store != nil {
		store.BlockReadiness(err)
	}
	return RestoreScanResult{}, err
}
