package changeset

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/ratelimit"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

// Run once with credentials exported from deployments/.env:
// NL_LIVE_SANDBOX=1 go test ./internal/changeset -run TestChangeSet_S10_LiveScratchCells -count=1 -v
// Resource discovery is by exact name; every write is restricted to A10:C10
// in Testing MCP / Sheet1. The cleanup uses a second approved change set.
func TestChangeSet_S10_LiveScratchCells(t *testing.T) {
	if os.Getenv("NL_LIVE_SANDBOX") != "1" {
		t.Skip("set NL_LIVE_SANDBOX=1 with sandbox credentials")
	}
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkivaClientID == "" || cfg.WorkivaClientSecret == "" {
		t.Fatal("sandbox credentials are required")
	}
	base, err := url.Parse(cfg.BaseURL())
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	tokens := workiva.NewTokenProvider(base, cfg.WorkivaClientID, cfg.WorkivaClientSecret, "file:read file:write", httpClient)
	client := workiva.NewClient(base, tokens, ratelimit.NewLimiter(), httpClient)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	spreadsheets, err := client.ListSpreadsheets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var spreadsheetID, sheetID string
	for _, sp := range spreadsheets {
		if sp.Name == "Testing MCP" {
			if spreadsheetID != "" {
				t.Fatal("ambiguous Testing MCP resource")
			}
			spreadsheetID = sp.ID
		}
	}
	if spreadsheetID == "" {
		t.Fatal("Testing MCP spreadsheet not found")
	}
	sheets, err := client.ListSheets(ctx, spreadsheetID)
	if err != nil {
		t.Fatal(err)
	}
	for _, sh := range sheets {
		if sh.Name == "Sheet1" {
			if sheetID != "" {
				t.Fatal("ambiguous Sheet1")
			}
			sheetID = sh.ID
		}
	}
	if sheetID == "" {
		t.Fatal("Sheet1 not found in Testing MCP")
	}
	s, _, _ := fixture(t, 0)
	s.env.Client = client
	s.env.Cfg = &config.Config{AllowedResources: map[string][]string{spreadsheetID: {sheetID}}}
	var items, restore []Item
	for i, ref := range []string{"A10", "B10", "C10"} {
		// Read literal source values for exact restoration, including numeric
		// lexemes. Refuse formulas because this lane only writes literal values.
		data, err := client.GetSheetDataTyped(ctx, spreadsheetID, sheetID, ref, []string{"cells.value"})
		if err != nil {
			t.Fatal(err)
		}
		original := ""
		if len(data.Cells) > 0 && len(data.Cells[0]) > 0 {
			cell := data.Cells[0][0]
			if text, ok := cell.Value.(string); ok && strings.HasPrefix(text, "=") {
				t.Fatalf("scratch %s contains a formula; choose cleared scratch cells before running", ref)
			}
			original = cellText(cell)
		}
		name := fmt.Sprintf("live_l1_scratch_%d", i)
		if _, err := s.env.Store.UpsertField(ctx, mapping.Field{Name: name, SpreadsheetID: spreadsheetID, SheetID: sheetID, CellRange: ref}); err != nil {
			t.Fatal(err)
		}
		items = append(items, Item{FieldName: name, Value: fmt.Sprint(731001 + i)})
		restore = append(restore, Item{FieldName: name, Value: original})
	}
	preview, err := s.Stage(ctx, "l1-live-test", items)
	if err != nil {
		t.Fatal(err)
	}
	// Register restoration before the mutation, so a failed assertion or an
	// uncertain response still restores all three original literal values.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanupCancel()
		p, err := s.Stage(cleanupCtx, "l1-live-test", restore)
		if err != nil {
			t.Errorf("stage scratch restoration: %v", err)
			return
		}
		result, err := s.Confirm(cleanupCtx, "l1-live-test", p.ID, p.Token)
		if err != nil || result.Status != "completed" {
			t.Errorf("restore scratch values: status=%s err=%v", result.Status, err)
			return
		}
		for i, item := range result.Items {
			if item.ReadBack != restore[i].Value {
				t.Errorf("restoration read-back differs for %s", item.FieldName)
			}
		}
		t.Log("restored and read back original A10:C10 values in Testing MCP / Sheet1")
	})
	result, err := s.Confirm(ctx, "l1-live-test", preview.ID, preview.Token)
	if err != nil || result.Status != "completed" {
		t.Fatalf("scratch confirmation: status=%s err=%v", result.Status, err)
	}
	if len(result.Items) != 3 {
		t.Fatalf("got %d written fields", len(result.Items))
	}
	for i, item := range result.Items {
		if item.Status != "completed" || item.ReadBack != items[i].Value {
			t.Fatalf("scratch read-back differs for %s", item.FieldName)
		}
	}
	if err := s.env.Audit.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("confirmed and read back three scratch values in Testing MCP / Sheet1")
}
