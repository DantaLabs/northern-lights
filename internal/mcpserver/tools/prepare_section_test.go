package tools

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/bootstrap"
	"github.com/dantalabs/northern-lights/internal/changeset"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/demoenv"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/recipe"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/workiva"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

// fakeRecipes is a recipe.Service with canned data that records calls.
type fakeRecipes struct {
	sections []recipe.SectionInfo
	prepared recipe.Prepared
	err      error

	calls   int
	actor   string
	section string
}

func (f *fakeRecipes) Sections() []recipe.SectionInfo { return f.sections }

func (f *fakeRecipes) Prepare(_ context.Context, actor, sectionID string) (recipe.Prepared, error) {
	f.calls++
	f.actor, f.section = actor, sectionID
	return f.prepared, f.err
}

func callPrepareSection(t *testing.T, fake *fakeRecipes, sectionID string) *mcp.CallToolResult {
	t.Helper()
	deps := newTestEnv(t, http.NotFound).deps
	deps.Recipes = fake
	session := serveDemoTools(t, deps)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "workiva_prepare_section", Arguments: map[string]any{"section_id": sectionID},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	return result
}

func TestScenario8UnknownSectionListsSections(t *testing.T) {
	fake := &fakeRecipes{sections: []recipe.SectionInfo{
		{ID: "environmental", Title: "Environmental Disclosures"},
		{ID: "governance", Title: "Governance Disclosures"},
	}}
	result := callPrepareSection(t, fake, "nope")
	if !result.IsError {
		t.Fatalf("want an error result, got %+v", result)
	}
	text := resultText(result)
	for _, want := range []string{`unknown section ID \"nope\"`, "environmental (Environmental Disclosures)", "governance (Governance Disclosures)"} {
		if !strings.Contains(text, want) {
			t.Errorf("error text missing %q: %s", want, text)
		}
	}
	if fake.calls != 0 {
		t.Errorf("Prepare called %d times for an unknown section, want 0", fake.calls)
	}
}

func TestScenario8UnknownSectionWithNoRecipes(t *testing.T) {
	result := callPrepareSection(t, &fakeRecipes{}, "environmental")
	if !result.IsError || !strings.Contains(resultText(result), "NL_DEMO_SECTIONS_DIR") {
		t.Fatalf("want a hint naming NL_DEMO_SECTIONS_DIR, got %s", resultText(result))
	}
}

func TestScenario8KnownSectionReturnsPreparedSummaryAndNextStep(t *testing.T) {
	expires := time.Date(2026, 10, 20, 12, 30, 0, 0, time.FixedZone("x", 3600))
	fake := &fakeRecipes{
		sections: []recipe.SectionInfo{{ID: "environmental", Title: "Environmental Disclosures"}},
		prepared: recipe.Prepared{
			SectionID: "environmental",
			Findings:  []recipe.Finding{{Kind: "material_change", Field: "energy", Severity: "info", Message: "energy rose 8%"}},
			Facts: []recipe.Fact{{
				Key: "energy", Label: "Energy use", Unit: "GWh", Current: "1.24", Prior: "1.15",
				Change: "0.09", ChangePct: "7.8", Direction: "up", Material: true,
				Display: []string{"1.24 GWh", "8%", "7.8%"},
			}},
			Verbatim: []recipe.VerbatimBlock{{ID: "policy", Title: "Policy", Text: "Exact policy text."}},
			ChangeSet: changeset.Preview{
				ID: "cs-1", Token: "tok-1", ExpiresAt: expires,
				Summary: "energy: 1.15 -> 1.24 (awaiting_confirmation)\n" +
					`note: old -> line one\nline two\r (awaiting_confirmation)`,
				Items: []changeset.PreviewItem{
					{FieldName: "energy", SpreadsheetID: "sp-1", SheetID: "sh-1", Range: "B3", Before: "1.15", After: "1.24", Source: "meter"},
					{FieldName: "note", SpreadsheetID: "sp-1", SheetID: "sh-1", Range: "B4", Before: "old", After: "line one\nline two\r"},
				},
			},
		},
	}
	result := callPrepareSection(t, fake, "environmental")
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(result))
	}
	if fake.calls != 1 || fake.section != "environmental" || fake.actor != "tester@example.com" {
		t.Errorf("Prepare calls=%d section=%q actor=%q", fake.calls, fake.section, fake.actor)
	}
	out := structuredContent(t, result)

	if out["section_id"] != "environmental" {
		t.Errorf("section_id = %v", out["section_id"])
	}
	findings := out["findings"].([]any)
	if len(findings) != 1 || findings[0].(map[string]any)["message"] != "energy rose 8%" {
		t.Errorf("findings = %v", findings)
	}
	facts := out["facts"].([]any)
	fact := facts[0].(map[string]any)
	if fact["current"] != "1.24" || fact["prior"] != "1.15" || fact["change"] != "0.09" || fact["change_pct"] != "7.8" ||
		fact["direction"] != "up" || fact["material"] != true {
		t.Errorf("fact = %v", fact)
	}
	display := fact["display"].([]any)
	if len(display) != 3 || display[0] != "1.24 GWh" || display[1] != "8%" || display[2] != "7.8%" {
		t.Errorf("display = %v", display)
	}
	verbatim := out["verbatim"].([]any)[0].(map[string]any)
	if verbatim["id"] != "policy" || verbatim["text"] != "Exact policy text." {
		t.Errorf("verbatim = %v", verbatim)
	}

	cs := out["change_set"].(map[string]any)
	if cs["change_set_id"] != "cs-1" || cs["confirm_token"] != "tok-1" || cs["expires_at"] != "2026-10-20T11:30:00Z" {
		t.Errorf("change_set = %v", cs)
	}
	items := cs["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["range"] != "B3" || items[0].(map[string]any)["source"] != "meter" {
		t.Errorf("items = %v", items)
	}

	wantSummary := "energy: 1.15 -> 1.24 (awaiting_confirmation)\n" +
		`note: old -> line one\nline two\r (awaiting_confirmation)`
	if out["summary"] != wantSummary {
		t.Errorf("summary = %q, want %q", out["summary"], wantSummary)
	}

	next, _ := out["next_step"].(string)
	last := -1
	for _, word := range []string{"findings", "summary", "approval", "workiva_confirm_change_set"} {
		i := strings.Index(next, word)
		if i <= last {
			t.Fatalf("next_step %q: %q not found after position %d", next, word, last)
		}
		last = i
	}
}

