package recipe

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/bootstrap"
	"github.com/dantalabs/northern-lights/internal/changeset"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/demoenv"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/workiva"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

// prepareTestBackend is a workivaprovider.Backend that serves one cell per
// range from a map and counts how it was read.
type prepareTestBackend struct {
	mu         sync.Mutex
	cells      map[string]any // range -> value; missing means nil
	typedCalls int
	plainCalls int
}

func (b *prepareTestBackend) ListSpreadsheets(context.Context) ([]workiva.Spreadsheet, error) {
	return nil, nil
}

func (b *prepareTestBackend) ListSheets(context.Context, string) ([]workiva.Sheet, error) {
	return nil, nil
}

func (b *prepareTestBackend) GetSheetData(_ context.Context, _, _, cellRange string, _ []string) (*workiva.SheetData, error) {
	b.mu.Lock()
	b.plainCalls++
	b.mu.Unlock()
	return &workiva.SheetData{Cells: [][]workiva.Cell{{{Value: b.cells[cellRange]}}}}, nil
}

func (b *prepareTestBackend) GetSheetDataTyped(_ context.Context, _, _, cellRange string, _ []string) (*workiva.SheetData, error) {
	b.mu.Lock()
	b.typedCalls++
	b.mu.Unlock()
	return &workiva.SheetData{Cells: [][]workiva.Cell{{{Value: b.cells[cellRange]}}}}, nil
}

func (b *prepareTestBackend) UpdateSheetWithRetryAfter(context.Context, string, string, workiva.SheetUpdate) (string, time.Duration, error) {
	return "", 0, errors.New("prepare must not write")
}

func (b *prepareTestBackend) WaitOperationWithInitialRetryAfter(context.Context, string, time.Duration) (string, error) {
	return "", errors.New("prepare must not write")
}

// prepareTestChanges is a recording fake changeset.Service. Stage resolves each
// target through the mapping store so the preview satisfies the tool schema.
type prepareTestChanges struct {
	store    *mapping.Store
	calls    [][]changeset.Item
	stageErr error
}

func (c *prepareTestChanges) Stage(ctx context.Context, _ string, items []changeset.Item) (changeset.Preview, error) {
	c.calls = append(c.calls, append([]changeset.Item(nil), items...))
	if c.stageErr != nil {
		return changeset.Preview{}, c.stageErr
	}
	p := changeset.Preview{ID: "cs-1", Token: "tok-1", ExpiresAt: time.Now().Add(time.Hour)}
	for _, it := range items {
		f, err := c.store.GetField(ctx, it.FieldName)
		if err != nil || f == nil {
			return changeset.Preview{}, errors.New("unknown target " + it.FieldName)
		}
		p.Items = append(p.Items, changeset.PreviewItem{
			FieldName: it.FieldName, SpreadsheetID: f.SpreadsheetID, SheetID: f.SheetID,
			Range: f.CellRange, After: it.Value, Source: it.Source,
		})
	}
	return p, nil
}

func (c *prepareTestChanges) Confirm(context.Context, string, string, string) (changeset.Result, error) {
	return changeset.Result{}, errors.New("not used")
}

type prepareTestRig struct {
	svc     Service
	facts   FactStore
	backend *prepareTestBackend
	changes *prepareTestChanges
	store   *mapping.Store
}

// prepareTestSetup loads the named testdata/prepare recipes into a fresh
// sections directory (plus extra files from prepareTestWrite) and builds the
// service through New. Pass nil cells to leave every cell empty.
func prepareTestSetup(t *testing.T, cells map[string]any, yamls ...string) prepareTestRig {
	t.Helper()
	dir := t.TempDir()
	prepareTestCopyRecipes(t, dir, yamls...)
	return prepareTestBuild(t, dir, cells, true)
}

