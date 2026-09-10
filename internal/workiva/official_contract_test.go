package workiva

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGetRangeValuesDecodesOfficialEnvelopeAndPages(t *testing.T) {
	var requests []string
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			_, _ = fmt.Fprintf(w, `{"data":[{"range":"A1:C3","values":[["Field",125000,"kWh"]]}],"@nextLink":%q}`, "http://"+r.Host+"/spreadsheets/s-1/sheets/sh-1/values/A1:C3?page=2")
			return
		}
		_, _ = fmt.Fprint(w, `{"data":[{"range":"A4:C4","values":[["Total",225000,"kWh"]]}]}`)
	}))

	got, err := c.GetRangeValues(context.Background(), "s-1", "sh-1", "A1:C3")
	if err != nil {
		t.Fatalf("GetRangeValues: %v", err)
	}
	if len(got.Data) != 2 {
		t.Fatalf("len(data) = %d, want 2 pages", len(got.Data))
	}
	if got.Data[0].Range != "A1:C3" || got.Data[1].Range != "A4:C4" {
		t.Errorf("ranges = %q, %q", got.Data[0].Range, got.Data[1].Range)
	}
	if got.Data[1].Values[0][1] != float64(225000) {
		t.Errorf("second page values = %v, want 225000", got.Data[1].Values)
	}
	if len(requests) != 2 || !strings.Contains(requests[1], "page=2") {
		t.Errorf("requests = %v, want a second page request", requests)
	}
}

func TestGetRangeValuesUsesOfficialFixture(t *testing.T) {
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(loadFixture(t, "values_response.json"))
	}))

	got, err := c.GetRangeValues(context.Background(), "s-1", "sh-1", "A1:C3")
	if err != nil {
		t.Fatalf("GetRangeValues: %v", err)
	}
	if got.Data[0].Range != "A1:C3" || len(got.Data[0].Values) != 3 {
		t.Fatalf("decoded values = %+v, want official values response", got.Data)
	}
}

func TestSheetUpdateUsesOfficialPostUpdateAndNestedEditCells(t *testing.T) {
	var method, path string
	var body []byte
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		body, _ = ioReadAll(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"operationLocation":"/operations/op-1"}`)
	}))

	_, err := c.UpdateSheet(context.Background(), "s-1", "sh-1", NewEditCellsUpdate([]CellEdit{{Column: 0, Row: 0, Value: 42}}))
	if err != nil {
		t.Fatalf("UpdateSheet: %v", err)
	}
	if method != http.MethodPost || path != "/spreadsheets/s-1/sheets/sh-1/update" {
		t.Fatalf("request = %s %s, want POST .../update", method, path)
	}

	var payload struct {
		EditCells struct {
			Cells []struct {
				Column int `json:"column"`
				Row    int `json:"row"`
				Value  any `json:"value"`
			} `json:"cells"`
		} `json:"editCells"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if len(payload.EditCells.Cells) != 1 || payload.EditCells.Cells[0].Column != 0 || payload.EditCells.Cells[0].Row != 0 || payload.EditCells.Cells[0].Value != float64(42) {
		t.Errorf("payload = %+v, want nested official editCells cells", payload)
	}
}

func ioReadAll(r *http.Request) ([]byte, error) {
	return io.ReadAll(r.Body)
}
