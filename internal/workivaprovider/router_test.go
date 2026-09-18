package workivaprovider

import (
	"context"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/workiva"
)

type fakeBackend struct {
	name             string
	reads            int
	listSheetsCalls  int
	getSheetCalls    int
	writes           int
	polls            int
	spreadsheetValue string
}

func (f *fakeBackend) ListSpreadsheets(context.Context) ([]workiva.Spreadsheet, error) {
	f.reads++
	return []workiva.Spreadsheet{{ID: f.spreadsheetValue}}, nil
}

func (f *fakeBackend) ListSheets(context.Context, string) ([]workiva.Sheet, error) {
	f.reads++
	f.listSheetsCalls++
	return nil, nil
}

func (f *fakeBackend) GetSheetData(context.Context, string, string, string, []string) (*workiva.SheetData, error) {
	f.reads++
	f.getSheetCalls++
	return &workiva.SheetData{}, nil
}

func (f *fakeBackend) UpdateSheetWithRetryAfter(context.Context, string, string, workiva.SheetUpdate) (string, time.Duration, error) {
	f.writes++
	return f.name + "-operation", 0, nil
}

func (f *fakeBackend) WaitOperationWithInitialRetryAfter(context.Context, string, time.Duration) (string, error) {
	f.polls++
	return f.name + "-resource", nil
}

func TestRouterUsesPrimaryBackendByDefault(t *testing.T) {
	primary := &fakeBackend{name: "rest", spreadsheetValue: "rest-spreadsheet"}
	router := NewRouter(primary)

	spreadsheets, err := router.ListSpreadsheets(context.Background())
	if err != nil {
		t.Fatalf("ListSpreadsheets: %v", err)
	}
	if got := spreadsheets[0].ID; got != "rest-spreadsheet" {
		t.Fatalf("spreadsheet ID = %q, want rest-spreadsheet", got)
	}
	if primary.reads != 1 {
		t.Fatalf("primary reads = %d, want 1", primary.reads)
	}
}

func TestRouterCanReplaceReadsWithoutReplacingWrites(t *testing.T) {
	primary := &fakeBackend{name: "rest", spreadsheetValue: "rest-spreadsheet"}
	official := &fakeBackend{name: "official-mcp", spreadsheetValue: "official-spreadsheet"}
	router := NewRouter(primary, WithReadBackend(official))

	spreadsheets, err := router.ListSpreadsheets(context.Background())
	if err != nil {
		t.Fatalf("ListSpreadsheets: %v", err)
	}
	if got := spreadsheets[0].ID; got != "official-spreadsheet" {
		t.Fatalf("spreadsheet ID = %q, want official-spreadsheet", got)
	}
	if _, err := router.ListSheets(context.Background(), "sp"); err != nil {
		t.Fatalf("ListSheets: %v", err)
	}
	if _, err := router.GetSheetData(context.Background(), "sp", "sh", "A1", []string{"cells.value"}); err != nil {
		t.Fatalf("GetSheetData: %v", err)
	}

	opURL, _, err := router.UpdateSheetWithRetryAfter(context.Background(), "sp", "sh", workiva.NewEditCellsUpdate([]workiva.CellEdit{{Column: 0, Row: 0, Value: "test"}}))
	if err != nil {
		t.Fatalf("UpdateSheetWithRetryAfter: %v", err)
	}
	if opURL != "rest-operation" {
		t.Fatalf("operation URL = %q, want rest-operation", opURL)
	}
	if official.reads != 3 || official.listSheetsCalls != 1 || official.getSheetCalls != 1 || official.writes != 0 {
		t.Fatalf("official calls: reads=%d listSheets=%d getSheet=%d writes=%d, want 3,1,1,0", official.reads, official.listSheetsCalls, official.getSheetCalls, official.writes)
	}
	if primary.reads != 0 || primary.writes != 1 {
		t.Fatalf("primary calls: reads=%d writes=%d, want reads=0 writes=1", primary.reads, primary.writes)
	}
}

func TestRouterCanReplaceWritesWithoutReplacingReads(t *testing.T) {
	primary := &fakeBackend{name: "rest", spreadsheetValue: "rest-spreadsheet"}
	official := &fakeBackend{name: "official-mcp", spreadsheetValue: "official-spreadsheet"}
	router := NewRouter(primary, WithWriteBackend(official))

	spreadsheets, err := router.ListSpreadsheets(context.Background())
	if err != nil {
		t.Fatalf("ListSpreadsheets: %v", err)
	}
	if got := spreadsheets[0].ID; got != "rest-spreadsheet" {
		t.Fatalf("spreadsheet ID = %q, want rest-spreadsheet", got)
	}

	opURL, _, err := router.UpdateSheetWithRetryAfter(context.Background(), "sp", "sh", workiva.NewEditCellsUpdate([]workiva.CellEdit{{Column: 0, Row: 0, Value: "test"}}))
	if err != nil {
		t.Fatalf("UpdateSheetWithRetryAfter: %v", err)
	}
	if opURL != "official-mcp-operation" {
		t.Fatalf("operation URL = %q, want official-mcp-operation", opURL)
	}
	resourceURL, err := router.WaitOperationWithInitialRetryAfter(context.Background(), opURL, time.Second)
	if err != nil {
		t.Fatalf("WaitOperationWithInitialRetryAfter: %v", err)
	}
	if resourceURL != "official-mcp-resource" {
		t.Fatalf("resource URL = %q, want official-mcp-resource", resourceURL)
	}
	if primary.reads != 1 || primary.writes != 0 || primary.polls != 0 {
		t.Fatalf("primary calls: reads=%d writes=%d polls=%d, want 1,0,0", primary.reads, primary.writes, primary.polls)
	}
	if official.reads != 0 || official.writes != 1 || official.polls != 1 {
		t.Fatalf("official calls: reads=%d writes=%d polls=%d, want 0,1,1", official.reads, official.writes, official.polls)
	}
}

func TestRouterRejectsMissingPrimaryBackend(t *testing.T) {
	if _, err := NewRouterChecked(nil); err == nil {
		t.Fatal("NewRouterChecked(nil) should fail")
	}
}

func TestRouterRejectsTypedNilPrimaryBackend(t *testing.T) {
	var primary *fakeBackend
	if _, err := NewRouterChecked(primary); err == nil {
		t.Fatal("NewRouterChecked(typed nil) should fail")
	}
}

func TestRouterIgnoresTypedNilCapabilityOverrides(t *testing.T) {
	primary := &fakeBackend{name: "rest", spreadsheetValue: "rest-spreadsheet"}
	var missing *fakeBackend
	router := NewRouter(primary, WithReadBackend(missing), WithWriteBackend(missing))

	if _, err := router.ListSpreadsheets(context.Background()); err != nil {
		t.Fatalf("ListSpreadsheets: %v", err)
	}
	if _, _, err := router.UpdateSheetWithRetryAfter(context.Background(), "sp", "sh", workiva.NewEditCellsUpdate([]workiva.CellEdit{{Column: 0, Row: 0, Value: "test"}})); err != nil {
		t.Fatalf("UpdateSheetWithRetryAfter: %v", err)
	}
	if primary.reads != 1 || primary.writes != 1 {
		t.Fatalf("primary calls: reads=%d writes=%d, want 1,1", primary.reads, primary.writes)
	}
}
