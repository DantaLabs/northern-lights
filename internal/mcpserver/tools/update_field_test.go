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
)

// writeMock returns a handler covering the Workiva endpoints a write
// touches: sheetdata reads (before value), the PATCH data endpoint
// (editCells), and the operations poll endpoint.
func writeMock(t *testing.T, edits *[][]byte) http.HandlerFunc {
	t.Helper()
	return tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sheetdata"):
			fmt.Fprint(w, sheetdataBody)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/data"):
			body, _ := io.ReadAll(r.Body)
			*edits = append(*edits, body)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"operationLocation": "/operations/op-1"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/operations/"):
			fmt.Fprint(w, `{"id": "op-1", "status": "completed", "resourceUrl": "/spreadsheets/sp-1/sheets/sh-1"}`)
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
		t.Fatalf("phase 1 must not write: got %d PATCH calls", len(edits))
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
		t.Fatalf("phase 2 PATCH calls = %d, want 1", len(edits))
	}
	var payload map[string]any
	if err := json.Unmarshal(edits[0], &payload); err != nil {
		t.Fatalf("PATCH payload is not JSON: %v", err)
	}
	ecEdits, ok := payload["editCells"].([]any)
	if !ok || len(ecEdits) != 1 {
		t.Fatalf("editCells = %v, want one entry", payload["editCells"])
	}
	edit, _ := ecEdits[0].(map[string]any)
	rng, _ := edit["range"].(map[string]any)
	if rng["startRow"] != float64(2) || rng["startColumn"] != float64(1) || rng["stopRow"] != float64(2) || rng["stopColumn"] != float64(1) {
		t.Errorf("edit range = %v, want B3", rng)
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
		t.Errorf("PATCH calls = %d, want 1 (replay must not write)", len(edits))
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
		t.Errorf("PATCH calls = %d, want 0 (expired token must not write)", len(edits))
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
		t.Errorf("PATCH calls = %d, want 0 (unknown token must not write)", len(edits))
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
		t.Errorf("PATCH calls = %d, want 1", len(edits))
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
