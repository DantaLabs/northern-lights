package workiva

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestGetSheetDataDecodesFixture(t *testing.T) {
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/spreadsheets/s-1/sheets/sh-1/sheetdata" {
			t.Errorf("request path = %q, want /spreadsheets/s-1/sheets/sh-1/sheetdata", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(loadFixture(t, "sheetdata_response.json")); err != nil {
			t.Errorf("write sheetdata response: %v", err)
		}
	}))

	data, err := c.GetSheetData(context.Background(), "s-1", "sh-1", "B3:D10", []string{"cells.value", "cells.calculatedValue"})
	if err != nil {
		t.Fatalf("GetSheetData: %v", err)
	}

	if data.Range == nil {
		t.Fatal("Range is nil")
	}
	if data.Range.StartRow != 2 || data.Range.StartCol != 1 || data.Range.StopRow != 3 || data.Range.StopCol != 2 {
		t.Errorf("Range = %+v, want startRow 2 startColumn 1 stopRow 3 stopColumn 2", *data.Range)
	}

	if len(data.Cells) != 2 {
		t.Fatalf("len(Cells) = %d, want 2", len(data.Cells))
	}
	if len(data.Cells[0]) != 2 {
		t.Fatalf("len(Cells[0]) = %d, want 2", len(data.Cells[0]))
	}

	cell := data.Cells[0][1]
	if cell.Value != "=1+1" {
		t.Errorf("Cells[0][1].Value = %v, want \"=1+1\"", cell.Value)
	}
	if cell.CalculatedValue != float64(2) {
		t.Errorf("Cells[0][1].CalculatedValue = %v (%T), want 2", cell.CalculatedValue, cell.CalculatedValue)
	}

	if got := data.Cells[1][0].CalculatedValue; got != float64(42) {
		t.Errorf("Cells[1][0].CalculatedValue = %v, want 42", got)
	}
	if len(data.Merges) != 1 {
		t.Errorf("len(Merges) = %d, want 1", len(data.Merges))
	}
	if len(data.ColumnMetadata) != 1 {
		t.Errorf("len(ColumnMetadata) = %d, want 1", len(data.ColumnMetadata))
	}
	if len(data.RowMetadata) != 1 {
		t.Errorf("len(RowMetadata) = %d, want 1", len(data.RowMetadata))
	}
}

func TestCellDecodesOfficialScalarValues(t *testing.T) {
	var response sheetDataResponse
	if err := json.Unmarshal([]byte(`{"data":{"cells":[[
		{"value":"literal"},
		{"value":125000},
		{"value":true},
		{"value":null},
		{"value":"=1+1","calculatedValue":2}
	]]}}`), &response); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	cells := response.Data.Cells[0]
	if cells[0].Value != "literal" || cells[1].Value != float64(125000) || cells[2].Value != true || cells[3].Value != nil {
		t.Errorf("decoded values = %#v, want string, number, boolean, nil", []any{cells[0].Value, cells[1].Value, cells[2].Value, cells[3].Value})
	}
	if cells[4].Value != "=1+1" || cells[4].CalculatedValue != float64(2) {
		t.Errorf("formula cell = %#v, want formula plus calculated value", cells[4])
	}
}

func TestGetSheetDataSendsQueryParams(t *testing.T) {
	var gotRange, gotFields string
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.URL.Query().Get("$cellrange")
		gotFields = r.URL.Query().Get("$fields")
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `{"data":{"cells":[]}}`); err != nil {
			t.Errorf("write sheetdata response: %v", err)
		}
	}))

	_, err := c.GetSheetData(context.Background(), "s-1", "sh-1", "B3:D10", []string{"cells.value", "cells.calculatedValue"})
	if err != nil {
		t.Fatalf("GetSheetData: %v", err)
	}
	if gotRange != "B3:D10" {
		t.Errorf("$cellrange = %q, want B3:D10", gotRange)
	}
	if gotFields != "cells.value,cells.calculatedValue" {
		t.Errorf("$fields = %q, want cells.value,cells.calculatedValue", gotFields)
	}
}

func TestGetSheetDataOmitsEmptyQueryParams(t *testing.T) {
	var rawQuery string
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `{"data":{"cells":[]}}`); err != nil {
			t.Errorf("write sheetdata response: %v", err)
		}
	}))

	_, err := c.GetSheetData(context.Background(), "s-1", "sh-1", "", nil)
	if err != nil {
		t.Fatalf("GetSheetData: %v", err)
	}
	if rawQuery != "" {
		t.Errorf("RawQuery = %q, want empty", rawQuery)
	}
}

func TestGetSheetDataFollowsNextLink(t *testing.T) {
	page2 := `{"data":{"cells":[[{"value":"page2"}]]}}`
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("$page") == "2" {
			if _, err := fmt.Fprint(w, page2); err != nil {
				t.Errorf("write sheetdata response: %v", err)
			}
			return
		}
		next := "http://" + r.Host + "/spreadsheets/s-1/sheets/sh-1/sheetdata?$page=2"
		if _, err := fmt.Fprintf(w, `{"data":{"cells":[[{"value":"p1a"}],[{"value":"p1b"}]]},"@nextLink":%q}`, next); err != nil {
			t.Errorf("write sheetdata response: %v", err)
		}
	}))

	data, err := c.GetSheetData(context.Background(), "s-1", "sh-1", "", nil)
	if err != nil {
		t.Fatalf("GetSheetData: %v", err)
	}
	if len(data.Cells) != 3 {
		t.Fatalf("len(Cells) = %d, want 3 (two pages concatenated)", len(data.Cells))
	}
	if data.Cells[2][0].Value != "page2" {
		t.Errorf("Cells[2][0].Value = %v, want page2", data.Cells[2][0].Value)
	}
}

