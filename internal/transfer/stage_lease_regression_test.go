package transfer

import (
	"context"

	"sync"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

type blockingStageBackend struct {
	*serviceFixture
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingStageBackend) ReadUncached(ctx context.Context, request assurance.SourceRequest) (assurance.ProviderRead, error) {
	blocked := false
	b.once.Do(func() { blocked = true; close(b.entered) })
	if blocked {
		select {
		case <-ctx.Done():
			return assurance.ProviderRead{}, ctx.Err()
		case <-b.release:
		}
	}
	return b.serviceFixture.ReadUncached(ctx, request)
}

func TestStageRenewsReservationDuringBlockedProviderRead(t *testing.T) {
	svc, fixture, store := newServiceFixture(t)
	backend := &blockingStageBackend{serviceFixture: fixture, entered: make(chan struct{}), release: make(chan struct{})}
	router, err := workivaprovider.NewRouterChecked(backend)
	if err != nil {
		t.Fatal(err)
	}
	svc.provider = router
	svc.leaseRenewInterval = 5 * time.Millisecond
	type outcome struct {
		result StageResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := svc.Stage(fixture.ctx, StageRequest{RouteID: "energy-copy", IdempotencyDigest: "blocked-stage-key", RequestDigest: "blocked-stage-request", Now: time.Now().UTC()})
		done <- outcome{result, err}
	}()
	select {
	case <-backend.entered:
	case <-time.After(time.Second):
		t.Fatal("provider read did not begin")
	}
	var initial string
	if err := store.db.QueryRow(`SELECT last_heartbeat_at FROM assurance_idempotency_records WHERE action='stage' AND state='reserved'`).Scan(&initial); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for {
		var current string
		if err := store.db.QueryRow(`SELECT last_heartbeat_at FROM assurance_idempotency_records WHERE action='stage' AND state='reserved'`).Scan(&current); err != nil {
			t.Fatal(err)
		}
		if current != initial {
			break
		}
		select {
		case <-deadline:
			t.Fatal("stage did not renew lease while provider read was blocked")
		case <-time.After(10 * time.Millisecond):
		}
	}
	expired, err := svc.assuranceStore.ExpireStaleReservations(fixture.ctx, time.Now().UTC())
	if err != nil || expired != 0 {
		t.Fatalf("janitor expired live stage: count=%d err=%v", expired, err)
	}
	close(backend.release)
	select {
	case got := <-done:
		if got.err != nil || got.result.TransferID == "" {
			t.Fatalf("stage=%+v err=%v", got.result, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("stage did not finish")
	}
	var state string
	if err := store.db.QueryRow(`SELECT state FROM assurance_idempotency_records WHERE action='stage'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "sealed" {
		t.Fatalf("stage reservation state=%s", state)
	}
}
