package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

const (
	fxMappings = "testdata/mappings.yaml"
	fxSections = "testdata/sections"
	fxData     = "testdata"
)

func fixtures(t *testing.T) (Mappings, []Recipe, []Row, []Row) {
	t.Helper()
	m, err := LoadMappings(fxMappings)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := LoadRecipes(fxSections)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := LoadCSV(filepath.Join(fxData, "esg-2025.csv"))
	if err != nil {
		t.Fatal(err)
	}
	pri, err := LoadCSV(filepath.Join(fxData, "esg-2024.csv"))
	if err != nil {
		t.Fatal(err)
	}
	return m, rs, cur, pri
}

func TestBuildPlanWritesBothYears(t *testing.T) {
	m, rs, cur, pri := fixtures(t)
	p, err := BuildPlan(m, rs, cur, pri, false)
	if err != nil {
		t.Fatal(err)
	}
	type w struct{ year, field, value string }
	want := []w{
		{"2025", "src_env_alpha", "1200.5"},
		{"2025", "src_soc_share", "0.465"},
		{"2024", "rpt_env_alpha_2024", ""},
		{"2024", "rpt_soc_share_2024", "0.4"},
	}
	if len(p.Writes) != len(want) {
		t.Fatalf("got %d writes, want %d: %+v", len(p.Writes), len(want), p.Writes)
	}
	for i, x := range want {
		g := p.Writes[i]
		if g.Year != x.year || g.Field != x.field || g.Value != x.value {
			t.Errorf("write %d = %s %s %q, want %s %s %q", i, g.Year, g.Field, g.Value, x.year, x.field, x.value)
		}
	}
	if g := p.Writes[0].Ref; g.SpreadsheetID != "sp-src-0001" || g.SheetID != "sh-src-0001" || g.Range != "B2" {
		t.Errorf("write 0 ref = %+v", g)
	}
	if len(p.Clears) != 0 || len(p.SectionDeletes) != 0 {
		t.Errorf("no reset: clears=%d deletes=%d", len(p.Clears), len(p.SectionDeletes))
	}
}

