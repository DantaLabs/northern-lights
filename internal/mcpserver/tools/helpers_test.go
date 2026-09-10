package tools

import (
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

func TestCellTextSupportsOfficialScalarValues(t *testing.T) {
	tests := []struct {
		name string
		cell workiva.Cell
		want string
	}{
		{"string", workiva.Cell{Value: "label"}, "label"},
		{"number", workiva.Cell{Value: float64(125000)}, "125000"},
		{"boolean", workiva.Cell{Value: true}, "true"},
		{"null", workiva.Cell{Value: nil}, ""},
		{"formula prefers calculated value", workiva.Cell{Value: "=1+1", CalculatedValue: float64(2)}, "2"},
		{"formula without calculated value", workiva.Cell{Value: "=1+1"}, "=1+1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := cellText(tc.cell); got != tc.want {
				t.Errorf("cellText(%#v) = %q, want %q", tc.cell, got, tc.want)
			}
		})
	}
}

func TestValueFromCellsRequiresEveryBoundedCellAndUsesRowMajorOrder(t *testing.T) {
	now := time.Now().UTC()
	cells := []mapping.CellValue{
		{Cell: "B2", Value: "b2", FetchedAt: now},
		{Cell: "A2", Value: "2", FetchedAt: now},
		{Cell: "B1", Value: "b1", FetchedAt: now},
		{Cell: "A1", Value: "1", FetchedAt: now},
		{Cell: "A3", Value: "3", FetchedAt: now},
		{Cell: "B3", Value: "b3", FetchedAt: now},
		{Cell: "A4", Value: "4", FetchedAt: now},
		{Cell: "B4", Value: "b4", FetchedAt: now},
		{Cell: "A5", Value: "5", FetchedAt: now},
		{Cell: "A6", Value: "6", FetchedAt: now},
		{Cell: "A7", Value: "7", FetchedAt: now},
		{Cell: "A8", Value: "8", FetchedAt: now},
		{Cell: "A9", Value: "9", FetchedAt: now},
	}

	got, ok := valueFromCells(cells, "A1:A10")
	if ok {
		t.Fatalf("partial A1:A10 cache returned %q, want cache miss", got)
	}

	cells = append(cells, mapping.CellValue{Cell: "A10", Value: "10", FetchedAt: now})
	got, ok = valueFromCells(cells, "A1:A10")
	if !ok {
		t.Fatal("complete A1:A10 cache returned miss")
	}
	if got != "1, 2, 3, 4, 5, 6, 7, 8, 9, 10" {
		t.Errorf("A1:A10 value = %q, want row-major order", got)
	}

	got, ok = valueFromCells(cells, "A1:B4")
	if !ok {
		t.Fatal("complete A1:B4 cache returned miss")
	}
	if got != "1, b1, 2, b2, 3, b3, 4, b4" {
		t.Errorf("A1:B4 value = %q, want row-major order", got)
	}
}

func TestValueFromCellsDoesNotClaimUnboundedRangeComplete(t *testing.T) {
	got, ok := valueFromCells([]mapping.CellValue{{Cell: "A1", Value: "1"}}, "A:A")
	if ok || got != "" {
		t.Errorf("unbounded cache result = %q, %v, want empty miss", got, ok)
	}
}
