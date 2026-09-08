package mapping

import (
	"context"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:) returned error: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUpsertAndGetSpreadsheet(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	sp := Spreadsheet{ID: "ss-1", Name: "VSME 2026", Region: "eu"}
	if err := s.UpsertSpreadsheet(ctx, sp); err != nil {
		t.Fatalf("UpsertSpreadsheet returned error: %v", err)
	}

	got, err := s.ListSpreadsheets(ctx)
	if err != nil {
		t.Fatalf("ListSpreadsheets returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListSpreadsheets returned %d spreadsheets, want 1", len(got))
	}
	if got[0].ID != sp.ID || got[0].Name != sp.Name || got[0].Region != sp.Region {
		t.Errorf("ListSpreadsheets[0] = %+v, want %+v", got[0], sp)
	}

	// Re-upsert updates instead of duplicating.
	sp.Name = "VSME 2027"
	if err := s.UpsertSpreadsheet(ctx, sp); err != nil {
		t.Fatalf("UpsertSpreadsheet (update) returned error: %v", err)
	}
	got, err = s.ListSpreadsheets(ctx)
	if err != nil {
		t.Fatalf("ListSpreadsheets returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("after re-upsert ListSpreadsheets returned %d spreadsheets, want 1", len(got))
	}
	if got[0].Name != "VSME 2027" {
		t.Errorf("spreadsheet name = %q, want %q", got[0].Name, "VSME 2027")
	}
}

func TestUpsertSheetAppearsInListSpreadsheets(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.UpsertSpreadsheet(ctx, Spreadsheet{ID: "ss-1", Name: "Book", Region: "eu"}); err != nil {
		t.Fatalf("UpsertSpreadsheet returned error: %v", err)
	}
	if err := s.UpsertSheet(ctx, Sheet{ID: "sh-1", SpreadsheetID: "ss-1", Name: "B3 Energy"}); err != nil {
		t.Fatalf("UpsertSheet returned error: %v", err)
	}

	got, err := s.ListSpreadsheets(ctx)
	if err != nil {
		t.Fatalf("ListSpreadsheets returned error: %v", err)
	}
	if len(got) != 1 || len(got[0].Sheets) != 1 {
		t.Fatalf("ListSpreadsheets = %+v, want 1 spreadsheet with 1 sheet", got)
	}
	if got[0].Sheets[0].ID != "sh-1" || got[0].Sheets[0].Name != "B3 Energy" {
		t.Errorf("sheet = %+v, want {sh-1 B3 Energy}", got[0].Sheets[0])
	}
}

func TestUpsertFieldRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	f := Field{
		SpreadsheetID: "ss-1",
		SheetID:       "sh-1",
		Name:          "scope2_energy_kwh",
		Aliases:       "scope 2,indirect energy",
		CellRange:     "B3",
		FieldType:     "number",
		Description:   "Scope 2 electricity consumption",
	}
	got, err := s.UpsertField(ctx, f)
	if err != nil {
		t.Fatalf("UpsertField returned error: %v", err)
	}
	if got.ID == 0 {
		t.Error("UpsertField returned ID 0, want assigned ID")
	}

	back, err := s.GetField(ctx, "scope2_energy_kwh")
	if err != nil {
		t.Fatalf("GetField returned error: %v", err)
	}
	if back == nil {
		t.Fatal("GetField returned nil, want field")
	}
	if back.ID != got.ID || back.Name != f.Name || back.Aliases != f.Aliases ||
		back.CellRange != f.CellRange || back.FieldType != f.FieldType ||
		back.Description != f.Description || back.SpreadsheetID != f.SpreadsheetID ||
		back.SheetID != f.SheetID {
		t.Errorf("GetField = %+v, want %+v", back, f)
	}
}

func TestGetFieldNotFound(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	f, err := s.GetField(ctx, "missing")
	if err != nil {
		t.Fatalf("GetField returned error: %v", err)
	}
	if f != nil {
		t.Errorf("GetField = %+v, want nil", f)
	}
}

func TestUpsertFieldUpdatesOnUniqueKey(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	f := Field{SpreadsheetID: "ss-1", SheetID: "sh-1", Name: "revenue", CellRange: "B1"}
	if _, err := s.UpsertField(ctx, f); err != nil {
		t.Fatalf("UpsertField returned error: %v", err)
	}
	f.CellRange = "C5"
	f.Description = "updated"
	second, err := s.UpsertField(ctx, f)
	if err != nil {
		t.Fatalf("UpsertField (re-upsert) returned error: %v", err)
	}

	back, err := s.GetField(ctx, "revenue")
	if err != nil {
		t.Fatalf("GetField returned error: %v", err)
	}
	if back == nil {
		t.Fatal("GetField returned nil, want field")
	}
	if back.ID != second.ID {
		t.Errorf("field ID changed across re-upsert: %d -> %d", back.ID, second.ID)
	}
	if back.CellRange != "C5" || back.Description != "updated" {
		t.Errorf("field after re-upsert = %+v, want updated range and description", back)
	}
}

