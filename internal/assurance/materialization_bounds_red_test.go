package assurance

import "testing"

func TestPackageBoundsIncludeSubjectCSVManifestAndAllRows(t *testing.T) {
	if packageWithinBounds(40, 40, 40, 100, 3) {
		t.Fatal("package byte bound ignored an artifact")
	}
	if packageWithinBounds(10, 10, 10, 100, 2) {
		t.Fatal("package row bound ignored manifest and derivative rows")
	}
	if !packageWithinBounds(10, 10, 10, 100, 3) {
		t.Fatal("package within exact byte and row boundaries was rejected")
	}
}
