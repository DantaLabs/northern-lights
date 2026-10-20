package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/workiva"
)

type sheetKey struct{ spreadsheet, sheet string }

type writeCall struct {
	key   sheetKey
	edits []workiva.CellEdit
}

// fakeWorkiva records every call. Read-backs come from readBack when the
// cell is listed there, otherwise from what was written.
type fakeWorkiva struct {
	writes    []writeCall
	deletes   []string
	sections  map[string][]Section
	readBack  map[string]any
	cells     map[string]any
	failWrite error
	failList  error
}

func cellKey(k sheetKey, row, col int) string {
	return fmt.Sprintf("%s/%s/%d/%d", k.spreadsheet, k.sheet, row, col)
}

func (f *fakeWorkiva) WriteCells(_ context.Context, sp, sh string, edits []workiva.CellEdit) error {
	if f.failWrite != nil {
		return f.failWrite
	}
	k := sheetKey{sp, sh}
	f.writes = append(f.writes, writeCall{k, edits})
	if f.cells == nil {
		f.cells = map[string]any{}
	}
	for _, e := range edits {
		f.cells[cellKey(k, e.Row, e.Column)] = e.Value
	}
	return nil
}

func (f *fakeWorkiva) GetSheetDataTyped(_ context.Context, sp, sh, cellRange string, _ []string) (*workiva.SheetData, error) {
	r, err := workiva.A1ToRange(cellRange)
	if err != nil {
		return nil, err
	}
	key := cellKey(sheetKey{sp, sh}, r.StartRow, r.StartCol)
	v, ok := f.readBack[key]
	if !ok {
		v = f.cells[key]
	}
	return &workiva.SheetData{Cells: [][]workiva.Cell{{{Value: v}}}}, nil
}

func (f *fakeWorkiva) ListSections(_ context.Context, doc string) ([]Section, error) {
	if f.failList != nil {
		return nil, f.failList
	}
	return f.sections[doc], nil
}

func (f *fakeWorkiva) DeleteSection(_ context.Context, doc, id string) error {
	f.deletes = append(f.deletes, doc+"/"+id)
	return nil
}

func ref(sp, sh, a1 string) FieldRef {
	return FieldRef{SpreadsheetID: sp, SheetID: sh, Range: a1, SpreadsheetName: "Fixture Book " + strings.ToUpper(sp), SheetName: "Fixture Sheet " + strings.ToUpper(sh)}
}

func testPlan() Plan {
	return Plan{
		Writes: []Write{
			{Year: "2025", Field: "src_a", Ref: ref("sp1", "sh1", "B2"), Value: "1200.5"},
			{Year: "2025", Field: "src_b", Ref: ref("sp1", "sh1", "B3"), Value: ""},
			{Year: "2024", Field: "rpt_a_2024", Ref: ref("sp2", "sh3", "D2"), Value: "0.4"},
		},
		Clears: []Write{
			{Year: "2025", Field: "rpt_a_2025", Ref: ref("sp2", "sh2", "D2"), Value: ""},
		},
	}
}

func applyOut(t *testing.T, p Plan, wk Workiva) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Apply(context.Background(), p, wk, &out)
	return out.String(), err
}

func TestApplyWritesOnePerSheetAndReadsBack(t *testing.T) {
	f := &fakeWorkiva{}
	out, err := applyOut(t, testPlan(), f)
	if err != nil {
		t.Fatalf("Apply: %v\n%s", err, out)
	}
	if len(f.writes) != 3 {
		t.Fatalf("WriteCells calls = %d, want 3 (one per sheet): %+v", len(f.writes), f.writes)
	}
	first := f.writes[0]
	if first.key != (sheetKey{"sp1", "sh1"}) || len(first.edits) != 2 {
		t.Fatalf("first call = %+v", first)
	}
	want := []workiva.CellEdit{{Row: 1, Column: 1, Value: "1200.5"}, {Row: 2, Column: 1, Value: ""}}
	for i, e := range want {
		if first.edits[i] != e {
			t.Errorf("edit %d = %+v, want %+v", i, first.edits[i], e)
		}
	}
	clear := f.writes[2]
	if clear.key != (sheetKey{"sp2", "sh2"}) || clear.edits[0].Value != "" {
		t.Errorf("clear call = %+v; the README clear-value is the empty string, not null", clear)
	}
	if b, _ := json.Marshal(clear.edits[0]); !strings.Contains(string(b), `"value":""`) {
		t.Errorf("clear edit JSON = %s", b)
	}
	for _, line := range []string{"read-back: 4 ok, 0 mismatch\n", "done: elapsed "} {
		if !strings.Contains(out, line) {
			t.Errorf("missing %q in:\n%s", line, out)
		}
	}
	if strings.Contains(out, "sp1") || strings.Contains(out, "sh1") {
		t.Errorf("output contains IDs:\n%s", out)
	}
}

