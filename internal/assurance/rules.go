package assurance

import (
	"fmt"
	"sort"
)

// RuleKind is deliberately closed. Rules are data, not executable expressions.
type RuleKind string

const (
	RuleRequired          RuleKind = "required"
	RuleType              RuleKind = "type"
	RuleUnit              RuleKind = "unit"
	RuleNumericRange      RuleKind = "numeric_range"
	RuleAllowedValues     RuleKind = "allowed_values"
	RuleReconciliationSum RuleKind = "reconciliation_sum"
	RuleVariance          RuleKind = "variance"
	RuleCompleteness      RuleKind = "completeness"
)

var ruleKinds = map[RuleKind]bool{
	RuleRequired: true, RuleType: true, RuleUnit: true, RuleNumericRange: true,
	RuleAllowedValues: true, RuleReconciliationSum: true, RuleVariance: true,
	RuleCompleteness: true,
}

// RuleDefinition contains only typed, bounded operands. Rule is retained as a
// convenient public name for callers that use the canonical plan vocabulary.
type RuleDefinition struct {
	RuleID            string       `json:"rule_id" yaml:"rule_id"`
	Revision          int          `json:"revision" yaml:"revision"`
	Order             int          `json:"order" yaml:"order"`
	Kind              RuleKind     `json:"kind" yaml:"kind"`
	FieldIDs          []string     `json:"field_ids" yaml:"field_ids"`
	ExpectedKind      ValueKind    `json:"expected_kind,omitempty" yaml:"expected_kind,omitempty"`
	Unit              string       `json:"unit,omitempty" yaml:"unit,omitempty"`
	Lower             string       `json:"lower,omitempty" yaml:"lower,omitempty"`
	Upper             string       `json:"upper,omitempty" yaml:"upper,omitempty"`
	AllowedValues     []TypedValue `json:"allowed_values,omitempty" yaml:"allowed_values,omitempty"`
	TargetFieldID     string       `json:"target_field_id,omitempty" yaml:"target_field_id,omitempty"`
	AbsoluteTolerance string       `json:"absolute_tolerance,omitempty" yaml:"absolute_tolerance,omitempty"`
	RelativeTolerance string       `json:"relative_tolerance,omitempty" yaml:"relative_tolerance,omitempty"`
	FailureSeverity   string       `json:"failure_severity,omitempty" yaml:"failure_severity,omitempty"`
}

type Rule = RuleDefinition

type RuleSet struct {
	RuleSetID   string           `json:"rule_set_id" yaml:"rule_set_id"`
	Revision    int              `json:"revision" yaml:"revision"`
	Status      string           `json:"status" yaml:"status"`
	ContentHash string           `json:"content_hash,omitempty" yaml:"content_hash,omitempty"`
	Rules       []RuleDefinition `json:"rules" yaml:"rules"`
}

type MaterialityPolicy struct {
	PolicyID               string `json:"policy_id" yaml:"policy_id"`
	Revision               int    `json:"revision" yaml:"revision"`
	Status                 string `json:"status" yaml:"status"`
	AbsoluteThreshold      string `json:"absolute_threshold" yaml:"absolute_threshold"`
	RelativeThreshold      string `json:"relative_threshold" yaml:"relative_threshold"`
	Direction              string `json:"direction" yaml:"direction"`
	ZeroBaseline           string `json:"zero_baseline" yaml:"zero_baseline"`
	MissingBehavior        string `json:"missing_behavior" yaml:"missing_behavior"`
	TypeChangeBehavior     string `json:"type_change_behavior" yaml:"type_change_behavior"`
	Rounding               string `json:"rounding" yaml:"rounding"`
	AllowPartialCompare    bool   `json:"allow_partial_compare" yaml:"allow_partial_compare"`
	AllowPartialComparison bool   `json:"-" yaml:"-"`
}

func (p MaterialityPolicy) permitsPartialComparison() bool {
	return p.AllowPartialCompare || p.AllowPartialComparison
}

type ExportProfile struct {
	ProfileID         string   `json:"profile_id" yaml:"profile_id"`
	Revision          int      `json:"revision" yaml:"revision"`
	Status            string   `json:"status" yaml:"status"`
	PermittedSubjects []string `json:"permitted_subjects" yaml:"permitted_subjects"`
	RedactionProfile  string   `json:"redaction_profile" yaml:"redaction_profile"`
	RetentionClass    string   `json:"retention_class" yaml:"retention_class"`
	MaxRows           int      `json:"max_rows" yaml:"max_rows"`
	MaxBytes          int      `json:"max_bytes" yaml:"max_bytes"`
	DeliveryPolicy    string   `json:"delivery_policy" yaml:"delivery_policy"`
}

