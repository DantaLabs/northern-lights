package transfer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	_ "modernc.org/sqlite"
)

type restoreFenceInventory struct {
	objects []FenceObject
	err     error
	scans   int
}

func restoreScanDigest(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

func (f *restoreFenceInventory) Scan(context.Context, FenceScanRequest) ([]FenceObject, error) {
	f.scans++
	return f.objects, f.err
}

func TestRestoreJoinMissingClaimQuarantinesBeforeReadiness(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	store, err := NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	assuranceStore, err := assurance.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	auditLog, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	intent := testIntent()
	claimTime := time.Date(2026, 10, 6, 1, 2, 3, 0, time.UTC)
	if _, err := store.Stage(context.Background(), intent, "token", "idem", "request", claimTime); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.Claim(context.Background(), Claim{
		TenantID: intent.TenantID, ActorID: intent.ActorID, Permission: intent.Permission,
		TransferID: intent.ID, Token: "token", LeaseID: "lease", Now: claimTime,
		LeaseUntil: claimTime.Add(time.Minute),
	}); err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}

	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: intent.TenantID})
	result, err := ScanAndJoinRestore(ctx, store, assuranceStore, auditLog, &restoreFenceInventory{}, RestoreScanOptions{
		TenantID: intent.TenantID, EnvironmentDigest: "env-a",
		HighWater: assurance.BackupFenceHighWater{
			TenantDigest: digest(intent.TenantID), EnvironmentDigest: "env-a",
			CapturedAt: claimTime.Format(time.RFC3339Nano), Authority: "azure_blob_last_modified",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Findings != 1 {
		t.Fatalf("findings=%d, want 1", result.Findings)
	}
	got, err := store.Get(context.Background(), intent.TenantID, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateReconciliationRequired {
		t.Fatalf("state=%q, want reconciliation_required", got.State)
	}
	if err := db.QueryRow(`SELECT count(*) FROM assurance_transfer_startup_quarantines WHERE tenant_id=? AND transfer_id=?`, intent.TenantID, intent.ID).Scan(&result.Findings); err != nil {
		t.Fatal(err)
	}
	if result.Findings != 1 {
		t.Fatalf("durable quarantine rows=%d, want 1", result.Findings)
	}
	if err := db.QueryRow(`SELECT count(*) FROM audit_log WHERE tenant_id=? AND action='startup_quarantine'`, intent.TenantID).Scan(&result.Findings); err != nil {
		t.Fatal(err)
	}
	if result.Findings != 1 {
		t.Fatalf("audit rows=%d, want 1", result.Findings)
	}
}

func restoreScanObjectPath(tenant, env, id, kind string) string {
	return "env/" + env + "/tenant/" + digest(tenant) + "/fences/" + id + "/" + kind + ".json"
}

func newRestoreScanFixture(t *testing.T, state State, terminal bool) (*Store, *assurance.Store, *audit.Log, Intent, time.Time, *restoreFenceInventory, context.Context, RestoreScanOptions) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	assuranceStore, err := assurance.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	auditLog, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	intent := testIntent()
	claimTime := time.Date(2026, 10, 6, 1, 2, 3, 0, time.UTC)
	if _, err := store.Stage(context.Background(), intent, "token", "idem", "request", claimTime); err != nil {
		t.Fatal(err)
	}
	if state != StateStaged {
		ok, err := store.Claim(context.Background(), Claim{TenantID: intent.TenantID, ActorID: intent.ActorID, Permission: intent.Permission, TransferID: intent.ID, Token: "token", LeaseID: "lease", Now: claimTime, LeaseUntil: claimTime.Add(time.Hour)})
		if err != nil || !ok {
			t.Fatalf("claim ok=%v err=%v", ok, err)
		}
	}
	tenantDigest := digest(intent.TenantID)
	claimBody, err := json.Marshal(claimPayload{
		EnvironmentDigest: "env-a", TenantDigest: tenantDigest, TransferID: intent.ID,
		IdempotencyDigest: "confirm-idempotency", RequestDigest: "confirm-request", ActorDigest: digest(intent.ActorID),
		SourceHash: intent.Source.Fingerprint, TargetHash: intent.Target.Fingerprint,
		IntendedHash: digest(intent.Intended), PolicyHash: intent.PolicyHash, MappingHash: intent.MappingHash,
		ClaimState: "claimed", ClaimTime: claimTime.Format(time.RFC3339Nano), ClaimRowVersion: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimDigest := restoreScanDigest(claimBody)
	objects := []FenceObject{{Name: restoreScanObjectPath(intent.TenantID, "env-a", intent.ID, "claim"), ObjectName: restoreScanObjectPath(intent.TenantID, "env-a", intent.ID, "claim"), TenantDigest: tenantDigest, EnvironmentDigest: "env-a", TransferID: intent.ID, Kind: "claim", FenceKind: "claim", Digest: claimDigest, FenceDigest: claimDigest, Body: claimBody, LastModified: claimTime}}
	terminalDigest := ""
	if terminal {
		terminalBody, err := json.Marshal(terminalPayload{ClaimDigest: claimDigest, EnvironmentDigest: "env-a", TenantDigest: tenantDigest, TransferID: intent.ID, ProviderOutcomeDigest: digest("accepted"), OperationReferenceDigest: digest("op-1"), ReadbackDigest: digest(intent.Intended), TerminalKind: "accepted", Reason: "api_verified", TerminalTime: claimTime.Add(time.Minute).Format(time.RFC3339Nano)})
		if err != nil {
			t.Fatal(err)
		}
		terminalDigest = restoreScanDigest(terminalBody)
		objects = append(objects, FenceObject{Name: restoreScanObjectPath(intent.TenantID, "env-a", intent.ID, "terminal"), ObjectName: restoreScanObjectPath(intent.TenantID, "env-a", intent.ID, "terminal"), TenantDigest: tenantDigest, EnvironmentDigest: "env-a", TransferID: intent.ID, Kind: "terminal", FenceKind: "terminal", Digest: terminalDigest, FenceDigest: terminalDigest, Body: terminalBody, LastModified: claimTime.Add(time.Minute)})
	}
	if state != StateStaged {
		operation, completed, outcome, provenance := "", 0, "write_outcome_unknown", "restore_fence_join"
		if terminal && state == StateMachineVerified {
			operation, completed, outcome, provenance = "op-1", 1, "api_verified", "provider_read_back"
		}
		if _, err := db.Exec(`UPDATE transfer_intents SET state=?,claim_fence_digest=?,terminal_fence_digest=?,operation_reference=?,operation_completed=?,machine_outcome=?,machine_outcome_provenance=?,row_version=row_version+1 WHERE tenant_id=? AND transfer_id=?`, state, claimDigest, terminalDigest, operation, completed, outcome, provenance, intent.TenantID, intent.ID); err != nil {
			t.Fatal(err)
		}
	}
	if terminal && state == StateMachineVerified {
		if _, err := db.Exec(`INSERT INTO assurance_transfer_readbacks(tenant_id,transfer_id,readback_id,typed_value_json,cache_bypassed,observed_at) VALUES(?,?,?,?,1,?)`, intent.TenantID, intent.ID, "rb-1", intent.Intended, claimTime.Add(time.Minute).Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		insertRestoreConfirmReservation(t, db, intent, "confirm-idempotency", "confirm-request", "actor-a", "sealed", "machine_verified_visual_ack_pending", intent.ID, "confirm-success")
	}
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: intent.TenantID})
	opts := RestoreScanOptions{TenantID: intent.TenantID, EnvironmentDigest: "env-a", HighWater: assurance.BackupFenceHighWater{TenantDigest: tenantDigest, EnvironmentDigest: "env-a", CapturedAt: claimTime.Add(2 * time.Minute).Format(time.RFC3339Nano), Authority: "azure_blob_last_modified"}}
	return store, assuranceStore, auditLog, intent, claimTime, &restoreFenceInventory{objects: objects}, ctx, opts
}

func insertRestoreConfirmReservation(t *testing.T, db *sql.DB, intent Intent, key, request, actor, state, status, entity, recordID string) {
	t.Helper()
	idem := stageReservationDigest(key)
	requestDigest := stageReservationDigest(request)
	if _, err := db.Exec(`INSERT INTO assurance_idempotency_records
	 (tenant_id,record_id,actor_id,tool,action,idempotency_digest,request_digest,response_envelope,response_hash,response_status,state,owner_nonce,lease_expires_at,last_heartbeat_at,execution_started,entity_reference,correlation_id,created_at,terminal_at,expires_at,retention_class)
	 VALUES(?,?,?,?,?,?,?,?,?,?,?, ?,?,?,?,?,?,?,?,?,?)`, intent.TenantID, recordID, actor, "workiva_transfer_value", "confirm", hex.EncodeToString(idem[:]), hex.EncodeToString(requestDigest[:]), "{}", digest("{}"), status, state, "owner", intent.ExpiresAt.Format(time.RFC3339Nano), intent.ExpiresAt.Format(time.RFC3339Nano), 1, entity, "correlation-"+recordID, intent.ExpiresAt.Add(-time.Hour).Format(time.RFC3339Nano), intent.ExpiresAt.Format(time.RFC3339Nano), intent.ExpiresAt.Add(time.Hour).Format(time.RFC3339Nano), "standard"); err != nil {
		t.Fatalf("insert restore confirmation reservation: %v", err)
	}
}

func TestRestoreClaimRequiresUniqueBoundSuccessfulConfirmReservation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testing.T, *sql.DB, Intent)
		want   string
	}{
		{name: "missing", mutate: func(t *testing.T, db *sql.DB, intent Intent) {
			if _, err := db.Exec(`DELETE FROM assurance_idempotency_records WHERE tenant_id=? AND entity_reference=? AND action='confirm'`, intent.TenantID, intent.ID); err != nil {
				t.Fatal(err)
			}
		}, want: "missing_confirmation_reservation"},
		{name: "ambiguous_success_from_other_actor", mutate: func(t *testing.T, db *sql.DB, intent Intent) {
			insertRestoreConfirmReservation(t, db, intent, "other-key", "other-request", "different-actor", "sealed", "machine_verified_visual_ack_pending", intent.ID, "confirm-second")
		}, want: "ambiguous_confirmation_reservation"},
		{name: "wrong_actor", mutate: func(t *testing.T, db *sql.DB, intent Intent) {
			if _, err := db.Exec(`UPDATE assurance_idempotency_records SET actor_id='different-actor' WHERE tenant_id=? AND record_id='confirm-success'`, intent.TenantID); err != nil {
				t.Fatal(err)
			}
		}, want: "confirmation_actor_mismatch"},
		{name: "wrong_key_digest", mutate: func(t *testing.T, db *sql.DB, intent Intent) {
			if _, err := db.Exec(`UPDATE assurance_idempotency_records SET idempotency_digest=? WHERE tenant_id=? AND record_id='confirm-success'`, strings.Repeat("f", 64), intent.TenantID); err != nil {
				t.Fatal(err)
			}
		}, want: "claim_confirmation_digest_mismatch"},
		{name: "wrong_request_digest", mutate: func(t *testing.T, db *sql.DB, intent Intent) {
			if _, err := db.Exec(`UPDATE assurance_idempotency_records SET request_digest=? WHERE tenant_id=? AND record_id='confirm-success'`, strings.Repeat("e", 64), intent.TenantID); err != nil {
				t.Fatal(err)
			}
		}, want: "claim_confirmation_digest_mismatch"},
		{name: "unsealed", mutate: func(t *testing.T, db *sql.DB, intent Intent) {
			if _, err := db.Exec(`UPDATE assurance_idempotency_records SET state='failed' WHERE tenant_id=? AND record_id='confirm-success'`, intent.TenantID); err != nil {
				t.Fatal(err)
			}
		}, want: "invalid_confirmation_reservation"},
		{name: "wrong_response_status", mutate: func(t *testing.T, db *sql.DB, intent Intent) {
			if _, err := db.Exec(`UPDATE assurance_idempotency_records SET response_status='failed' WHERE tenant_id=? AND record_id='confirm-success'`, intent.TenantID); err != nil {
				t.Fatal(err)
			}
		}, want: "invalid_confirmation_reservation"},
		{name: "wrong_tenant_scope", mutate: func(t *testing.T, db *sql.DB, intent Intent) {
			if _, err := db.Exec(`UPDATE assurance_idempotency_records SET tenant_id='another-tenant' WHERE tenant_id=? AND record_id='confirm-success'`, intent.TenantID); err != nil {
				t.Fatal(err)
			}
		}, want: "missing_confirmation_reservation"},
		{name: "wrong_tool_scope", mutate: func(t *testing.T, db *sql.DB, intent Intent) {
			if _, err := db.Exec(`UPDATE assurance_idempotency_records SET tool='other_tool' WHERE tenant_id=? AND record_id='confirm-success'`, intent.TenantID); err != nil {
				t.Fatal(err)
			}
		}, want: "missing_confirmation_reservation"},
		{name: "wrong_action_scope", mutate: func(t *testing.T, db *sql.DB, intent Intent) {
			if _, err := db.Exec(`UPDATE assurance_idempotency_records SET action='stage' WHERE tenant_id=? AND record_id='confirm-success'`, intent.TenantID); err != nil {
				t.Fatal(err)
			}
		}, want: "missing_confirmation_reservation"},
		{name: "wrong_entity_scope", mutate: func(t *testing.T, db *sql.DB, intent Intent) {
			if _, err := db.Exec(`UPDATE assurance_idempotency_records SET entity_reference='another-transfer' WHERE tenant_id=? AND record_id='confirm-success'`, intent.TenantID); err != nil {
				t.Fatal(err)
			}
		}, want: "missing_confirmation_reservation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, as, log, intent, _, inventory, ctx, opts := newRestoreScanFixture(t, StateMachineVerified, true)
			tc.mutate(t, store.db, intent)
			result, err := ScanAndJoinRestore(ctx, store, as, log, inventory, opts)
			if err != nil || result.Ready || result.Findings != 1 {
				t.Fatalf("bad confirmation evidence admitted: result=%+v err=%v", result, err)
			}
			var reason string
			if err := store.db.QueryRow(`SELECT reason FROM assurance_transfer_startup_quarantines WHERE tenant_id=? AND transfer_id=?`, intent.TenantID, intent.ID).Scan(&reason); err != nil || !strings.Contains(reason, tc.want) {
				t.Fatalf("quarantine reason=%q want component %q err=%v", reason, tc.want, err)
			}
		})
	}
}

