package transfer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/workiva"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

type serviceFixture struct {
	mu               sync.Mutex
	value            string
	posts            atomic.Int32
	postErr          error
	waitErr          error
	op               string
	writeSpreadsheet string
	writeSheet       string
	writeUpdate      workiva.SheetUpdate
	ctx              context.Context
}

func (f *serviceFixture) ListSpreadsheets(context.Context) ([]workiva.Spreadsheet, error) {
	return nil, nil
}
func (f *serviceFixture) ListSheets(context.Context, string) ([]workiva.Sheet, error) {
	return nil, nil
}
func (f *serviceFixture) GetSheetData(context.Context, string, string, string, []string) (*workiva.SheetData, error) {
	return nil, errors.New("unused")
}
func (f *serviceFixture) ReadUncached(_ context.Context, req assurance.SourceRequest) (assurance.ProviderRead, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value := f.value
	if req.ExternalResourceID == "dst" {
		value = `{"kind":"number","number":"0"}`
		if f.posts.Load() > 0 {
			value = `{"kind":"number","number":"2"}`
		}
	}
	var envelope struct {
		Number string `json:"number"`
	}
	if err := json.Unmarshal([]byte(value), &envelope); err != nil {
		return assurance.ProviderRead{}, err
	}
	return assurance.ProviderRead{Value: assurance.ProviderValue{Value: json.Number(envelope.Number)}, CacheBypassed: true}, nil
}
func (f *serviceFixture) UpdateSheetWithRetryAfter(_ context.Context, spreadsheetID, sheetID string, update workiva.SheetUpdate) (string, time.Duration, error) {
	f.writeSpreadsheet, f.writeSheet, f.writeUpdate = spreadsheetID, sheetID, update
	f.posts.Add(1)
	f.mu.Lock()
	f.value = `{"kind":"number","number":"2"}`
	f.mu.Unlock()
	if f.postErr != nil {
		return "", 0, f.postErr
	}
	return f.op, 0, nil
}
func (f *serviceFixture) WaitOperationWithInitialRetryAfter(context.Context, string, time.Duration) (string, error) {
	return "completed", f.waitErr
}
func newServiceFixture(t *testing.T) (*Service, *serviceFixture, *Store) {
	t.Helper()
	store, _ := testStore(t)
	f := &serviceFixture{value: `{"kind":"number","number":"1"}`, op: "https://provider.invalid/operations/1"}
	router, err := workivaprovider.NewRouterChecked(f)
	if err != nil {
		t.Fatal(err)
	}
	as, ctx := activateTransferRoute(t, store)
	f.ctx = ctx
	svc, err := NewTestService(store, as, router, &FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	return svc, f, store
}
func activateTransferRoute(t *testing.T, store *Store) (*assurance.Store, context.Context) {
	t.Helper()
	const tenant = "11111111-1111-1111-1111-111111111111"
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bundle := assurance.Bundle{
		SchemaVersion: 1, BundleID: "bundle-1", BundleVersion: 1, TenantID: tenant,
		Reports:            []assurance.ReportRevision{{ReportID: "energy-report", Revision: 1, Name: "Energy report", Description: "Quarterly energy assurance", Owner: "sustainability", Status: "active", RetentionClass: "standard", ResourcePolicyHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Periods: []assurance.Period{{Key: "2026-Q3", Label: "Q3 2026", Start: "2026-07-01", End: "2026-09-30"}}, Fields: []assurance.FieldDefinition{{FieldID: "scope2-kwh", ResourceID: "resource-1", ExternalResourceID: "sp-1", SubresourceID: "sh-1", Locator: "B3", Kind: assurance.ValueNumber, Unit: "kWh", Scale: "ones", Required: true, Order: 1}}}},
		TransferRoutes:     []assurance.TransferRoute{{RouteID: "energy-copy", Revision: 1, SourceMappingID: "mapping-energy", SourceResourceID: "src", SourceSheetID: "src-sheet", SourceLocator: "Scope!B2", TargetResourceID: "dst", TargetSheetID: "dst-sheet", TargetLocator: "Results!C4", ConversionPolicyID: "exact", ConversionPolicyVersion: 1, Capabilities: []string{"workiva.write.preview"}}},
		ConversionPolicies: []assurance.ConversionPolicy{{PolicyID: "exact", Revision: 1, Mode: "typed_copy", SourceKind: assurance.ValueNumber, TargetKind: assurance.ValueNumber}},
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := assurance.CanonicalJSONBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := assurance.ValidateBundle(raw, ed25519.Sign(private, canonical), tenant, public)
	if err != nil {
		t.Fatalf("ValidateBundle: %v", err)
	}
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: tenant, ObjectID: "actor-1", Permissions: []identity.Permission{identity.PermissionWorkivaWritePreview}})
	as, err := assurance.NewWithDB(store.db)
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewWithDB(store.db)
	if err != nil {
		t.Fatal(err)
	}
	as.SetAuditLog(log)
	if err = as.StageBundle(ctx, validated); err != nil {
		t.Fatal(err)
	}
	if err = as.RequestActivation(ctx, bundle.BundleID, bundle.BundleVersion); err != nil {
		t.Fatal(err)
	}
	if err = as.Bootstrap(ctx, tenant, public); err != nil {
		t.Fatal(err)
	}
	return as, ctx
}

