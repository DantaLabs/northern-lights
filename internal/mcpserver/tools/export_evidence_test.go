package tools

import "testing"

func TestExportEvidenceSchemaOmitsOptionalCheckpointMembers(t *testing.T) {
	schema := exportEvidenceOutputSchema()
	if schema["additionalProperties"] != false {
		t.Fatal("evidence schema is open")
	}
}
