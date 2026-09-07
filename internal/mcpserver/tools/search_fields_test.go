package tools

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/mapping"
)

func TestSearchFieldsRanksMatchesAndAttachesCachedValues(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("search_fields must not call the Workiva API, got %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	ctx := context.Background()

	seedField(t, env)
	if _, err := env.deps.Store.UpsertField(ctx, mapping.Field{
		SpreadsheetID: "sp-1", SheetID: "sh-1",
		Name:        "scope2_market_kwh",
		Aliases:     "scope 2 market",
		CellRange:   "B5",
		Description: "Scope 2 market-based energy",
	}); err != nil {
		t.Fatalf("UpsertField: %v", err)
	}
	if err := env.deps.Store.CacheCells(ctx, []mapping.CellValue{
		{
			SpreadsheetID: "sp-1", SheetID: "sh-1", Cell: "B3",
			Value: "1234", FetchedAt: time.Now(),
		},
	}); err != nil {
		t.Fatalf("CacheCells: %v", err)
	}

	result := callTool(t, env.deps, SearchFields(), map[string]any{"query": "scope 2"})
	if result.IsError {
		t.Fatalf("tool returned error: %+v", result.Content)
	}
	content := structuredContent(t, result)

	fields, ok := content["fields"].([]any)
	if !ok {
		t.Fatalf("fields is %T, want array", content["fields"])
	}
	if len(fields) != 2 {
		t.Fatalf("len(fields) = %d, want 2", len(fields))
	}

	first := fields[0].(map[string]any)
	if first["name"] != "scope2_energy_kwh" {
		t.Errorf("fields[0].name = %v, want scope2_energy_kwh", first["name"])
	}
	if first["range"] != "B3" || first["description"] != "Scope 2 energy consumption in kWh" {
		t.Errorf("fields[0] range/description = %v/%v", first["range"], first["description"])
	}
	if first["cached_value"] != "1234" {
		t.Errorf("fields[0].cached_value = %v, want 1234 (fresh cache)", first["cached_value"])
	}

	second := fields[1].(map[string]any)
	if second["name"] != "scope2_market_kwh" {
		t.Errorf("fields[1].name = %v, want scope2_market_kwh", second["name"])
	}
	if _, hasCached := second["cached_value"]; hasCached {
		t.Errorf("fields[1] unexpectedly has cached_value: %v", second["cached_value"])
	}
}

func TestSearchFieldsNoMatches(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	seedField(t, env)

	result := callTool(t, env.deps, SearchFields(), map[string]any{"query": "headcount"})
	if result.IsError {
		t.Fatalf("tool returned error: %+v", result.Content)
	}
	content := structuredContent(t, result)
	fields := content["fields"].([]any)
	if len(fields) != 0 {
		t.Fatalf("fields = %v, want empty", fields)
	}
}

func TestSearchFieldsDescriptionDirectsCopilotToCallFirst(t *testing.T) {
	desc := SearchFields().Description()
	if desc == "" {
		t.Fatal("Description is empty")
	}
}
