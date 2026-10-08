package transfer

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
	_ "modernc.org/sqlite"
)

type b2CountedProvider struct {
	*serviceFixture
	reads            atomic.Int32
	firstReadStarted chan struct{}
	releaseFirstRead chan struct{}
	blockFirstRead   atomic.Bool
}

func (p *b2CountedProvider) ReadUncached(ctx context.Context, req assurance.SourceRequest) (assurance.ProviderRead, error) {
	p.reads.Add(1)
	if p.blockFirstRead.CompareAndSwap(false, true) {
		close(p.firstReadStarted)
		select {
		case <-p.releaseFirstRead:
		case <-ctx.Done():
			return assurance.ProviderRead{}, ctx.Err()
		}
	}
	return p.serviceFixture.ReadUncached(ctx, req)
}

func b2StageRequest() StageRequest {
	return StageRequest{RouteID: "energy-copy", IdempotencyDigest: "b2-idempotency", RequestDigest: "b2-request", Now: time.Now().UTC()}
}

func b2ServiceWithProvider(t *testing.T, provider *b2CountedProvider) (*Service, *serviceFixture, *Store) {
	t.Helper()
	store, _ := testStore(t)
	fixture := &serviceFixture{value: `{"kind":"number","number":"1"}`, op: "https://provider.invalid/operations/b2"}
	provider.serviceFixture = fixture
	router, err := workivaprovider.NewRouterChecked(provider)
	if err != nil {
		t.Fatal(err)
	}
	as, ctx := activateTransferRoute(t, store)
	fixture.ctx = ctx
	svc, err := NewTestService(store, as, router, &FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	return svc, fixture, store
}

func TestB2StageReplayPerformsZeroProviderReads(t *testing.T) {
	provider := &b2CountedProvider{firstReadStarted: make(chan struct{}), releaseFirstRead: make(chan struct{})}
	close(provider.releaseFirstRead)
	svc, fixture, _ := b2ServiceWithProvider(t, provider)
	request := b2StageRequest()
	first, err := svc.Stage(fixture.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	reads := provider.reads.Load()
	replay, err := svc.Stage(fixture.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Token != "" || !replay.ReplayRequiresRestage || replay.TransferID != first.TransferID {
		t.Fatalf("replay=%+v, want token-free stored replay", replay)
	}
	if got := provider.reads.Load(); got != reads {
		t.Fatalf("replay provider GET count=%d, want unchanged %d", got, reads)
	}
	if fixture.posts.Load() != 0 {
		t.Fatalf("stage replay POST count=%d, want 0", fixture.posts.Load())
	}
}

func TestB2StageConflictPerformsZeroProviderReads(t *testing.T) {
	provider := &b2CountedProvider{firstReadStarted: make(chan struct{}), releaseFirstRead: make(chan struct{})}
	close(provider.releaseFirstRead)
	svc, fixture, _ := b2ServiceWithProvider(t, provider)
	request := b2StageRequest()
	if _, err := svc.Stage(fixture.ctx, request); err != nil {
		t.Fatal(err)
	}
	reads := provider.reads.Load()
	request.RequestDigest = "b2-different-request"
	if _, err := svc.Stage(fixture.ctx, request); err == nil || !strings.Contains(err.Error(), "idempotency_conflict") {
		t.Fatalf("conflict err=%v", err)
	}
	if got := provider.reads.Load(); got != reads {
		t.Fatalf("conflict provider GET count=%d, want unchanged %d", got, reads)
	}
}

func TestB2ConcurrentStageHasOneExecutorAndInProgressCallerDoesZeroReads(t *testing.T) {
	provider := &b2CountedProvider{firstReadStarted: make(chan struct{}), releaseFirstRead: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-provider.releaseFirstRead:
		default:
			close(provider.releaseFirstRead)
		}
	})
	svc, fixture, _ := b2ServiceWithProvider(t, provider)
	request := b2StageRequest()
	firstResult := make(chan StageResult, 1)
	firstErr := make(chan error, 1)
	go func() {
		result, err := svc.Stage(fixture.ctx, request)
		firstResult <- result
		firstErr <- err
	}()
	<-provider.firstReadStarted

	secondResult := make(chan StageResult, 1)
	secondErr := make(chan error, 1)
	go func() {
		result, err := svc.Stage(fixture.ctx, request)
		secondResult <- result
		secondErr <- err
	}()
	select {
	case err := <-secondErr:
		if err == nil || !strings.Contains(err.Error(), "idempotency_in_progress") {
			t.Fatalf("concurrent stage err=%v, want idempotency_in_progress", err)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent stage did not return its reservation disposition")
	}
	if got := provider.reads.Load(); got != 1 {
		t.Fatalf("concurrent stage started %d provider GETs before owner completed, want 1", got)
	}
	close(provider.releaseFirstRead)
	if err := <-firstErr; err != nil {
		t.Fatal(err)
	}
	<-firstResult
	if got := provider.reads.Load(); got != 2 {
		t.Fatalf("single stage executor provider GET count=%d, want 2", got)
	}
	if fixture.posts.Load() != 0 {
		t.Fatalf("concurrent stage POST count=%d, want 0", fixture.posts.Load())
	}
}

func TestB2StageReplayEnvelopeSurvivesDurableReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wave3-b2.db")
	db, err := sql.Open("sqlite", sqlitedb.SharedFileDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	store, err := NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	provider := &b2CountedProvider{firstReadStarted: make(chan struct{}), releaseFirstRead: make(chan struct{})}
	close(provider.releaseFirstRead)
	fixture := &serviceFixture{value: `{"kind":"number","number":"1"}`, op: "https://provider.invalid/operations/b2-reopen"}
	provider.serviceFixture = fixture
	router, err := workivaprovider.NewRouterChecked(provider)
	if err != nil {
		t.Fatal(err)
	}
	as, ctx := activateTransferRoute(t, store)
	fixture.ctx = ctx
	svc, err := NewTestService(store, as, router, &FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	request := b2StageRequest()
	first, err := svc.Stage(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedDB, err := sql.Open("sqlite", sqlitedb.SharedFileDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopenedDB.Close() }()
	reopenedDB.SetMaxOpenConns(1)
	reopened, err := assurance.NewWithDB(reopenedDB)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := reopened.Reserve(ctx, assurance.ReservationRequest{
		ActorID:           "11111111-1111-1111-1111-111111111111/actor-1",
		Tool:              "workiva_transfer_value",
		Action:            "stage",
		IdempotencyDigest: assurance.HashBytes([]byte(request.IdempotencyDigest)),
		RequestDigest:     assurance.HashBytes([]byte(request.RequestDigest)),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if replay.Disposition != assurance.ReservationReplay {
		t.Fatalf("reopened reservation=%+v, want replay", replay)
	}
	if bytes := string(replay.Envelope); strings.Contains(bytes, first.Token) {
		t.Fatalf("reopened replay envelope contains confirmation token: %s", bytes)
	}
	var envelope struct {
		TransferID string `json:"transfer_id"`
	}
	if err := json.Unmarshal(replay.Envelope, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.TransferID != first.TransferID {
		t.Fatalf("reopened replay transfer=%q, want %q", envelope.TransferID, first.TransferID)
	}
}

func TestB2ConfirmRejectsTamperedRouteBeforeProviderPost(t *testing.T) {
	svc, fixture, store := newServiceFixture(t)
	staged, tenant, actor, permission := stageForConfirm(t, svc, fixture)
	if _, err := store.db.Exec(`UPDATE assurance_transfer_route_revisions SET content_hash='changed' WHERE tenant_id=? AND route_id=?`, tenant, "energy-copy"); err != nil {
		t.Fatal(err)
	}
	_, _ = svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now().UTC(), IdempotencyDigest: "test-confirm-key", RequestDigest: "test-confirm-request"})
	if fixture.posts.Load() != 0 {
		t.Fatalf("tampered route reached provider POST count=%d", fixture.posts.Load())
	}
}

func TestB2ConfirmRejectsTamperedPolicyBeforeProviderPost(t *testing.T) {
	svc, fixture, store := newServiceFixture(t)
	staged, tenant, actor, permission := stageForConfirm(t, svc, fixture)
	if _, err := store.db.Exec(`UPDATE assurance_conversion_policy_revisions SET content_hash='changed' WHERE tenant_id=? AND policy_id=?`, tenant, "exact"); err != nil {
		t.Fatal(err)
	}
	_, _ = svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now().UTC(), IdempotencyDigest: "test-confirm-key", RequestDigest: "test-confirm-request"})
	if fixture.posts.Load() != 0 {
		t.Fatalf("tampered policy reached provider POST count=%d", fixture.posts.Load())
	}
}
