package assurance

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExportEvidenceSealedReplaySkipsStorageAndSubjectReads(t *testing.T) {
	store, _ := openTestStore(t)
	store.setReadiness(false, nil)
	store.SetAuditLog(nil)
	ctx := assuranceContext()
	request := EvidenceRequest{SubjectKind: "snapshot", SubjectID: "sealed-subject", Format: "json", RedactionProfile: RedactionStandard, IncludeAuditChain: false, RetentionClass: "long_term", IdempotencyKey: "replay-key"}
	canonical, err := CanonicalJSON(struct {
		SubjectKind  string           `json:"subject_kind"`
		SubjectID    string           `json:"subject_id"`
		Format       string           `json:"format"`
		Profile      RedactionProfile `json:"redaction_profile"`
		IncludeAudit bool             `json:"include_audit_chain"`
		Retention    string           `json:"retention_class"`
	}{request.SubjectKind, request.SubjectID, request.Format, request.RedactionProfile, request.IncludeAuditChain, request.RetentionClass})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := store.Reserve(ctx, ReservationRequest{ActorID: testTenant + "/actor-1", Tool: "workiva_export_evidence", Action: "export", IdempotencyDigest: DigestIdempotencyKey(request.IdempotencyKey), RequestDigest: HashBytes(canonical), RetentionClass: request.RetentionClass}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	envelope := []byte(`{"nl_audit_id":"audit","status":"completed","evidence_manifest_id":"manifest","manifest_version":2,"artifacts":[],"package_hash":"hash","audit":{"integrity":{"chain_verified":false,"hash_version_coverage":[],"tenant_identity_hashed":true},"checkpoint":{"status":"not_requested"},"completeness":{"status":"unknown","expected_count":0,"included_count":0,"omitted_count":0,"omissions_recorded":true,"terminal_anchor":"unknown","terminal_anchor_verified":false,"final_row_deletion_detectable":false},"caveats":["audit chain not requested"]},"expires_at":"2026-01-01T00:00:00Z"}`)
	if err := store.SealReservation(ctx, reservation.RecordID, reservation.OwnerNonce, envelope, EvidenceCompleted, "manifest", "audit", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got, err := store.ExportEvidence(ctx, "actor-1", "audit", request)
	if err != nil {
		t.Fatalf("sealed replay failed without storage: %v", err)
	}
	if got.Status != EvidenceIdempotencyReplay || got.EvidenceManifestID != "manifest" {
		t.Fatalf("replay=%#v", got)
	}
}

func TestFailedEvidenceReservationReplaysItsTypedFailureBeforeDependencies(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := assuranceContext()
	request := EvidenceRequest{SubjectKind: "snapshot", SubjectID: "subject", Format: "json", RedactionProfile: RedactionStandard, RetentionClass: "long_term", IdempotencyKey: "failed-replay"}
	canonical, err := CanonicalJSON(struct {
		SubjectKind  string           `json:"subject_kind"`
		SubjectID    string           `json:"subject_id"`
		Format       string           `json:"format"`
		Profile      RedactionProfile `json:"redaction_profile"`
		IncludeAudit bool             `json:"include_audit_chain"`
		Retention    string           `json:"retention_class"`
	}{request.SubjectKind, request.SubjectID, request.Format, request.RedactionProfile, false, request.RetentionClass})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := store.Reserve(ctx, ReservationRequest{ActorID: testTenant + "/actor-1", Tool: "workiva_export_evidence", Action: "export", IdempotencyDigest: DigestIdempotencyKey(request.IdempotencyKey), RequestDigest: HashBytes(canonical), RetentionClass: request.RetentionClass}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, StructuredError{Code: "subject_not_found", Message: "missing export subject"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	store.setReadiness(false, nil)
	store.SetAuditLog(nil)
	_, err = store.ExportEvidence(ctx, "untrusted-actor", "audit", request)
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "subject_not_found" {
		t.Fatalf("failed replay error=%v, want typed subject_not_found", err)
	}
}

func TestExportPrerequisiteMissDoesNotReserveNewIdempotencyKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Store)
		want string
	}{
		{name: "readiness unavailable", set: func(s *Store) { s.setReadiness(false, nil) }, want: "not ready"},
		{name: "rich audit unavailable", set: func(s *Store) { s.setReadiness(true, nil); s.SetAuditLog(nil) }, want: "audit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, db := openTestStore(t)
			tc.set(store)
			_, err := store.ExportEvidence(assuranceContext(), "caller", "audit", EvidenceRequest{SubjectKind: "snapshot", SubjectID: "missing", Format: "json", RedactionProfile: RedactionStandard, RetentionClass: "long_term", IdempotencyKey: "must-not-be-reserved"}, NewMemoryEvidenceStorage())
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("err=%v, want substring %q", err, tc.want)
			}
			var count int
			if queryErr := db.QueryRow(`SELECT count(*) FROM assurance_idempotency_records WHERE tenant_id=? AND tool='workiva_export_evidence' AND idempotency_digest=?`, testTenant, digestHex(DigestIdempotencyKey("must-not-be-reserved"))).Scan(&count); queryErr != nil {
				t.Fatal(queryErr)
			}
			if count != 0 {
				t.Fatalf("preflight failure reserved idempotency key; rows=%d", count)
			}
		})
	}
}

func TestExportEvidenceUnsupportedTransferReplayIsClassifiedBeforeWave3Rejection(t *testing.T) {
	store, _ := openTestStore(t)
	store.setReadiness(false, nil)
	store.SetAuditLog(nil)
	ctx := assuranceContext()
	request := EvidenceRequest{SubjectKind: "transfer", SubjectID: "transfer-1", Format: "json", RedactionProfile: RedactionStandard, RetentionClass: "long_term", IdempotencyKey: "transfer-replay-key"}
	canonical, err := CanonicalJSON(struct {
		SubjectKind  string           `json:"subject_kind"`
		SubjectID    string           `json:"subject_id"`
		Format       string           `json:"format"`
		Profile      RedactionProfile `json:"redaction_profile"`
		IncludeAudit bool             `json:"include_audit_chain"`
		Retention    string           `json:"retention_class"`
	}{request.SubjectKind, request.SubjectID, request.Format, request.RedactionProfile, false, request.RetentionClass})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := store.Reserve(ctx, ReservationRequest{ActorID: testTenant + "/actor-1", Tool: "workiva_export_evidence", Action: "export", IdempotencyDigest: DigestIdempotencyKey(request.IdempotencyKey), RequestDigest: HashBytes(canonical), RetentionClass: request.RetentionClass}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SealReservation(ctx, reservation.RecordID, reservation.OwnerNonce, []byte(`{"status":"too_large"}`), EvidenceTooLarge, "transfer-1", "audit", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got, err := store.ExportEvidence(ctx, "actor-1", "audit", request)
	if err != nil {
		t.Fatalf("transfer replay failed before classification: %v", err)
	}
	if got.Status != EvidenceIdempotencyReplay {
		t.Fatalf("status=%q, want idempotency_replay", got.Status)
	}
}