func TestApplyReadBackAcceptsStringsAndNumbers(t *testing.T) {
	f := &fakeWorkiva{readBack: map[string]any{
		cellKey(sheetKey{"sp1", "sh1"}, 1, 1): json.Number("1200.50"),
		cellKey(sheetKey{"sp2", "sh3"}, 1, 3): "0.40",
		cellKey(sheetKey{"sp1", "sh1"}, 2, 1): nil,
	}}
	out, err := applyOut(t, testPlan(), f)
	if err != nil {
		t.Fatalf("Apply: %v\n%s", err, out)
	}
	if !strings.Contains(out, "read-back: 4 ok, 0 mismatch\n") {
		t.Errorf("output:\n%s", out)
	}
	p := Plan{Writes: []Write{{Year: "2025", Field: "x", Ref: ref("sp1", "sh1", "B2"), Value: "46.5"}}}
	for _, v := range []any{json.Number("46.50"), "46.5", " 46.5 "} {
		f := &fakeWorkiva{readBack: map[string]any{cellKey(sheetKey{"sp1", "sh1"}, 1, 1): v}}
		if out, err := applyOut(t, p, f); err != nil {
			t.Errorf("value %#v: %v\n%s", v, err, out)
		}
	}
}

func TestApplyReportsMismatch(t *testing.T) {
	f := &fakeWorkiva{readBack: map[string]any{
		cellKey(sheetKey{"sp1", "sh1"}, 1, 1): json.Number("1200.6"),
		cellKey(sheetKey{"sp1", "sh1"}, 2, 1): json.Number("7"),
	}}
	out, err := applyOut(t, testPlan(), f)
	if err == nil {
		t.Fatalf("Apply returned nil with mismatches:\n%s", out)
	}
	for _, line := range []string{
		"read-back: 2 ok, 2 mismatch\n",
		"mismatch src_a: wrote 1200.5 read 1200.6\n",
		"mismatch src_b: wrote (empty) read 7\n",
		"done: elapsed ",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("missing %q in:\n%s", line, out)
		}
	}
	if len(f.writes) != 3 {
		t.Errorf("mismatch stopped the writes: %d calls", len(f.writes))
	}
}

func TestApplyDeletesOnlyMatchingSections(t *testing.T) {
	f := &fakeWorkiva{sections: map[string][]Section{"doc1": {
		{ID: "s1", Name: "Environmental disclosures"},
		{ID: "s2", Name: "2. Environmental information"},
		{ID: "s3", Name: "Social disclosures"},
	}}}
	p := Plan{SectionDeletes: []SectionDelete{{DocumentID: "doc1", Title: "Environmental disclosures"}}}
	out, err := applyOut(t, p, f)
	if err != nil {
		t.Fatalf("Apply: %v\n%s", err, out)
	}
	if len(f.deletes) != 1 || f.deletes[0] != "doc1/s1" {
		t.Errorf("deletes = %v, want [doc1/s1]", f.deletes)
	}
	if len(f.writes) != 0 {
		t.Errorf("unplanned writes: %+v", f.writes)
	}
	if !strings.Contains(out, `deleted-section "Environmental disclosures" (1)`+"\n") {
		t.Errorf("output:\n%s", out)
	}
}

func TestApplyStopsOnError(t *testing.T) {
	boom := errors.New("boom")
	f := &fakeWorkiva{failWrite: boom}
	out, err := applyOut(t, testPlan(), f)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped boom", err)
	}
	if !strings.Contains(err.Error(), "src_a") || strings.Contains(err.Error(), "sp1") {
		t.Errorf("error should name the field and no ID: %v", err)
	}
	if strings.Contains(out, "read-back") || strings.Contains(out, "done:") {
		t.Errorf("Apply went on after an error:\n%s", out)
	}

	f = &fakeWorkiva{failList: boom}
	p := Plan{SectionDeletes: []SectionDelete{{DocumentID: "doc1", Title: "Social disclosures"}}}
	_, err = applyOut(t, p, f)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "Social disclosures") || strings.Contains(err.Error(), "doc1") {
		t.Errorf("list error = %v", err)
	}
}
