package assurance

import "testing"

func TestValidationBlankRequiredAndWarnSeverity(t *testing.T) {
	status, _, _ := evaluateRule(RuleDefinition{RuleID: "x", Revision: 1, Kind: RuleRequired, FieldIDs: []string{"f"}, FailureSeverity: "warn"}, map[string]SnapshotObservation{"f": {TypedValue: TypedValue{Kind: ValueText, Text: " "}}})
	if status != "warn" {
		t.Fatalf("blank required status=%q", status)
	}
}
