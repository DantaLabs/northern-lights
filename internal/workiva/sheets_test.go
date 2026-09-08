package workiva

import (
	"context"
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

	data, err := c.GetSheetData(context.Background(), "s-1", "sh-1", "B3:D10", []string{"value", "calculatedValue"})
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
	if cell.Value == nil || *cell.Value != "=1+1" {
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

func TestGetSheetDataSendsQueryParams(t *testing.T) {
	var gotRange, gotFields string
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.URL.Query().Get("$cellrange")
		gotFields = r.URL.Query().Get("$fields")
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `{"cells":[]}`); err != nil {
			t.Errorf("write sheetdata response: %v", err)
		}
	}))

	_, err := c.GetSheetData(context.Background(), "s-1", "sh-1", "B3:D10", []string{"value", "calculatedValue"})
	if err != nil {
		t.Fatalf("GetSheetData: %v", err)
	}
	if gotRange != "B3:D10" {
		t.Errorf("$cellrange = %q, want B3:D10", gotRange)
	}
	if gotFields != "value,calculatedValue" {
		t.Errorf("$fields = %q, want value,calculatedValue", gotFields)
	}
}

func TestGetSheetDataOmitsEmptyQueryParams(t *testing.T) {
	var rawQuery string
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `{"cells":[]}`); err != nil {
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
	page2 := `{"cells":[[{"value":"page2"}]]}`
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("$page") == "2" {
			if _, err := fmt.Fprint(w, page2); err != nil {
				t.Errorf("write sheetdata response: %v", err)
			}
			return
		}
		next := "http://" + r.Host + "/spreadsheets/s-1/sheets/sh-1/sheetdata?$page=2"
		if _, err := fmt.Fprintf(w, `{"cells":[[{"value":"p1a"}],[{"value":"p1b"}]],"@nextLink":%q}`, next); err != nil {
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
	if data.Cells[2][0].Value == nil || *data.Cells[2][0].Value != "page2" {
		t.Errorf("Cells[2][0].Value = %v, want page2", data.Cells[2][0].Value)
	}
}

func TestGetRangeValues(t *testing.T) {
	var gotPath string
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `[[1,"a"],[null,true]]`); err != nil {
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
	rows, ok := values.([]any)
	if !ok {
		t.Fatalf("values type = %T, want []any", values)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
}

func TestGetSheetDataCapsPaginationAtFiftyPages(t *testing.T) {
	var apiCalls atomic.Int32
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		next := "http://" + r.Host + "/spreadsheets/s-1/sheets/sh-1/sheetdata?$page=next"
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprintf(w, `{"cells":[[{"value":"x"}]],"@nextLink":%q}`, next); err != nil {
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
