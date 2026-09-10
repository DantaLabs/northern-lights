package workiva

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync"
	"testing"
	"time"
)

// setupFastClient wires a Client with no rate limiter and a no-op sleep
// so polling tests run instantly.
func setupFastClient(t *testing.T, handler http.Handler) (*Client, *url.URL) {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}

	tokens := NewTokenProvider(u, "test-client", "test-secret", "file:write", srv.Client())
	c := NewClient(u, tokens, nil, srv.Client())
	c.sleep = func(ctx context.Context, d time.Duration) error { return nil }
	return c, u
}

func TestUpdateSheetReturnsOperationLocation(t *testing.T) {
	c, u := setupFastClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/spreadsheets/s-1/sheets/sh-1/update" {
			t.Errorf("path = %q, want /spreadsheets/s-1/sheets/sh-1/update", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		if err := json.NewEncoder(w).Encode(map[string]string{
			"operationLocation": "http://" + r.Host + "/operations/op-1",
		}); err != nil {
			t.Errorf("write operation response: %v", err)
		}
	}))

	opURL, err := c.UpdateSheet(context.Background(), "s-1", "sh-1",
		NewEditCellsUpdate([]CellEdit{{Column: 0, Row: 0, Value: 1}}))
	if err != nil {
		t.Fatalf("UpdateSheet: %v", err)
	}
	want := u.String() + "/operations/op-1"
	if opURL != want {
		t.Errorf("operationURL = %q, want %q", opURL, want)
	}
}

func TestUpdateSheetFallsBackToLocationHeader(t *testing.T) {
	c, u := setupFastClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://"+r.Host+"/operations/op-9")
		w.WriteHeader(http.StatusAccepted)
	}))

	opURL, err := c.UpdateSheet(context.Background(), "s-1", "sh-1",
		NewEditCellsUpdate([]CellEdit{{Column: 0, Row: 0, Value: 1}}))
	if err != nil {
		t.Fatalf("UpdateSheet: %v", err)
	}
	if want := u.String() + "/operations/op-9"; opURL != want {
		t.Errorf("operationURL = %q, want %q", opURL, want)
	}
}

func TestUpdateSheetSendsEditCellsPayload(t *testing.T) {
	var body []byte
	c, _ := setupFastClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
		if err := json.NewEncoder(w).Encode(map[string]string{"operationLocation": "http://x/operations/op-1"}); err != nil {
			t.Errorf("write operation response: %v", err)
		}
	}))

	edits := []CellEdit{
		{Column: 0, Row: 0, Value: 42},
		{Column: 1, Row: 2, Value: "hello"},
	}
	_, err := c.UpdateSheet(context.Background(), "s-1", "sh-1", NewEditCellsUpdate(edits))
	if err != nil {
		t.Fatalf("UpdateSheet: %v", err)
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
		t.Fatalf("request body is not JSON: %v", err)
	}
	if len(payload.EditCells.Cells) != 2 {
		t.Fatalf("len(editCells.cells) = %d, want 2", len(payload.EditCells.Cells))
	}
	first := payload.EditCells.Cells[0]
	if first.Column != 0 || first.Row != 0 || first.Value != float64(42) {
		t.Errorf("first edit = %+v, want column 0 row 0 value 42", first)
	}
	second := payload.EditCells.Cells[1]
	if second.Column != 1 || second.Row != 2 || second.Value != "hello" {
		t.Errorf("second edit = %+v, want column 1 row 2 value hello", second)
	}
}

func TestUpdateSheetRejectsNon202(t *testing.T) {
	c, _ := setupFastClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := fmt.Fprint(w, `{"unexpected":true}`); err != nil {
			t.Errorf("write update response: %v", err)
		}
	}))

	_, err := c.UpdateSheet(context.Background(), "s-1", "sh-1",
		NewEditCellsUpdate([]CellEdit{{Column: 0, Row: 0, Value: 1}}))
	if err == nil {
		t.Fatal("expected error for non-202 response, got nil")
	}
}