func TestSearchFieldsRanking(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	seed := []Field{
		{SpreadsheetID: "ss-1", SheetID: "sh-1", Name: "energy", Aliases: "power", CellRange: "A1"},
		{SpreadsheetID: "ss-1", SheetID: "sh-1", Name: "energy_total", Aliases: "", CellRange: "A2"},
		{SpreadsheetID: "ss-1", SheetID: "sh-1", Name: "scope2_energy_kwh", Aliases: "indirect energy usage", CellRange: "A3"},
	}
	for _, f := range seed {
		if _, err := s.UpsertField(ctx, f); err != nil {
			t.Fatalf("UpsertField(%s) returned error: %v", f.Name, err)
		}
	}

	got, err := s.SearchFields(ctx, "energy")
	if err != nil {
		t.Fatalf("SearchFields returned error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("SearchFields returned %d fields, want 3: %+v", len(got), got)
	}
	// Exact name match must rank first.
	if got[0].Name != "energy" {
		t.Errorf("SearchFields[0].Name = %q, want exact match %q", got[0].Name, "energy")
	}
	// Alias-only match must rank below name matches.
	if got[len(got)-1].Name != "scope2_energy_kwh" {
		t.Errorf("SearchFields last = %q, want alias-only match %q", got[len(got)-1].Name, "scope2_energy_kwh")
	}
}

func TestSearchFieldsNoMatch(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	got, err := s.SearchFields(ctx, "zzz-no-such-field")
	if err != nil {
		t.Fatalf("SearchFields returned error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("SearchFields = %+v, want empty", got)
	}
}

func TestCacheCellsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	now := time.Now().UTC()
	cells := []CellValue{
		{SpreadsheetID: "ss-1", SheetID: "sh-1", Cell: "B3", Value: "12345", FetchedAt: now},
		{SpreadsheetID: "ss-1", SheetID: "sh-1", Cell: "B4", Value: "678", FetchedAt: now},
	}
	if err := s.CacheCells(ctx, cells); err != nil {
		t.Fatalf("CacheCells returned error: %v", err)
	}

	got, err := s.GetCachedCells(ctx, "ss-1", "sh-1", time.Minute)
	if err != nil {
		t.Fatalf("GetCachedCells returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("GetCachedCells returned %d cells, want 2: %+v", len(got), got)
	}
	byCell := map[string]string{}
	for _, c := range got {
		byCell[c.Cell] = c.Value
	}
	if byCell["B3"] != "12345" || byCell["B4"] != "678" {
		t.Errorf("cached cells = %v, want B3=12345 B4=678", byCell)
	}
}

func TestGetCachedCellsRespectsMaxAge(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	now := time.Now().UTC()
	cells := []CellValue{
		{SpreadsheetID: "ss-1", SheetID: "sh-1", Cell: "B3", Value: "fresh", FetchedAt: now},
		{SpreadsheetID: "ss-1", SheetID: "sh-1", Cell: "B4", Value: "stale", FetchedAt: now.Add(-time.Hour)},
	}
	if err := s.CacheCells(ctx, cells); err != nil {
		t.Fatalf("CacheCells returned error: %v", err)
	}

	got, err := s.GetCachedCells(ctx, "ss-1", "sh-1", 5*time.Minute)
	if err != nil {
		t.Fatalf("GetCachedCells returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("GetCachedCells returned %d cells, want 1 (stale row filtered): %+v", len(got), got)
	}
	if got[0].Value != "fresh" {
		t.Errorf("cached value = %q, want %q", got[0].Value, "fresh")
	}
}

func TestSearchFieldsLikeEscaping(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	for _, name := range []string{"100pct", "100x", "200"} {
		if _, err := s.UpsertField(ctx, Field{SpreadsheetID: "ss-1", SheetID: "sh-1", Name: name, CellRange: "A1"}); err != nil {
			t.Fatalf("UpsertField %q: %v", name, err)
		}
	}

	// A literal percent in the query must not act as a LIKE wildcard.
	got, err := s.SearchFields(ctx, "100%")
	if err != nil {
		t.Fatalf("SearchFields: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("query with literal %% matched %d fields, want 0 (no field contains \"100%%\")", len(got))
	}

	// Underscores must not act as single-char wildcards either.
	if _, err := s.UpsertField(ctx, Field{SpreadsheetID: "ss-1", SheetID: "sh-1", Name: "scope_1", CellRange: "A1"}); err != nil {
		t.Fatalf("UpsertField: %v", err)
	}
	if _, err := s.UpsertField(ctx, Field{SpreadsheetID: "ss-1", SheetID: "sh-1", Name: "scopex1", CellRange: "A1"}); err != nil {
		t.Fatalf("UpsertField: %v", err)
	}
	got, err = s.SearchFields(ctx, "scope_1")
	if err != nil {
		t.Fatalf("SearchFields: %v", err)
	}
	for _, f := range got {
		if f.Name == "scopex1" {
			t.Errorf("query \"scope_1\" matched scopex1; underscore treated as wildcard")
		}
	}
}
