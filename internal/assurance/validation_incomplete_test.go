package assurance

import "testing"

func TestValidationIncompleteNeverPasses(t *testing.T) {
	results := evaluateRules(SnapshotAnalysis{Response: SnapshotResponse{Completeness: CompletenessIncomplete}}, []RuleDefinition{{RuleID: "x", Revision: 1, Kind: RuleRequired, FieldIDs: []string{"f"}}})
	if len(results) != 1 || results[0].Status != "not_evaluable" {
		t.Fatalf("results=%#v", results)
	}
}
