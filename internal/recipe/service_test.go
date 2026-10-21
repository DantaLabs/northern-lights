package recipe

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/bootstrap"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/demoenv"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

func loadTestStore(t *testing.T) *mapping.Store {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return loadTestStoreFromDB(t, db)
}

func loadTestStoreFromDB(t *testing.T, db *sql.DB) *mapping.Store {
	t.Helper()
	store, err := mapping.NewWithDB(db)
	if err != nil {
		t.Fatalf("mapping store: %v", err)
	}
	if err := bootstrap.LoadMappings(context.Background(), filepath.Join("testdata", "load", "mappings.yaml"), store); err != nil {
		t.Fatalf("load mappings: %v", err)
	}
	return store
}

// loadTestDir copies the named testdata/load files (and the verbatim directory)
// into a fresh temp directory and returns it.
func loadTestDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	loadTestCopy(t, filepath.Join("testdata", "load", "verbatim", "policy.txt"), filepath.Join(dir, "verbatim", "policy.txt"))
	for _, n := range names {
		loadTestCopy(t, filepath.Join("testdata", "load", n), filepath.Join(dir, n))
	}
	return dir
}

func loadTestCopy(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	loadTestWrite(t, to, string(data))
}

func loadTestWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// loadTestVariant writes the valid fixture as name in dir, optionally replacing
// one substring.
func loadTestVariant(t *testing.T, dir, name, old, repl string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "load", "valid.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if old != "" {
		if !strings.Contains(s, old) {
			t.Fatalf("fixture has no %q", old)
		}
		s = strings.Replace(s, old, repl, 1)
	}
	loadTestWrite(t, filepath.Join(dir, name), s)
}

func loadTestNew(t *testing.T, dir string, env demoenv.Env) (Service, error) {
	t.Helper()
	t.Setenv("NL_DEMO_SECTIONS_DIR", dir)
	svc, _, err := New(env, nil)
	return svc, err
}

func TestScenario1InvalidRecipeStopsStartup(t *testing.T) {
	store := loadTestStore(t)
	cases := []struct {
		name, old, repl, want string
	}{
		{"unknown_key", "title: Valid section\n", "title: Valid section\nbogus_top: 1\n", "bogus_top"},
		{"unknown_field_key", "    unit: MWh\n", "    unit: MWh\n    displayunit: GWh\n", "displayunit"},
		{"unknown_verbatim_key", "{ id: policy,", "{ id: policy, colour: red,", "colour"},
		{"missing_id", "id: load-valid\n", "", "id is required"},
		{"unknown_mapped_field", "source: load_src_energy", "source: load_src_missing", "load_src_missing"},
		{"empty_verbatim_file", "verbatim/policy.txt", "verbatim/empty.txt", "empty.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := loadTestDir(t)
			loadTestWrite(t, filepath.Join(dir, "verbatim", "empty.txt"), "")
			loadTestVariant(t, dir, "bad.yaml", tc.old, tc.repl)
			_, err := loadTestNew(t, dir, demoenv.Env{Store: store})
			if err == nil {
				t.Fatal("New: want error")
			}
			if !strings.Contains(err.Error(), "bad.yaml") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q should name bad.yaml and %q", err, tc.want)
			}
		})
	}
}

