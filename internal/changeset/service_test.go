package changeset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/demoenv"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

type fakeBackend struct {
	values                       map[string]string
	reads, updates, polls        int
	mismatch, updateErr, pollErr bool
	retryAfter                   time.Duration
}

func (*fakeBackend) ListSpreadsheets(context.Context) ([]workiva.Spreadsheet, error) { return nil, nil }
func (*fakeBackend) ListSheets(context.Context, string) ([]workiva.Sheet, error)     { return nil, nil }
func (f *fakeBackend) GetSheetData(_ context.Context, sp, sh, ref string, _ []string) (*workiva.SheetData, error) {
	f.reads++
	r, err := workiva.A1ToRange(ref)
	if err != nil {
		return nil, err
	}
	var grid [][]workiva.Cell
	for row := r.StartRow; row <= r.StopRow; row++ {
		var cells []workiva.Cell
		for col := r.StartCol; col <= r.StopCol; col++ {
			value := f.values[fmt.Sprintf("%s/%s/%d/%d", sp, sh, row, col)]
			if f.mismatch && f.updates > 0 && row == 0 {
				value = "different"
			}
			cells = append(cells, workiva.Cell{Value: value})
		}
		grid = append(grid, cells)
	}
	return &workiva.SheetData{Range: &r, Cells: grid}, nil
}
func (f *fakeBackend) UpdateSheetWithRetryAfter(_ context.Context, sp, sh string, update workiva.SheetUpdate) (string, time.Duration, error) {
	f.updates++
	raw, err := json.Marshal(update)
	if err != nil {
		return "", 0, err
	}
	var payload struct {
		EditCells struct {
			Cells   []workiva.CellEdit
			Options struct{ ApplyEnteredInScaling, SkipEditMergeChildren *bool }
		}
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", 0, err
	}
	if payload.EditCells.Options.ApplyEnteredInScaling == nil || *payload.EditCells.Options.ApplyEnteredInScaling || payload.EditCells.Options.SkipEditMergeChildren == nil || *payload.EditCells.Options.SkipEditMergeChildren {
		return "", 0, fmt.Errorf("literal options missing")
	}
	for _, cell := range payload.EditCells.Cells {
		f.values[fmt.Sprintf("%s/%s/%d/%d", sp, sh, cell.Row, cell.Column)] = fmt.Sprint(cell.Value)
	}
	if f.updateErr {
		return "", 0, fmt.Errorf("connection lost after acceptance")
	}
	return "https://provider.invalid/operations/" + sh, f.retryAfter, nil
}
func (f *fakeBackend) WaitOperationWithInitialRetryAfter(_ context.Context, _ string, delay time.Duration) (string, error) {
	f.polls++
	if delay != f.retryAfter {
		return "", fmt.Errorf("Retry-After not forwarded")
	}
	if f.pollErr {
		return "", context.DeadlineExceeded
	}
	return "https://provider.invalid/resource", nil
}

