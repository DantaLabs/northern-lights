package transfer

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestConfirmFenceBindsCommittedClaimAndTerminalOutcome(t *testing.T) {
	svc, provider, store := newServiceFixture(t)
	staged, tenant, actor, permission := stageForConfirm(t, svc, provider)
	req := ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now().UTC(), IdempotencyDigest: "confirm-claim-digest", RequestDigest: "confirm-request-digest"}
	got, err := svc.Confirm(b1TrustedConfirmContext(tenant), req)
	if err != nil {
		t.Fatal(err)
	}
	fake := svc.fence.(*FakeFence)
	fake.mu.Lock()
	claim := append([]byte(nil), fake.claimBytes[staged.TransferID]...)
	terminal := append([]byte(nil), fake.terminalBytes[staged.TransferID]...)
	fake.mu.Unlock()
	var cp claimPayload
	if err := json.Unmarshal(claim, &cp); err != nil {
		t.Fatal(err)
	}
	var tp terminalPayload
	if err := json.Unmarshal(terminal, &tp); err != nil {
		t.Fatal(err)
	}
	var committedTime string
	if err := store.db.QueryRowContext(context.Background(), `SELECT created_at FROM transfer_audit_events WHERE tenant_id=? AND transfer_id=? AND event_id=?`, tenant, staged.TransferID, "claim:"+staged.TransferID).Scan(&committedTime); err != nil {
		t.Fatal(err)
	}
	if cp.EnvironmentDigest != "test-only-environment" || cp.TenantDigest != digest(tenant) || cp.TransferID != staged.TransferID || cp.IdempotencyDigest != req.IdempotencyDigest || cp.RequestDigest != req.RequestDigest || cp.ActorDigest != digest(actor) || cp.SourceHash != got.Intent.Source.Fingerprint || cp.TargetHash != got.Intent.Target.Fingerprint || cp.IntendedHash != digest(got.Intent.Intended) || cp.PolicyHash != got.Intent.PolicyHash || cp.MappingHash != got.Intent.MappingHash || cp.ClaimState != "claimed" || cp.ClaimTime != committedTime || cp.ClaimRowVersion != 2 {
		t.Fatalf("claim binding incorrect: %+v", cp)
	}
	if digest(string(claim)) != got.ClaimFenceDigest || tp.ClaimDigest != got.ClaimFenceDigest || tp.EnvironmentDigest != cp.EnvironmentDigest || tp.TenantDigest != cp.TenantDigest || tp.TransferID != staged.TransferID || tp.ProviderOutcomeDigest != digest("accepted") || tp.OperationReferenceDigest != digest(provider.op) || tp.ReadbackDigest != digest(got.Intent.Intended) || tp.TerminalKind != "accepted" || tp.Reason != "api_verified" || tp.TerminalTime == "" || digest(string(terminal)) != got.TerminalFenceDigest {
		t.Fatalf("terminal binding incorrect: %+v", tp)
	}
	if err := fake.VerifyClaim(context.Background(), staged.TransferID, got.ClaimFenceDigest, claim); err != nil {
		t.Fatal(err)
	}
	if err := fake.VerifyTerminal(context.Background(), staged.TransferID, got.TerminalFenceDigest, terminal); err != nil {
		t.Fatal(err)
	}
}
