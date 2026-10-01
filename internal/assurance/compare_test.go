package assurance

import "testing"

func TestCompareZeroBaselineModes(t *testing.T) {
	policy := MaterialityPolicy{AbsoluteThreshold: "5", RelativeThreshold: "0.5", Direction: "absolute_or_relative", ZeroBaseline: "absolute_only"}
	if got := materialityStatus(policy, "5", "5", "0"); got != "material" {
		t.Fatalf("absolute_only=%q", got)
	}
	policy.ZeroBaseline = "not_comparable"
	if got := materialityStatus(policy, "5", "5", "0"); got != "not_comparable" {
		t.Fatalf("not_comparable=%q", got)
	}
}
