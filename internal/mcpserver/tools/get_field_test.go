package tools

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/mapping"
)

// sheetdataBody is the Workiva response for a single cell B3 holding 1234.
const sheetdataBody = `{
	"range": {"startRow": 2, "startColumn": 1, "stopRow": 2, "stopColumn": 1},
	"cells": [[{"value": "1234", "calculatedValue": 1234}]]
}`

func TestGetFieldLiveReadCachesValue(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/spreadsheets/sp-1/sheets/sh-1/sheetdata" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("$cellrange"); got != "B3" {
			t.Errorf("$cellrange = %q, want B3", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, sheetdataBody)
	}))
	seedField(t, env)

	result := callTool(t, env.deps, GetField(), map[string]any{"name": "scope2_energy_kwh"})
	if result.IsError {
		t.Fatalf("tool returned error: %+v", result.Content)
	}
	content := structuredContent(t, result)

	if content["value"] != "1234" {
		t.Errorf("value = %v, want 1234", content["value"])
	}
	if content["source"] != "live" {
		t.Errorf("source = %v, want live on cold cache", content["source"])
	}
	if content["fetched_at"] == "" {
		t.Error("fetched_at is empty")
	}
	if content["range"] != "B3" || content["field_type"] != "number" {
		t.Errorf("range/field_type = %v/%v, want B3/number", content["range"], content["field_type"])
	}
	if content["description"] != "Scope 2 energy consumption in kWh" {
		t.Errorf("description = %v", content["description"])
	}

	cached, err := env.deps.Store.GetCachedCells(context.Background(), "sp-1", "sh-1", 0)
	if err != nil {
		t.Fatalf("GetCachedCells: %v", err)
	}
	if len(cached) != 1 || cached[0].Value != "1234" {
		t.Errorf("cached cells = %+v, want B3=1234", cached)
	}
	if env.apiCalls.Load() != 1 {
		t.Errorf("apiCalls = %d, want 1", env.apiCalls.Load())
	}
}

func TestGetFieldServesFreshCacheWithoutAPICall(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Workiva API call: %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	seedField(t, env)
	if err := env.deps.Store.CacheCells(context.Background(), []mapping.CellValue{
		{
			SpreadsheetID: "sp-1", SheetID: "sh-1", Cell: "B3",
			Value: "9999", FetchedAt: time.Now(),
		},
	}); err != nil {
		t.Fatalf("CacheCells: %v", err)
	}

	result := callTool(t, env.deps, GetField(), map[string]any{"name": "scope2_energy_kwh"})
	if result.IsError {
		t.Fatalf("tool returned error: %+v", result.Content)
	}
	content := structuredContent(t, result)

	if content["value"] != "9999" {
		t.Errorf("value = %v, want 9999 from cache", content["value"])
	}
	if content["source"] != "cache" {
		t.Errorf("source = %v, want cache", content["source"])
	}
	if env.apiCalls.Load() != 0 {
		t.Errorf("apiCalls = %d, want 0 (fresh cache must not hit the API)", env.apiCalls.Load())
	}
}

func TestGetFieldRefreshesStaleCache(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, sheetdataBody)
	}))
	seedField(t, env)
	if err := env.deps.Store.CacheCells(context.Background(), []mapping.CellValue{
		{
			SpreadsheetID: "sp-1", SheetID: "sh-1", Cell: "B3",
			Value: "9999", FetchedAt: time.Now().Add(-time.Hour),
		},
	}); err != nil {
		t.Fatalf("CacheCells: %v", err)
	}

	result := callTool(t, env.deps, GetField(), map[string]any{"name": "scope2_energy_kwh"})
	if result.IsError {
		t.Fatalf("tool returned error: %+v", result.Content)
	}
	content := structuredContent(t, result)

	if content["value"] != "1234" {
		t.Errorf("value = %v, want 1234 (stale cache must be refreshed)", content["value"])
	}
	if content["source"] != "live" {
		t.Errorf("source = %v, want live", content["source"])
	}
	if env.apiCalls.Load() != 1 {
		t.Errorf("apiCalls = %d, want 1", env.apiCalls.Load())
	}
}

func TestGetFieldUnknownNameIsToolError(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	seedField(t, env)

	result := callTool(t, env.deps, GetField(), map[string]any{"name": "nope"})
	if !result.IsError {
		t.Fatal("expected tool error for unknown field name")
	}
}
