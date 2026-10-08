package transfer

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/workiva"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

func TestC2QuarantineExpiredQuarantinesMalformedLeasesExactlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name, state, leaseID, leaseExpiry, claimFence, operation string
	}{
		{name: "claimed blank", state: "claimed"},
		{name: "submitting malformed", state: "submitting", leaseID: "lease-2", leaseExpiry: "not-a-timestamp", operation: "op-2"},
		{name: "fenced blank", state: "fenced", leaseExpiry: "", claimFence: "claim-3"},
		{name: "fenced missing fence", state: "fenced", leaseID: "lease-4", leaseExpiry: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, db := testStore(t)
			intent := testIntent()
			if _, err := store.Stage(context.Background(), intent, "token", "key-"+tc.name, "request", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE transfer_intents SET state=?,lease_id=?,lease_expires_at=?,claim_fence_digest=?,operation_reference=? WHERE tenant_id=? AND transfer_id=?`, tc.state, tc.leaseID, tc.leaseExpiry, tc.claimFence, tc.operation, intent.TenantID, intent.ID); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			if err := store.QuarantineExpired(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			if err := store.QuarantineExpired(context.Background(), now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			got, err := store.Get(context.Background(), intent.TenantID, intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != StateReconciliationRequired || got.MachineOutcome != "write_outcome_unknown" {
				t.Fatalf("quarantine state=%+v", got)
			}
			var audits, reconciliations int
			if err := db.QueryRow(`SELECT count(*) FROM transfer_audit_events WHERE tenant_id=? AND transfer_id=? AND disposition='crash_recovery_quarantined'`, intent.TenantID, intent.ID).Scan(&audits); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM transfer_reconciliations WHERE tenant_id=? AND transfer_id=?`, intent.TenantID, intent.ID).Scan(&reconciliations); err != nil {
				t.Fatal(err)
			}
			if audits != 1 || reconciliations != 1 {
				t.Fatalf("recovery evidence audits=%d reconciliations=%d, want 1/1", audits, reconciliations)
			}
		})
	}
}

