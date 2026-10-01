package assurance

import "testing"

func TestRuleSetMustMatchHistoricalReportApproval(t *testing.T) {
	analysis := SnapshotAnalysis{ReportID: "report", Revision: 4, ApprovedRuleSetID: "rules-approved"}
	if ruleSetApprovedForSnapshot(analysis, RuleSet{RuleSetID: "rules-other"}) {
		t.Fatal("a different active rule set was accepted for the historical report revision")
	}
	if !ruleSetApprovedForSnapshot(analysis, RuleSet{RuleSetID: "rules-approved"}) {
		t.Fatal("the historically approved rule set was rejected")
	}
}

func TestDisjointReportsHaveNotEvaluableCompleteness(t *testing.T) {
	if got := incompatibleComparisonCompleteness(SnapshotAnalysis{ReportID: "current"}, SnapshotAnalysis{ReportID: "prior"}); got != CompletenessNotEvaluable {
		t.Fatalf("completeness=%q, want %q", got, CompletenessNotEvaluable)
	}
}

func TestDisjointReportsBypassPolicyApplicabilityGate(t *testing.T) {
	if !comparisonReportsDisjoint(SnapshotAnalysis{ReportID: "current"}, SnapshotAnalysis{ReportID: "prior"}) {
		t.Fatal("disjoint reports were treated as policy-compatible")
	}
	if comparisonReportsDisjoint(SnapshotAnalysis{ReportID: "same"}, SnapshotAnalysis{ReportID: "same"}) {
		t.Fatal("same report revisions were treated as disjoint")
	}
}

func TestRelativeZeroBaselineIsDimensionlessAndAbsoluteOnlyIgnoresRelative(t *testing.T) {
	relativeZero := MaterialityPolicy{Direction: "relative", RelativeThreshold: "0.01", ZeroBaseline: "relative_zero"}
	if got := materialityStatus(relativeZero, "1", "1", "0"); got != "material" {
		t.Fatalf("relative_zero status=%q, want material for non-zero change", got)
	}
	absoluteOnly := MaterialityPolicy{Direction: "absolute", AbsoluteThreshold: "10", RelativeThreshold: "0.01", ZeroBaseline: "absolute_only"}
	if got := materialityStatus(absoluteOnly, "1", "1", "100"); got != "immaterial" {
		t.Fatalf("absolute-only status=%q, want immaterial", got)
	}
}
