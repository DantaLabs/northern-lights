package assurance

import "testing"

func TestCanonicalSetIDsSortWithoutConfigurationLookup(t *testing.T) {
	got, err := canonicalizeSetStrings([]string{"field-b", "field-a", "field-c"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"field-a", "field-b", "field-c"}
	if len(got) != len(want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got=%v want=%v", got, want)
		}
	}
}

func TestCanonicalSetIDsRejectDuplicatesBeforeRequestDigest(t *testing.T) {
	if _, err := canonicalizeSetStrings([]string{"field-a", "field-b", "field-a"}); err == nil {
		t.Fatal("duplicate set member was silently normalized")
	}
}

func TestCanonicalSetRequestsMatchForEquivalentOrdering(t *testing.T) {
	leftIDs, err := canonicalizeSetStrings([]string{"b", "a"})
	if err != nil {
		t.Fatal(err)
	}
	rightIDs, err := canonicalizeSetStrings([]string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	left, _ := CanonicalJSON(struct {
		IDs []string `json:"ids"`
	}{leftIDs})
	right, _ := CanonicalJSON(struct {
		IDs []string `json:"ids"`
	}{rightIDs})
	if string(left) != string(right) {
		t.Fatalf("canonical set requests differ: %s vs %s", left, right)
	}
}
