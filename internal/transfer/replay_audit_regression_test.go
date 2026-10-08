package transfer

import (
	"testing"
	"time"
)

func TestStageSealedReplayDoesNotRequireAuditAfterSuccess(t *testing.T) {
	svc, fixture, store := newServiceFixture(t)
	request := StageRequest{RouteID: "energy-copy", IdempotencyDigest: "audit-replay-key", RequestDigest: "audit-replay-request", Now: time.Now().UTC()}
	first, err := svc.Stage(fixture.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	beforeReads := fixture.reads.Load()
	if _, err := store.db.Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	replay, err := svc.Stage(fixture.ctx, request)
	if err != nil {
		t.Fatalf("sealed replay depended on audit: %v", err)
	}
	if replay.TransferID != first.TransferID || replay.Token != "" || !replay.ReplayRequiresRestage {
		t.Fatalf("unsafe replay: %+v", replay)
	}
	if reads := fixture.reads.Load(); reads != beforeReads {
		t.Fatalf("replay read provider: %d -> %d", beforeReads, reads)
	}
}