func TestUpdateSheetRejectsEmptyUpdate(t *testing.T) {
	c, _ := setupFastClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("API must not be called for an empty SheetUpdate")
	}))

	_, err := c.UpdateSheet(context.Background(), "s-1", "sh-1", SheetUpdate{})
	if err == nil {
		t.Fatal("expected error for empty SheetUpdate, got nil")
	}
}

func TestSheetUpdateMarshalSingleField(t *testing.T) {
	// Each constructor must marshal to exactly one top-level key.
	tests := []struct {
		name string
		upd  SheetUpdate
		key  string
	}{
		{"editCells", NewEditCellsUpdate([]CellEdit{{Column: 0, Row: 0, Value: 1}}), "editCells"},
		{"editRange", NewEditRangeUpdate(EditRangeOp{Range: Range{StartRow: 0, StartCol: 0, StopRow: 1, StopCol: 1}, Values: [][]any{{1, 2}}}), "editRange"},
		{"applyFormats", NewApplyFormatsUpdate([]ApplyFormatsOp{{
			CellFormat: map[string]any{"backgroundColor": "#d0e0f0"},
			Ranges:     []Range{{StartRow: 0, StartCol: 0, StopRow: 0, StopCol: -1}},
			TextFormat: map[string]any{"bold": true},
		}}), "applyFormats"},
		{"insertRows", NewInsertRowsUpdate("BEFORE", []Insertion{{Index: 3, Count: 2}}), "insertRows"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.upd)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("payload not an object: %v", err)
			}
			if len(m) != 1 {
				t.Errorf("payload has %d keys, want 1: %s", len(m), b)
			}
			if _, ok := m[tc.key]; !ok {
				t.Errorf("payload missing key %q: %s", tc.key, b)
			}
		})
	}
}