func ValidateRuleSet(set RuleSet) error {
	if set.RuleSetID == "" || len(set.RuleSetID) > maxReferenceLength || set.Revision <= 0 || set.Revision > maxRevision || set.Status != "active" {
		return fmt.Errorf("invalid rule set identity or status")
	}
	if len(set.Rules) == 0 || len(set.Rules) > maxFieldCount {
		return fmt.Errorf("rule set must contain 1 through %d rules", maxFieldCount)
	}
	seenIDs := make(map[string]bool, len(set.Rules))
	seenOrders := make(map[int]bool, len(set.Rules))
	for i, rule := range set.Rules {
		if rule.RuleID == "" || len(rule.RuleID) > maxReferenceLength || seenIDs[rule.RuleID] || rule.Revision <= 0 || rule.Revision > maxRevision {
			return fmt.Errorf("rule IDs and revisions must be unique and bounded")
		}
		order := rule.Order
		if order == 0 {
			order = i + 1
		}
		if order <= 0 || seenOrders[order] {
			return fmt.Errorf("rule order must be unique and positive")
		}
		if !ruleKinds[rule.Kind] {
			return fmt.Errorf("unsupported rule kind %q", rule.Kind)
		}
		if len(rule.FieldIDs) == 0 || len(rule.FieldIDs) > maxFieldCount {
			return fmt.Errorf("rule %s has an invalid field set", rule.RuleID)
		}
		fieldSeen := make(map[string]bool, len(rule.FieldIDs))
		for _, fieldID := range rule.FieldIDs {
			if fieldID == "" || len(fieldID) > maxFieldIDLength || fieldSeen[fieldID] {
				return fmt.Errorf("rule %s has duplicate or invalid field IDs", rule.RuleID)
			}
			fieldSeen[fieldID] = true
		}
		if rule.TargetFieldID != "" && fieldSeen[rule.TargetFieldID] {
			return fmt.Errorf("rule %s target field must be distinct", rule.RuleID)
		}
		if err := validateRuleOperands(rule); err != nil {
			return err
		}
		for _, decimal := range []string{rule.Lower, rule.Upper, rule.AbsoluteTolerance, rule.RelativeTolerance} {
			if decimal != "" {
				canonical, err := canonicalDecimal(decimal)
				if err != nil || stringsHasMinus(canonical) {
					return fmt.Errorf("rule %s has invalid decimal operand", rule.RuleID)
				}
			}
		}
		for _, value := range rule.AllowedValues {
			if !knownValueKind(value.Kind) {
				return fmt.Errorf("rule %s has an invalid allowed value kind", rule.RuleID)
			}
		}
		seenIDs[rule.RuleID], seenOrders[order] = true, true
	}
	return nil
}

func validateRuleOperands(rule RuleDefinition) error {
	switch rule.Kind {
	case RuleRequired:
		if rule.ExpectedKind != "" || rule.Unit != "" || rule.Lower != "" || rule.Upper != "" || len(rule.AllowedValues) > 0 || rule.TargetFieldID != "" || rule.AbsoluteTolerance != "" || rule.RelativeTolerance != "" {
			return fmt.Errorf("rule %s: required has forbidden operands", rule.RuleID)
		}
	case RuleType:
		if rule.ExpectedKind == "" || !knownValueKind(rule.ExpectedKind) || rule.Unit != "" || rule.Lower != "" || rule.Upper != "" || len(rule.AllowedValues) > 0 || rule.TargetFieldID != "" || rule.AbsoluteTolerance != "" || rule.RelativeTolerance != "" {
			return fmt.Errorf("rule %s: type operand matrix is invalid", rule.RuleID)
		}
	case RuleUnit:
		if rule.Unit == "" || rule.ExpectedKind != "" || rule.Lower != "" || rule.Upper != "" || len(rule.AllowedValues) > 0 || rule.TargetFieldID != "" || rule.AbsoluteTolerance != "" || rule.RelativeTolerance != "" {
			return fmt.Errorf("rule %s: unit operand matrix is invalid", rule.RuleID)
		}
	case RuleNumericRange:
		if len(rule.FieldIDs) != 1 || (rule.Lower == "" && rule.Upper == "") || rule.ExpectedKind != "" || rule.Unit != "" || len(rule.AllowedValues) > 0 || rule.TargetFieldID != "" || rule.AbsoluteTolerance != "" || rule.RelativeTolerance != "" {
			return fmt.Errorf("rule %s: numeric range operand matrix is invalid", rule.RuleID)
		}
	case RuleAllowedValues:
		if len(rule.FieldIDs) != 1 || len(rule.AllowedValues) == 0 || rule.ExpectedKind != "" || rule.Unit != "" || rule.Lower != "" || rule.Upper != "" || rule.TargetFieldID != "" || rule.AbsoluteTolerance != "" || rule.RelativeTolerance != "" {
			return fmt.Errorf("rule %s: allowed values operand matrix is invalid", rule.RuleID)
		}
	case RuleReconciliationSum:
		if len(rule.FieldIDs) < 2 || rule.TargetFieldID == "" || (rule.AbsoluteTolerance == "" && rule.RelativeTolerance == "") || rule.ExpectedKind != "" || rule.Unit != "" || rule.Lower != "" || rule.Upper != "" || len(rule.AllowedValues) > 0 {
			return fmt.Errorf("rule %s: reconciliation operand matrix is invalid", rule.RuleID)
		}
	case RuleVariance:
		if len(rule.FieldIDs) != 1 || rule.TargetFieldID == "" || (rule.AbsoluteTolerance == "" && rule.RelativeTolerance == "") || rule.ExpectedKind != "" || rule.Unit != "" || rule.Lower != "" || rule.Upper != "" || len(rule.AllowedValues) > 0 {
			return fmt.Errorf("rule %s: variance operand matrix is invalid", rule.RuleID)
		}
	case RuleCompleteness:
		if rule.ExpectedKind != "" || rule.Unit != "" || rule.Lower != "" || rule.Upper != "" || len(rule.AllowedValues) > 0 || rule.TargetFieldID != "" || rule.AbsoluteTolerance != "" || rule.RelativeTolerance != "" {
			return fmt.Errorf("rule %s: completeness has forbidden operands", rule.RuleID)
		}
	}
	if rule.FailureSeverity != "" && rule.FailureSeverity != "fail" && rule.FailureSeverity != "warn" {
		return fmt.Errorf("rule %s: invalid failure severity", rule.RuleID)
	}
	return nil
}

