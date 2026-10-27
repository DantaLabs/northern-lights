package recipe

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCompareMatchesPrepareFactsWithoutStaging(t *testing.T) {
	cells := map[string]any{"A1": json.Number("1240000"), "C1": json.Number("1350000")}
	rig := prepareTestSetup(t, cells, "energy.yaml")
	comparer := rig.svc.(Comparer)

	got, err := comparer.Compare(context.Background(), "prep-energy", nil)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(rig.changes.calls) != 0 {
		t.Fatalf("Compare staged %d change sets, want 0", len(rig.changes.calls))
	}
	if facts, _, err := rig.facts.LatestFacts(context.Background(), "analyst", "prep-energy"); err != nil || len(facts) != 0 {
		t.Fatalf("Compare stored facts = %v (err %v), want none", facts, err)
	}
	if got.SectionTitle != "Energy section" || got.Period != "2025" || got.PriorPeriod != "2024" || got.MaterialityPct != "10" {
		t.Errorf("comparison header = %+v", got)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(got.Items))
	}
	item := got.Items[0]
	if item.CurrentCell != (Cell{SpreadsheetID: "prep-src", SheetID: "prep-src-1", Range: "A1"}) ||
		item.PriorCell == nil || item.PriorCell.Range != "C1" {
		t.Errorf("cells = %+v / %+v, want A1 and C1", item.CurrentCell, item.PriorCell)
	}

	prepared, err := rig.svc.Prepare(context.Background(), "analyst", "prep-energy")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := prepared.Facts[0]
	if item.ChangePct != want.ChangePct || item.Material != want.Material || item.Direction != want.Direction ||
		strings.Join(item.Display, "|") != strings.Join(want.Display, "|") {
		t.Errorf("compare fact = %+v, prepare fact = %+v; they must match", item.Fact, want)
	}
}

func TestCompareSelectsByKeyOrLabelAndFlagsMissing(t *testing.T) {
	rig := prepareTestSetup(t, map[string]any{"A4": json.Number("125"), "C4": json.Number("100")}, "findings.yaml")
	comparer := rig.svc.(Comparer)

	got, err := comparer.Compare(context.Background(), "prep-findings", []string{"WASTE", "headcount"})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	labels := map[string]ComparedField{}
	for _, it := range got.Items {
		labels[it.Label] = it
	}
	if waste, ok := labels["Waste generated"]; !ok || !waste.Material || waste.Direction != "up" {
		t.Errorf("waste item = %+v, want material rise", waste)
	}
	if hc, ok := labels["Reported headcount"]; !ok || !hc.Missing {
		t.Errorf("headcount item = %+v, want missing", hc)
	}
	if len(got.Items) != 2 {
		t.Errorf("items = %d, want 2", len(got.Items))
	}

	_, err = comparer.Compare(context.Background(), "prep-findings", []string{"no such field"})
	if err == nil || !strings.Contains(err.Error(), "field keys:") {
		t.Errorf("unmatched field error = %v, want a list of field keys", err)
	}
	if _, err := comparer.Compare(context.Background(), "nope", nil); err == nil || !strings.Contains(err.Error(), "available sections") {
		t.Errorf("unknown section error = %v", err)
	}
}
