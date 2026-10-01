package assurance

import "testing"

func TestMaterialityCountsMissingRows(t *testing.T) {
	items, count := buildComparisonItems(SnapshotAnalysis{Membership: []FieldDefinition{{FieldID: "f"}}}, SnapshotAnalysis{}, MaterialityPolicy{MissingBehavior: "material"}, []string{"f"}, true)
	if len(items) != 1 || count != 1 || items[0].MaterialityStatus != "material" {
		t.Fatalf("items=%#v count=%d", items, count)
	}
}