func ValidateMaterialityPolicy(policy MaterialityPolicy) error {
	if policy.PolicyID == "" || len(policy.PolicyID) > maxReferenceLength || policy.Revision <= 0 || policy.Status != "active" {
		return fmt.Errorf("invalid materiality policy identity or status")
	}
	for _, value := range []string{policy.AbsoluteThreshold, policy.RelativeThreshold} {
		if value == "" {
			continue
		}
		if canonical, err := canonicalDecimal(value); err != nil || stringsHasMinus(canonical) {
			return fmt.Errorf("materiality thresholds must be non-negative decimals")
		}
	}
	if policy.Direction != "absolute_or_relative" && policy.Direction != "absolute_and_relative" && policy.Direction != "absolute" && policy.Direction != "relative" {
		return fmt.Errorf("unsupported materiality direction %q", policy.Direction)
	}
	if policy.ZeroBaseline != "not_comparable" && policy.ZeroBaseline != "absolute_only" && policy.ZeroBaseline != "relative_zero" {
		return fmt.Errorf("unsupported zero baseline behavior %q", policy.ZeroBaseline)
	}
	if policy.MissingBehavior != "not_comparable" && policy.MissingBehavior != "material" && policy.MissingBehavior != "immaterial" {
		return fmt.Errorf("unsupported missing behavior %q", policy.MissingBehavior)
	}
	if policy.TypeChangeBehavior != "not_comparable" && policy.TypeChangeBehavior != "material" && policy.TypeChangeBehavior != "error" {
		return fmt.Errorf("unsupported type change behavior %q", policy.TypeChangeBehavior)
	}
	if policy.Rounding != "half_even" && policy.Rounding != "half_up" && policy.Rounding != "truncate" {
		return fmt.Errorf("unsupported rounding policy %q", policy.Rounding)
	}
	return nil
}

func stringsHasMinus(value string) bool { return len(value) > 0 && value[0] == '-' }

func ValidateExportProfile(profile ExportProfile) error {
	if profile.ProfileID == "" || len(profile.ProfileID) > maxReferenceLength || profile.Revision <= 0 || profile.Revision > maxRevision || profile.Status != "active" || profile.RetentionClass == "" || profile.MaxRows <= 0 || profile.MaxRows > maxFieldCount*100 || profile.MaxBytes <= 0 || profile.MaxBytes > 64<<20 {
		return fmt.Errorf("invalid export profile")
	}
	if profile.RedactionProfile != "standard" && profile.RedactionProfile != "strict" {
		return fmt.Errorf("unsupported redaction profile %q", profile.RedactionProfile)
	}
	if profile.RetentionClass != "standard" && profile.RetentionClass != "long_term" {
		return fmt.Errorf("unsupported retention class %q", profile.RetentionClass)
	}
	if profile.DeliveryPolicy != "opaque_reference" {
		return fmt.Errorf("unsupported delivery policy %q", profile.DeliveryPolicy)
	}
	if len(profile.PermittedSubjects) == 0 || len(profile.PermittedSubjects) > 4 {
		return fmt.Errorf("export profile must permit one through four subject kinds")
	}
	seen := make(map[string]struct{}, len(profile.PermittedSubjects))
	for _, subject := range profile.PermittedSubjects {
		if subject != "snapshot" && subject != "validation_run" && subject != "comparison" && subject != "transfer" {
			return fmt.Errorf("unsupported evidence subject %q", subject)
		}
		if _, exists := seen[subject]; exists {
			return fmt.Errorf("duplicate evidence subject %q", subject)
		}
		seen[subject] = struct{}{}
	}
	return nil
}

func ruleOrder(set RuleSet) []RuleDefinition {
	rules := append([]RuleDefinition(nil), set.Rules...)
	sort.SliceStable(rules, func(i, j int) bool {
		left, right := rules[i].Order, rules[j].Order
		if left == 0 {
			left = i + 1
		}
		if right == 0 {
			right = j + 1
		}
		return left < right
	})
	return rules
}