func fixture(t *testing.T, count int) (*service, *fakeBackend, []Item) {
	t.Helper()
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	store, err := mapping.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{values: map[string]string{}, retryAfter: 3 * time.Second}
	var items []Item
	for i := 0; i < count; i++ {
		sh := fmt.Sprintf("sheet-%d", i%2)
		field, err := store.UpsertField(context.Background(), mapping.Field{Name: fmt.Sprintf("field-%02d", i), SpreadsheetID: "spreadsheet", SheetID: sh, CellRange: fmt.Sprintf("A%d", i+1)})
		if err != nil {
			t.Fatal(err)
		}
		backend.values[fmt.Sprintf("spreadsheet/%s/%d/0", sh, i)] = fmt.Sprint(i)
		items = append(items, Item{FieldName: field.Name, Value: fmt.Sprint(i + 100), Source: "recipe"})
	}
	svc := New(demoenv.Env{DB: db, Store: store, Audit: log, Client: backend}).(*service)
	if svc.initErr != nil {
		t.Fatal(svc.initErr)
	}
	return svc, backend, items
}
func stage(t *testing.T, s *service, items []Item) Preview {
	t.Helper()
	p, err := s.Stage(context.Background(), "analyst", items)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestChangeSet_S1_InvalidItemsLeaveNoRows(t *testing.T) {
	for _, name := range []string{"unknown", "empty", "501 items", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			s, f, items := fixture(t, 1)
			switch name {
			case "unknown":
				items = []Item{{FieldName: "missing-a"}, {FieldName: "missing-b"}}
			case "empty":
				items = nil
			case "501 items":
				items = make([]Item, 501)
			case "duplicate":
				items = append(items, items[0])
			}
			_, err := s.Stage(context.Background(), "analyst", items)
			if err == nil {
				t.Fatal("invalid list accepted")
			}
			if name == "unknown" && (!strings.Contains(err.Error(), "missing-a") || !strings.Contains(err.Error(), "missing-b")) {
				t.Fatal(err)
			}
			var count int
			if err := s.env.DB.QueryRow(`SELECT count(*) FROM change_set_pending`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 || f.reads != 0 {
				t.Fatalf("rows=%d reads=%d", count, f.reads)
			}
		})
	}
}
func TestChangeSet_S2_AllowlistBeforeReads(t *testing.T) {
	s, f, items := fixture(t, 2)
	s.env.Cfg = &config.Config{AllowedResources: map[string][]string{"spreadsheet": {"sheet-0"}}}
	if _, err := s.Stage(context.Background(), "analyst", items); err == nil {
		t.Fatal("disallowed sheet accepted")
	}
	if f.reads != 0 {
		t.Fatal("read before all targets checked")
	}
}
func TestChangeSet_S3_ThirtyFieldsDigestAndPreview(t *testing.T) {
	s, f, items := fixture(t, 30)
	p := stage(t, s, items)
	if len(p.Items) != 30 || p.Token == "" || p.ID == "" || f.reads != 30 {
		t.Fatalf("preview=%+v reads=%d", p, f.reads)
	}
	for i, item := range p.Items {
		if item.Before != fmt.Sprint(i) {
			t.Fatalf("before: %+v", item)
		}
	}
	var token, actor, permission, payload string
	var expiry int64
	if err := s.env.DB.QueryRow(`SELECT token_digest,actor,required_permission,expires_at,items_json FROM change_set_pending`).Scan(&token, &actor, &permission, &expiry, &payload); err != nil {
		t.Fatal(err)
	}
	if token != digest(p.Token) || strings.Contains(payload, p.Token) || actor != "analyst" || permission != confirmPermission || expiry != p.ExpiresAt.UnixNano() || time.Until(p.ExpiresAt) > 10*time.Minute || time.Until(p.ExpiresAt) < 9*time.Minute {
		t.Fatal("incorrect persisted bindings")
	}
	var staged []stagedItem
	if err := json.Unmarshal([]byte(payload), &staged); err != nil {
		t.Fatal(err)
	}
	for _, item := range staged {
		if item.FieldID == 0 || item.Range == "" || item.SpreadsheetID == "" || item.SheetID == "" {
			t.Fatalf("unbound item: %+v", item)
		}
	}
}
func TestNewWithoutDependenciesFailsClosed(t *testing.T) {
	s := New(demoenv.Env{})
	if _, err := s.Stage(context.Background(), "actor", []Item{{FieldName: "f"}}); err == nil {
		t.Fatal("Stage succeeded")
	}
	if _, err := s.Confirm(context.Background(), "actor", "id", "token"); err == nil {
		t.Fatal("Confirm succeeded")
	}
}

func TestChangeSet_S4_DistinctConfirmationErrors(t *testing.T) {
	s, f, items := fixture(t, 1)
	p := stage(t, s, items)
	ctx := context.Background()
	check := func(actor, id, token string, context context.Context, want error) {
		t.Helper()
		if _, err := s.Confirm(context, actor, id, token); !errors.Is(err, want) {
			t.Fatalf("got %v want %v", err, want)
		}
		if f.updates != 0 {
			t.Fatal("rejected confirmation wrote")
		}
	}
	check("analyst", p.ID, "wrong", ctx, ErrUnknownToken)
	check("other", p.ID, p.Token, ctx, ErrForeignActor)
	denied := identity.ContextWithPrincipal(ctx, identity.Principal{})
	check("analyst", p.ID, p.Token, denied, ErrMissingPermission)
	if _, err := s.consume(ctx, "analyst", p.ID, p.Token); err != nil {
		t.Fatal(err)
	}
	check("analyst", p.ID, p.Token, ctx, ErrReusedToken)
	p = stage(t, s, items)
	if _, err := s.env.DB.Exec(`UPDATE change_set_pending SET expires_at=0 WHERE id=?`, p.ID); err != nil {
		t.Fatal(err)
	}
	check("analyst", p.ID, p.Token, ctx, ErrExpiredToken)
}

func TestChangeSet_S5_MappingChangedBeforeConfirmation(t *testing.T) {
	s, f, items := fixture(t, 2)
	p := stage(t, s, items)
	field, err := s.env.Store.GetField(context.Background(), items[1].FieldName)
	if err != nil {
		t.Fatal(err)
	}
	field.CellRange = "B20"
	if _, err := s.env.Store.UpsertField(context.Background(), *field); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Confirm(context.Background(), "analyst", p.ID, p.Token); !errors.Is(err, ErrMappingChanged) {
		t.Fatalf("got %v", err)
	}
	if f.updates != 0 {
		t.Fatal("changed mapping wrote")
	}
	if _, err := s.Confirm(context.Background(), "analyst", p.ID, p.Token); !errors.Is(err, ErrReusedToken) {
		t.Fatal("token not consumed")
	}
}

