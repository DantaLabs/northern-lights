package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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
			if got := r.URL.Query().Get("$fields"); got != "cells.value,cells.calculatedValue,range" {
				t.Errorf("$fields = %q, want cells.value,cells.calculatedValue,range", got)
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

func TestCompletedWriteWithAuditFailureMustNotInviteRetry(t *testing.T) {
	var edits [][]byte
	base := writeMock(t, &edits)
	var env testEnv
	var closeOnce sync.Once
	env = newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/operations/") {
			closeOnce.Do(func() {
				if err := env.deps.Audit.Close(); err != nil {
					t.Errorf("close audit log: %v", err)
				}
			})
		}
		base(w, r)
	})
	env.deps.Cfg.RequireWriteConfirmation = false
	seedField(t, env)

	result := callTool(t, env.deps, UpdateField(), map[string]any{
		"name": "scope2_energy_kwh", "value": "777",
	})
	if result.IsError {
		t.Fatalf("completed external write returned MCP error: %+v", result.Content)
	}
	content := structuredContent(t, result)
	if content["status"] != "written_audit_failed" {
		t.Fatalf("status = %v, want written_audit_failed", content["status"])
	}
	message, _ := content["message"].(string)
	if !strings.Contains(strings.ToLower(message), "do not retry") {
		t.Fatalf("message = %q, want explicit do not retry instruction", message)
	}
	if content["workiva_op_url"] == "" {
		t.Fatal("workiva_op_url missing from reconciliation response")
	}
	if len(edits) != 1 {
		t.Fatalf("Workiva writes = %d, want 1", len(edits))
	}
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

func TestUpdateFieldRejectsConfirmationAfterMappingTargetChanges(t *testing.T) {
	var edits [][]byte
	env := newTestEnv(t, writeMock(t, &edits))
	seedField(t, env)

	stage := callTool(t, env.deps, UpdateField(), map[string]any{
		"name": "scope2_energy_kwh", "value": "5678",
	})
	if stage.IsError {
		t.Fatalf("stage returned error: %+v", stage.Content)
	}
	token := structuredContent(t, stage)["confirm_token"].(string)
	original, err := env.deps.Store.GetField(context.Background(), "scope2_energy_kwh")
	if err != nil || original == nil {
		t.Fatalf("get original field: field=%+v err=%v", original, err)
	}

	mutated, err := env.deps.Store.UpsertField(context.Background(), mapping.Field{
		ID: original.ID, SpreadsheetID: original.SpreadsheetID, SheetID: original.SheetID,
		Name: original.Name, CellRange: "C3", FieldType: original.FieldType,
		Aliases: original.Aliases, Description: original.Description,
	})
	if err != nil {
		t.Fatalf("mutate mapping target: %v", err)
	}
	if mutated.ID != original.ID {
		t.Fatalf("mapping row ID changed from %d to %d", original.ID, mutated.ID)
	}

	confirm := callTool(t, env.deps, UpdateField(), map[string]any{
		"name": "scope2_energy_kwh", "value": "5678", "confirm_token": token,
	})
	if !confirm.IsError {
		t.Fatal("confirmation with changed target unexpectedly succeeded")
	}
	if len(edits) != 0 {
		t.Fatalf("changed-target confirmation POST calls = %d, want 0", len(edits))
	}
	rawContent, err := json.Marshal(confirm.Content)
	if err != nil {
		t.Fatalf("marshal changed-target error: %v", err)
	}
	if !strings.Contains(string(rawContent), "stage") {
		t.Errorf("changed-target error = %s, want restage hint", rawContent)
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

func TestExpandCellEditsAllowsExactCap(t *testing.T) {
	edits, err := expandCellEdits(workiva.Range{StartRow: 0, StartCol: 0, StopRow: 99999, StopCol: 0}, "x")
	if err != nil {
		t.Fatalf("exact 100,000-cell expansion returned error: %v", err)
	}
	if len(edits) != 100000 {
		t.Fatalf("edit count = %d, want 100000", len(edits))
	}
	if edits[0].Row != 0 || edits[0].Column != 0 || edits[len(edits)-1].Row != 99999 || edits[len(edits)-1].Column != 0 {
		t.Fatalf("unexpected boundary edits: first=%+v last=%+v", edits[0], edits[len(edits)-1])
	}
}

func TestUpdateFieldRejectsOverCapBeforeAnyWorkivaRequest(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unexpected Workiva request", http.StatusInternalServerError)
	}))
	env.deps.Cfg.RequireWriteConfirmation = false
	seedField(t, env)
	if _, err := env.deps.Store.UpsertField(context.Background(), mapping.Field{
		SpreadsheetID: "sp-1", SheetID: "sh-1", Name: "scope2_energy_kwh",
		CellRange: "A1:A100001", FieldType: "number",
	}); err != nil {
		t.Fatalf("UpsertField: %v", err)
	}

	result := callTool(t, env.deps, UpdateField(), map[string]any{
		"name": "scope2_energy_kwh", "value": "x",
	})
	if !result.IsError {
		t.Fatalf("100,001-cell tool write must fail, got %+v", result.StructuredContent)
	}
	rawContent, err := json.Marshal(result.Content)
	if err != nil {
		t.Fatalf("marshal tool error: %v", err)
	}
	if !strings.Contains(string(rawContent), "exceeds maximum of 100000 cells") {
		t.Fatalf("unexpected tool error: %s", rawContent)
	}
	if got := env.apiCalls.Load(); got != 0 {
		t.Fatalf("Workiva requests = %d, want 0", got)
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

func TestUpdateFieldClassifiesMutationOutcomesWithoutRetry(t *testing.T) {
	for _, tc := range []struct {
		name, pollBody, wantStatus string
		malformed                  bool
		postStatus                 int
		closeSubmit                bool
	}{
		{name: "accepted malformed body", malformed: true, postStatus: http.StatusAccepted, wantStatus: "write_outcome_unknown"},
		{name: "definite HTTP rejection", postStatus: http.StatusBadRequest, wantStatus: "write_rejected"},
		{name: "ambiguous submission transport failure", closeSubmit: true, wantStatus: "write_outcome_unknown"},
		{name: "accepted then poll decoding failure", postStatus: http.StatusAccepted, pollBody: `{`, wantStatus: "write_outcome_unknown"},
		{name: "terminal operation failure", postStatus: http.StatusAccepted, pollBody: `{"id":"op-1","status":"failed","error":{"message":"invalid edit"}}`, wantStatus: "write_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mutations atomic.Int32
			env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/sheetdata"):
					_, _ = fmt.Fprint(w, sheetdataBody)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/update"):
					mutations.Add(1)
					if tc.closeSubmit {
						panic(http.ErrAbortHandler)
					}
					w.Header().Set("Location", "/operations/op-1")
					w.WriteHeader(tc.postStatus)
					if tc.malformed {
						_, _ = fmt.Fprint(w, "{")
						return
					}
					if tc.postStatus == http.StatusAccepted {
						_, _ = fmt.Fprint(w, `{"operationLocation":"/operations/op-1"}`)
					}
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/operations/"):
					_, _ = fmt.Fprint(w, tc.pollBody)
				default:
					http.NotFound(w, r)
				}
			}))
			env.deps.Cfg.RequireWriteConfirmation = false
			seedField(t, env)
			result := callTool(t, env.deps, UpdateField(), map[string]any{"name": "scope2_energy_kwh", "value": "new"})
			if result.IsError {
				t.Fatalf("expected reconciliation output, got MCP error: %+v", result.Content)
			}
			got := structuredContent(t, result)
			if got["status"] != tc.wantStatus {
				t.Fatalf("status = %v, want %s", got["status"], tc.wantStatus)
			}
			if got["spreadsheet_id"] != "sp-1" || got["sheet_id"] != "sh-1" || got["range"] != "B3" || got["before"] != "1234" || got["after_preview"] != "new" {
				t.Fatalf("incomplete reconciliation output: %#v", got)
			}
			if tc.postStatus == http.StatusAccepted && got["workiva_op_url"] != "/operations/op-1" {
				t.Fatalf("operation URL = %v", got["workiva_op_url"])
			}
			if mutations.Load() != 1 {
				t.Fatalf("mutation requests = %d, want 1", mutations.Load())
			}
		})
	}
}

