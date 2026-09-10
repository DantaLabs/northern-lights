package tools

import (
	"testing"

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
