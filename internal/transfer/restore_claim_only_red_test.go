package transfer

import (
	"testing"
)

func TestRestoreClaimOnlyReconciliationCannotBeReady(t *testing.T) {
	store, assuranceStore, auditLog, intent, _, inventory, ctx, opts := newRestoreScanFixture(t, StateReconciliationRequired, false)
	result, err := ScanAndJoinRestore(ctx, store, assuranceStore, auditLog, inventory, opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Ready || result.Findings == 0 {
		t.Fatalf("claim-only unresolved transfer admitted: %+v", result)
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM assurance_transfer_startup_quarantines WHERE tenant_id=? AND transfer_id=? AND disposition='quarantined'`, intent.TenantID, intent.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("durable quarantine rows=%d, want 1", count)
	}
}