func TestC2MalformedLeaseQuarantineSurvivesDurableReopenWithoutProviderWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c2-reopen.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	store, err := NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	intent := testIntent()
	if _, err := store.Stage(context.Background(), intent, "token", "reopen-key", "request", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE transfer_intents SET state='submitting',lease_id='',lease_expires_at='',operation_reference='op-reopen',submissions=1 WHERE tenant_id=? AND transfer_id=?`, intent.TenantID, intent.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopenedDB.Close() }()
	reopenedDB.SetMaxOpenConns(1)
	reopened, err := NewWithDB(reopenedDB)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get(context.Background(), intent.TenantID, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateReconciliationRequired {
		t.Fatalf("reopened state=%s, want reconciliation_required", got.State)
	}
	var polls, submissions int
	if err := reopenedDB.QueryRow(`SELECT count(*) FROM assurance_transfer_poll_events WHERE transfer_id=?`, intent.ID).Scan(&polls); err != nil {
		t.Fatal(err)
	}
	if err := reopenedDB.QueryRow(`SELECT submissions FROM transfer_intents WHERE transfer_id=?`, intent.ID).Scan(&submissions); err != nil {
		t.Fatal(err)
	}
	if polls != 0 || submissions != 1 {
		t.Fatalf("reopen performed provider work polls=%d submissions=%d", polls, submissions)
	}
}

type c2Fence struct {
	base       *FakeFence
	createHook func()
	verifyHook func()
	verifyErr  error
}

func (f *c2Fence) Durable() bool                                  { return false }
func (f *c2Fence) Identity(tenant string) (string, string, error) { return f.base.Identity(tenant) }
func (f *c2Fence) CreateClaim(ctx context.Context, id string, body []byte) (string, error) {
	digest, err := f.base.CreateClaim(ctx, id, body)
	if err == nil && f.createHook != nil {
		f.createHook()
	}
	return digest, err
}
func (f *c2Fence) VerifyClaim(ctx context.Context, id, digest string, expected ...[]byte) error {
	err := f.base.VerifyClaim(ctx, id, digest, expected...)
	if err == nil && f.verifyErr != nil {
		return f.verifyErr
	}
	if err == nil && f.verifyHook != nil {
		f.verifyHook()
	}
	return err
}
func (f *c2Fence) CreateTerminal(ctx context.Context, id string, body []byte) (string, error) {
	return f.base.CreateTerminal(ctx, id, body)
}
func (f *c2Fence) VerifyTerminal(ctx context.Context, id, digest string, expected ...[]byte) error {
	return f.base.VerifyTerminal(ctx, id, digest, expected...)
}

type c2Provider struct {
	*serviceFixture
	beforePost      func()
	beforeWait      func()
	blockPost       chan struct{}
	returnOpOnError string
	returnErr       error
	waits           atomic.Int32
}

func (p *c2Provider) UpdateSheetWithRetryAfter(ctx context.Context, spreadsheetID, sheetID string, update workiva.SheetUpdate) (string, time.Duration, error) {
	if p.beforePost != nil {
		p.beforePost()
	}
	if p.blockPost != nil {
		select {
		case <-p.blockPost:
		case <-ctx.Done():
			return "", 0, ctx.Err()
		}
	}
	if p.returnErr != nil {
		p.posts.Add(1)
		return p.returnOpOnError, 0, p.returnErr
	}
	return p.serviceFixture.UpdateSheetWithRetryAfter(ctx, spreadsheetID, sheetID, update)
}

func (p *c2Provider) WaitOperationWithInitialRetryAfter(ctx context.Context, operationURL string, retryAfter time.Duration) (string, error) {
	p.waits.Add(1)
	if p.beforeWait != nil {
		p.beforeWait()
	}
	return p.serviceFixture.WaitOperationWithInitialRetryAfter(ctx, operationURL, retryAfter)
}

func TestC2KnownOperationReferenceSurvivesSubmitErrorWithoutPolling(t *testing.T) {
	provider := &c2Provider{serviceFixture: &serviceFixture{value: `{"kind":"number","number":"1"}`}, returnOpOnError: "operation-body-close", returnErr: errors.New("response body close failed")}
	fence := &c2Fence{base: &FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}}}
	svc, fixture, store := newC2Service(t, provider, fence)
	staged, tenant, actor, permission := stageForConfirm(t, svc, fixture)
	got, err := svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now().UTC(), IdempotencyDigest: "test-confirm-key", RequestDigest: "test-confirm-request"})
	var unknown *ConfirmUnknownError
	if !errors.As(err, &unknown) || !unknown.ReconciliationRequired || !unknown.NoRepeat || unknown.OperationReference != "operation-body-close" {
		t.Fatalf("submit error must preserve operation reference in explicit unknown: %v", err)
	}
	if got.State != StateReconciliationRequired || provider.waits.Load() != 0 || fixture.posts.Load() != 1 {
		t.Fatalf("submit error state=%s waits=%d posts=%d", got.State, provider.waits.Load(), fixture.posts.Load())
	}
	var operation string
	if err := store.db.QueryRow(`SELECT operation_reference FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&operation); err != nil {
		t.Fatal(err)
	}
	if operation != "operation-body-close" {
		t.Fatalf("known operation reference lost: %q", operation)
	}
}

func newC2Service(t *testing.T, provider workivaprovider.Backend, fence Fence) (*Service, *serviceFixture, *Store) {
	t.Helper()
	store, _ := testStore(t)
	router, err := workivaprovider.NewRouterChecked(provider)
	if err != nil {
		t.Fatal(err)
	}
	as, ctx := activateTransferRoute(t, store)
	fixture := provider.(*c2Provider).serviceFixture
	fixture.ctx = ctx
	svc := &Service{store: store, assuranceStore: as, provider: router, fence: fence}
	return svc, fixture, store
}