func TestUpdateFieldTreatsServerErrorsAsUnknownWithoutRetry(t *testing.T) {
	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var mutations atomic.Int32
			env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/sheetdata"):
					_, _ = fmt.Fprint(w, sheetdataBody)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/update"):
					mutations.Add(1)
					w.Header().Set("Location", "/operations/server-error")
					w.WriteHeader(status)
					_, _ = fmt.Fprint(w, `{"message":"server failure"}`)
				default:
					http.NotFound(w, r)
				}
			}))
			env.deps.Cfg.RequireWriteConfirmation = false
			seedField(t, env)

			result := callTool(t, env.deps, UpdateField(), map[string]any{"name": "scope2_energy_kwh", "value": "new"})
			if result.IsError {
				t.Fatalf("expected reconciliation output, got MCP error: %+v", result.Content)
			}
			got := structuredContent(t, result)
			if got["status"] != "write_outcome_unknown" {
				t.Fatalf("status = %v, want write_outcome_unknown", got["status"])
			}
			if got["spreadsheet_id"] != "sp-1" || got["sheet_id"] != "sh-1" || got["range"] != "B3" || got["before"] != "1234" || got["after_preview"] != "new" {
				t.Fatalf("incomplete reconciliation output: %#v", got)
			}
			if got["workiva_op_url"] != "/operations/server-error" {
				t.Fatalf("operation URL = %v, want response Location", got["workiva_op_url"])
			}
			message, _ := got["message"].(string)
			if !strings.Contains(strings.ToLower(message), "do not retry") {
				t.Fatalf("message = %q, want explicit do not retry guidance", message)
			}
			if mutations.Load() != 1 {
				t.Fatalf("mutation requests = %d, want exactly 1", mutations.Load())
			}
		})
	}
}
