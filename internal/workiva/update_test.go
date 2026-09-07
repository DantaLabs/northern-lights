package workiva

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		if r.URL.Path != "/spreadsheets/s-1/sheets/sh-1/data" {
			t.Errorf("path = %q, want /spreadsheets/s-1/sheets/sh-1/data", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{
			"operationLocation": "http://" + r.Host + "/operations/op-1",
		})
	}))

	opURL, err := c.UpdateSheet(context.Background(), "s-1", "sh-1",
		NewEditCellsUpdate([]CellEdit{{Range: Range{StartRow: 0, StartCol: 0, StopRow: 0, StopCol: 0}, Value: 1}}))
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
		NewEditCellsUpdate([]CellEdit{{Range: Range{StartRow: 0, StartCol: 0, StopRow: 0, StopCol: 0}, Value: 1}}))
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
		json.NewEncoder(w).Encode(map[string]string{"operationLocation": "http://x/operations/op-1"})
	}))

	edits := []CellEdit{
		{Range: Range{StartRow: 0, StartCol: 0, StopRow: 0, StopCol: 1}, Value: 42},
		{Range: Range{StartRow: 2, StartCol: 1, StopRow: 2, StopCol: 1}, Value: "hello"},
	}
	_, err := c.UpdateSheet(context.Background(), "s-1", "sh-1", NewEditCellsUpdate(edits))
	if err != nil {
		t.Fatalf("UpdateSheet: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if len(payload) != 1 {
		t.Fatalf("payload has %d top-level keys, want exactly 1: %s", len(payload), body)
	}
	rawEdits, ok := payload["editCells"].([]any)
	if !ok {
		t.Fatalf("payload[\"editCells\"] type = %T, want array", payload["editCells"])
	}
	if len(rawEdits) != 2 {
		t.Fatalf("len(editCells) = %d, want 2", len(rawEdits))
	}

	first, ok := rawEdits[0].(map[string]any)
	if !ok {
		t.Fatalf("editCells[0] type = %T, want object", rawEdits[0])
	}
	rng, ok := first["range"].(map[string]any)
	if !ok {
		t.Fatalf("editCells[0].range type = %T, want object", first["range"])
	}
	for _, k := range []string{"startRow", "startColumn", "stopRow", "stopColumn"} {
		if _, ok := rng[k]; !ok {
			t.Errorf("editCells[0].range missing key %q", k)
		}
	}
	if rng["startRow"] != float64(0) || rng["startColumn"] != float64(0) || rng["stopRow"] != float64(0) || rng["stopColumn"] != float64(1) {
		t.Errorf("editCells[0].range = %v", rng)
	}
	if first["value"] != float64(42) {
		t.Errorf("editCells[0].value = %v, want 42", first["value"])
	}
	if rawEdits[1].(map[string]any)["value"] != "hello" {
		t.Errorf("editCells[1].value = %v, want hello", rawEdits[1].(map[string]any)["value"])
	}
}

func TestUpdateSheetRejectsNon202(t *testing.T) {
	c, _ := setupFastClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"unexpected":true}`)
	}))

	_, err := c.UpdateSheet(context.Background(), "s-1", "sh-1",
		NewEditCellsUpdate([]CellEdit{{Range: Range{StartRow: 0, StartCol: 0, StopRow: 0, StopCol: 0}, Value: 1}}))
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
		{"editCells", NewEditCellsUpdate([]CellEdit{{Range: Range{StartRow: 0, StartCol: 0, StopRow: 0, StopCol: 0}, Value: 1}}), "editCells"},
		{"editRange", NewEditRangeUpdate(EditRangeOp{Range: Range{StartRow: 0, StartCol: 0, StopRow: 1, StopCol: 1}, Values: [][]any{{1, 2}}}), "editRange"},
		{"applyFormats", NewApplyFormatsUpdate([]ApplyFormatsOp{{Range: Range{StartRow: 0, StartCol: 0, StopRow: 0, StopCol: 0}, Format: map[string]any{"bold": true}}}), "applyFormats"},
		{"insertRows", NewInsertRowsUpdate([]InsertRowsOp{{StartRow: 3, Count: 2}}), "insertRows"},
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
		case r.URL.Path == "/spreadsheets/s-1/sheets/sh-1/data":
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{
				"operationLocation": "http://" + r.Host + "/operations/op-1",
			})
		case r.URL.Path == "/operations/op-1" && n <= 2:
			// First poll after the 202: still running.
			w.Write(loadFixture(t, "operation_started.json"))
		case r.URL.Path == "/operations/op-1":
			w.Write(loadFixture(t, "operation_completed.json"))
		default:
			http.NotFound(w, r)
		}
	}))

	edits := []CellEdit{
		{Range: Range{StartRow: 2, StartCol: 1, StopRow: 2, StopCol: 1}, Value: 100},
		{Range: Range{StartRow: 3, StartCol: 1, StopRow: 3, StopCol: 1}, Value: 200},
	}
	if err := c.WriteCells(context.Background(), "s-1", "sh-1", edits); err != nil {
		t.Fatalf("WriteCells: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) < 3 {
		t.Fatalf("requests = %v, want data write plus at least two polls", paths)
	}
	if paths[0] != "PATCH /spreadsheets/s-1/sheets/sh-1/data" {
		t.Errorf("first request = %q, want PATCH .../data", paths[0])
	}
	if paths[1] != "GET /operations/op-1" || paths[2] != "GET /operations/op-1" {
		t.Errorf("poll requests = %v, want two GETs to /operations/op-1", paths[1:3])
	}
}