// README line percent-cells-store-fractions: yes selects division by 100.
func TestBuildPlanPercentConvention(t *testing.T) {
	m, rs, cur, pri := fixtures(t)
	p, err := BuildPlan(m, rs, cur, pri, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, wr := range p.Writes {
		switch wr.Field {
		case "src_soc_share":
			if wr.Value != "0.465" {
				t.Errorf("46.5 percent = %q, want 0.465", wr.Value)
			}
		case "src_env_alpha":
			if wr.Value != "1200.5" {
				t.Errorf("number field changed: %q", wr.Value)
			}
		}
	}
	for in, want := range map[string]string{"46.5": "0.465", "100": "1", "0": "0", "-5": "-0.05", "12.345": "0.12345", "7": "0.07"} {
		got, err := percentToFraction(in)
		if err != nil || got != want {
			t.Errorf("percentToFraction(%s) = %q, %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"abc", "", "1e", "4,5", "1/3"} {
		if got, err := percentToFraction(in); err == nil {
			t.Errorf("percentToFraction(%q) = %q, want an error", in, got)
		}
	}
}

func TestBuildPlanRejectsNonNumericPercent(t *testing.T) {
	m, rs, cur, pri := fixtures(t)
	for i := range cur {
		if cur[i].Key == "share" {
			cur[i].Value = "n/a"
		}
	}
	if p, err := BuildPlan(m, rs, cur, pri, false); err == nil || !strings.Contains(err.Error(), "src_soc_share") {
		t.Errorf("BuildPlan = %+v, %v; want an error naming src_soc_share", p, err)
	}
}

func TestBuildPlanResetAddsClearsAndSectionDeletes(t *testing.T) {
	m, rs, cur, pri := fixtures(t)
	p, err := BuildPlan(m, rs, cur, pri, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Clears) != 2 || p.Clears[0].Field != "rpt_env_alpha_2025" || p.Clears[1].Field != "rpt_soc_share_2025" {
		t.Fatalf("clears = %+v", p.Clears)
	}
	for _, c := range p.Clears {
		if c.Year != "2025" || c.Value != "" {
			t.Errorf("clear = %+v", c)
		}
	}
	if len(p.SectionDeletes) != 2 || p.SectionDeletes[0] != (SectionDelete{DocumentID: "doc-fixture-0001", Title: "Fixture environmental"}) ||
		p.SectionDeletes[1].Title != "Fixture social" {
		t.Fatalf("section deletes = %+v", p.SectionDeletes)
	}
	m.DocumentID = ""
	if _, err := BuildPlan(m, rs, cur, pri, true); err == nil {
		t.Error("reset without document_id must fail")
	}
}

func TestBuildPlanRejectsUnmappedField(t *testing.T) {
	m, rs, cur, pri := fixtures(t)
	for _, tc := range []struct{ name, field string }{{"source", "src_env_alpha"}, {"prior", "rpt_env_alpha_2024"}, {"target", "rpt_env_alpha_2025"}} {
		bad := append([]Recipe(nil), rs...)
		bad[0].Fields = append([]RecipeField(nil), rs[0].Fields...)
		switch tc.name {
		case "source":
			bad[0].Fields[0].Source = "src_env_missing"
		case "prior":
			bad[0].Fields[0].Prior = "rpt_env_missing_2024"
		case "target":
			bad[0].Fields[0].Target = "rpt_env_missing_2025"
		}
		p, err := BuildPlan(m, bad, cur, pri, true)
		if err == nil || !strings.Contains(err.Error(), "_missing") {
			t.Errorf("%s: err = %v, want error naming the field", tc.name, err)
		}
		if len(p.Writes)+len(p.Clears)+len(p.SectionDeletes) != 0 {
			t.Errorf("%s: plan must be empty on error", tc.name)
		}
	}
	if _, err := BuildPlan(m, rs, cur[1:], pri, false); err == nil || !strings.Contains(err.Error(), "alpha") {
		t.Errorf("missing csv row: err = %v", err)
	}
}

func TestPlanPrintHasNoIDs(t *testing.T) {
	m, rs, cur, pri := fixtures(t)
	p, err := BuildPlan(m, rs, cur, pri, true)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := p.Print(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, id := range []string{"sp-src-0001", "sh-src-0001", "sp-rpt-0001", "sh-cy-0001", "sh-py-0001", "doc-fixture-0001"} {
		if strings.Contains(out, id) {
			t.Errorf("output contains ID %s", id)
		}
	}
	for _, line := range []string{
		"write 2025 src_env_alpha -> Fixture Source Book / Source Sheet ! B2 = 1200.5",
		"write 2024 rpt_env_alpha_2024 -> Fixture Report Book / PY Output ! D2 = (empty)",
		"clear 2025 rpt_env_alpha_2025 -> Fixture Report Book / CY Output ! D2",
		`delete-section "Fixture environmental"`,
		"planned: 4 writes, 2 clears, 2 section deletes",
	} {
		if !strings.Contains(out, line+"\n") {
			t.Errorf("missing line %q in:\n%s", line, out)
		}
	}
}

func TestRunDryRunSendsNothing(t *testing.T) {
	old := newWorkiva
	newWorkiva = func(context.Context) (Workiva, error) {
		t.Error("newWorkiva called in dry run")
		return nil, errRealClientInTest
	}
	t.Cleanup(func() { newWorkiva = old })
	var out, errb bytes.Buffer
	args := []string{"-dry-run", "-mappings", demoDir + "/mappings-sandbox.example.yaml", "-data", dataDir, "-sections", sectionsDir}
	if code := run(context.Background(), args, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	fields := 0
	for _, r := range mustRecipes(t) {
		fields += len(r.Fields)
	}
	got := 0
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "write ") {
			got++
		}
	}
	if got != 2*fields {
		t.Errorf("write lines = %d, want %d", got, 2*fields)
	}
	if strings.Contains(out.String(), "<id-") || strings.Contains(out.String(), "<document-id>") {
		t.Error("output contains ID placeholder")
	}
}

func TestRunErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"-nope"}, &out, &errb); code != 2 {
		t.Errorf("bad flag exit %d", code)
	}
	if code := run(context.Background(), []string{"-dry-run", "-mappings", "testdata/absent.yaml"}, &out, &errb); code != 2 {
		t.Errorf("load error exit %d", code)
	}
	args := []string{"-mappings", fxMappings, "-data", fxData, "-sections", fxSections}
	fakeSeam(t, nil, errors.New("no credentials"))
	errb.Reset()
	if code := run(context.Background(), args, &out, &errb); code != 1 || !strings.Contains(errb.String(), "no credentials") {
		t.Errorf("client error exit %d stderr %q", code, errb.String())
	}
	fakeSeam(t, &fakeWorkiva{failWrite: errors.New("boom")}, nil)
	errb.Reset()
	if code := run(context.Background(), args, &out, &errb); code != 1 || !strings.Contains(errb.String(), "boom") {
		t.Errorf("apply error exit %d stderr %q", code, errb.String())
	}
	fakeSeam(t, &fakeWorkiva{}, nil)
	if code := run(context.Background(), args, &out, &errb); code != 0 {
		t.Errorf("fake run exit %d stderr %q", code, errb.String())
	}
}
