package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/mapping"
)

func TestDeniedReadRangeDoesNotReachWorkiva(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, nil))
	env.deps.Cfg.AllowedResources = map[string][]string{"allowed": {"sheet"}}
	result := callTool(t, env.deps, ReadRange(), map[string]any{"spreadsheet_id": "denied", "sheet_id": "sheet", "range": "A1"})
	if !result.IsError {
		t.Fatal("denied read succeeded")
	}
	if got := env.apiCalls.Load(); got != 0 {
		t.Fatalf("Workiva calls = %d, want 0", got)
	}
}

func TestDeniedMappingDoesNotLeakFromSearch(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, nil))
	env.deps.Cfg.AllowedResources = map[string][]string{"allowed": {"sheet"}}
	if err := env.deps.Store.UpsertSpreadsheet(context.Background(), mapping.Spreadsheet{ID: "denied", Region: "eu"}); err != nil {
		t.Fatal(err)
	}
	if err := env.deps.Store.UpsertSheet(context.Background(), mapping.Sheet{ID: "sheet", SpreadsheetID: "denied"}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.deps.Store.UpsertField(context.Background(), mapping.Field{Name: "secret_energy", SpreadsheetID: "denied", SheetID: "sheet", CellRange: "A1"}); err != nil {
		t.Fatal(err)
	}
	result := callTool(t, env.deps, SearchFields(), map[string]any{"query": "secret"})
	if result.IsError {
		t.Fatalf("search error: %+v", result.Content)
	}
	if fields := structuredContent(t, result)["fields"].([]any); len(fields) != 0 {
		t.Fatalf("denied fields leaked: %+v", fields)
	}
}

func TestConfirmationRechecksChangedPolicy(t *testing.T) {
	var edits [][]byte
	env := newTestEnv(t, writeMock(t, &edits))
	seedField(t, env)
	staged := callTool(t, env.deps, UpdateField(), map[string]any{"name": "scope2_energy_kwh", "value": "9"})
	if staged.IsError {
		t.Fatalf("stage: %+v", staged.Content)
	}
	token := structuredContent(t, staged)["confirm_token"].(string)
	callsBefore := env.apiCalls.Load()
	env.deps.Cfg.AllowedResources = map[string][]string{"sp-1": {"another-sheet"}}
	confirmed := callTool(t, env.deps, UpdateField(), map[string]any{"name": "scope2_energy_kwh", "value": "9", "confirm_token": token})
	if !confirmed.IsError {
		t.Fatal("confirmation bypassed changed policy")
	}
	if env.apiCalls.Load() != callsBefore {
		t.Fatal("denied confirmation reached Workiva")
	}
	if len(edits) != 0 {
		t.Fatalf("Workiva writes = %d, want 0", len(edits))
	}
}

func TestDeniedMappedFieldDoesNotExposeTarget(t *testing.T) {
	for _, tool := range []mcpserver.Tool{GetField(), UpdateField()} {
		t.Run(tool.Name(), func(t *testing.T) {
			env := newTestEnv(t, tokenHandler(t, nil))
			seedField(t, env)
			env.deps.Cfg.AllowedResources = map[string][]string{"allowed": {"sheet"}}
			args := map[string]any{"name": "scope2_energy_kwh"}
			if tool.Name() == "workiva_update_field" {
				args["value"] = "9"
			}
			result := callTool(t, env.deps, tool, args)
			if !result.IsError {
				t.Fatal("denied resource succeeded")
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "sp-1") || strings.Contains(string(data), "sh-1") {
				t.Fatalf("target leaked: %s", data)
			}
			if env.apiCalls.Load() != 0 {
				t.Fatal("denied resource reached Workiva")
			}
		})
	}
}

