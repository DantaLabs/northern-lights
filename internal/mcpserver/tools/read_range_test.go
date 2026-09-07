package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/dantalabs/northern-lights/internal/audit"
)

func TestReadRangeFetchesGridAndCachesCells(t *testing.T) {
	var gotRange string
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/spreadsheets/sp-1/sheets/sh-1/sheetdata" {
			http.NotFound(w, r)
			return
		}
		gotRange = r.URL.Query().Get("$cellrange")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"range": {"startRow": 2, "startColumn": 1, "stopRow": 3, "stopColumn": 2},
			"cells": [
				[{"value": "Scope 2"}, {"value": "=1+1", "calculatedValue": 2}],
				[{"value": "kWh"}, {"value": "42", "calculatedValue": 42}]
			]
		}`)
	}))

	result := callTool(t, env.deps, ReadRange(), map[string]any{
		"spreadsheet_id": "sp-1",
		"sheet_id":       "sh-1",
		"range":          "B3:C4",
	})
	if result.IsError {
		t.Fatalf("tool returned error: %+v", result.Content)
	}
	if gotRange != "B3:C4" {
		t.Fatalf("requested $cellrange = %q, want B3:C4", gotRange)
	}

	content := structuredContent(t, result)
	if content["spreadsheet_id"] != "sp-1" || content["sheet_id"] != "sh-1" || content["range"] != "B3:C4" {
		t.Errorf("metadata = %v/%v/%v, want sp-1/sh-1/B3:C4",
			content["spreadsheet_id"], content["sheet_id"], content["range"])
	}
	if content["fetched_at"] == "" {
		t.Error("fetched_at is empty")
	}

	rows, ok := content["rows"].([]any)
	if !ok {
		t.Fatalf("rows is %T, want array", content["rows"])
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	row0 := rows[0].([]any)
	if row0[0] != "Scope 2" || row0[1] != "2" {
		t.Errorf("rows[0] = %v, want [Scope 2 2] (formula resolves to calculated value)", row0)
	}
	row1 := rows[1].([]any)
	if row1[0] != "kWh" || row1[1] != "42" {
		t.Errorf("rows[1] = %v, want [kWh 42]", row1)
	}

	// Fetched cells must be cached so later reads can be served from the
	// snapshot store.
	cached, err := env.deps.Store.GetCachedCells(context.Background(), "sp-1", "sh-1", 0)
	if err != nil {
		t.Fatalf("GetCachedCells: %v", err)
	}
	if len(cached) != 4 {
		t.Fatalf("len(cached) = %d, want 4", len(cached))
	}
	byCell := map[string]string{}
	for _, c := range cached {
		byCell[c.Cell] = c.Value
	}
	if byCell["B3"] != "Scope 2" || byCell["C3"] != "2" || byCell["B4"] != "kWh" || byCell["C4"] != "42" {
		t.Errorf("cached cells = %v, want B3..C4 mapped", byCell)
	}

	// A read audit entry with the fully qualified target must exist.
	entries := exportAudit(t, env.deps.Audit)
	found := false
	for _, e := range entries {
		if e.Tool == "workiva_read_range" && e.Action == "read" {
			found = true
			if e.Target != "sp-1/sh-1/B3:C4" {
				t.Errorf("read audit target = %q, want sp-1/sh-1/B3:C4", e.Target)
			}
			if e.BeforeJSON != "" || e.AfterJSON != "" {
				t.Errorf("read audit must not carry before/after values: before=%q after=%q", e.BeforeJSON, e.AfterJSON)
			}
		}
	}
	if !found {
		t.Error("no read audit entry appended")
	}
	if err := env.deps.Audit.Verify(context.Background()); err != nil {
		t.Fatalf("audit chain invalid: %v", err)
	}
}

func TestReadRangeAPIErrorSurfacesHint(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"sheet not found"}`, http.StatusNotFound)
	}))

	result := callTool(t, env.deps, ReadRange(), map[string]any{
		"spreadsheet_id": "sp-1",
		"sheet_id":       "missing",
		"range":          "A1",
	})
	if !result.IsError {
		t.Fatal("expected tool error for 404 response")
	}
}

func exportAudit(t *testing.T, log *audit.Log) []audit.Entry {
	t.Helper()
	var buf bytes.Buffer
	if err := log.Export(&buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	var entries []audit.Entry
	for line := range bytes.Lines(buf.Bytes()) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e audit.Entry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("unmarshal audit entry %q: %v", line, err)
		}
		entries = append(entries, e)
	}
	return entries
}
