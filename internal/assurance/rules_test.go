package assurance

import "testing"

func TestRulesKindSpecificOperands(t *testing.T) {
	if err := ValidateRuleSet(RuleSet{RuleSetID: "r", Revision: 1, Status: "active", Rules: []RuleDefinition{{RuleID: "x", Revision: 1, Kind: RuleRequired, FieldIDs: []string{"f"}, ExpectedKind: ValueText}}}); err == nil {
		t.Fatal("required rule accepted type operand")
	}
}