func TestPolicyDeniesEveryTargetedToolBeforeHTTP(t *testing.T) {
	for _, policy := range []map[string][]string{{"other": {"*"}}, {"sp-1": {"other"}}} {
		for _, tool := range []mcpserver.Tool{ReadRange(), SyncMapping(), GetField(), UpdateField()} {
			for _, confirmation := range []bool{true, false} {
				t.Run(tool.Name(), func(t *testing.T) {
					env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
						t.Errorf("denied request: %s", r.URL.Path)
						http.Error(w, "unexpected", 500)
					})
					seedField(t, env)
					env.deps.Cfg.AllowedResources = policy
					env.deps.Cfg.RequireWriteConfirmation = confirmation
					if err := env.deps.Store.CacheCells(context.Background(), []mapping.CellValue{{SpreadsheetID: "sp-1", SheetID: "sh-1", Cell: "B3", Value: "PRIVATE-CACHE", FetchedAt: time.Now()}}); err != nil {
						t.Fatal(err)
					}
					args := map[string]any{"spreadsheet_id": "sp-1", "sheet_id": "sh-1"}
					switch tool.Name() {
					case "workiva_read_range":
						args["range"] = "B3"
					case "workiva_get_field":
						args = map[string]any{"name": "scope2_energy_kwh"}
					case "workiva_update_field":
						args = map[string]any{"name": "scope2_energy_kwh", "value": "9"}
					}
					result := callTool(t, env.deps, tool, args)
					if !result.IsError {
						t.Fatal("denied tool succeeded")
					}
					data, err := json.Marshal(result)
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(data), "PRIVATE-CACHE") {
						t.Fatal("cache leaked")
					}
					if env.apiCalls.Load() != 0 {
						t.Fatal("denied target reached Workiva")
					}
				})
			}
		}
	}
}

func TestPolicyFiltersLiveAndStoredDiscovery(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprint(live), func(t *testing.T) {
			env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/spreadsheets":
					_, _ = fmt.Fprint(w, `{"data":[{"id":"allowed"},{"id":"denied"}]}`)
				case "/spreadsheets/allowed/sheets":
					_, _ = fmt.Fprint(w, `{"data":[{"id":"sheet"},{"id":"secret-sheet"}]}`)
				default:
					t.Errorf("denied discovery request: %s", r.URL.Path)
					http.Error(w, "unexpected", 500)
				}
			}))
			ctx := context.Background()
			for _, id := range []string{"allowed", "denied"} {
				if err := env.deps.Store.UpsertSpreadsheet(ctx, mapping.Spreadsheet{ID: id}); err != nil {
					t.Fatal(err)
				}
				for _, sheet := range []string{"sheet", "secret-sheet"} {
					if err := env.deps.Store.UpsertSheet(ctx, mapping.Sheet{ID: sheet, SpreadsheetID: id}); err != nil {
						t.Fatal(err)
					}
				}
			}
			env.deps.Cfg.AllowedResources = map[string][]string{"allowed": {"sheet"}}
			if !live {
				env.deps.Client = nil
			}
			result := callTool(t, env.deps, ListSpreadsheets(), map[string]any{})
			if result.IsError {
				t.Fatalf("list: %+v", result.Content)
			}
			spreadsheets := structuredContent(t, result)["spreadsheets"].([]any)
			if len(spreadsheets) != 1 {
				t.Fatalf("spreadsheets: %+v", spreadsheets)
			}
			sp := spreadsheets[0].(map[string]any)
			sheets := sp["sheets"].([]any)
			if sp["id"] != "allowed" || len(sheets) != 1 || sheets[0].(map[string]any)["id"] != "sheet" {
				t.Fatalf("unfiltered: %+v", sp)
			}
		})
	}
}

func TestSearchFiltersDeniedSheetsAndCachedValues(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, nil))
	ctx := context.Background()
	if err := env.deps.Store.UpsertSpreadsheet(ctx, mapping.Spreadsheet{ID: "sp"}); err != nil {
		t.Fatal(err)
	}
	for _, sheet := range []string{"allowed", "denied"} {
		if err := env.deps.Store.UpsertSheet(ctx, mapping.Sheet{SpreadsheetID: "sp", ID: sheet}); err != nil {
			t.Fatal(err)
		}
		if _, err := env.deps.Store.UpsertField(ctx, mapping.Field{SpreadsheetID: "sp", SheetID: sheet, Name: "energy_" + sheet, CellRange: "A1"}); err != nil {
			t.Fatal(err)
		}
		if err := env.deps.Store.CacheCells(ctx, []mapping.CellValue{{SpreadsheetID: "sp", SheetID: sheet, Cell: "A1", Value: sheet + "-value", FetchedAt: time.Now()}}); err != nil {
			t.Fatal(err)
		}
	}
	env.deps.Cfg.AllowedResources = map[string][]string{"sp": {"allowed"}}
	result := callTool(t, env.deps, SearchFields(), map[string]any{"query": "energy"})
	if result.IsError {
		t.Fatalf("search: %+v", result.Content)
	}
	fields := structuredContent(t, result)["fields"].([]any)
	if len(fields) != 1 {
		t.Fatalf("fields: %+v", fields)
	}
	field := fields[0].(map[string]any)
	if field["name"] != "energy_allowed" || field["cached_value"] != "allowed-value" {
		t.Fatalf("field: %+v", field)
	}
	if env.apiCalls.Load() != 0 {
		t.Fatal("search called Workiva")
	}
}