func TestC2ClaimCommitsBeforeCreateOnlyFenceAndFenceFailureNeverPosts(t *testing.T) {
	provider := &c2Provider{serviceFixture: &serviceFixture{value: `{"kind":"number","number":"1"}`, op: "op"}}
	fence := &c2Fence{base: &FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}}}
	svc, fixture, store := newC2Service(t, provider, fence)
	fence.base.Fail = "claim"
	staged, tenant, actor, permission := stageForConfirm(t, svc, fixture)
	_, _ = svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now().UTC(), IdempotencyDigest: "test-confirm-key", RequestDigest: "test-confirm-request"})
	var claims int
	if err := store.db.QueryRow(`SELECT count(*) FROM transfer_audit_events WHERE tenant_id=? AND transfer_id=? AND disposition='claim_committed'`, tenant, staged.TransferID).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if claims != 1 || fixture.posts.Load() != 0 {
		t.Fatalf("claim audit rows=%d posts=%d", claims, fixture.posts.Load())
	}
}

func TestC2FenceVerificationFailureNeverPostsAndIsReconciliationRequired(t *testing.T) {
	provider := &c2Provider{serviceFixture: &serviceFixture{value: `{"kind":"number","number":"1"}`, op: "op"}}
	fence := &c2Fence{base: &FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}}, verifyErr: errors.New("fence read-back timeout")}
	svc, fixture, store := newC2Service(t, provider, fence)
	staged, tenant, actor, permission := stageForConfirm(t, svc, fixture)
	got, err := svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now().UTC(), IdempotencyDigest: "test-confirm-key", RequestDigest: "test-confirm-request"})
	var unknown *ConfirmUnknownError
	if !errors.As(err, &unknown) || !unknown.ReconciliationRequired || !unknown.NoRepeat || unknown.OperationReference != "" {
		t.Fatalf("fence verification failure must have explicit no-repeat disposition: %v", err)
	}
	if got.State != StateReconciliationRequired || fixture.posts.Load() != 0 {
		t.Fatalf("fence verification failure state=%s posts=%d", got.State, fixture.posts.Load())
	}
	var reconciliations int
	if err := store.db.QueryRow(`SELECT count(*) FROM transfer_reconciliations WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&reconciliations); err != nil {
		t.Fatal(err)
	}
	if reconciliations != 1 {
		t.Fatalf("fence verification recovery evidence=%d, want 1", reconciliations)
	}
}

func TestC2FencedAndSubmittingStateCommitBeforeProviderPost(t *testing.T) {
	provider := &c2Provider{serviceFixture: &serviceFixture{value: `{"kind":"number","number":"1"}`, op: "op"}}
	fence := &c2Fence{base: &FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}}}
	svc, fixture, store := newC2Service(t, provider, fence)
	staged, tenant, actor, permission := stageForConfirm(t, svc, fixture)
	var fenceVerified atomic.Bool
	fence.verifyHook = func() { fenceVerified.Store(true) }
	provider.beforePost = func() {
		var state, claimFence string
		if err := store.db.QueryRow(`SELECT state,claim_fence_digest FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&state, &claimFence); err != nil {
			t.Fatal(err)
		}
		if !fenceVerified.Load() || state != "submitting" || claimFence == "" {
			t.Fatalf("provider observed state=%q claim_fence=%q", state, claimFence)
		}
	}
	if _, err := svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now().UTC(), IdempotencyDigest: "test-confirm-key", RequestDigest: "test-confirm-request"}); err != nil {
		t.Fatal(err)
	}
}