func prepareTestCopyRecipes(t *testing.T, dir string, yamls ...string) {
	t.Helper()
	for _, name := range yamls {
		data, err := os.ReadFile(filepath.Join("testdata", "prepare", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func prepareTestBuild(t *testing.T, dir string, cells map[string]any, withClient bool) prepareTestRig {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "prep.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := mapping.NewWithDB(db)
	if err != nil {
		t.Fatalf("mapping store: %v", err)
	}
	if err := bootstrap.LoadMappings(context.Background(), filepath.Join("testdata", "prepare", "mappings.yaml"), store); err != nil {
		t.Fatalf("load mappings: %v", err)
	}
	backend := &prepareTestBackend{cells: cells}
	changes := &prepareTestChanges{store: store}
	env := demoenv.Env{DB: db, Store: store, Cfg: &config.Config{DemoMode: true}}
	if withClient {
		env.Client = workivaprovider.NewRouter(backend)
	}
	t.Setenv("NL_DEMO_SECTIONS_DIR", dir)
	svc, facts, err := New(env, changes)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return prepareTestRig{svc: svc, facts: facts, backend: backend, changes: changes, store: store}
}

func prepareTestCount(findings []Finding, field, kind string) int {
	n := 0
	for _, f := range findings {
		if f.Field == field && f.Kind == kind {
			n++
		}
	}
	return n
}

func TestScenario2PrepareReadsUncachedThroughRouter(t *testing.T) {
	rig := prepareTestSetup(t, map[string]any{"A1": json.Number("1240000"), "C1": json.Number("1350000")}, "energy.yaml")
	got, err := rig.svc.Prepare(context.Background(), "analyst", "prep-energy")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(got.Facts) != 1 {
		t.Fatalf("facts = %d, want 1", len(got.Facts))
	}
	f := got.Facts[0]
	if f.Change != "-110000" || !strings.HasPrefix(f.ChangePct, "-8.148148") || f.Direction != "down" || f.Material {
		t.Errorf("fact = %+v, want change -110000, pct -8.148148..., down, not material", f)
	}
	if rig.backend.typedCalls != 2 || rig.backend.plainCalls != 0 {
		t.Errorf("typed reads = %d, plain reads = %d, want 2 and 0", rig.backend.typedCalls, rig.backend.plainCalls)
	}
	cached, err := rig.store.GetCachedCells(context.Background(), "prep-src", "prep-src-1", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(cached) != 0 {
		t.Errorf("range cache holds %d cells, want 0", len(cached))
	}
}

func TestScenario5Findings(t *testing.T) {
	rig := prepareTestSetup(t, map[string]any{
		// A = source, C = prior, per mapping row number.
		"A2": json.Number("3"),                             // below min 10
		"A3": json.Number("0.46"),                          // 46%, min 50
		"A4": json.Number("125"), "C4": json.Number("100"), // +25%
		"A5": json.Number("-50"), "C5": json.Number("100"), // sign flip
		"A6": json.Number("102"), "C6": json.Number("100"), // +2%
		"A7": json.Number("100"),
		"A8": json.Number("0.6"), // 60%, inside min 50 / max 80 only when compared as a percent
	}, "findings.yaml")
	got, err := rig.svc.Prepare(context.Background(), "analyst", "prep-findings")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := map[string]map[string]int{
		"Reported headcount": {"missing": 1},
		"Water withdrawn":    {"out_of_range": 1},
		"Renewable share":    {"out_of_range": 1},
		"Waste generated":    {"material_change": 1},
		"Net balance":        {"material_change": 1, "sign_change": 1},
		"Recycled share":     {},
	}
	total := 0
	for label, kinds := range want {
		for _, kind := range []string{"missing", "out_of_range", "material_change", "sign_change"} {
			if n := prepareTestCount(got.Findings, label, kind); n != kinds[kind] {
				t.Errorf("%s %s findings = %d, want %d", label, kind, n, kinds[kind])
			}
			total += kinds[kind]
		}
	}
	if len(got.Findings) != total {
		t.Errorf("findings = %d, want %d: %+v", len(got.Findings), total, got.Findings)
	}
	wantSeverity := map[string]string{"missing": "high", "out_of_range": "high", "material_change": "medium", "sign_change": "medium"}
	for _, f := range got.Findings {
		if !strings.HasPrefix(f.Message, f.Field) || !strings.HasSuffix(f.Message, ".") {
			t.Errorf("finding %+v: message must start with the label and end with a period", f)
		}
		if f.Severity != wantSeverity[f.Kind] {
			t.Errorf("finding %+v: severity = %q, want %q", f, f.Severity, wantSeverity[f.Kind])
		}
		if f.Field == "Renewable share" && !strings.Contains(f.Message, "46%") {
			t.Errorf("Renewable share message %q must show the percent-scaled value 46%%", f.Message)
		}
	}
}

func TestScenario6StagesOneItemPerSourceValue(t *testing.T) {
	rig := prepareTestSetup(t, map[string]any{
		"A1": json.Number("585.0"), "A2": "Compliant", "A3": json.Number("7"),
		"A4": json.Number("0.5"), "A5": json.Number("12"), // A6 empty
	}, "stage.yaml")
	got, err := rig.svc.Prepare(context.Background(), "analyst", "prep-stage")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(rig.changes.calls) != 1 {
		t.Fatalf("Stage calls = %d, want 1", len(rig.changes.calls))
	}
	want := []changeset.Item{
		{FieldName: "p_tgt_1", Value: "585", Source: "p_src_1"},
		{FieldName: "p_tgt_2", Value: "Compliant", Source: "p_src_2"},
		{FieldName: "p_tgt_3", Value: "7", Source: "p_src_3"},
		{FieldName: "p_tgt_4", Value: "0.5", Source: "p_src_4"},
		{FieldName: "p_tgt_5", Value: "12", Source: "p_src_5"},
	}
	if !reflect.DeepEqual(rig.changes.calls[0], want) {
		t.Errorf("staged items = %+v, want %+v", rig.changes.calls[0], want)
	}
	if got.ChangeSet.ID == "" || len(got.ChangeSet.Items) != 5 {
		t.Errorf("ChangeSet = %+v, want the staged preview with 5 items", got.ChangeSet)
	}
	if len(got.Facts) != 4 {
		t.Errorf("facts = %d, want 4 (the text cell has none)", len(got.Facts))
	}
	if got.Findings == nil || got.Verbatim == nil {
		t.Error("Findings and Verbatim must be non-nil empty slices")
	}
}

func TestPrepareRejectsUnsupportedCellType(t *testing.T) {
	rig := prepareTestSetup(t, map[string]any{"A1": true}, "types.yaml")
	_, err := rig.svc.Prepare(context.Background(), "analyst", "prep-types")
	if err == nil || !strings.Contains(err.Error(), "flag") {
		t.Fatalf("err = %v, want an error naming field \"flag\"", err)
	}
	if len(rig.changes.calls) != 0 {
		t.Error("nothing may be staged when a cell is unsupported")
	}
}

func TestScenario7VerbatimByteForByteAndLatestFacts(t *testing.T) {
	dir := t.TempDir()
	prepareTestCopyRecipes(t, dir, "verbatim.yaml")
	text := "Première ligne\r\nSecond — 18 400 m³\r\n\r\nFin\n"
	if err := os.MkdirAll(filepath.Join(dir, "verbatim"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "verbatim", "policy.txt"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	rig := prepareTestBuild(t, dir, map[string]any{"A1": json.Number("120"), "C1": json.Number("100")}, true)
	ctx := context.Background()
	got, err := rig.svc.Prepare(ctx, "analyst-a", "prep-verbatim")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(got.Verbatim) != 1 || got.Verbatim[0].Text != text || got.Verbatim[0].ID != "policy" || got.Verbatim[0].Title != "Policy" {
		t.Fatalf("Verbatim = %+v, want the file bytes exactly", got.Verbatim)
	}
	facts, blocks, err := rig.facts.LatestFacts(ctx, "analyst-a", "prep-verbatim")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(facts, got.Facts) || !reflect.DeepEqual(blocks, got.Verbatim) {
		t.Errorf("LatestFacts = %+v / %+v, want the prepared facts and blocks", facts, blocks)
	}
	otherFacts, otherBlocks, err := rig.facts.LatestFacts(ctx, "analyst-b", "prep-verbatim")
	if err != nil {
		t.Fatal(err)
	}
	if len(otherFacts) != 0 || len(otherBlocks) != 0 {
		t.Errorf("another actor sees %d facts and %d blocks, want none", len(otherFacts), len(otherBlocks))
	}
}

func TestPrepareUnknownSectionListsAvailable(t *testing.T) {
	rig := prepareTestSetup(t, nil, "stage.yaml", "energy.yaml")
	_, err := rig.svc.Prepare(context.Background(), "analyst", "nope")
	want := `unknown section ID "nope"; available sections: prep-energy (Energy section), prep-stage (Stage section)`
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	empty := prepareTestSetup(t, nil)
	_, err = empty.svc.Prepare(context.Background(), "analyst", "nope")
	if err == nil || err.Error() != `unknown section ID "nope"; no section recipes are loaded` {
		t.Fatalf("empty err = %v", err)
	}
}

func TestPrepareNeedsClientAndChangeSetService(t *testing.T) {
	dir := t.TempDir()
	prepareTestCopyRecipes(t, dir, "energy.yaml")
	rig := prepareTestBuild(t, dir, nil, false)
	_, err := rig.svc.Prepare(context.Background(), "analyst", "prep-energy")
	if err == nil || !errors.Is(err, errClientUnavailable) {
		t.Fatalf("err = %v, want client unavailable", err)
	}

	cells := map[string]any{"A1": json.Number("1"), "C1": json.Number("1")}
	withClient := prepareTestBuild(t, dir, cells, true)
	svc, _, err := New(demoenv.Env{Store: withClient.store, Client: workivaprovider.NewRouter(withClient.backend)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Prepare(context.Background(), "analyst", "prep-energy")
	if err == nil || err.Error() != "change set service is not available" {
		t.Fatalf("err = %v, want change set unavailable", err)
	}
}

func TestPrepareStageFailureStoresNothing(t *testing.T) {
	rig := prepareTestSetup(t, map[string]any{"A1": json.Number("5"), "C1": json.Number("4")}, "energy.yaml")
	rig.changes.stageErr = errors.New("stage exploded")
	_, err := rig.svc.Prepare(context.Background(), "analyst", "prep-energy")
	if err == nil || !strings.Contains(err.Error(), "stage exploded") {
		t.Fatalf("err = %v, want the wrapped stage error", err)
	}
	facts, blocks, err := rig.facts.LatestFacts(context.Background(), "analyst", "prep-energy")
	if err != nil || len(facts) != 0 || len(blocks) != 0 {
		t.Errorf("stored %d facts and %d blocks (err %v), want nothing", len(facts), len(blocks), err)
	}
}

func TestPrepareNoSourceValuesIsAnError(t *testing.T) {
	rig := prepareTestSetup(t, nil, "stage.yaml")
	_, err := rig.svc.Prepare(context.Background(), "analyst", "prep-stage")
	if err == nil || err.Error() != `section "prep-stage" has no source values to stage` {
		t.Fatalf("err = %v", err)
	}
}
