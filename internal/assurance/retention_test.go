package assurance

import "testing"

func TestRetentionPolicyRequiresImmutableActiveDefinition(t *testing.T) {
	if err := ValidateRetentionPolicy(RetentionPolicy{RetentionClass: "standard", DurationSeconds: 0, Status: "active"}); err == nil {
		t.Fatal("zero-duration policy accepted")
	}
}