func TestScenario1ValidRecipeLoads(t *testing.T) {
	dir := loadTestDir(t, "valid.yaml")
	svc, err := loadTestNew(t, dir, demoenv.Env{Store: loadTestStore(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := svc.Sections()
	want := SectionInfo{ID: "load-valid", Title: "Valid section", Period: "2025", PriorPeriod: "2024", DocumentID: "doc-yaml"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("Sections = %+v, want [%+v]", got, want)
	}
}

func TestLoadRecipeRejectsDuplicateID(t *testing.T) {
	store := loadTestStore(t)
	dir := loadTestDir(t, "valid.yaml")
	loadTestVariant(t, dir, "second.yaml", "", "")
	_, err := loadTestNew(t, dir, demoenv.Env{Store: store})
	if err == nil || !strings.Contains(err.Error(), "second.yaml") || !strings.Contains(err.Error(), "valid.yaml") {
		t.Fatalf("duplicate id error = %v, want both file names", err)
	}
}

func TestLoadRecipeVerbatimContainment(t *testing.T) {
	store := loadTestStore(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	loadTestWrite(t, outside, "secret")
	cases := []struct {
		name string
		file string
		prep func(t *testing.T, dir string)
		want string
	}{
		{"parent_element", "../secret.txt", func(t *testing.T, dir string) {
			loadTestWrite(t, filepath.Join(filepath.Dir(dir), "secret.txt"), "secret")
		}, "must be relative"},
		{"absolute", outside, nil, "must be relative"},
		{"symlink_file_outside", "verbatim/link.txt", func(t *testing.T, dir string) {
			if err := os.Symlink(outside, filepath.Join(dir, "verbatim", "link.txt")); err != nil {
				t.Skipf("symlink: %v", err)
			}
		}, "escapes"},
		{"symlink_dir_outside", "linkdir/secret.txt", func(t *testing.T, dir string) {
			if err := os.Symlink(filepath.Dir(outside), filepath.Join(dir, "linkdir")); err != nil {
				t.Skipf("symlink: %v", err)
			}
		}, "escapes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := loadTestDir(t)
			if tc.prep != nil {
				tc.prep(t, dir)
			}
			loadTestVariant(t, dir, "esc.yaml", "verbatim/policy.txt", tc.file)
			_, err := loadTestNew(t, dir, demoenv.Env{Store: store})
			if err == nil || !strings.Contains(err.Error(), "esc.yaml") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want esc.yaml and %q", err, tc.want)
			}
		})
	}
}

func TestLoadRecipeAllowsSymlinkInsideSectionsDir(t *testing.T) {
	dir := loadTestDir(t)
	if err := os.Symlink("policy.txt", filepath.Join(dir, "verbatim", "alias.txt")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	loadTestVariant(t, dir, "ok.yaml", "verbatim/policy.txt", "verbatim/alias.txt")
	if _, err := loadTestNew(t, dir, demoenv.Env{Store: loadTestStore(t)}); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestLoadRecipeRejectsSecondYAMLDocument(t *testing.T) {
	dir := loadTestDir(t, "valid.yaml")
	f, err := os.OpenFile(filepath.Join(dir, "valid.yaml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("---\nid: extra\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = loadTestNew(t, dir, demoenv.Env{Store: loadTestStore(t)})
	if err == nil || !strings.Contains(err.Error(), "valid.yaml") || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("error = %v", err)
	}
}

func TestNewWithRecipesRequiresStore(t *testing.T) {
	dir := loadTestDir(t, "valid.yaml")
	_, err := loadTestNew(t, dir, demoenv.Env{})
	if err == nil || !strings.Contains(err.Error(), "valid.yaml") || !strings.Contains(err.Error(), "mapping store is not available") {
		t.Fatalf("error = %v, want valid.yaml and mapping store is not available", err)
	}
}

func TestScenario8SectionsSortedByID(t *testing.T) {
	dir := loadTestDir(t)
	loadTestVariant(t, dir, "one.yaml", "id: load-valid", "id: b-second")
	loadTestVariant(t, dir, "two.yaml", "id: load-valid", "id: a-first")
	svc, err := loadTestNew(t, dir, demoenv.Env{Store: loadTestStore(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := svc.Sections()
	want := []SectionInfo{
		{ID: "a-first", Title: "Valid section", Period: "2025", PriorPeriod: "2024", DocumentID: "doc-yaml"},
		{ID: "b-second", Title: "Valid section", Period: "2025", PriorPeriod: "2024", DocumentID: "doc-yaml"},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Sections = %+v, want %+v", got, want)
	}
}

func TestDocumentIDOverrideFromEnvironment(t *testing.T) {
	store := loadTestStore(t)
	t.Run("nil_cfg_keeps_yaml", func(t *testing.T) {
		t.Setenv("NL_DEMO_DOCUMENT_ID", "doc-env")
		svc, err := loadTestNew(t, loadTestDir(t, "valid.yaml"), demoenv.Env{Store: store})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if got := svc.Sections()[0].DocumentID; got != "doc-yaml" {
			t.Fatalf("DocumentID = %q, want doc-yaml", got)
		}
	})
	cases := []struct {
		name    string
		demo    bool
		envVal  *string
		wantDoc string
	}{
		{"demo_mode_keeps_yaml", true, loadTestPtr("doc-env"), "doc-yaml"},
		{"sandbox_uses_env", false, loadTestPtr("doc-env"), "doc-env"},
		{"sandbox_trims_env", false, loadTestPtr("  doc-env \n"), "doc-env"},
		{"sandbox_unset_keeps_yaml", false, nil, "doc-yaml"},
		{"sandbox_blank_keeps_yaml", false, loadTestPtr("  "), "doc-yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := loadTestDir(t, "valid.yaml")
			if tc.envVal != nil {
				t.Setenv("NL_DEMO_DOCUMENT_ID", *tc.envVal)
			} else {
				t.Setenv("NL_DEMO_DOCUMENT_ID", "")
				if err := os.Unsetenv("NL_DEMO_DOCUMENT_ID"); err != nil {
					t.Fatal(err)
				}
			}
			svc, err := loadTestNew(t, dir, demoenv.Env{Store: store, Cfg: &config.Config{DemoMode: tc.demo}})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if got := svc.Sections()[0].DocumentID; got != tc.wantDoc {
				t.Fatalf("DocumentID = %q, want %q", got, tc.wantDoc)
			}
		})
	}
}

func loadTestPtr(s string) *string { return &s }

func TestNewSucceedsWithoutRecipes(t *testing.T) {
	for name, dir := range map[string]string{
		"empty":   t.TempDir(),
		"missing": filepath.Join(t.TempDir(), "absent"),
	} {
		t.Run(name, func(t *testing.T) {
			svc, err := loadTestNew(t, dir, demoenv.Env{})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if n := len(svc.Sections()); n != 0 {
				t.Fatalf("Sections len = %d, want 0", n)
			}
		})
	}
}

func TestNewRejectsSectionsPathThatIsAFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	loadTestWrite(t, f, "x")
	if _, err := loadTestNew(t, f, demoenv.Env{}); err == nil {
		t.Fatal("New: want error for non-directory")
	}
}