func TestRestoreRejectsLegacyStructOrderedTerminalReadbackDigest(t *testing.T) {
	store, as, log, intent, _, inventory, ctx, opts := newRestoreScanFixture(t, StateMachineVerified, true)
	typed := assurance.TypedValue{Kind: assurance.ValueNumber, Number: "2", Unit: "kg", Scale: "ones", Precision: 2}
	legacyBytes, err := json.Marshal(canonicalTransferValue{Kind: typed.Kind, Number: typed.Number, Unit: typed.Unit, Scale: typed.Scale, Precision: typed.Precision, Formula: typed.Formula})
	if err != nil {
		t.Fatal(err)
	}
	canonicalReadback, err := assurance.CanonicalJSONBytes(legacyBytes)
	if err != nil {
		t.Fatal(err)
	}
	intent.Intended = string(canonicalReadback)
	intentBytes, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE transfer_intents SET intent_json=? WHERE tenant_id=? AND transfer_id=?`, string(intentBytes), intent.TenantID, intent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE assurance_transfer_readbacks SET typed_value_json=? WHERE tenant_id=? AND transfer_id=?`, string(canonicalReadback), intent.TenantID, intent.ID); err != nil {
		t.Fatal(err)
	}
	var claim claimPayload
	if err := json.Unmarshal(inventory.objects[0].Body, &claim); err != nil {
		t.Fatal(err)
	}
	claim.IntendedHash = digest(intent.Intended)
	claimBody, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	claimDigest := restoreScanDigest(claimBody)
	inventory.objects[0].Body = claimBody
	inventory.objects[0].Digest, inventory.objects[0].FenceDigest = claimDigest, claimDigest
	if _, err := store.db.Exec(`UPDATE transfer_intents SET claim_fence_digest=? WHERE tenant_id=? AND transfer_id=?`, claimDigest, intent.TenantID, intent.ID); err != nil {
		t.Fatal(err)
	}
	var terminal terminalPayload
	if err := json.Unmarshal(inventory.objects[1].Body, &terminal); err != nil {
		t.Fatal(err)
	}
	terminal.ClaimDigest = claimDigest
	terminal.ReadbackDigest = digest(string(legacyBytes))
	terminalBody, err := json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	if err := canonicalFenceBody(terminalBody, "terminal", intent.ID, opts.EnvironmentDigest, opts.HighWater.TenantDigest); err != nil {
		t.Fatalf("legacy terminal fixture is not canonical: %v", err)
	}
	terminalDigest := restoreScanDigest(terminalBody)
	inventory.objects[1].Body = terminalBody
	inventory.objects[1].Digest, inventory.objects[1].FenceDigest = terminalDigest, terminalDigest
	if _, err := store.db.Exec(`UPDATE transfer_intents SET terminal_fence_digest=? WHERE tenant_id=? AND transfer_id=?`, terminalDigest, intent.TenantID, intent.ID); err != nil {
		t.Fatal(err)
	}
	result, err := ScanAndJoinRestore(ctx, store, as, log, inventory, opts)
	if err != nil || result.Ready || result.Findings != 1 {
		t.Fatalf("legacy struct-ordered readback digest was admitted: result=%+v err=%v", result, err)
	}
	var reason string
	if err := store.db.QueryRow(`SELECT reason FROM assurance_transfer_startup_quarantines WHERE tenant_id=? AND transfer_id=?`, intent.TenantID, intent.ID).Scan(&reason); err != nil || !strings.Contains(reason, "terminal_readback_mismatch") {
		t.Fatalf("legacy readback quarantine reason=%q err=%v", reason, err)
	}
}