func TestWriteCellsUpdatesAndWaits(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var polls int

	c, _ := setupFastClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		polls++
		n := polls
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/spreadsheets/s-1/sheets/sh-1/update":
			w.WriteHeader(http.StatusAccepted)
			if err := json.NewEncoder(w).Encode(map[string]string{
				"operationLocation": "http://" + r.Host + "/operations/op-1",
			}); err != nil {
				t.Errorf("write operation response: %v", err)
			}
		case r.URL.Path == "/operations/op-1" && n <= 2:
			// First poll after the 202: still running.
			if _, err := w.Write(loadFixture(t, "operation_started.json")); err != nil {
				t.Errorf("write operation fixture: %v", err)
			}
		case r.URL.Path == "/operations/op-1":
			if _, err := w.Write(loadFixture(t, "operation_completed.json")); err != nil {
				t.Errorf("write operation fixture: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))

	edits := []CellEdit{
		{Column: 1, Row: 2, Value: 100},
		{Column: 1, Row: 3, Value: 200},
	}
	if err := c.WriteCells(context.Background(), "s-1", "sh-1", edits); err != nil {
		t.Fatalf("WriteCells: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) < 3 {
		t.Fatalf("requests = %v, want data write plus at least two polls", paths)
	}
	if paths[0] != "POST /spreadsheets/s-1/sheets/sh-1/update" {
		t.Errorf("first request = %q, want POST .../update", paths[0])
	}
	if paths[1] != "GET /operations/op-1" || paths[2] != "GET /operations/op-1" {
		t.Errorf("poll requests = %v, want two GETs to /operations/op-1", paths[1:3])
	}
}

func TestSheetUpdateMarshalUsesExactOfficialApplyFormatsShape(t *testing.T) {
	upd := NewApplyFormatsUpdate([]ApplyFormatsOp{{
		CellFormat:             map[string]any{"backgroundColor": "#d0e0f0"},
		ClearValueFormatStyles: true,
		Ranges:                 []Range{{StartRow: 0, StartCol: 0, StopRow: 0, StopCol: -1}},
		TextFormat:             map[string]any{"bold": true},
		ValueFormat:            map[string]any{"valueFormatType": "TEXT"},
	}})
	got, err := json.Marshal(upd)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"applyFormats":{"formats":[{"cellFormat":{"backgroundColor":"#d0e0f0"},"clearValueFormatStyles":true,"ranges":[{"startRow":0,"startColumn":0,"stopRow":0,"stopColumn":null}],"textFormat":{"bold":true},"valueFormat":{"valueFormatType":"TEXT"}}]}}`
	var gotJSON, wantJSON any
	if err := json.Unmarshal(got, &gotJSON); err != nil {
		t.Fatalf("decode got: %v", err)
	}
	if err := json.Unmarshal([]byte(want), &wantJSON); err != nil {
		t.Fatalf("decode want: %v", err)
	}
	if !reflect.DeepEqual(gotJSON, wantJSON) {
		t.Errorf("JSON = %s, want %s", got, want)
	}
}

func TestSheetUpdateMarshalUsesExactOfficialInsertRowsShape(t *testing.T) {
	got, err := json.Marshal(NewInsertRowsUpdate("AFTER", []Insertion{{Index: 6, Count: 2}}))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"insertRows":{"inheritFrom":"AFTER","insertions":[{"index":6,"count":2}]}}`
	var gotJSON, wantJSON any
	if err := json.Unmarshal(got, &gotJSON); err != nil {
		t.Fatalf("decode got: %v", err)
	}
	if err := json.Unmarshal([]byte(want), &wantJSON); err != nil {
		t.Fatalf("decode want: %v", err)
	}
	if !reflect.DeepEqual(gotJSON, wantJSON) {
		t.Errorf("JSON = %s, want %s", got, want)
	}
}

func TestSheetUpdateRejectsEmptyOrAmbiguousOperations(t *testing.T) {
	tests := []struct {
		name string
		upd  SheetUpdate
	}{
		{"empty edit cells", NewEditCellsUpdate(nil)},
		{"empty edit range", NewEditRangeUpdate(EditRangeOp{Range: Range{StartRow: 0, StartCol: 0, StopRow: 0, StopCol: 0}})},
		{"empty formats", NewApplyFormatsUpdate(nil)},
		{"format without ranges", NewApplyFormatsUpdate([]ApplyFormatsOp{{TextFormat: map[string]any{"bold": true}}})},
		{"empty insertions", NewInsertRowsUpdate("BEFORE", nil)},
		{"invalid inheritFrom", NewInsertRowsUpdate("MIDDLE", []Insertion{{Index: 0, Count: 1}})},
		{"zero count", NewInsertRowsUpdate("BEFORE", []Insertion{{Index: 0, Count: 0}})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := json.Marshal(tc.upd); err == nil {
				t.Fatal("Marshal returned nil error for invalid update")
			}
		})
	}
}

func TestWriteCellsHonorsInitialRetryAfterPerOperation(t *testing.T) {
	var updates, polls int
	var delays []time.Duration
	c, _ := setupFastClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/spreadsheets/s-1/sheets/sh-1/update":
			updates++
			if updates == 1 {
				w.Header().Set("Retry-After", "7")
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = fmt.Fprint(w, `{"operationLocation":"http://`+r.Host+`/operations/op-1"}`)
		case "/operations/op-1":
			polls++
			_, _ = fmt.Fprint(w, `{"id":"op-1","status":"completed"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	c.sleep = func(ctx context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	}
	if err := c.WriteCells(context.Background(), "s-1", "sh-1", []CellEdit{{Column: 0, Row: 0, Value: 1}}); err != nil {
		t.Fatalf("first WriteCells: %v", err)
	}
	if err := c.WriteCells(context.Background(), "s-1", "sh-1", []CellEdit{{Column: 0, Row: 0, Value: 2}}); err != nil {
		t.Fatalf("second WriteCells: %v", err)
	}
	if updates != 2 || polls != 2 {
		t.Fatalf("updates/polls = %d/%d, want 2/2", updates, polls)
	}
	if len(delays) != 1 || delays[0] != 7*time.Second {
		t.Fatalf("delays = %v, want only the first operation's 7s delay", delays)
	}
}
