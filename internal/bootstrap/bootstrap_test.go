package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dantalabs/northern-lights/internal/mapping"
)

func TestLoadMappingsMissingFileIsNoOp(t *testing.T) {
	store, err := mapping.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	if err := LoadMappings(context.Background(), missing, store); err != nil {
		t.Fatalf("LoadMappings on missing file: %v", err)
	}
	sps, err := store.ListSpreadsheets(context.Background())
	if err != nil {
		t.Fatalf("list spreadsheets: %v", err)
	}
	if len(sps) != 0 {
		t.Errorf("got %d spreadsheets, want 0", len(sps))
	}
}

func TestLoadMappingsExampleFile(t *testing.T) {
	ctx := context.Background()
	store, err := mapping.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if err := LoadMappings(ctx, "../../configs/example.yaml", store); err != nil {
		t.Fatalf("LoadMappings: %v", err)
	}

	sps, err := store.ListSpreadsheets(ctx)
	if err != nil {
		t.Fatalf("list spreadsheets: %v", err)
	}
	if len(sps) != 1 {
		t.Fatalf("got %d spreadsheets, want 1", len(sps))
	}
	sp := sps[0]
	if sp.ID != "abc123" || sp.Name != "VSME 2026" {
		t.Errorf("spreadsheet = %+v, want abc123 / VSME 2026", sp)
	}
	if len(sp.Sheets) != 1 {
		t.Fatalf("got %d sheets, want 1", len(sp.Sheets))
	}
	if sp.Sheets[0].ID != "sheet-1" || sp.Sheets[0].Name != "B3 Energy" {
		t.Errorf("sheet = %+v, want sheet-1 / B3 Energy", sp.Sheets[0])
	}

	f, err := store.GetField(ctx, "scope1_kwh")
	if err != nil {
		t.Fatalf("GetField: %v", err)
	}
	if f == nil {
		t.Fatal("GetField(scope1_kwh) = nil, want a field")
	}
	if f.CellRange != "B3" || f.FieldType != "number" {
		t.Errorf("field range/type = %q/%q, want B3/number", f.CellRange, f.FieldType)
	}
	if f.Aliases != "scope 1,direct energy" {
		t.Errorf("aliases = %q, want %q", f.Aliases, "scope 1,direct energy")
	}

	matches, err := store.SearchFields(ctx, "scope 1")
	if err != nil {
		t.Fatalf("SearchFields: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("SearchFields(\"scope 1\") returned no matches, want scope1_kwh via alias")
	}
	if matches[0].Name != "scope1_kwh" {
		t.Errorf("top match = %q, want scope1_kwh", matches[0].Name)
	}
}

func TestLoadMappingsIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store, err := mapping.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	for i := 0; i < 2; i++ {
		if err := LoadMappings(ctx, "../../configs/example.yaml", store); err != nil {
			t.Fatalf("LoadMappings run %d: %v", i+1, err)
		}
	}

	sps, err := store.ListSpreadsheets(ctx)
	if err != nil {
		t.Fatalf("list spreadsheets: %v", err)
	}
	if len(sps) != 1 || len(sps[0].Sheets) != 1 {
		t.Fatalf("after double load: %d spreadsheets, first has %d sheets; want 1/1", len(sps), len(sps[0].Sheets))
	}
	matches, err := store.SearchFields(ctx, "scope")
	if err != nil {
		t.Fatalf("SearchFields: %v", err)
	}
	if len(matches) != 2 {
		t.Errorf("SearchFields(\"scope\") = %d matches, want 2 (no duplicates)", len(matches))
	}
}

func TestLoadMappingsUpdatesExistingRows(t *testing.T) {
	ctx := context.Background()
	store, err := mapping.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "mappings.yaml")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write mappings file: %v", err)
		}
	}

	write("spreadsheets:\n  - id: s1\n    name: Old\n    sheets:\n      - id: sh1\n        name: Sheet\n        fields:\n          - { name: f1, range: A1 }\n")
	if err := LoadMappings(ctx, path, store); err != nil {
		t.Fatalf("LoadMappings v1: %v", err)
	}

	write("spreadsheets:\n  - id: s1\n    name: New\n    sheets:\n      - id: sh1\n        name: Sheet\n        fields:\n          - { name: f1, range: B2, description: moved }\n")
	if err := LoadMappings(ctx, path, store); err != nil {
		t.Fatalf("LoadMappings v2: %v", err)
	}

	f, err := store.GetField(ctx, "f1")
	if err != nil || f == nil {
		t.Fatalf("GetField after update: %v (field %v)", err, f)
	}
	if f.CellRange != "B2" || f.Description != "moved" {
		t.Errorf("field after update = %+v, want B2/moved", f)
	}
}
