package tools

import "testing"

func TestComparePeriodsSchemaIsClosed(t *testing.T) {
	schema := comparePeriodsInputSchema()
	if schema["additionalProperties"] != false {
		t.Fatal("comparison schema is open")
	}
}