func TestStageRejectsCallerSuppliedIntentValuesBeforeProviderRead(t *testing.T) {
	svc, f, _ := newServiceFixture(t)
	_, err := svc.Stage(context.Background(), StageRequest{RouteID: "caller-route", IdempotencyDigest: "idem", RequestDigest: "req", Now: time.Now()})
	if err == nil {
		t.Fatal("caller-controlled intent accepted; stage must resolve route, values, fingerprints, and token server-side")
	}
	if f.posts.Load() != 0 {
		t.Fatalf("stage submitted %d provider writes", f.posts.Load())
	}
}

func TestStageReadsAuthoritativeValuesAndReturnsTokenOnce(t *testing.T) {
	svc, f, _ := newServiceFixture(t)
	ctx := f.ctx
	req := StageRequest{RouteID: "energy-copy", IdempotencyDigest: "idem", RequestDigest: "req", Now: time.Now()}
	got, err := svc.Stage(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token == "" || f.posts.Load() != 0 {
		t.Fatalf("stage token-empty=%t posts=%d", got.Token == "", f.posts.Load())
	}
	replay, err := svc.Stage(ctx, req)
	if err != nil || replay.Token != "" {
		t.Fatalf("stage replay token=%q err=%v", replay.Token, err)
	}
	if f.posts.Load() != 0 {
		t.Fatalf("stage submitted %d provider writes", f.posts.Load())
	}
}
func stageForConfirm(t *testing.T, svc *Service, f *serviceFixture) (StageResult, string, string, string) {
	t.Helper()
	const tenant, permission = "11111111-1111-1111-1111-111111111111", "workiva.write.confirm"
	actor := tenant + "/actor-1"
	got, err := svc.Stage(f.ctx, StageRequest{RouteID: "energy-copy", IdempotencyDigest: "idem-" + time.Now().Format("150405.000000000"), RequestDigest: "req", Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return got, tenant, actor, permission
}

func TestConfirmRejectsStaleValuesAndNeverPosts(t *testing.T) {
	svc, f, _ := newServiceFixture(t)
	staged, tenant, actor, permission := stageForConfirm(t, svc, f)
	f.value = `{"kind":"number","number":"9"}`
	_, err := svc.Confirm(context.Background(), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := svc.store.Get(context.Background(), tenant, staged.TransferID)
	if got.State != StateReconciliationRequired {
		t.Fatalf("stale source state %s", got.State)
	}
	if f.posts.Load() != 0 {
		t.Fatal("stale source reached provider POST")
	}
}
func TestConfirmVerifiedFencePostsAtMostOnceAndTerminalFenceRequired(t *testing.T) {
	svc, f, _ := newServiceFixture(t)
	st, tenant, actor, permission := stageForConfirm(t, svc, f)
	f.value = `{"kind":"number","number":"1"}`
	_, err := svc.Confirm(context.Background(), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: st.TransferID, Token: st.Token, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if f.posts.Load() != 1 {
		var reason string
		_ = svc.store.db.QueryRow(`SELECT recovery_reason FROM transfer_intents WHERE transfer_id=?`, st.TransferID).Scan(&reason)
		t.Fatalf("posts=%d reason=%q", f.posts.Load(), reason)
	}
}
func TestConfirmPollTimeoutPersistsOperationAndFreezesTransfer(t *testing.T) {
	svc, f, _ := newServiceFixture(t)
	staged, tenant, actor, permission := stageForConfirm(t, svc, f)
	f.value = `{"kind":"number","number":"1"}`
	f.waitErr = errors.New("poll timeout")
	got, err := svc.Confirm(context.Background(), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if f.posts.Load() != 1 || got.State != StateReconciliationRequired {
		t.Fatalf("posts=%d state=%s", f.posts.Load(), got.State)
	}
	var operation string
	if err := svc.store.db.QueryRow(`SELECT operation_reference FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&operation); err != nil {
		t.Fatal(err)
	}
	if operation != f.op {
		t.Fatalf("operation reference=%q want %q", operation, f.op)
	}
	_, _ = svc.Confirm(context.Background(), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now()})
	if f.posts.Load() != 1 {
		t.Fatalf("repeated POST after poll timeout: %d", f.posts.Load())
	}
}

func TestConfirmConcurrentAndPostTimeoutNeverRepeats(t *testing.T) {
	svc, f, _ := newServiceFixture(t)
	st, tenant, actor, permission := stageForConfirm(t, svc, f)
	f.value = `{"kind":"number","number":"1"}`
	f.postErr = errors.New("timeout after send")
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.Confirm(context.Background(), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: st.TransferID, Token: st.Token, Now: time.Now()})
		}()
	}
	wg.Wait()
	if f.posts.Load() != 1 {
		t.Fatalf("POST count %d", f.posts.Load())
	}
	_, _ = svc.Confirm(context.Background(), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: st.TransferID, Token: st.Token, Now: time.Now()})
	if f.posts.Load() != 1 {
		t.Fatal("repeat after ambiguous POST")
	}
}
func TestConfirmWritesExactResolvedTargetCell(t *testing.T) {
	svc, f, _ := newServiceFixture(t)
	st, tenant, actor, permission := stageForConfirm(t, svc, f)
	f.value = `{"kind":"number","number":"1"}`
	_, err := svc.Confirm(context.Background(), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: st.TransferID, Token: st.Token, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if f.writeSpreadsheet != "dst" || f.writeSheet != "dst-sheet" {
		t.Fatalf("write route=(%q,%q)", f.writeSpreadsheet, f.writeSheet)
	}
	body, err := json.Marshal(f.writeUpdate)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"editCells":{"cells":[{"column":2,"row":3,"value":1}]}}` {
		t.Fatalf("write payload %s", body)
	}
}

func TestProductionServiceRequiresFence(t *testing.T) {
	store, _ := testStore(t)
	if _, err := NewService(store, nil, nil, nil); err == nil {
		t.Fatal("service constructed without verified fence")
	}
	f := &serviceFixture{}
	router, err := workivaprovider.NewRouterChecked(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(store, nil, router, &FakeFence{}); err == nil {
		t.Fatal("production service accepted non-durable fake fence")
	}
	unready, err := assurance.NewWithDB(store.db)
	if err != nil {
		t.Fatal(err)
	}
	if err := unready.Ready(); err == nil {
		t.Fatal("fixture assurance store unexpectedly ready without bootstrap")
	}
	if _, err := NewTestService(store, unready, router, &FakeFence{}); err == nil {
		t.Fatal("test service accepted unready assurance store")
	}
}
