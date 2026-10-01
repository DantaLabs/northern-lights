package assurance

import (
	"database/sql"
	"testing"
	"time"
)

func TestFailReservationRollsBackWhenRichAuditAppendFails(t *testing.T) {
	store, db := openTestStore(t)
	request := ReservationRequest{ActorID: "trusted-actor", Tool: "tool", Action: "action", IdempotencyDigest: HashBytes([]byte("key")), RequestDigest: HashBytes([]byte("request"))}
	reservation, err := store.Reserve(assuranceContext(), request, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	err = store.FailReservation(assuranceContext(), reservation.RecordID, reservation.OwnerNonce, StructuredError{Code: "forced", Message: "forced", NLAuditID: "audit"}, time.Now().UTC())
	if err == nil {
		t.Fatal("FailReservation succeeded without an audit append")
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=?`, testTenant, reservation.RecordID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(ReservationStateReserved) {
		t.Fatalf("state=%q, want rollback to reserved", state)
	}
	var auditCount int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='audit_log'`).Scan(&auditCount); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
}

func TestValidationFinalizerRequiresRichAudit(t *testing.T) {
	store, _ := openTestStore(t)
	store.auditLog = nil
	request := ReservationRequest{ActorID: "trusted-actor", Tool: "tool", Action: "action", IdempotencyDigest: HashBytes([]byte("key-finalizer")), RequestDigest: HashBytes([]byte("request-finalizer"))}
	reservation, err := store.Reserve(assuranceContext(), request, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	response := ValidationResponse{NLAuditID: "audit", Status: ValidationPassed, ValidationRunID: "run", SnapshotID: "snapshot", RuleSetID: "rules", RuleSetRevision: 1, Counts: map[string]int{"pass": 0, "fail": 0, "warn": 0, "not_evaluable": 0, "error": 0}, Results: []ValidationResult{}}
	if err := store.finalizeValidation(assuranceContext(), reservation, response, digestHex(request.IdempotencyDigest), false, nil, "", time.Now().UTC()); err == nil {
		t.Fatal("finalizer succeeded without rich audit")
	}
}