func TestC2OperationReferenceIsDurableBeforeFirstPoll(t *testing.T) {
	provider := &c2Provider{serviceFixture: &serviceFixture{value: `{"kind":"number","number":"1"}`, op: "operation-c2"}}
	fence := &c2Fence{base: &FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}}}
	svc, fixture, store := newC2Service(t, provider, fence)
	staged, tenant, actor, permission := stageForConfirm(t, svc, fixture)
	provider.beforeWait = func() {
		var operation string
		if err := store.db.QueryRow(`SELECT operation_reference FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&operation); err != nil {
			t.Fatal(err)
		}
		if operation != "operation-c2" {
			t.Fatalf("poll began before operation reference persistence: %q", operation)
		}
	}
	if _, err := svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now().UTC(), IdempotencyDigest: "test-confirm-key", RequestDigest: "test-confirm-request"}); err != nil {
		t.Fatal(err)
	}
}

func TestC2OwnerRenewsTransferAndReservationLeasesDuringBoundedPost(t *testing.T) {
	provider := &c2Provider{serviceFixture: &serviceFixture{value: `{"kind":"number","number":"1"}`, op: "operation-renew"}, blockPost: make(chan struct{})}
	fence := &c2Fence{base: &FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}}}
	svc, fixture, store := newC2Service(t, provider, fence)
	svc.leaseRenewInterval = 10 * time.Millisecond
	staged, tenant, actor, permission := stageForConfirm(t, svc, fixture)
	done := make(chan struct{})
	go func() {
		_, _ = svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now().UTC(), IdempotencyDigest: "test-confirm-key", RequestDigest: "test-confirm-request"})
		close(done)
	}()
	var oldTransferLease, oldReservationLease string
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		err1 := store.db.QueryRow(`SELECT lease_expires_at FROM transfer_intents WHERE tenant_id=? AND transfer_id=? AND state IN ('claimed','fenced','submitting')`, tenant, staged.TransferID).Scan(&oldTransferLease)
		err2 := store.db.QueryRow(`SELECT lease_expires_at FROM assurance_idempotency_records WHERE tenant_id=? AND action='confirm' ORDER BY created_at DESC LIMIT 1`, tenant).Scan(&oldReservationLease)
		if err1 == nil && err2 == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if oldTransferLease == "" || oldReservationLease == "" {
		t.Fatal("confirm did not persist both execution leases")
	}
	var renewed bool
	for time.Now().Before(deadline) {
		var currentTransferLease, currentReservationLease string
		_ = store.db.QueryRow(`SELECT lease_expires_at FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&currentTransferLease)
		_ = store.db.QueryRow(`SELECT lease_expires_at FROM assurance_idempotency_records WHERE tenant_id=? AND action='confirm' ORDER BY created_at DESC LIMIT 1`, tenant).Scan(&currentReservationLease)
		if currentTransferLease > oldTransferLease && currentReservationLease > oldReservationLease {
			renewed = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(provider.blockPost)
	<-done
	if !renewed {
		t.Fatalf("owner leases were not renewed during bounded post: transfer %q->? reservation %q->?", oldTransferLease, oldReservationLease)
	}
}

func TestC2OneHundredConfirmCallersCannotReplayOrPollAfterUnknownPost(t *testing.T) {
	provider := &c2Provider{serviceFixture: &serviceFixture{value: `{"kind":"number","number":"1"}`, op: "operation-100", postErr: errors.New("transport timeout after possible send")}}
	fence := &c2Fence{base: &FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}}}
	svc, fixture, store := newC2Service(t, provider, fence)
	staged, tenant, actor, permission := stageForConfirm(t, svc, fixture)
	var wg sync.WaitGroup
	var completed atomic.Int32
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, IdempotencyDigest: "same-c2-key", RequestDigest: "same-c2-request", Now: time.Now().UTC()})
			completed.Add(1)
		}()
	}
	wg.Wait()
	if completed.Load() != 100 || fixture.posts.Load() != 1 {
		t.Fatalf("callers=%d posts=%d, want 100/1", completed.Load(), fixture.posts.Load())
	}
	var state string
	if err := store.db.QueryRow(`SELECT state FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(StateReconciliationRequired) {
		t.Fatalf("unknown post state=%q", state)
	}
}
