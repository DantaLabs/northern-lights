package transfer

import (
	"encoding/json"
	"testing"
)

func azureClaimFixture(t *testing.T, id string) []byte {
	t.Helper()
	b, e := json.Marshal(claimPayload{EnvironmentDigest: "env-a", TenantDigest: digest("tenant-a"), TransferID: id, IdempotencyDigest: "idem", RequestDigest: "request", ActorDigest: digest("actor"), SourceHash: "source", TargetHash: "target", IntendedHash: "intended", PolicyHash: "policy", MappingHash: "mapping", ClaimState: "claimed", ClaimTime: "2026-01-01T00:00:00Z", ClaimRowVersion: 2})
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func azureTerminalFixture(t *testing.T, id string) []byte {
	t.Helper()
	b, e := json.Marshal(terminalPayload{ClaimDigest: "claim", EnvironmentDigest: "env-a", TenantDigest: digest("tenant-a"), TransferID: id, ProviderOutcomeDigest: "outcome", TerminalKind: "accepted", Reason: "api_verified", TerminalTime: "2026-01-01T00:00:00Z"})
	if e != nil {
		t.Fatal(e)
	}
	return b
}