func TestRestoreClaimFenceFieldsRemainBoundAfterDigestJoin(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*claimPayload)
	}{
		{name: "actor", mutate: func(p *claimPayload) { p.ActorDigest = digest("other-actor") }},
		{name: "source", mutate: func(p *claimPayload) { p.SourceHash = digest("other-source") }},
		{name: "target", mutate: func(p *claimPayload) { p.TargetHash = digest("other-target") }},
		{name: "intended", mutate: func(p *claimPayload) { p.IntendedHash = digest("other-intended") }},
		{name: "policy", mutate: func(p *claimPayload) { p.PolicyHash = digest("other-policy") }},
		{name: "mapping", mutate: func(p *claimPayload) { p.MappingHash = digest("other-mapping") }},
		{name: "claim_time", mutate: func(p *claimPayload) { p.ClaimTime = "2026-10-06T01:02:04Z" }},
		{name: "claim_row_version", mutate: func(p *claimPayload) { p.ClaimRowVersion++ }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, as, log, intent, _, inventory, ctx, opts := newRestoreScanFixture(t, StateMachineVerified, true)
			var claim claimPayload
			if err := json.Unmarshal(inventory.objects[0].Body, &claim); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&claim)
			claimBody, err := json.Marshal(claim)
			if err != nil {
				t.Fatal(err)
			}
			claimDigest := restoreScanDigest(claimBody)
			inventory.objects[0].Body = claimBody
			inventory.objects[0].Digest, inventory.objects[0].FenceDigest = claimDigest, claimDigest
			if _, err := store.db.Exec(`UPDATE transfer_intents SET claim_fence_digest=? WHERE tenant_id=? AND transfer_id=?`, claimDigest, intent.TenantID, intent.ID); err != nil {
				t.Fatal(err)
			}
			var terminal terminalPayload
			if err := json.Unmarshal(inventory.objects[1].Body, &terminal); err != nil {
				t.Fatal(err)
			}
			terminal.ClaimDigest = claimDigest
			terminalBody, err := json.Marshal(terminal)
			if err != nil {
				t.Fatal(err)
			}
			terminalDigest := restoreScanDigest(terminalBody)
			inventory.objects[1].Body = terminalBody
			inventory.objects[1].Digest, inventory.objects[1].FenceDigest = terminalDigest, terminalDigest
			if _, err := store.db.Exec(`UPDATE transfer_intents SET terminal_fence_digest=? WHERE tenant_id=? AND transfer_id=?`, terminalDigest, intent.TenantID, intent.ID); err != nil {
				t.Fatal(err)
			}
			result, err := ScanAndJoinRestore(ctx, store, as, log, inventory, opts)
			if err != nil || result.Ready || result.Findings != 1 {
				t.Fatalf("mutated claim field admitted after matching external/local digests: result=%+v err=%v", result, err)
			}
			var reason string
			if err := store.db.QueryRow(`SELECT reason FROM assurance_transfer_startup_quarantines WHERE tenant_id=? AND transfer_id=?`, intent.TenantID, intent.ID).Scan(&reason); err != nil || !strings.Contains(reason, "claim_binding_mismatch") {
				t.Fatalf("claim field quarantine reason=%q err=%v", reason, err)
			}
		})
	}
}

