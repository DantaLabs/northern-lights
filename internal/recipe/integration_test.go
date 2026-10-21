package recipe_test

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/bootstrap"
	"github.com/dantalabs/northern-lights/internal/changeset"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/demoenv"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/recipe"
	"github.com/dantalabs/northern-lights/internal/sectiontext"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/workiva"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

// TestPrepareWithRealChangeSetFeedsSectionTextGuard wires the services the way
// cmd/workiva-mcp does: L1's real change set service stages Prepare's items, and
// the FactStore returned by recipe.New is the one L3's number guard reads.
// Unit tests keep using fakes.
func TestPrepareWithRealChangeSetFeedsSectionTextGuard(t *testing.T) {
	abs := func(rel string) string {
		p, err := filepath.Abs(filepath.Join("..", "..", "configs", "demo", rel))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Setenv("NL_DEMO_ESG_DATA", abs("data"))
	t.Setenv("NL_DEMO_SECTIONS_DIR", abs("sections"))
	ctx := context.Background()

	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "demo.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := mapping.NewWithDB(db)
	if err != nil {
		t.Fatalf("mapping store: %v", err)
	}
	if err := bootstrap.LoadMappings(ctx, abs("mappings-demo.yaml"), store); err != nil {
		t.Fatalf("load mappings: %v", err)
	}
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	base, err := url.Parse("https://demo.invalid")
	if err != nil {
		t.Fatal(err)
	}
	env := demoenv.Env{
		DB: db, Store: store, Audit: log,
		Client: workivaprovider.NewRouter(workiva.NewDemoClient(base)),
		Cfg:    &config.Config{DemoMode: true},
	}

	recipes, facts, err := recipe.New(env, changeset.New(env))
	if err != nil {
		t.Fatalf("recipe.New: %v", err)
	}
	got, err := recipes.Prepare(ctx, "analyst", "esg-environmental")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	cs := got.ChangeSet
	if cs.ID == "" || cs.Token == "" || len(cs.Items) != 25 {
		t.Fatalf("change set id=%q token set=%v items=%d, want a staged set of 25", cs.ID, cs.Token != "", len(cs.Items))
	}
	if lines := strings.Split(cs.Summary, "\n"); len(lines) != 25 || !strings.HasSuffix(lines[0], "(awaiting_confirmation)") {
		t.Errorf("summary = %q, want 25 awaiting_confirmation lines", cs.Summary)
	}

	text := sectiontext.New(env, recipes, facts)
	if _, err := text.Stage(ctx, "analyst", "esg-environmental", []sectiontext.Block{
		{Kind: "narrative", Text: "Total energy consumption was 1.98 GWh in 2025, against 2.03 GWh in 2024."},
	}); err != nil {
		t.Fatalf("Stage with prepared numbers: %v", err)
	}
	_, err = text.Stage(ctx, "analyst", "esg-environmental", []sectiontext.Block{
		{Kind: "narrative", Text: "Total energy consumption was 7.77 GWh in 2025."},
	})
	if err == nil || !strings.Contains(err.Error(), "7.77") {
		t.Errorf("Stage with an unprepared number: err = %v, want the 7.77 refusal", err)
	}
	if _, err := text.Stage(ctx, "other-analyst", "esg-environmental", []sectiontext.Block{
		{Kind: "narrative", Text: "Energy was 1.98 GWh."},
	}); err == nil || !strings.Contains(err.Error(), "Prepare the section first") {
		t.Errorf("Stage for an actor without facts: err = %v, want Prepare the section first", err)
	}
}
