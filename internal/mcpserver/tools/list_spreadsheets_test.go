package tools

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/mapping"
)

func TestListSpreadsheetsReturnsMappedSpreadsheets(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	ctx := context.Background()

	if err := env.deps.Store.UpsertSpreadsheet(ctx, mapping.Spreadsheet{
		ID: "sp-1", Name: "VSME 2026", Region: "eu", SyncedAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertSpreadsheet: %v", err)
	}
	for _, sh := range []mapping.Sheet{
		{ID: "sh-1", SpreadsheetID: "sp-1", Name: "B3 Energy"},
		{ID: "sh-2", SpreadsheetID: "sp-1", Name: "B4 Water"},
	} {
		if err := env.deps.Store.UpsertSheet(ctx, sh); err != nil {
			t.Fatalf("UpsertSheet: %v", err)
		}
	}
	if err := env.deps.Store.UpsertSpreadsheet(ctx, mapping.Spreadsheet{
		ID: "sp-2", Name: "Empty Report", Region: "us",
	}); err != nil {
		t.Fatalf("UpsertSpreadsheet: %v", err)
	}
	env.deps.Client = nil

	result := callTool(t, env.deps, ListSpreadsheets(), map[string]any{})
	if result.IsError {
		t.Fatalf("tool returned error: %+v", result.Content)
	}
	content := structuredContent(t, result)

	spreadsheets, ok := content["spreadsheets"].([]any)
	if !ok {
		t.Fatalf("spreadsheets is %T, want array", content["spreadsheets"])
	}
	if len(spreadsheets) != 2 {
		t.Fatalf("len(spreadsheets) = %d, want 2", len(spreadsheets))
	}

	first, ok := spreadsheets[0].(map[string]any)
	if !ok {
		t.Fatalf("spreadsheets[0] is %T, want map", spreadsheets[0])
	}
	if first["id"] != "sp-1" || first["name"] != "VSME 2026" {
		t.Errorf("spreadsheets[0] id/name = %v/%v, want sp-1/VSME 2026", first["id"], first["name"])
	}
	sheets, ok := first["sheets"].([]any)
	if !ok {
		t.Fatalf("sheets is %T, want array", first["sheets"])
	}
	if len(sheets) != 2 {
		t.Fatalf("len(sheets) = %d, want 2", len(sheets))
	}
	sheet0 := sheets[0].(map[string]any)
	if sheet0["id"] != "sh-1" || sheet0["name"] != "B3 Energy" {
		t.Errorf("sheets[0] = %v, want sh-1/B3 Energy", sheet0)
	}

	second := spreadsheets[1].(map[string]any)
	if second["id"] != "sp-2" {
		t.Errorf("spreadsheets[1].id = %v, want sp-2", second["id"])
	}
	if sheets, ok := second["sheets"].([]any); ok && len(sheets) != 0 {
		t.Errorf("sp-2 sheets = %v, want none", sheets)
	}
}

func TestListSpreadsheetsEmptyStore(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	env.deps.Client = nil

	result := callTool(t, env.deps, ListSpreadsheets(), map[string]any{})
	if result.IsError {
		t.Fatalf("tool returned error: %+v", result.Content)
	}
	content := structuredContent(t, result)
	if spreadsheets := content["spreadsheets"].([]any); len(spreadsheets) != 0 {
		t.Fatalf("spreadsheets = %v, want empty", spreadsheets)
	}
}

func TestListSpreadsheetsDescriptionExplainsLiveAndMappedModes(t *testing.T) {
	desc := ListSpreadsheets().Description()
	for _, phrase := range []string{"live", "local mapping", "semantic mappings"} {
		if !strings.Contains(strings.ToLower(desc), phrase) {
			t.Errorf("description %q does not mention %q", desc, phrase)
		}
	}
}

func TestListSpreadsheetsDiscoversLiveWorkivaFilesAndSheets(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/spreadsheets":
			if _, err := fmt.Fprint(w, `{"data":[{"id":"live-sp-1","name":"EU Report","template":false}]}`); err != nil {
				t.Errorf("write spreadsheets response: %v", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/spreadsheets/live-sp-1/sheets":
			if _, err := fmt.Fprint(w, `{"data":[{"id":"live-sh-1","name":"Energy","index":0},{"id":"live-sh-2","name":"Water","index":1}]}`); err != nil {
				t.Errorf("write sheets response: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))

	result := callTool(t, env.deps, ListSpreadsheets(), map[string]any{})
	if result.IsError {
		t.Fatalf("tool returned error: %+v", result.Content)
	}
	content := structuredContent(t, result)
	spreadsheets, ok := content["spreadsheets"].([]any)
	if !ok || len(spreadsheets) != 1 {
		t.Fatalf("spreadsheets = %v, want one live spreadsheet", content["spreadsheets"])
	}
	sp := spreadsheets[0].(map[string]any)
	if sp["id"] != "live-sp-1" || sp["name"] != "EU Report" {
		t.Errorf("live spreadsheet = %v, want live-sp-1/EU Report", sp)
	}
	sheets, ok := sp["sheets"].([]any)
	if !ok || len(sheets) != 2 {
		t.Fatalf("live sheets = %v, want two sheets", sp["sheets"])
	}
	if sheets[0].(map[string]any)["id"] != "live-sh-1" || sheets[0].(map[string]any)["name"] != "Energy" {
		t.Errorf("first live sheet = %v, want live-sh-1/Energy", sheets[0])
	}
}
