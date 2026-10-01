package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dantalabs/northern-lights/internal/assurance"
)

func TestWave2FailureEnvelopeIsTypedAndReplaySafe(t *testing.T) {
	raw, err := json.Marshal(failWithContext(context.Background(), &assurance.Error{Code: "idempotency_conflict", Message: "different request", Retryable: false, ReconciliationRequired: true}, "retry with a new key"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != "error" {
		t.Fatalf("status=%v, want error: %s", got["status"], raw)
	}
	errorObject, ok := got["error"].(map[string]any)
	if !ok || errorObject["code"] != "idempotency_conflict" || errorObject["message"] != "different request" || errorObject["reconciliation_required"] != true {
		t.Fatalf("typed error=%#v", got["error"])
	}
	for _, key := range []string{"code", "message", "retryable", "reconciliation_required", "nl_audit_id"} {
		if _, ok := errorObject[key]; !ok {
			t.Fatalf("typed error missing %q: %#v", key, errorObject)
		}
	}
}