func TestChangeSet_ConcurrentConsumptionAndRestart(t *testing.T) {
	s, _, items := fixture(t, 1)
	p := stage(t, s, items)
	restarted := New(s.env).(*service)
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := restarted.consume(context.Background(), "analyst", p.ID, p.Token)
			if err == nil {
				winners.Add(1)
			} else if !errors.Is(err, ErrReusedToken) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("%d consumers", winners.Load())
	}
}

func TestChangeSet_S6_TwoSheetsOneUpdateEach(t *testing.T) {
	s, f, items := fixture(t, 30)
	p := stage(t, s, items)
	out, err := s.Confirm(context.Background(), "analyst", p.ID, p.Token)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" || len(out.Items) != 30 || f.updates != 2 || f.polls != 2 || f.reads != 60 {
		t.Fatalf("result=%+v updates=%d polls=%d reads=%d", out, f.updates, f.polls, f.reads)
	}
	for _, item := range out.Items {
		if item.Status != "completed" || item.ReadBack != item.After || item.AuditID == "" || item.OperationURL == "" {
			t.Fatalf("item=%+v", item)
		}
	}
	rows, err := s.env.Audit.Recent(context.Background(), 100, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 30 {
		t.Fatalf("%d audit rows", len(rows))
	}
	for _, row := range rows {
		if row.Actor != "analyst" || !strings.Contains(row.AfterJSON, p.ID) || !strings.Contains(row.AfterJSON, "field-") || row.WorkivaOpURL == "" || row.BeforeJSON == "" {
			t.Fatalf("audit=%+v", row)
		}
	}
	if err := s.env.Audit.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, sh := range []string{"sheet-0", "sheet-1"} {
		cells, err := s.env.Store.GetCachedCells(context.Background(), "spreadsheet", sh, time.Hour)
		if err != nil || len(cells) != 15 {
			t.Fatalf("cache len=%d err=%v", len(cells), err)
		}
		for _, cell := range cells {
			if !strings.HasPrefix(cell.Value, "1") {
				t.Fatal("cache not refreshed")
			}
		}
	}
}

func TestChangeSet_S7_ReadBackMismatch(t *testing.T) {
	s, f, items := fixture(t, 3)
	p := stage(t, s, items)
	f.mismatch = true
	out, err := s.Confirm(context.Background(), "analyst", p.ID, p.Token)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "partial" || out.Items[0].Status != "mismatch" || out.Items[1].Status != "completed" || out.Items[2].Status != "completed" {
		t.Fatalf("result=%+v", out)
	}
	cells, err := s.env.Store.GetCachedCells(context.Background(), "spreadsheet", "sheet-0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if cells[0].Value != "different" {
		t.Fatal("cache guessed intended value")
	}
}

func TestChangeSet_S8_AcceptedUpdateTransportError(t *testing.T) {
	for _, mode := range []string{"submit transport", "poll timeout"} {
		t.Run(mode, func(t *testing.T) {
			s, f, items := fixture(t, 3)
			p := stage(t, s, items)
			f.updateErr = mode == "submit transport"
			f.pollErr = mode == "poll timeout"
			out, err := s.Confirm(context.Background(), "analyst", p.ID, p.Token)
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != "reconciliation_required" || f.updates != 1 || out.Items[0].Status != "reconciliation_required" || out.Items[2].Status != "reconciliation_required" || out.Items[1].Status != "not_submitted" {
				t.Fatalf("result=%+v updates=%d", out, f.updates)
			}
			if _, err := s.Confirm(context.Background(), "analyst", p.ID, p.Token); !errors.Is(err, ErrReusedToken) {
				t.Fatal(err)
			}
			if f.updates != 1 {
				t.Fatal("unknown write retried")
			}
		})
	}
}

func TestChangeSet_MultiCellVerification(t *testing.T) {
	s, f, items := fixture(t, 1)
	field, err := s.env.Store.GetField(context.Background(), items[0].FieldName)
	if err != nil {
		t.Fatal(err)
	}
	field.CellRange = "A1:B2"
	if _, err := s.env.Store.UpsertField(context.Background(), *field); err != nil {
		t.Fatal(err)
	}
	p := stage(t, s, items)
	f.mismatch = true
	out, err := s.Confirm(context.Background(), "analyst", p.ID, p.Token)
	if err != nil || out.Items[0].Status != "mismatch" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	cells, err := s.env.Store.GetCachedCells(context.Background(), "spreadsheet", "sheet-0", time.Hour)
	if err != nil || len(cells) != 4 {
		t.Fatalf("cache len=%d err=%v", len(cells), err)
	}
}
