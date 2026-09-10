package tools

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// mapperSheetdataBody is a two-column sheet: a header row plus two data
// rows and one row with an empty name that must be skipped.
const mapperSheetdataBody = `{
	"data": {
		"range": {"startRow": 0, "startColumn": 0, "stopRow": 3, "stopColumn": 1},
		"cells": [
			[{"value": "Field Name"}, {"value": "Value"}],
			[{"value": "Scope 2 Energy (kWh)"}, {"value": "1234"}],
			[{"value": "Water Use (m3)"}, {"value": "56"}],
			[{"value": ""}, {"value": "999"}]
		]
	}
}`

func syncMock(t *testing.T) http.HandlerFunc {
	t.Helper()
	return tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sheetdata") {
			if got := r.URL.Query().Get("$cellrange"); got != "A:B" {
				t.Errorf("$cellrange = %q, want A:B", got)
			}
			w.Header().Set("Content-Type", "application/json")
			if _, err := fmt.Fprint(w, mapperSheetdataBody); err != nil {
				t.Errorf("write sheetdata response: %v", err)
			}
			return
		}
		http.NotFound(w, r)
	})
}

func TestSyncMappingUpsertsTwoColumnStructure(t *testing.T) {
	env := newTestEnv(t, syncMock(t))

	result := callTool(t, env.deps, SyncMapping(), map[string]any{
		"spreadsheet_id": "sp-9",
		"sheet_id":       "sh-9",
	})
	if result.IsError {
		t.Fatalf("sync returned error: %+v", result.Content)
	}
	sc := structuredContent(t, result)
	if sc["fields_count"] != float64(2) {
		t.Errorf("fields_count = %v, want 2", sc["fields_count"])
	}
	names, _ := sc["fields"].([]any)
	if len(names) != 2 || names[0] != "scope_2_energy_kwh" || names[1] != "water_use_m3" {
		t.Errorf("fields = %v, want [scope_2_energy_kwh water_use_m3]", sc["fields"])
	}

	field, err := env.deps.Store.GetField(context.Background(), "scope_2_energy_kwh")
	if err != nil {
		t.Fatalf("GetField: %v", err)
	}
	if field == nil {
		t.Fatal("scope_2_energy_kwh not found after sync")
	}
	if field.SpreadsheetID != "sp-9" || field.SheetID != "sh-9" || field.CellRange != "B2" {
		t.Errorf("field = %+v, want sp-9/sh-9/B2", *field)
	}
	water, err := env.deps.Store.GetField(context.Background(), "water_use_m3")
	if err != nil {
		t.Fatalf("GetField: %v", err)
	}
	if water == nil || water.CellRange != "B3" {
		t.Errorf("water_use_m3 = %+v, want B3", water)
	}

	// The sync is audited.
	entries, err := env.deps.Audit.Recent(context.Background(), 1, "sp-9/sh-9")
	if err != nil {
		t.Fatalf("Audit.Recent: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != "sync" {
		t.Errorf("audit entries = %+v, want one sync entry", entries)
	}
}

func TestSyncMappingHonorsStartRow(t *testing.T) {
	env := newTestEnv(t, syncMock(t))

	// start_row 1 keeps the header row, which then becomes a field named
	// field_name pointing at B1.
	result := callTool(t, env.deps, SyncMapping(), map[string]any{
		"spreadsheet_id": "sp-9",
		"sheet_id":       "sh-9",
		"start_row":      1,
	})
	if result.IsError {
		t.Fatalf("sync returned error: %+v", result.Content)
	}
	sc := structuredContent(t, result)
	if sc["fields_count"] != float64(3) {
		t.Errorf("fields_count = %v, want 3", sc["fields_count"])
	}
}

func TestSyncMappingRecordsActorFromHeader(t *testing.T) {
	env := newTestEnv(t, syncMock(t))

	result := callToolWithActor(t, env.deps, SyncMapping(), "eu-operator@example.com", map[string]any{
		"spreadsheet_id": "sp-9",
		"sheet_id":       "sh-9",
	})
	if result.IsError {
		t.Fatalf("sync returned error: %+v", result.Content)
	}

	entries, err := env.deps.Audit.Recent(context.Background(), 10, "sp-9/sh-9")
	if err != nil {
		t.Fatalf("Audit.Recent: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no audit entry for sync")
	}
	if entries[0].Actor != "eu-operator@example.com" {
		t.Errorf("audit actor = %q, want eu-operator@example.com", entries[0].Actor)
	}
}

func TestSyncMappingCustomColumns(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("$cellrange"); got != "C:D" {
			t.Errorf("$cellrange = %q, want C:D", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `{"data":{"range":{"startRow":1,"startColumn":2,"stopRow":1,"stopColumn":3},"cells":[[{"value":"Revenue EUR"},{"value":"1000"}]]}}`); err != nil {
			t.Errorf("write sheetdata response: %v", err)
		}
	}))

	result := callTool(t, env.deps, SyncMapping(), map[string]any{
		"spreadsheet_id": "sp-9",
		"sheet_id":       "sh-9",
		"name_column":    "C",
		"value_column":   "D",
		"start_row":      2,
	})
	if result.IsError {
		t.Fatalf("sync returned error: %+v", result.Content)
	}
	sc := structuredContent(t, result)
	if sc["fields_count"] != float64(1) {
		t.Fatalf("fields_count = %v, want 1", sc["fields_count"])
	}
	field, err := env.deps.Store.GetField(context.Background(), "revenue_eur")
	if err != nil {
		t.Fatalf("GetField: %v", err)
	}
	if field == nil || field.CellRange != "D2" {
		t.Errorf("revenue_eur = %+v, want D2", field)
	}
}
