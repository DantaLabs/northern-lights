package tools

import "testing"

func TestValidateReportSchemaIsClosed(t *testing.T) {
	schema := validateReportInputSchema()
	if schema["additionalProperties"] != false {
		t.Fatal("validation schema is open")
	}
}
