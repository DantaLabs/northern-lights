package transfer

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// A failed operation can have changed cells before failing. Status alone is not a no-effect contract.
func TestReconciliationFailedOperationCannotProveNoEffect(t *testing.T) {
	for _, status := range []string{"failed", "rejected", "cancelled", "canceled"} {
		t.Run(status, func(t *testing.T) {
			svc, fixture, store := newServiceFixture(t)
			staged, tenant, actor, permission := stageForConfirm(t, svc, fixture)
			fixture.waitErr = errors.New("poll lost after partial mutation")
			_, _ = svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now().UTC(), IdempotencyDigest: "failure-confirm", RequestDigest: "failure-confirm-request"})
			ctx := trustedTransferOperatorContext(tenant)
			var ref string
			if err := store.db.QueryRow(`SELECT operation_reference FROM transfer_intents WHERE transfer_id=?`, staged.TransferID).Scan(&ref); err != nil {
				t.Fatal(err)
			}
			if ref == "" {
				t.Fatal("expected persisted operation")
			}
			// Also model an old failed operation and a forged generic read_only event: neither is provider no-effect proof.
			for i, event := range []string{fmt.Sprintf(`{"operation_reference":%q,"status":%q,"read_only":true}`, ref, status), fmt.Sprintf(`{"operation_reference":%q,"status":%q,"read_only":true,"no_effect":true}`, ref, status)} {
				if _, err := store.db.Exec(`INSERT INTO assurance_transfer_poll_events(tenant_id,transfer_id,event_id,event_json,observed_at) VALUES(?,?,?,?,?)`, tenant, staged.TransferID, fmt.Sprintf("generic-%d", i), event, time.Now().Add(time.Duration(i-1)*time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
					t.Fatal(err)
				}
				_, err := svc.Reconcile(ctx, ReconcileRequest{TransferID: staged.TransferID, Action: "classify", Classification: "confirmed_not_applied", IdempotencyDigest: fmt.Sprintf("generic-key-%d", i), RequestDigest: fmt.Sprintf("generic-request-%d", i), Now: time.Now().UTC()})
				if err == nil || !strings.Contains(err.Error(), "capability_unverified") {
					t.Fatalf("generic status accepted or wrong failure: %v", err)
				}
				got, e := store.Get(ctx, tenant, staged.TransferID)
				if e != nil || got.State != StateReconciliationRequired || fixture.posts.Load() != 1 {
					t.Fatalf("state=%+v err=%v posts=%d", got, e, fixture.posts.Load())
				}
			}
		})
	}
}

func TestReconciliationConcurrentClassificationCannotReuseEvidence(t *testing.T) {
	svc, fixture, store := newServiceFixture(t)
	staged, tenant, actor, permission := stageForConfirm(t, svc, fixture)
	fixture.waitErr = errors.New("poll lost")
	_, _ = svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now().UTC(), IdempotencyDigest: "race-confirm", RequestDigest: "race-confirm-request"})
	ctx := trustedTransferOperatorContext(tenant)
	if _, err := svc.Reconcile(ctx, ReconcileRequest{TransferID: staged.TransferID, Action: "read_back", IdempotencyDigest: "race-read", RequestDigest: "race-read-request"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.Reconcile(ctx, ReconcileRequest{TransferID: staged.TransferID, Action: "classify", Classification: "confirmed_applied", IdempotencyDigest: fmt.Sprintf("race-classify-%d", i), RequestDigest: fmt.Sprintf("race-request-%d", i)})
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("classifications succeeded=%d, want exactly one", successes)
	}
	got, err := store.Get(ctx, tenant, staged.TransferID)
	if err != nil || got.State != StateMachineVerified || fixture.posts.Load() != 1 {
		t.Fatalf("state=%+v err=%v posts=%d", got, err, fixture.posts.Load())
	}
}

func TestReconciliationAppliedRequiresFreshLatestReadback(t *testing.T) {
	for _, scenario := range []string{"old_equal", "newer_conflict", "reopened", "replayed_readback"} {
		t.Run(scenario, func(t *testing.T) {
			svc, fixture, store := newServiceFixture(t)
			staged, tenant, actor, permission := stageForConfirm(t, svc, fixture)
			fixture.waitErr = errors.New("poll lost")
			ctx := trustedTransferOperatorContext(tenant)
			if scenario == "old_equal" {
				var intended string
				if err := store.db.QueryRow(`SELECT json_extract(intent_json,'$.intended') FROM transfer_intents WHERE transfer_id=?`, staged.TransferID).Scan(&intended); err != nil {
					t.Fatal(err)
				}
				_, err := store.db.Exec(`INSERT INTO assurance_transfer_readbacks(tenant_id,transfer_id,readback_id,typed_value_json,cache_bypassed,observed_at) VALUES(?,?,?,?,1,?)`, tenant, staged.TransferID, "pre-uncertainty", intended, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano))
				if err != nil {
					t.Fatal(err)
				}
			}
			_, _ = svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now().UTC(), IdempotencyDigest: "applied-confirm", RequestDigest: "applied-confirm-request"})
			if scenario != "old_equal" {
				rb, err := svc.Reconcile(ctx, ReconcileRequest{TransferID: staged.TransferID, Action: "read_back", IdempotencyDigest: "fresh-read", RequestDigest: "fresh-request", Now: time.Now().UTC()})
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "newer_conflict" {
					_, err = store.db.Exec(`INSERT INTO assurance_transfer_readbacks(tenant_id,transfer_id,readback_id,typed_value_json,cache_bypassed,observed_at) VALUES(?,?,?,?,1,?)`, tenant, staged.TransferID, "later-conflict", `0`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano))
					if err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "reopened" {
					_, err = store.db.Exec(`UPDATE transfer_intents SET state='machine_verified_visual_ack_pending' WHERE transfer_id=?`, staged.TransferID)
					if err != nil {
						t.Fatal(err)
					}
					_, err = store.db.Exec(`UPDATE transfer_intents SET state='reconciliation_required' WHERE transfer_id=?`, staged.TransferID)
					if err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "replayed_readback" {
					_, err = svc.Reconcile(ctx, ReconcileRequest{TransferID: staged.TransferID, Action: "read_back", IdempotencyDigest: "fresh-read", RequestDigest: "fresh-request", Now: time.Now().UTC()})
					if err != nil {
						t.Fatal(err)
					}
					if rb.ReadbackID == "" {
						t.Fatal("missing readback")
					}
					_, err = store.db.Exec(`INSERT INTO assurance_transfer_readbacks(tenant_id,transfer_id,readback_id,typed_value_json,cache_bypassed,observed_at) VALUES(?,?,?,?,1,?)`, tenant, staged.TransferID, "later-conflict", `0`, time.Now().UTC().Format(time.RFC3339Nano))
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			_, err := svc.Reconcile(ctx, ReconcileRequest{TransferID: staged.TransferID, Action: "classify", Classification: "confirmed_applied", IdempotencyDigest: "applied-classify", RequestDigest: "applied-classify-request", Now: time.Now().UTC()})
			if err == nil {
				t.Fatal("stale or conflicting readback accepted")
			}
			got, e := store.Get(ctx, tenant, staged.TransferID)
			if e != nil || got.State != StateReconciliationRequired || fixture.posts.Load() != 1 {
				t.Fatalf("state=%+v err=%v posts=%d", got, e, fixture.posts.Load())
			}
		})
	}
}
