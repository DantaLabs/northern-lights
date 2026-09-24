package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

type tenantIsolationBackend struct {
	mu               sync.Mutex
	listCalls        int
	listSheetCalls   []string
	readCalls        int
	mutationCalls    int
	spreadsheetNames []workiva.Spreadsheet
	sheetsByBook     map[string][]workiva.Sheet
	readDelay        time.Duration
}

func (b *tenantIsolationBackend) ListSpreadsheets(context.Context) ([]workiva.Spreadsheet, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listCalls++
	return b.spreadsheetNames, nil
}

func (b *tenantIsolationBackend) ListSheets(_ context.Context, spreadsheetID string) ([]workiva.Sheet, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listSheetCalls = append(b.listSheetCalls, spreadsheetID)
	return b.sheetsByBook[spreadsheetID], nil
}

func (b *tenantIsolationBackend) GetSheetData(context.Context, string, string, string, []string) (*workiva.SheetData, error) {
	b.mu.Lock()
	b.readCalls++
	delay := b.readDelay
	b.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	return &workiva.SheetData{
		Range: &workiva.Range{StartRow: 0, StartCol: 0, StopRow: 1, StopCol: 1},
		Cells: [][]workiva.Cell{{{Value: "Field"}, {Value: "1"}}, {{Value: "Energy"}, {Value: "2"}}},
	}, nil
}

func (b *tenantIsolationBackend) UpdateSheetWithRetryAfter(context.Context, string, string, workiva.SheetUpdate) (string, time.Duration, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mutationCalls++
	return "/operations/test", 0, nil
}

func (b *tenantIsolationBackend) WaitOperationWithInitialRetryAfter(context.Context, string, time.Duration) (string, error) {
	return "completed", nil
}

func callEntraTool(t *testing.T, deps mcpserver.Deps, tool mcpserver.Tool, principal identity.Principal, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	reg := mcpserver.NewRegistry()
	reg.Register(tool)
	handler, err := mcpserver.New(deps, reg, &mcpserver.Options{
		AuthMode:      config.AuthModeEntra,
		TokenVerifier: toolTokenVerifier{principal: principal},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "tenant-test", Version: "dev"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			req.Header.Set("Authorization", "Bearer test-only-entra-token")
			return http.DefaultTransport.RoundTrip(req)
		})},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool.Name(), Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func tenantPrincipal(tenant, object string, permissions ...identity.Permission) identity.Principal {
	return identity.Principal{TenantID: tenant, ObjectID: object, TokenType: identity.TokenTypeDelegated, Permissions: permissions}
}

