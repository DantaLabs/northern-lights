package assurance

import "testing"

func TestRequiredRuleUsesExplicitBlankSemanticsForEveryValueKind(t *testing.T) {
	tests := []struct {
		name  string
		value TypedValue
		want  string
	}{
		{name: "blank", value: TypedValue{Kind: ValueBlank}, want: "fail"},
		{name: "text", value: TypedValue{Kind: ValueText, Text: "value"}, want: "pass"},
		{name: "integer", value: TypedValue{Kind: ValueInteger, Number: "0"}, want: "pass"},
		{name: "number", value: TypedValue{Kind: ValueNumber, Number: "0"}, want: "pass"},
		{name: "boolean false", value: TypedValue{Kind: ValueBoolean, Boolean: false}, want: "pass"},
		{name: "boolean true", value: TypedValue{Kind: ValueBoolean, Boolean: true}, want: "pass"},
		{name: "date", value: TypedValue{Kind: ValueDate, Date: "2026-01-01"}, want: "pass"},
		{name: "datetime", value: TypedValue{Kind: ValueDateTime, DateTime: "2026-01-01T00:00:00Z"}, want: "pass"},
		{name: "currency", value: TypedValue{Kind: ValueCurrency, Number: "0", Unit: "USD"}, want: "pass"},
		{name: "percent", value: TypedValue{Kind: ValuePercent, Number: "0", PercentBasis: "0..1"}, want: "pass"},
		{name: "error", value: TypedValue{Kind: ValueError, ErrorCode: "provider_error"}, want: "fail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, _ := evaluateRule(RuleDefinition{RuleID: "required", Kind: RuleRequired, FieldIDs: []string{"f"}}, map[string]SnapshotObservation{
				"f": {FieldID: "f", TypedValue: tt.value},
			})
			if got != tt.want {
				t.Fatalf("status=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestMissingRequiredRuleHonorsWarningSeverity(t *testing.T) {
	got, _, _ := evaluateRule(RuleDefinition{RuleID: "required", Kind: RuleRequired, FieldIDs: []string{"missing"}, FailureSeverity: "warn"}, nil)
	if got != "warn" {
		t.Fatalf("status=%q, want warn", got)
	}
}

func TestWithinToleranceNormalizesSignedDeltaAtBoundaries(t *testing.T) {
	tests := []struct {
		name     string
		delta    string
		absolute string
		relative string
		baseline string
		want     bool
	}{
		{name: "positive boundary", delta: "5", absolute: "5", want: true},
		{name: "negative boundary", delta: "-5", absolute: "5", want: true},
		{name: "negative outside", delta: "-6", absolute: "5", want: false},
		{name: "relative boundary", delta: "-10", relative: "0.1", baseline: "100", want: true},
		{name: "relative outside", delta: "-11", relative: "0.1", baseline: "100", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withinTolerance(tt.delta, tt.absolute, tt.relative, tt.baseline); got != tt.want {
				t.Fatalf("withinTolerance(%q,%q,%q,%q)=%v, want %v", tt.delta, tt.absolute, tt.relative, tt.baseline, got, tt.want)
			}
		})
	}
}

func TestValidateRuleSetRejectsNegativeTolerances(t *testing.T) {
	for _, field := range []string{"AbsoluteTolerance", "RelativeTolerance"} {
		t.Run(field, func(t *testing.T) {
			rule := RuleDefinition{RuleID: "r", Revision: 1, Kind: RuleVariance, FieldIDs: []string{"actual"}, TargetFieldID: "target", AbsoluteTolerance: "1"}
			if field == "AbsoluteTolerance" {
				rule.AbsoluteTolerance = "-1"
			} else {
				rule.AbsoluteTolerance = ""
				rule.RelativeTolerance = "-0.1"
			}
			err := ValidateRuleSet(RuleSet{RuleSetID: "set", Revision: 1, Status: "active", Rules: []RuleDefinition{rule}})
			if err == nil {
				t.Fatal("negative tolerance was accepted")
			}
		})
	}
}

func TestReconciliationUsesAbsoluteSignedDelta(t *testing.T) {
	status, _, _ := evaluateReconciliation(RuleDefinition{
		RuleID: "sum", Kind: RuleReconciliationSum, FieldIDs: []string{"a", "b", "target"}, TargetFieldID: "target", AbsoluteTolerance: "5",
	}, map[string]SnapshotObservation{
		"a":      {FieldID: "a", TypedValue: TypedValue{Kind: ValueNumber, Number: "40"}},
		"b":      {FieldID: "b", TypedValue: TypedValue{Kind: ValueNumber, Number: "40"}},
		"target": {FieldID: "target", TypedValue: TypedValue{Kind: ValueNumber, Number: "85"}},
	})
	if status != "pass" {
		t.Fatalf("status=%q, want pass at absolute negative delta boundary", status)
	}
}
