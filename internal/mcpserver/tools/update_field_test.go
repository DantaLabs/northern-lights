package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

// writeMock returns a handler covering the Workiva endpoints a write
// touches: sheetdata reads (before value), the POST update endpoint
// (editCells), and the operations poll endpoint.
func writeMock(t *testing.T, edits *[][]byte) http.HandlerFunc {
	t.Helper()
	return tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sheetdata"):
			if got := r.URL.Query().Get("$fields"); got != "cells.value,cells.calculatedValue" {
				t.Errorf("$fields = %q, want cells.value,cells.calculatedValue", got)
			}
			if _, err := fmt.Fprint(w, sheetdataBody); err != nil {
				t.Errorf("write sheetdata response: %v", err)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/update"):
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read update request: %v", err)
				return
			}
			*edits = append(*edits, body)
			w.WriteHeader(http.StatusAccepted)
			if _, err := fmt.Fprint(w, `{"operationLocation": "/operations/op-1"}`); err != nil {
				t.Errorf("write update response: %v", err)
			}
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/operations/"):
			if _, err := fmt.Fprint(w, `{"id": "op-1", "status": "completed", "resourceUrl": "/spreadsheets/sp-1/sheets/sh-1"}`); err != nil {
				t.Errorf("write operation response: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	})
}

func TestUpdateFieldTwoPhaseFlow(t *testing.T) {
	var edits [][]byte
	env := newTestEnv(t, writeMock(t, &edits))
	seedField(t, env)

	// Phase 1: stage the write, no API mutation yet.
	stage := callTool(t, env.deps, UpdateField(), map[string]any{
		"name":  "scope2_energy_kwh",
		"value": "5678",
	})
	if stage.IsError {
		t.Fatalf("phase 1 returned error: %+v", stage.Content)
	}
	sc := structuredContent(t, stage)
	token, _ := sc["confirm_token"].(string)
	if token == "" {
		t.Fatalf("phase 1 confirm_token missing: %+v", sc)
	}
	if sc["before"] != "1234" {
		t.Errorf("before = %v, want 1234", sc["before"])
	}
	if sc["after_preview"] != "5678" {
		t.Errorf("after_preview = %v, want 5678", sc["after_preview"])
	}
	if sc["field"] != "scope2_energy_kwh" {
		t.Errorf("field = %v, want scope2_energy_kwh", sc["field"])
	}
	if len(edits) != 0 {
		t.Fatalf("phase 1 must not write: got %d POST calls", len(edits))
	}

	// Phase 2: confirm with the token, writes and audits.
	exec := callTool(t, env.deps, UpdateField(), map[string]any{
		"name":          "scope2_energy_kwh",
		"value":         "5678",
		"confirm_token": token,
	})
	if exec.IsError {
		t.Fatalf("phase 2 returned error: %+v", exec.Content)
	}
	ec := structuredContent(t, exec)
	if ec["status"] != "written" {
		t.Errorf("status = %v, want written", ec["status"])
	}
	if ec["before"] != "1234" || ec["after"] != "5678" {
		t.Errorf("before/after = %v/%v, want 1234/5678", ec["before"], ec["after"])
	}
	if len(edits) != 1 {
		t.Fatalf("phase 2 POST calls = %d, want 1", len(edits))
	}
	var payload map[string]any
	if err := json.Unmarshal(edits[0], &payload); err != nil {
		t.Fatalf("POST payload is not JSON: %v", err)
	}
	ecEdits, ok := payload["editCells"].(map[string]any)
	if !ok {
		t.Fatalf("editCells = %v, want object", payload["editCells"])
	}
	rawCells, ok := ecEdits["cells"].([]any)
	if !ok || len(rawCells) != 1 {
		t.Fatalf("editCells.cells = %v, want one entry", ecEdits["cells"])
	}
	edit, _ := rawCells[0].(map[string]any)
	if edit["row"] != float64(2) || edit["column"] != float64(1) {
		t.Errorf("edit cell = %v, want B3", edit)
	}
	if edit["value"] != "5678" {
		t.Errorf("edit value = %v, want 5678", edit["value"])
	}

	// The write is audited with before/after and the operation URL.
	entries, err := env.deps.Audit.Recent(context.Background(), 10, "sp-1/sh-1/B3")
	if err != nil {
		t.Fatalf("Audit.Recent: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no audit entry for the write")
	}
	wr := entries[0]
	if wr.Action != "write" {
		t.Errorf("audit action = %q, want write", wr.Action)
	}
	if !strings.Contains(wr.BeforeJSON, "1234") || !strings.Contains(wr.AfterJSON, "5678") {
		t.Errorf("audit before/after = %q/%q", wr.BeforeJSON, wr.AfterJSON)
	}
	if wr.WorkivaOpURL == "" {
		t.Error("audit workiva_op_url is empty")
	}
}

func TestUpdateFieldRejectsTokenReplay(t *testing.T) {
	var edits [][]byte
	env := newTestEnv(t, writeMock(t, &edits))
	seedField(t, env)

	stage := callTool(t, env.deps, UpdateField(), map[string]any{
		"name":  "scope2_energy_kwh",
		"value": "5678",
	})
	token := structuredContent(t, stage)["confirm_token"].(string)

	exec := callTool(t, env.deps, UpdateField(), map[string]any{
		"name":          "scope2_energy_kwh",
		"value":         "5678",
		"confirm_token": token,
	})
	if exec.IsError {
		t.Fatalf("phase 2 returned error: %+v", exec.Content)
	}

	replay := callTool(t, env.deps, UpdateField(), map[string]any{
		"name":          "scope2_energy_kwh",
		"value":         "5678",
		"confirm_token": token,
	})
	if !replay.IsError {
		t.Fatal("expected error when replaying a consumed token")
	}
	if len(edits) != 1 {
		t.Errorf("POST calls = %d, want 1 (replay must not write)", len(edits))
	}
}

func TestUpdateFieldRejectsExpiredToken(t *testing.T) {
	var edits [][]byte
	env := newTestEnv(t, writeMock(t, &edits))
	seedField(t, env)

	// Shorten the TTL, then stage a write that outlives it.
	oldTTL := pendingWriteTTL
	pendingWriteTTL = 50 * time.Millisecond
	t.Cleanup(func() { pendingWriteTTL = oldTTL })

	stage := callTool(t, env.deps, UpdateField(), map[string]any{
		"name":  "scope2_energy_kwh",
		"value": "5678",
	})
	token := structuredContent(t, stage)["confirm_token"].(string)
	time.Sleep(100 * time.Millisecond)

	exec := callTool(t, env.deps, UpdateField(), map[string]any{
		"name":          "scope2_energy_kwh",
		"value":         "5678",
		"confirm_token": token,
	})
	if !exec.IsError {
		t.Fatal("expected error for an expired token")
	}
	if len(edits) != 0 {
		t.Errorf("POST calls = %d, want 0 (expired token must not write)", len(edits))
	}
}

func TestUpdateFieldRejectsUnknownToken(t *testing.T) {
	var edits [][]byte
	env := newTestEnv(t, writeMock(t, &edits))
	seedField(t, env)

	exec := callTool(t, env.deps, UpdateField(), map[string]any{
		"name":          "scope2_energy_kwh",
		"value":         "5678",
		"confirm_token": "00000000-0000-0000-0000-000000000000",
	})
	if !exec.IsError {
		t.Fatal("expected error for an unknown token")
	}
	if len(edits) != 0 {
		t.Errorf("POST calls = %d, want 0 (unknown token must not write)", len(edits))
	}
}

func TestUpdateFieldSinglePhaseWhenConfirmationDisabled(t *testing.T) {
	var edits [][]byte
	env := newTestEnv(t, writeMock(t, &edits))
	seedField(t, env)
	env.deps.Cfg.RequireWriteConfirmation = false

	exec := callTool(t, env.deps, UpdateField(), map[string]any{
		"name":  "scope2_energy_kwh",
		"value": "5678",
	})
	if exec.IsError {
		t.Fatalf("single-phase write returned error: %+v", exec.Content)
	}
	ec := structuredContent(t, exec)
	if ec["status"] != "written" {
		t.Errorf("status = %v, want written", ec["status"])
	}
	if len(edits) != 1 {
		t.Errorf("POST calls = %d, want 1", len(edits))
	}
}

func TestUpdateFieldUnknownNameIsToolError(t *testing.T) {
	var edits [][]byte
	env := newTestEnv(t, writeMock(t, &edits))
	seedField(t, env)

	result := callTool(t, env.deps, UpdateField(), map[string]any{"name": "nope", "value": "1"})
	if !result.IsError {
		t.Fatal("expected tool error for unknown field name")
	}
}

func TestUpdateFieldRefreshCacheReportsStoreErrors(t *testing.T) {
	store, err := mapping.Open(":memory:")
	if err != nil {
		t.Fatalf("mapping.Open: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close mapping store: %v", err)
	}

	err = refreshCacheAfterWrite(context.Background(), mcpserver.Deps{Store: store}, &mapping.Field{
		SpreadsheetID: "sp-1",
		SheetID:       "sh-1",
	}, workiva.Range{StartRow: 2, StopRow: 2, StartCol: 1, StopCol: 1}, "5678")
	if err == nil || !strings.Contains(err.Error(), "cache updated cell B3") {
		t.Fatalf("refreshCacheAfterWrite error = %v, want cache error for B3", err)
	}
}

func TestExpandCellEditsRejectsUnboundedRanges(t *testing.T) {
	_, err := expandCellEdits(workiva.Range{StartRow: -1, StartCol: 0, StopRow: -1, StopCol: 0}, "999")
	if err == nil || !strings.Contains(err.Error(), "bounded") {
		t.Fatalf("expandCellEdits error = %v, want bounded-range error", err)
	}
}

func TestUpdateFieldExpandsBoundedMappedRangeIntoOfficialCells(t *testing.T) {
	var bodies [][]byte
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sheetdata"):
			_, _ = fmt.Fprint(w, `{"data":{"range":{"startRow":2,"startColumn":1,"stopRow":3,"stopColumn":2},"cells":[[{"value":"1"},{"value":"2"}],[{"value":"3"},{"value":"4"}]]}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/update"):
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, body)
			w.WriteHeader(http.StatusAccepted)
			_, _ = fmt.Fprint(w, `{"operationLocation":"/operations/op-1"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/operations/"):
			_, _ = fmt.Fprint(w, `{"id":"op-1","status":"completed","resourceUrl":"/spreadsheets/sp-1/sheets/sh-1/update"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	seedField(t, env)
	if _, err := env.deps.Store.UpsertField(context.Background(), mapping.Field{
		SpreadsheetID: "sp-1", SheetID: "sh-1", Name: "energy_block", CellRange: "B3:C4",
	}); err != nil {
		t.Fatalf("UpsertField: %v", err)
	}
	env.deps.Cfg.RequireWriteConfirmation = false

	result := callTool(t, env.deps, UpdateField(), map[string]any{"name": "energy_block", "value": "999"})
	if result.IsError {
		t.Fatalf("bounded range write returned error: %+v", result.Content)
	}
	if len(bodies) != 1 {
		t.Fatalf("update requests = %d, want 1", len(bodies))
	}
	var payload struct {
		EditCells struct {
			Cells []struct {
				Column int    `json:"column"`
				Row    int    `json:"row"`
				Value  string `json:"value"`
			} `json:"cells"`
		} `json:"editCells"`
	}
	if err := json.Unmarshal(bodies[0], &payload); err != nil {
		t.Fatalf("update payload is not JSON: %v", err)
	}
	if len(payload.EditCells.Cells) != 4 {
		t.Fatalf("cells = %+v, want four expanded cells", payload.EditCells.Cells)
	}
	got := map[[2]int]string{}
	for _, cell := range payload.EditCells.Cells {
		got[[2]int{cell.Row, cell.Column}] = cell.Value
	}
	for key, value := range map[[2]int]string{{2, 1}: "999", {2, 2}: "999", {3, 1}: "999", {3, 2}: "999"} {
		if got[key] != value {
			t.Errorf("cell %v = %q, want %q", key, got[key], value)
		}
	}
}

func TestUpdateFieldRefreshesSnapshotCache(t *testing.T) {
	var edits [][]byte
	env := newTestEnv(t, writeMock(t, &edits))
	seedField(t, env)
	ctx := context.Background()
	if err := env.deps.Store.CacheCells(ctx, []mapping.CellValue{{
		SpreadsheetID: "sp-1", SheetID: "sh-1", Cell: "B3",
		Value: "1234", FetchedAt: time.Now(),
	}}); err != nil {
		t.Fatalf("CacheCells: %v", err)
	}

	env.deps.Cfg.RequireWriteConfirmation = false
	result := callTool(t, env.deps, UpdateField(), map[string]any{
		"name":  "scope2_energy_kwh",
		"value": "5678",
	})
	if result.IsError {
		t.Fatalf("write returned error: %+v", result.Content)
	}

	cells, err := env.deps.Store.GetCachedCells(ctx, "sp-1", "sh-1", time.Minute)
	if err != nil {
		t.Fatalf("GetCachedCells: %v", err)
	}
	if len(cells) != 1 || cells[0].Value != "5678" {
		t.Errorf("cached cells after write = %+v, want B3=5678", cells)
	}
}

func TestExpandCellEditsRejectsExpansionAboveCap(t *testing.T) {
	_, err := expandCellEdits(workiva.Range{StartRow: 0, StartCol: 0, StopRow: 100000, StopCol: 0}, "x")
	if err == nil {
		t.Fatal("expected expansion cap error")
	}
}

func TestExpandCellEditsRejectsOverflowSizedRange(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	_, err := expandCellEdits(workiva.Range{StartRow: 0, StartCol: 0, StopRow: maxInt, StopCol: 1}, "x")
	if err == nil {
		t.Fatal("expected overflow-sized range to be rejected")
	}
}