func seedTenantResource(t *testing.T, store *mapping.Store, principal identity.Principal, spreadsheetID, sheetID, fieldName string) {
	t.Helper()
	ctx := identity.ContextWithPrincipal(context.Background(), principal)
	if err := store.UpsertSpreadsheet(ctx, mapping.Spreadsheet{ID: spreadsheetID, Name: spreadsheetID, Region: "eu"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSheet(ctx, mapping.Sheet{ID: sheetID, SpreadsheetID: spreadsheetID, Name: sheetID}); err != nil {
		t.Fatal(err)
	}
	if fieldName != "" {
		if _, err := store.UpsertField(ctx, mapping.Field{SpreadsheetID: spreadsheetID, SheetID: sheetID, Name: fieldName, CellRange: "B2"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEntraDirectReadAndSyncDenyForeignResourceBeforeWorkiva(t *testing.T) {
	deps, _ := authorizationTestDeps(t)
	backend := &tenantIsolationBackend{}
	deps.Client = backend
	a := tenantPrincipal("11111111-1111-1111-1111-111111111111", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		identity.PermissionWorkivaRead, identity.PermissionMappingSync)
	b := tenantPrincipal("22222222-2222-2222-2222-222222222222", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	seedTenantResource(t, deps.Store, b, "foreign-sp", "foreign-sh", "foreign_field")

	read := callEntraTool(t, deps, ReadRange(), a, map[string]any{"spreadsheet_id": "foreign-sp", "sheet_id": "foreign-sh", "range": "A1"})
	if !read.IsError {
		t.Fatalf("foreign direct read succeeded: %+v", read.StructuredContent)
	}
	syncResult := callEntraTool(t, deps, SyncMapping(), a, map[string]any{"spreadsheet_id": "foreign-sp", "sheet_id": "foreign-sh"})
	if !syncResult.IsError {
		t.Fatalf("foreign sync succeeded: %+v", syncResult.StructuredContent)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.readCalls != 0 || backend.mutationCalls != 0 {
		t.Fatalf("foreign resource reached Workiva: reads=%d mutations=%d", backend.readCalls, backend.mutationCalls)
	}
}

func TestEntraSearchGetWriteAndAuditCannotExposeForeignTenantState(t *testing.T) {
	deps, _ := authorizationTestDeps(t)
	backend := &tenantIsolationBackend{}
	deps.Client = backend
	a := tenantPrincipal("11111111-1111-1111-1111-111111111111", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		identity.PermissionWorkivaRead, identity.PermissionWorkivaWritePreview, identity.PermissionAuditRead)
	b := tenantPrincipal("22222222-2222-2222-2222-222222222222", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	seedTenantResource(t, deps.Store, b, "foreign-sp", "foreign-sh", "foreign_secret_field")
	bctx := identity.ContextWithPrincipal(context.Background(), b)
	if _, err := deps.Audit.Append(bctx, audit.Entry{Actor: b.AuditActor(), Tool: "foreign", Action: "read", Target: "B-SECRET", AfterJSON: `{"value":"B-PRIVATE"}`}); err != nil {
		t.Fatal(err)
	}
	actx := identity.ContextWithPrincipal(context.Background(), a)
	if _, err := deps.Audit.Append(actx, audit.Entry{Actor: a.AuditActor(), Tool: "own", Action: "read", Target: "A-VISIBLE"}); err != nil {
		t.Fatal(err)
	}

	search := callEntraTool(t, deps, SearchFields(), a, map[string]any{"query": "foreign"})
	if search.IsError || len(structuredContent(t, search)["fields"].([]any)) != 0 {
		t.Fatalf("foreign search result = %+v", search.StructuredContent)
	}
	if get := callEntraTool(t, deps, GetField(), a, map[string]any{"name": "foreign_secret_field"}); !get.IsError {
		t.Fatalf("foreign get succeeded: %+v", get.StructuredContent)
	}
	if update := callEntraTool(t, deps, UpdateField(), a, map[string]any{"name": "foreign_secret_field", "value": "changed"}); !update.IsError {
		t.Fatalf("foreign write staged: %+v", update.StructuredContent)
	}
	auditResult := callEntraTool(t, deps, AuditTrail(), a, map[string]any{"limit": 100})
	if auditResult.IsError {
		t.Fatalf("audit read failed: %+v", auditResult.Content)
	}
	payload, err := json.Marshal(auditResult.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "B-SECRET") || strings.Contains(string(payload), "B-PRIVATE") {
		t.Fatalf("foreign audit semantics leaked: %s", payload)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.readCalls != 0 || backend.mutationCalls != 0 {
		t.Fatalf("foreign mapped tools reached Workiva: reads=%d mutations=%d", backend.readCalls, backend.mutationCalls)
	}
}

func TestEntraSyncDeniedWhenOnlySpreadsheetRowIsForeign(t *testing.T) {
	deps, _ := authorizationTestDeps(t)
	backend := &tenantIsolationBackend{}
	deps.Client = backend
	a := tenantPrincipal("11111111-1111-1111-1111-111111111111", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", identity.PermissionMappingSync)
	b := tenantPrincipal("22222222-2222-2222-2222-222222222222", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	// Tenant B holds only a spreadsheets row for sp-1: no sheet or field rows.
	bctx := identity.ContextWithPrincipal(context.Background(), b)
	if err := deps.Store.UpsertSpreadsheet(bctx, mapping.Spreadsheet{ID: "sp-1", Name: "B book"}); err != nil {
		t.Fatal(err)
	}

	result := callEntraTool(t, deps, SyncMapping(), a, map[string]any{"spreadsheet_id": "sp-1", "sheet_id": "new-sh"})
	if !result.IsError {
		t.Fatalf("sync claiming a spreadsheet-level foreign resource succeeded: %+v", result.StructuredContent)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.listCalls != 0 || backend.readCalls != 0 || backend.mutationCalls != 0 {
		t.Fatalf("foreign claim reached Workiva: lists=%d reads=%d mutations=%d",
			backend.listCalls, backend.readCalls, backend.mutationCalls)
	}
}

func TestEntraSyncClaimsGloballyUnownedResource(t *testing.T) {
	deps, _ := authorizationTestDeps(t)
	backend := &tenantIsolationBackend{}
	deps.Client = backend
	a := tenantPrincipal("11111111-1111-1111-1111-111111111111", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", identity.PermissionMappingSync)
	result := callEntraTool(t, deps, SyncMapping(), a, map[string]any{"spreadsheet_id": "new-sp", "sheet_id": "new-sh"})
	if result.IsError {
		t.Fatalf("unowned onboarding failed: %+v", result.Content)
	}
	owned, _, err := deps.Store.ResourceOwnership(identity.ContextWithPrincipal(context.Background(), a), "new-sp", "new-sh")
	if err != nil || !owned {
		t.Fatalf("onboarded resource ownership = %v, err=%v", owned, err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.readCalls != 1 {
		t.Fatalf("unowned sync Workiva reads=%d, want 1", backend.readCalls)
	}
}

func TestConcurrentEntraSyncAllowsOnlyOneTenantToClaimUnownedResource(t *testing.T) {
	deps, _ := authorizationTestDeps(t)
	backend := &tenantIsolationBackend{readDelay: 100 * time.Millisecond}
	deps.Client = backend
	a := tenantPrincipal("11111111-1111-1111-1111-111111111111", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", identity.PermissionMappingSync)
	b := tenantPrincipal("22222222-2222-2222-2222-222222222222", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", identity.PermissionMappingSync)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for _, principal := range []identity.Principal{a, b} {
		principal := principal
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := callEntraTool(t, deps, SyncMapping(), principal, map[string]any{"spreadsheet_id": "race-sp", "sheet_id": "race-sh"})
			if !result.IsError {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	backend.mu.Lock()
	reads := backend.readCalls
	backend.mu.Unlock()
	if successes.Load() != 1 || reads != 1 {
		t.Fatalf("concurrent claims successes=%d Workiva reads=%d, want 1/1", successes.Load(), reads)
	}
}

func TestEntraLiveListingExposesOnlyTenantOwnedResources(t *testing.T) {
	deps, _ := authorizationTestDeps(t)
	backend := &tenantIsolationBackend{
		spreadsheetNames: []workiva.Spreadsheet{{ID: "owned-sp", Name: "Owned live"}, {ID: "foreign-sp", Name: "Foreign secret"}},
		sheetsByBook: map[string][]workiva.Sheet{
			"owned-sp":   {{ID: "owned-sh", Name: "Owned sheet"}},
			"foreign-sp": {{ID: "foreign-sh", Name: "Foreign sheet"}},
		},
	}
	deps.Client = backend
	a := tenantPrincipal("11111111-1111-1111-1111-111111111111", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", identity.PermissionWorkivaRead)
	b := tenantPrincipal("22222222-2222-2222-2222-222222222222", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	seedTenantResource(t, deps.Store, a, "owned-sp", "owned-sh", "")
	seedTenantResource(t, deps.Store, b, "foreign-sp", "foreign-sh", "")

	result := callEntraTool(t, deps, ListSpreadsheets(), a, map[string]any{})
	if result.IsError {
		t.Fatalf("list failed: %+v", result.Content)
	}
	books := structuredContent(t, result)["spreadsheets"].([]any)
	if len(books) != 1 || books[0].(map[string]any)["id"] != "owned-sp" {
		t.Fatalf("tenant live listing = %+v, want owned-sp only", books)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.listSheetCalls) != 1 || backend.listSheetCalls[0] != "owned-sp" {
		t.Fatalf("sheet discovery calls = %+v, want owned-sp only", backend.listSheetCalls)
	}
}

func TestConfirmationTokenIsThirtyTwoRandomURLSafeBytes(t *testing.T) {
	deps, _ := authorizationTestDeps(t)
	backend := &tenantIsolationBackend{}
	deps.Client = backend
	p := tenantPrincipal("11111111-1111-1111-1111-111111111111", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", identity.PermissionWorkivaWritePreview)
	seedTenantResource(t, deps.Store, p, "sp", "sh", "energy")
	first := callEntraTool(t, deps, UpdateField(), p, map[string]any{"name": "energy", "value": "2"})
	second := callEntraTool(t, deps, UpdateField(), p, map[string]any{"name": "energy", "value": "2"})
	for i, result := range []*mcp.CallToolResult{first, second} {
		if result.IsError {
			t.Fatalf("stage %d failed: %+v", i+1, result.Content)
		}
		token := structuredContent(t, result)["confirm_token"].(string)
		raw, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(raw) != 32 {
			t.Fatalf("token %d decoded bytes=%d err=%v token=%q", i+1, len(raw), err, token)
		}
	}
	if structuredContent(t, first)["confirm_token"] == structuredContent(t, second)["confirm_token"] {
		t.Fatal("two staged writes returned the same random token")
	}
}

func TestAliceCannotConsumeBobsTokenAndBobCanStillUseIt(t *testing.T) {
	deps, _ := authorizationTestDeps(t)
	backend := &tenantIsolationBackend{}
	deps.Client = backend
	tenant := "11111111-1111-1111-1111-111111111111"
	bobPreview := tenantPrincipal(tenant, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", identity.PermissionWorkivaWritePreview)
	bobConfirm := tenantPrincipal(tenant, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", identity.PermissionWorkivaWriteConfirm)
	aliceConfirm := tenantPrincipal(tenant, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", identity.PermissionWorkivaWriteConfirm)
	seedTenantResource(t, deps.Store, bobPreview, "sp", "sh", "energy")
	stage := callEntraTool(t, deps, UpdateField(), bobPreview, map[string]any{"name": "energy", "value": "2"})
	if stage.IsError {
		t.Fatalf("Bob stage failed: %+v", stage.Content)
	}
	token := structuredContent(t, stage)["confirm_token"].(string)
	alice := callEntraTool(t, deps, UpdateField(), aliceConfirm, map[string]any{"name": "energy", "value": "2", "confirm_token": token})
	if !alice.IsError {
		t.Fatalf("Alice consumed Bob's token: %+v", alice.StructuredContent)
	}
	bob := callEntraTool(t, deps, UpdateField(), bobConfirm, map[string]any{"name": "energy", "value": "2", "confirm_token": token})
	if bob.IsError || structuredContent(t, bob)["status"] != "written" {
		t.Fatalf("Bob could not use intact token: %+v", bob.Content)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.mutationCalls != 1 {
		t.Fatalf("mutation calls=%d, want 1", backend.mutationCalls)
	}
}

// A foreign tenant presenting another tenant's confirmation token fails
// before any Workiva call, and the owner's token stays intact and usable.
func TestForeignTenantConfirmationFailsAndLeavesOwnersTokenIntact(t *testing.T) {
	deps, _ := authorizationTestDeps(t)
	backend := &tenantIsolationBackend{}
	deps.Client = backend
	tenantA := "11111111-1111-1111-1111-111111111111"
	aPreview := tenantPrincipal(tenantA, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", identity.PermissionWorkivaWritePreview)
	aConfirm := tenantPrincipal(tenantA, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", identity.PermissionWorkivaWriteConfirm)
	bConfirm := tenantPrincipal("22222222-2222-2222-2222-222222222222", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", identity.PermissionWorkivaWriteConfirm)
	seedTenantResource(t, deps.Store, aPreview, "sp", "sh", "energy")

	stage := callEntraTool(t, deps, UpdateField(), aPreview, map[string]any{"name": "energy", "value": "2"})
	if stage.IsError {
		t.Fatalf("stage failed: %+v", stage.Content)
	}
	token := structuredContent(t, stage)["confirm_token"].(string)

	backend.mu.Lock()
	readsBefore, mutationsBefore := backend.readCalls, backend.mutationCalls
	backend.mu.Unlock()

	foreign := callEntraTool(t, deps, UpdateField(), bConfirm, map[string]any{"name": "energy", "value": "2", "confirm_token": token})
	if !foreign.IsError {
		t.Fatalf("foreign tenant confirmation succeeded: %+v", foreign.StructuredContent)
	}
	backend.mu.Lock()
	if backend.readCalls != readsBefore || backend.mutationCalls != mutationsBefore {
		t.Fatalf("foreign confirmation reached Workiva: reads %d->%d mutations %d->%d",
			readsBefore, backend.readCalls, mutationsBefore, backend.mutationCalls)
	}
	backend.mu.Unlock()

	owner := callEntraTool(t, deps, UpdateField(), aConfirm, map[string]any{"name": "energy", "value": "2", "confirm_token": token})
	if owner.IsError || structuredContent(t, owner)["status"] != "written" {
		t.Fatalf("owner could not use the intact token: %+v", owner.Content)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.mutationCalls != 1 {
		t.Fatalf("mutation calls=%d, want exactly 1 (the owner's)", backend.mutationCalls)
	}
}

func TestConfirmationRevalidatesCurrentPermissionWithoutConsumingToken(t *testing.T) {
	deps, _ := authorizationTestDeps(t)
	backend := &tenantIsolationBackend{}
	deps.Client = backend
	tenant := "11111111-1111-1111-1111-111111111111"
	without := tenantPrincipal(tenant, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	with := tenantPrincipal(tenant, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", identity.PermissionWorkivaWriteConfirm)
	seedTenantResource(t, deps.Store, with, "sp", "sh", "energy")
	ctx := identity.ContextWithPrincipal(context.Background(), with)
	field, err := deps.Store.GetField(ctx, "energy")
	if err != nil || field == nil {
		t.Fatalf("field=%+v err=%v", field, err)
	}
	actor := with.AuditActor()
	if err := deps.Store.CreatePendingWrite(ctx, mapping.PendingWrite{
		Token: "permission-token", Actor: actor, RequiredPermission: string(identity.PermissionWorkivaWriteConfirm),
		FieldID: field.ID, FieldName: field.Name, Value: "2", SpreadsheetID: field.SpreadsheetID, SheetID: field.SheetID,
		CellRange: field.CellRange, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	input := updateFieldInput{Name: field.Name, Value: "2", ConfirmToken: "permission-token"}
	if _, _, err := executeConfirmedWrite(identity.ContextWithPrincipal(context.Background(), without), deps, field, input, actor); err == nil {
		t.Fatal("confirmation succeeded after permission removal")
	}
	backend.mu.Lock()
	if backend.mutationCalls != 0 {
		t.Fatalf("unauthorized confirmation mutations=%d", backend.mutationCalls)
	}
	backend.mu.Unlock()
	if _, out, err := executeConfirmedWrite(identity.ContextWithPrincipal(context.Background(), with), deps, field, input, actor); err != nil || out.Status != "written" {
		t.Fatalf("authorized owner could not use intact token: out=%+v err=%v", out, err)
	}
}

func TestOneHundredConcurrentConfirmsCauseOneMutation(t *testing.T) {
	deps, _ := authorizationTestDeps(t)
	backend := &tenantIsolationBackend{}
	deps.Client = backend
	seedTenantResource(t, deps.Store, identity.Principal{}, "sp", "sh", "energy")
	field, err := deps.Store.GetField(context.Background(), "energy")
	if err != nil || field == nil {
		t.Fatalf("field=%+v err=%v", field, err)
	}
	const actor = "api-key-test-actor"
	const token = "concurrent-token"
	if err := deps.Store.CreatePendingWrite(context.Background(), mapping.PendingWrite{
		Token: token, Actor: actor, RequiredPermission: string(identity.PermissionWorkivaWriteConfirm), FieldID: field.ID,
		FieldName: field.Name, Value: "2", SpreadsheetID: field.SpreadsheetID, SheetID: field.SheetID, CellRange: field.CellRange,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, out, err := executeConfirmedWrite(context.Background(), deps, field, updateFieldInput{Name: field.Name, Value: "2", ConfirmToken: token}, actor)
			if err == nil && out.Status == "written" {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	backend.mu.Lock()
	mutations := backend.mutationCalls
	backend.mu.Unlock()
	if successes.Load() != 1 || mutations != 1 {
		t.Fatalf("concurrent confirmations successes=%d mutations=%d, want 1/1", successes.Load(), mutations)
	}
}
