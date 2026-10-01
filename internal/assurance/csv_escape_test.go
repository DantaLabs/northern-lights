package assurance

import (
	"strings"
	"testing"
)

func TestCSVCellEscaping(t *testing.T) {
	data, _, err := RenderCSV(map[string]any{"formula": "=1+1", "plus": "+2", "minus": "-3", "at": "@x"}, RedactionStandard)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"'=1+1", "'+2", "'-3", "'@x"} {
		if !strings.Contains(string(data), value) {
			t.Fatalf("missing escaped %q in %s", value, data)
		}
	}
}