func TestGetRangeValues(t *testing.T) {
	var gotPath string
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `{"data":[{"range":"A1:D10","values":[[1,"a"],[null,true]]}]}`); err != nil {
			t.Errorf("write values response: %v", err)
		}
	}))

	values, err := c.GetRangeValues(context.Background(), "s-1", "sh-1", "A1:D10")
	if err != nil {
		t.Fatalf("GetRangeValues: %v", err)
	}
	if gotPath != "/spreadsheets/s-1/sheets/sh-1/values/A1:D10" {
		t.Errorf("request path = %q, want /spreadsheets/s-1/sheets/sh-1/values/A1:D10", gotPath)
	}
	if len(values.Data) != 1 || values.Data[0].Range != "A1:D10" {
		t.Fatalf("values = %+v, want one typed range result", values.Data)
	}
	if len(values.Data[0].Values) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(values.Data[0].Values))
	}
}

func TestGetSheetDataCapsPaginationAtFiftyPages(t *testing.T) {
	var apiCalls atomic.Int32
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		next := "http://" + r.Host + "/spreadsheets/s-1/sheets/sh-1/sheetdata?$page=next"
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprintf(w, `{"data":{"cells":[[{"value":"x"}]]},"@nextLink":%q}`, next); err != nil {
			t.Errorf("write sheetdata response: %v", err)
		}
	}))

	_, err := c.GetSheetData(context.Background(), "s-1", "sh-1", "", nil)
	if err == nil {
		t.Fatal("expected error after pagination cap, got nil")
	}
	if got := apiCalls.Load(); got != 50 {
		t.Errorf("API calls = %d, want 50", got)
	}
	if !strings.Contains(err.Error(), "50") {
		t.Errorf("error = %q, want it to name the 50 page cap", err.Error())
	}
}

func TestListSpreadsheetsDecodesMetadataAndFollowsNextLink(t *testing.T) {
	var paths []string
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/spreadsheets" && r.URL.Query().Get("page") == "" {
			if _, err := fmt.Fprintf(w, `{"data":[{"id":"sp-1","name":"Report","template":true,"created":"2026-01-01T00:00:00Z","modified":"2026-01-02T00:00:00Z"}],"@nextLink":%q}`, "http://"+r.Host+"/spreadsheets?page=2"); err != nil {
				t.Errorf("write spreadsheets response: %v", err)
			}
		} else if r.URL.Path == "/spreadsheets" && r.URL.Query().Get("page") == "2" {
			if _, err := fmt.Fprint(w, `{"data":[{"id":"sp-2","name":"Second"}]}`); err != nil {
				t.Errorf("write spreadsheets page 2 response: %v", err)
			}
		} else {
			http.NotFound(w, r)
		}
	}))

	got, err := c.ListSpreadsheets(context.Background())
	if err != nil {
		t.Fatalf("ListSpreadsheets: %v", err)
	}
	if len(got) != 2 || got[0].ID != "sp-1" || got[0].Name != "Report" || !got[0].Template || got[0].Created != "2026-01-01T00:00:00Z" || got[1].ID != "sp-2" {
		t.Errorf("spreadsheets = %+v, want decoded paginated metadata", got)
	}
	if len(paths) != 2 || paths[1] != "/spreadsheets?page=2" {
		t.Errorf("request paths = %v, want both spreadsheet pages", paths)
	}
}

func TestListSheetsDecodesMetadata(t *testing.T) {
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/spreadsheets/sp-1/sheets" {
			t.Errorf("request path = %q, want /spreadsheets/sp-1/sheets", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `{"data":[{"id":"sh-1","name":"Energy","index":3}]}`); err != nil {
			t.Errorf("write sheets response: %v", err)
		}
	}))

	got, err := c.ListSheets(context.Background(), "sp-1")
	if err != nil {
		t.Fatalf("ListSheets: %v", err)
	}
	if len(got) != 1 || got[0].ID != "sh-1" || got[0].Name != "Energy" || got[0].Index != 3 {
		t.Errorf("sheets = %+v, want sh-1/Energy/index 3", got)
	}
}

func TestListSheetsDecodesMetadataAndCapsPagination(t *testing.T) {
	var apiCalls atomic.Int32
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		next := "http://" + r.Host + "/spreadsheets/sp-1/sheets?page=next"
		if _, err := fmt.Fprintf(w, `{"data":[{"id":"sh-%d","name":"Sheet","index":%d}],"@nextLink":%q}`, apiCalls.Load(), apiCalls.Load(), next); err != nil {
			t.Errorf("write sheets response: %v", err)
		}
	}))

	got, err := c.ListSheets(context.Background(), "sp-1")
	if err == nil {
		t.Fatal("expected error after pagination cap, got nil")
	}
	if len(got) != 0 {
		t.Errorf("sheets = %+v, want nil on pagination failure", got)
	}
	if apiCalls.Load() != 50 {
		t.Errorf("API calls = %d, want 50", apiCalls.Load())
	}
	if !strings.Contains(err.Error(), "50") {
		t.Errorf("error = %q, want it to name the 50 page cap", err.Error())
	}
}
