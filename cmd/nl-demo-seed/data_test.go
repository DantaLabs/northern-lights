package main

import (
	"context"
	"encoding/json"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/bootstrap"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

const (
	demoDir     = "../../configs/demo"
	dataDir     = demoDir + "/data"
	sectionsDir = demoDir + "/sections"
)

func rat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("not a decimal: %q", s)
	}
	return r
}

func mustCSV(t *testing.T, name string) []Row {
	t.Helper()
	rows, err := LoadCSV(filepath.Join(dataDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func mustRecipes(t *testing.T) map[string]Recipe {
	t.Helper()
	rs, err := LoadRecipes(sectionsDir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Recipe{}
	for _, r := range rs {
		out[r.ID] = r
	}
	return out
}

func byKey(rows []Row) map[string]Row {
	m := map[string]Row{}
	for _, r := range rows {
		m[r.Key] = r
	}
	return m
}

type finding struct{ kind, section, key string }

func TestDataConsistency(t *testing.T) {
	cur, prior := mustCSV(t, "esg-2025.csv"), mustCSV(t, "esg-2024.csv")
	curBy, priorBy := byKey(cur), byKey(prior)
	recipes := mustRecipes(t)

	// Totals equal the sum of their parts, both years.
	raw, err := os.ReadFile(filepath.Join(dataDir, "totals.csv"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if lines[0] != "total_key,part_keys" || len(lines) < 2 {
		t.Fatalf("totals.csv: bad header or no rows: %q", lines[0])
	}
	for _, line := range lines[1:] {
		total, parts, ok := strings.Cut(line, ",")
		if !ok {
			t.Fatalf("totals.csv: bad line %q", line)
		}
		for year, by := range map[string]map[string]Row{"2025": curBy, "2024": priorBy} {
			sum := new(big.Rat)
			for _, p := range strings.Split(parts, "+") {
				row, ok := by[p]
				if !ok || row.Value == "" {
					t.Errorf("%s %s: part %s missing", year, total, p)
					continue
				}
				sum.Add(sum, rat(t, row.Value))
			}
			if tr, ok := by[total]; !ok || tr.Value == "" {
				t.Errorf("%s: total %s missing", year, total)
			} else if rat(t, tr.Value).Cmp(sum) != 0 {
				t.Errorf("%s: total %s = %s, parts sum to %s", year, total, tr.Value, sum.RatString())
			}
		}
	}

	// Percent fields within 0..100.
	for year, rows := range map[string][]Row{"2025": cur, "2024": prior} {
		for _, r := range rows {
			if r.Unit == "%" && r.Value != "" {
				if v := rat(t, r.Value); v.Sign() < 0 || v.Cmp(big.NewRat(100, 1)) > 0 {
					t.Errorf("%s: %s = %s outside 0..100", year, r.Key, r.Value)
				}
			}
		}
	}

	// Findings, computed the way the section recipe schema defines them.
	var findings []finding
	planted := map[string]bool{}
	for _, rc := range []string{"esg-environmental", "esg-social"} {
		recipe, ok := recipes[rc]
		if !ok {
			t.Fatalf("recipe %s not found", rc)
		}
		section := strings.TrimPrefix(rc, "esg-")[:3]
		for _, f := range recipe.Fields {
			c, p := curBy[f.Key], priorBy[f.Key]
			var cv, pv *big.Rat
			if c.Value != "" {
				cv = rat(t, c.Value)
			}
			if p.Value != "" {
				pv = rat(t, p.Value)
			}
			add := func(kind string) {
				findings = append(findings, finding{kind, section, f.Key})
				planted[f.Key] = true
			}
			if cv == nil {
				if f.Required {
					add("missing")
				}
				continue
			}
			if f.Min != nil && cv.Cmp(rat(t, *f.Min)) < 0 || f.Max != nil && cv.Cmp(rat(t, *f.Max)) > 0 {
				add("out_of_range")
			}
			if pv == nil {
				continue
			}
			if pv.Sign()*cv.Sign() < 0 {
				add("sign_change")
				continue
			}
			if pv.Sign() == 0 {
				continue // direction "new": never a finding
			}
			change := new(big.Rat).Sub(cv, pv)
			if c.Unit != "%" {
				change.Quo(change, pv)
				change.Mul(change, big.NewRat(100, 1))
			}
			abs := new(big.Rat).Abs(change)
			if abs.Cmp(big.NewRat(30, 1)) > 0 {
				t.Errorf("%s changes by %s, beyond 30", f.Key, abs.FloatString(2))
			}
			if abs.Cmp(big.NewRat(int64(recipe.MaterialityPct), 1)) >= 0 {
				add("material_change")
			}
		}
	}
	want := []finding{
		{"missing", "env", "water_discharge_production"},
		{"material_change", "env", "ghg_scope3_business_travel"},
		{"material_change", "soc", "training_hours_female"},
		{"out_of_range", "env", "electricity_renewable"},
	}
	if len(findings) != len(want) {
		t.Fatalf("findings = %v, want %v", findings, want)
	}
	have := map[finding]bool{}
	for _, f := range findings {
		have[f] = true
	}
	readme, err := os.ReadFile(filepath.Join(dataDir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("missing expected finding %v; got %v", w, findings)
		}
		if !strings.Contains(string(readme), w.key) {
			t.Errorf("data README does not mention planted field %s", w.key)
		}
	}
	if len(planted) != 4 {
		t.Errorf("findings fall on %d fields, want 4 different fields", len(planted))
	}

	// Every non-planted change is below 10.
	for _, f := range cur {
		p := priorBy[f.Key]
		if planted[f.Key] || f.Value == "" || p.Value == "" || rat(t, p.Value).Sign() == 0 {
			continue
		}
		change := new(big.Rat).Sub(rat(t, f.Value), rat(t, p.Value))
		if f.Unit != "%" {
			change.Quo(change, rat(t, p.Value))
			change.Mul(change, big.NewRat(100, 1))
		}
		if new(big.Rat).Abs(change).Cmp(big.NewRat(10, 1)) >= 0 {
			t.Errorf("%s changes by %s, not below 10", f.Key, change.FloatString(2))
		}
	}
}

func TestRecipesMatchSchemaAndMappings(t *testing.T) {
	recipes := mustRecipes(t)
	if len(recipes) != 2 {
		t.Fatalf("got %d recipes, want 2", len(recipes))
	}
	cur, prior := mustCSV(t, "esg-2025.csv"), mustCSV(t, "esg-2024.csv")
	if len(cur) != len(prior) {
		t.Fatalf("CSV row counts differ: %d vs %d", len(cur), len(prior))
	}
	for i := range cur {
		if cur[i].Key != prior[i].Key || cur[i].Label != prior[i].Label || cur[i].Unit != prior[i].Unit {
			t.Errorf("row %d differs between years: %+v vs %+v", i+1, cur[i], prior[i])
		}
	}
	demo, err := LoadMappings(filepath.Join(demoDir, "mappings-demo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	example, err := LoadMappings(filepath.Join(demoDir, "mappings-sandbox.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	maps := map[string]Mappings{"mappings-demo.yaml": demo, "mappings-sandbox.example.yaml": example}

	// Row index of every key in the CSV (data row n sits on sheet row n+1).
	rowOf := map[string]int{}
	for i, r := range cur {
		rowOf[r.Key] = i + 2
	}

	var all []RecipeField
	sections := []struct {
		id, short string
		min       int
	}{{"esg-environmental", "env", 20}, {"esg-social", "soc", 12}}
	for _, sec := range sections {
		rc, ok := recipes[sec.id]
		if !ok {
			t.Fatalf("recipe %s missing", sec.id)
		}
		if rc.MaterialityPct != 10 {
			t.Errorf("%s materiality_pct = %d, want 10", sec.id, rc.MaterialityPct)
		}
		if len(rc.Fields) < sec.min {
			t.Errorf("%s has %d fields, want at least %d", sec.id, len(rc.Fields), sec.min)
		}
		all = append(all, rc.Fields...)
		for _, f := range rc.Fields {
			k := f.Key
			if f.Source != "src_"+sec.short+"_"+k || f.Target != "rpt_"+sec.short+"_"+k+"_2025" || f.Prior != "rpt_"+sec.short+"_"+k+"_2024" {
				t.Errorf("%s/%s: mapped names %q %q %q do not follow the convention", sec.id, k, f.Source, f.Target, f.Prior)
			}
			for file, m := range maps {
				for _, role := range []struct{ name, sheet, spreadsheet string }{
					{f.Source, "demo-esg-source-2025", "demo-esg-source"},
					{f.Target, "demo-esg-report-cy", "demo-esg-report"},
					{f.Prior, "demo-esg-report-py", "demo-esg-report"},
				} {
					name := role.name
					ref, ok := m.Field(name)
					if !ok {
						t.Errorf("%s: %s not mapped", file, name)
						continue
					}
					// R2: every description copies the CSV label.
					if d := descriptionOf(m, name); d != f.Label {
						t.Errorf("%s: %s description %q, want label %q", file, name, d, f.Label)
					}
					if file == "mappings-demo.yaml" && ref.Range != "D"+strconv.Itoa(rowOf[k]) {
						t.Errorf("%s: %s range %s, want D%d", file, name, ref.Range, rowOf[k])
					}
					if file == "mappings-demo.yaml" && (ref.SpreadsheetID != role.spreadsheet || ref.SheetID != role.sheet) {
						t.Errorf("%s: %s on %s/%s, want %s/%s", file, name, ref.SpreadsheetID, ref.SheetID, role.spreadsheet, role.sheet)
					}
				}
			}
		}
		// Verbatim block.
		if len(rc.Verbatim) != 1 {
			t.Errorf("%s has %d verbatim blocks, want 1", sec.id, len(rc.Verbatim))
			continue
		}
		txt, err := os.ReadFile(filepath.Join(sectionsDir, rc.Verbatim[0].File))
		if err != nil {
			t.Errorf("%s verbatim file: %v", sec.id, err)
		} else if s := strings.TrimSpace(string(txt)); s == "" || strings.HasPrefix(s, "#") {
			t.Errorf("%s verbatim file is empty or starts with #", sec.id)
		}
	}

	// Recipe fields equal the CSV rows, environmental first, in CSV order.
	if len(all) != len(cur) {
		t.Fatalf("recipes have %d fields, CSV has %d rows", len(all), len(cur))
	}
	for i, f := range all {
		if f.Key != cur[i].Key || f.Label != cur[i].Label || f.Unit != cur[i].Unit {
			t.Errorf("recipe field %d = %s/%q/%s, CSV row = %s/%q/%s", i+1, f.Key, f.Label, f.Unit, cur[i].Key, cur[i].Label, cur[i].Unit)
		}
	}
	for _, key := range []string{"energy_total", "employees_total"} {
		if _, ok := byKey(cur)[key]; !ok {
			t.Errorf("mandatory key %s missing", key)
		}
	}
}

func descriptionOf(m Mappings, name string) string {
	for _, sp := range m.Spreadsheets {
		for _, sh := range sp.Sheets {
			for _, f := range sh.Fields {
				if f.Name == name {
					return f.Description
				}
			}
		}
	}
	return ""
}

func TestDemoReadReturnsCSVValue(t *testing.T) {
	t.Setenv("NL_DEMO_ESG_DATA", dataDir)
	store, err := mapping.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := bootstrap.LoadMappings(ctx, filepath.Join(demoDir, "mappings-demo.yaml"), store); err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse("https://api.eu.wdesk.com")
	if err != nil {
		t.Fatal(err)
	}
	client := workiva.NewDemoClient(base)
	for _, tc := range []struct{ field, csv string }{
		{"src_env_energy_total", "esg-2025.csv"},
		{"rpt_env_energy_total_2024", "esg-2024.csv"},
	} {
		want := byKey(mustCSV(t, tc.csv))["energy_total"].Value
		f, err := store.GetField(ctx, tc.field)
		if err != nil || f == nil {
			t.Fatalf("GetField %s: %v", tc.field, err)
		}
		data, err := client.GetSheetDataTyped(ctx, f.SpreadsheetID, f.SheetID, f.CellRange, nil)
		if err != nil {
			t.Fatalf("read %s: %v", tc.field, err)
		}
		if data == nil || len(data.Cells) != 1 || len(data.Cells[0]) != 1 {
			t.Fatalf("%s: unexpected cells %+v", tc.field, data)
		}
		got, ok := DecimalText(data.Cells[0][0].Value)
		if !ok || got != want {
			t.Errorf("%s = %q (ok=%v), want %q", tc.field, got, ok, want)
		}
	}
}

func TestDecimalText(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want string
	}{
		{json.Number("1240000.5"), "1240000.5"},
		{"1240000.5", "1240000.5"},
		{" 46 ", "46"},
		{float64(1240000.5), "1240000.5"},
		{float64(0.1), "0.1"},
		{nil, ""},
		{"", ""},
	} {
		if got, ok := DecimalText(tc.in); !ok || got != tc.want {
			t.Errorf("DecimalText(%#v) = %q, %v; want %q, true", tc.in, got, ok, tc.want)
		}
	}
	if _, ok := DecimalText("abc"); ok {
		t.Error("DecimalText(abc) ok, want not ok")
	}
}

func TestLoadCSVRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct{ body, wantErr string }{
		"header.csv": {"k,l,u,v\n", "header.csv:1"},
		"value.csv":  {"key,label,unit,value\na,A,u,1e3\n", "value.csv:2"},
		"dup.csv":    {"key,label,unit,value\na,A,u,1\na,B,u,2\n", "dup.csv:3"},
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadCSV(p)
		if err == nil || !regexp.MustCompile(regexp.QuoteMeta(tc.wantErr)).MatchString(err.Error()) {
			t.Errorf("%s: err = %v, want mention of %s", name, err, tc.wantErr)
		}
	}
}
