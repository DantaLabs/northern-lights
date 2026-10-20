package recipe

import (
	"context"
	"encoding/csv"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/bootstrap"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/demoenv"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/workiva"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

// demoScenarioAbs resolves a path under configs/demo from this package.
func demoScenarioAbs(t *testing.T, rel string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "configs", "demo", rel))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// demoScenarioEnv builds the recipe service over the committed demo mappings,
// sections and data, with the offline demo client in demo mode.
func demoScenarioEnv(t *testing.T) (Service, *prepareTestChanges) {
	t.Helper()
	t.Setenv("NL_DEMO_ESG_DATA", demoScenarioAbs(t, "data"))
	t.Setenv("NL_DEMO_SECTIONS_DIR", demoScenarioAbs(t, "sections"))
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "demo.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := mapping.NewWithDB(db)
	if err != nil {
		t.Fatalf("mapping store: %v", err)
	}
	if err := bootstrap.LoadMappings(context.Background(), demoScenarioAbs(t, "mappings-demo.yaml"), store); err != nil {
		t.Fatalf("load mappings: %v", err)
	}
	if f, err := store.GetField(context.Background(), "src_env_energy_total"); err != nil || f == nil {
		t.Fatalf("src_env_energy_total not loaded (wrong mappings path?): %v", err)
	}
	base, err := url.Parse("https://demo.invalid")
	if err != nil {
		t.Fatal(err)
	}
	env := demoenv.Env{DB: db, Store: store, Client: workivaprovider.NewRouter(workiva.NewDemoClient(base)), Cfg: &config.Config{DemoMode: true}}
	changes := &prepareTestChanges{store: store}
	svc, _, err := New(env, changes)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, changes
}

// demoScenarioCanonical strips trailing zeros of a decimal string.
func demoScenarioCanonical(v string) string {
	if strings.Contains(v, ".") {
		v = strings.TrimRight(strings.TrimRight(v, "0"), ".")
	}
	return v
}

func TestScenario9DemoModeEnvironmentalSection(t *testing.T) {
	svc, changes := demoScenarioEnv(t)
	ids := map[string]bool{}
	for _, s := range svc.Sections() {
		ids[s.ID] = true
	}
	if !ids["esg-environmental"] || !ids["esg-social"] || len(ids) != 2 {
		t.Fatalf("sections = %v, want esg-environmental and esg-social", ids)
	}
	got, err := svc.Prepare(context.Background(), "analyst", "esg-environmental")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	wantFindings := map[string]string{
		"Water discharge from undertaking production processes (m³)": "missing",
		"6. Business Travel": "material_change",
		"Electricity (as reflected in utility billings) - Renewable": "out_of_range",
	}
	if len(got.Findings) != len(wantFindings) {
		t.Errorf("findings = %+v, want exactly %d", got.Findings, len(wantFindings))
	}
	for _, f := range got.Findings {
		if wantFindings[f.Field] != f.Kind {
			t.Errorf("unexpected finding %+v", f)
		}
	}

	// Expected change set: CSV rows whose key is an esg-environmental field key
	// and whose value is non-empty.
	keys := map[string]bool{}
	for _, f := range svc.(*service).recipes["esg-environmental"].Fields {
		keys[f.Key] = true
	}
	file, err := os.Open(demoScenarioAbs(t, "data/esg-2025.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	rows, err := csv.NewReader(file).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	type want struct{ field, source, value string }
	var wantItems []want
	for _, row := range rows[1:] {
		if keys[row[0]] && row[3] != "" {
			wantItems = append(wantItems, want{"rpt_env_" + row[0] + "_2025", "src_env_" + row[0], demoScenarioCanonical(row[3])})
		}
	}
	if len(wantItems) != 25 {
		t.Fatalf("expected %d items from CSV, want 25", len(wantItems))
	}
	if len(changes.calls) != 1 || len(changes.calls[0]) != len(wantItems) {
		t.Fatalf("Stage calls = %d, want 1 call with %d items", len(changes.calls), len(wantItems))
	}
	for i, it := range changes.calls[0] {
		w := wantItems[i]
		if it.FieldName != w.field || it.Source != w.source || it.Value != w.value {
			t.Errorf("item %d = %+v, want %+v", i, it, w)
		}
	}

	var energy *Fact
	for i := range got.Facts {
		if got.Facts[i].Key == "energy_total" {
			energy = &got.Facts[i]
		}
	}
	if energy == nil {
		t.Fatal("no energy_total fact")
	}
	shown := strings.Join(energy.Display, "|")
	if !strings.Contains(shown, "1.98 GWh") || !strings.Contains(shown, "2.03 GWh") {
		t.Errorf("energy_total display = %v, want 1.98 GWh and 2.03 GWh", energy.Display)
	}
}

func TestDemoModeIgnoresDocumentIDOverride(t *testing.T) {
	t.Setenv("NL_DEMO_DOCUMENT_ID", "real-document-id")
	svc, _ := demoScenarioEnv(t)
	for _, s := range svc.Sections() {
		if s.ID == "esg-environmental" {
			if s.DocumentID != "demo-doc-annual-report" {
				t.Errorf("DocumentID = %q, want demo-doc-annual-report in demo mode", s.DocumentID)
			}
			return
		}
	}
	t.Fatal("esg-environmental not loaded")
}