func TestRestoreScanSection9Table(t *testing.T) {
	tests := []struct {
		name         string
		state        State
		terminal     bool
		dropTerminal bool
		duplicate    bool
		mutate       func([]FenceObject)
		wantFindings int
		wantState    State
		wantError    bool
		orphan       bool
	}{
		{name: "verified claim before POST freezes", state: StateClaimed, wantFindings: 1, wantState: StateReconciliationRequired},
		{name: "missing body", state: StateClaimed, mutate: func(objects []FenceObject) { objects[0].Body = nil }, wantError: true},
		{name: "wrong path", state: StateClaimed, mutate: func(objects []FenceObject) { objects[0].Name = "wrong/claim.json" }, wantError: true},
		{name: "missing metadata", state: StateClaimed, mutate: func(objects []FenceObject) { objects[0].EnvironmentDigest = "" }, wantError: true},
		{name: "claim idempotency binding", state: StateClaimed, mutate: func(objects []FenceObject) {
			var p claimPayload
			_ = json.Unmarshal(objects[0].Body, &p)
			p.IdempotencyDigest = "other"
			objects[0].Body, _ = json.Marshal(p)
			objects[0].Digest = restoreScanDigest(objects[0].Body)
			objects[0].FenceDigest = objects[0].Digest
		}, wantFindings: 1, wantState: StateReconciliationRequired},
		{name: "claim row version binding", state: StateClaimed, mutate: func(objects []FenceObject) {
			var p claimPayload
			_ = json.Unmarshal(objects[0].Body, &p)
			p.ClaimRowVersion = 99
			objects[0].Body, _ = json.Marshal(p)
			objects[0].Digest = restoreScanDigest(objects[0].Body)
			objects[0].FenceDigest = objects[0].Digest
		}, wantFindings: 1, wantState: StateReconciliationRequired},
		{name: "terminal claim and evidence binding", state: StateMachineVerified, terminal: true, mutate: func(objects []FenceObject) {
			var p terminalPayload
			_ = json.Unmarshal(objects[1].Body, &p)
			p.ClaimDigest = "wrong"
			p.ReadbackDigest = "wrong"
			objects[1].Body, _ = json.Marshal(p)
			objects[1].Digest = restoreScanDigest(objects[1].Body)
			objects[1].FenceDigest = objects[1].Digest
		}, wantFindings: 1, wantState: StateReconciliationRequired},
		{name: "matching machine-verified claim and terminal", state: StateMachineVerified, terminal: true, wantFindings: 0, wantState: StateMachineVerified},
		{name: "successful missing terminal", state: StateMachineVerified, terminal: true, dropTerminal: true, wantFindings: 1, wantState: StateReconciliationRequired},
		{name: "duplicate inventory object", state: StateClaimed, duplicate: true, wantError: true},
		{name: "staged stale id reuse", state: StateStaged, wantFindings: 1, wantState: StateReconciliationRequired},
		{name: "nonterminal fences without operation or readback", state: StateReconciliationRequired, terminal: true, wantFindings: 1, wantState: StateReconciliationRequired},
		{name: "orphan claim is reconciled", state: StateStaged, mutate: func(objects []FenceObject) {
			for i := range objects {
				objects[i].TransferID = "orphan"
				objects[i].Name = restoreScanObjectPath("tenant-a", "env-a", "orphan", objects[i].Kind)
				objects[i].ObjectName = objects[i].Name
				if objects[i].Kind == "claim" {
					var p claimPayload
					_ = json.Unmarshal(objects[i].Body, &p)
					p.TransferID = "orphan"
					objects[i].Body, _ = json.Marshal(p)
				} else {
					var p terminalPayload
					_ = json.Unmarshal(objects[i].Body, &p)
					p.TransferID = "orphan"
					objects[i].Body, _ = json.Marshal(p)
				}
				objects[i].Digest = restoreScanDigest(objects[i].Body)
				objects[i].FenceDigest = objects[i].Digest
			}
		}, wantFindings: 1, orphan: true},
		{name: "inventory failure", state: StateStaged, mutate: func(objects []FenceObject) {}, wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, as, log, intent, _, inventory, ctx, opts := newRestoreScanFixture(t, tc.state, tc.terminal)
			if tc.mutate != nil {
				tc.mutate(inventory.objects)
			}
			if tc.dropTerminal {
				inventory.objects = inventory.objects[:1]
			}
			if tc.duplicate {
				inventory.objects = append(inventory.objects, inventory.objects[0])
			}
			if tc.name == "inventory failure" {
				inventory.err = errors.New("inventory unavailable")
			}
			result, err := ScanAndJoinRestore(ctx, store, as, log, inventory, opts)
			if tc.wantError {
				if err == nil || as.Ready() == nil {
					t.Fatalf("error=%v readiness=%v, want fail closed", err, as.Ready())
				}
				var state State
				if queryErr := store.db.QueryRow(`SELECT state FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, intent.TenantID, intent.ID).Scan(&state); queryErr != nil {
					t.Fatal(queryErr)
				}
				if state != tc.state {
					t.Fatalf("state=%q after normalization failure, want unchanged %q", state, tc.state)
				}
				var auditRows, quarantineRows int
				if queryErr := store.db.QueryRow(`SELECT count(*) FROM transfer_audit_events WHERE tenant_id=? AND disposition='startup_quarantine'`, intent.TenantID).Scan(&auditRows); queryErr != nil {
					t.Fatal(queryErr)
				}
				if queryErr := store.db.QueryRow(`SELECT count(*) FROM assurance_transfer_startup_quarantines WHERE tenant_id=?`, intent.TenantID).Scan(&quarantineRows); queryErr != nil {
					t.Fatal(queryErr)
				}
				if auditRows != 0 || quarantineRows != 0 {
					t.Fatalf("normalization failure mutated audit=%d quarantine=%d, want both zero", auditRows, quarantineRows)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.Findings != tc.wantFindings || result.Ready != (tc.wantFindings == 0) {
				t.Fatalf("result=%+v, want findings=%d ready=%t", result, tc.wantFindings, tc.wantFindings == 0)
			}
			if tc.orphan {
				var count int
				if err := store.db.QueryRow(`SELECT count(*) FROM transfer_reconciliations WHERE transfer_id='orphan'`).Scan(&count); err != nil || count != 1 {
					t.Fatalf("orphan reconciliation count=%d err=%v", count, err)
				}
				return
			}
			var got State
			if err := store.db.QueryRow(`SELECT state FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, intent.TenantID, intent.ID).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tc.wantState {
				t.Fatalf("state=%q, want %q", got, tc.wantState)
			}
			var auditRows, quarantineRows int
			if err := store.db.QueryRow(`SELECT count(*) FROM transfer_audit_events WHERE tenant_id=? AND disposition='startup_quarantine'`, intent.TenantID).Scan(&auditRows); err != nil {
				t.Fatal(err)
			}
			if err := store.db.QueryRow(`SELECT count(*) FROM assurance_transfer_startup_quarantines WHERE tenant_id=?`, intent.TenantID).Scan(&quarantineRows); err != nil {
				t.Fatal(err)
			}
			if auditRows != tc.wantFindings || quarantineRows != tc.wantFindings {
				t.Fatalf("audit=%d quarantine=%d, want %d", auditRows, quarantineRows, tc.wantFindings)
			}
			if inventory.scans != 1 {
				t.Fatalf("inventory scans=%d, want 1", inventory.scans)
			}
		})
	}
}