func TestScenario8EmptyPreparedUsesEmptyArrays(t *testing.T) {
	fake := &fakeRecipes{
		sections: []recipe.SectionInfo{{ID: "environmental", Title: "E"}},
		prepared: recipe.Prepared{SectionID: "environmental", ChangeSet: changeset.Preview{ID: "cs", Token: "t"}},
	}
	result := callPrepareSection(t, fake, "environmental")
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(result))
	}
	out := structuredContent(t, result)
	for _, k := range []string{"findings", "facts", "verbatim"} {
		if arr, ok := out[k].([]any); !ok || len(arr) != 0 {
			t.Errorf("%s = %#v, want empty array", k, out[k])
		}
	}
	if items, ok := out["change_set"].(map[string]any)["items"].([]any); !ok || len(items) != 0 {
		t.Errorf("items = %#v, want empty array", items)
	}
}

func TestPrepareSectionRequiresRecipes(t *testing.T) {
	session := serveDemoTools(t, newTestEnv(t, http.NotFound).deps)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "workiva_prepare_section", Arguments: map[string]any{"section_id": "environmental"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError || !strings.Contains(resultText(result), "Recipes is not available") {
		t.Fatalf("want Recipes is not available, got %s", resultText(result))
	}
}

// stagingChanges is a changeset.Service whose previews resolve each item
// through the mapping store, as L1's real service will.
type stagingChanges struct{ store *mapping.Store }

func (c stagingChanges) Stage(ctx context.Context, _ string, items []changeset.Item) (changeset.Preview, error) {
	p := changeset.Preview{ID: "cs-demo", Token: "tok-demo", ExpiresAt: time.Now().Add(time.Hour)}
	var lines []string
	for _, it := range items {
		f, err := c.store.GetField(ctx, it.FieldName)
		if err != nil || f == nil {
			return changeset.Preview{}, errors.New("unknown target " + it.FieldName)
		}
		p.Items = append(p.Items, changeset.PreviewItem{
			FieldName: it.FieldName, SpreadsheetID: f.SpreadsheetID, SheetID: f.SheetID,
			Range: f.CellRange, After: it.Value, Source: it.Source,
		})
		lines = append(lines, it.FieldName+": -> "+it.Value+" (awaiting_confirmation)")
	}
	p.Summary = strings.Join(lines, "\n")
	return p, nil
}

func (stagingChanges) Confirm(context.Context, string, string, string) (changeset.Result, error) {
	return changeset.Result{}, errors.New("not used")
}

func TestScenario9PrepareSectionToolOnDemoData(t *testing.T) {
	abs := func(rel string) string {
		p, err := filepath.Abs(filepath.Join("..", "..", "..", "configs", "demo", rel))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Setenv("NL_DEMO_ESG_DATA", abs("data"))
	t.Setenv("NL_DEMO_SECTIONS_DIR", abs("sections"))

	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "demo.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := mapping.NewWithDB(db)
	if err != nil {
		t.Fatalf("mapping store: %v", err)
	}
	ctx := context.Background()
	if err := bootstrap.LoadMappings(ctx, abs("mappings-demo.yaml"), store); err != nil {
		t.Fatalf("load mappings: %v", err)
	}
	if f, err := store.GetField(ctx, "src_env_energy_total"); err != nil || f == nil {
		t.Fatalf("src_env_energy_total not loaded (wrong configs path?): %v", err)
	}
	base, err := url.Parse("https://demo.invalid")
	if err != nil {
		t.Fatal(err)
	}
	router := workivaprovider.NewRouter(workiva.NewDemoClient(base))
	svc, facts, err := recipe.New(demoenv.Env{DB: db, Store: store, Client: router, Cfg: &config.Config{DemoMode: true}}, stagingChanges{store: store})
	if err != nil {
		t.Fatalf("recipe.New: %v", err)
	}

	deps := newTestEnv(t, http.NotFound).deps
	deps.Store, deps.Client, deps.Recipes, deps.Facts = store, router, svc, facts
	session := serveDemoTools(t, deps)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "workiva_prepare_section", Arguments: map[string]any{"section_id": "esg-environmental"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(result))
	}
	out := structuredContent(t, result)
	if n := len(out["findings"].([]any)); n != 3 {
		t.Errorf("findings = %d, want 3: %v", n, out["findings"])
	}
	items := out["change_set"].(map[string]any)["items"].([]any)
	if len(items) != 25 {
		t.Errorf("change_set items = %d, want 25", len(items))
	}
	if lines := strings.Split(out["summary"].(string), "\n"); len(lines) != 25 {
		t.Errorf("summary lines = %d, want 25", len(lines))
	}
	if next, _ := out["next_step"].(string); next == "" {
		t.Error("next_step is empty")
	}
}
