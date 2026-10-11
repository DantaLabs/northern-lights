package workiva

import (
	"encoding/json"
	"testing"
)

func TestLiteralEditCellsUpdateUsesExactWorkivaOptions(t *testing.T) {
	cell := CellEdit{Column: 1, Row: 3, Value: json.Number("12345678901234567890.123456789")}
	encoded, err := json.Marshal(NewLiteralEditCellsUpdate([]CellEdit{cell}))
	if err != nil {
		t.Fatalf("marshal literal edit: %v", err)
	}
	const want = `{"editCells":{"cells":[{"column":1,"row":3,"value":12345678901234567890.123456789}],"options":{"applyEnteredInScaling":false,"skipEditMergeChildren":false}}}`
	if string(encoded) != want {
		t.Fatalf("literal edit JSON = %s, want %s", encoded, want)
	}
}

func TestGenericEditCellsUpdateKeepsExistingOptionsOmitted(t *testing.T) {
	encoded, err := json.Marshal(NewEditCellsUpdate([]CellEdit{{Column: 0, Row: 0, Value: 1}}))
	if err != nil {
		t.Fatalf("marshal generic edit: %v", err)
	}
	const want = `{"editCells":{"cells":[{"column":0,"row":0,"value":1}]}}`
	if string(encoded) != want {
		t.Fatalf("generic edit JSON = %s, want %s", encoded, want)
	}
}
