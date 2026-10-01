package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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

func TestEveryWave2ToolUsesStructuredErrorContentWithAuditCorrelation(t *testing.T) {
	cases := []struct {
		tool mcpserver.Tool
		args map[string]any
	}{
		{SnapshotReport(), map[string]any{"report_id": "r", "period": map[string]any{"key": "p"}, "idempotency_key": "k"}},
		{ValidateReport(), map[string]any{"snapshot_id": "s", "rule_set_id": "r", "idempotency_key": "k"}},
		{ComparePeriods(), map[string]any{"current_snapshot_id": "c", "prior_snapshot_id": "p", "materiality_policy_id": "m", "idempotency_key": "k"}},
		{ExportEvidence(), map[string]any{"subject_kind": "snapshot", "subject_id": "s", "format": "json", "redaction_profile": "standard", "include_audit_chain": false, "retention_class": "long_term", "idempotency_key": "k"}},
	}
	for _, tc := range cases {
		t.Run(tc.tool.Name(), func(t *testing.T) {
			env := newTestEnv(t, http.NotFound)
			result := callTool(t, env.deps, tc.tool, tc.args)
			if !result.IsError {
				t.Fatalf("expected structured error, got %#v", result)
			}
			body := structuredContent(t, result)
			if body["status"] != "error" || body["nl_audit_id"] == "" {
				t.Fatalf("error envelope = %#v", body)
			}
			errObject, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatalf("error object = %#v", body["error"])
			}
			for _, key := range []string{"code", "message", "retryable", "reconciliation_required", "nl_audit_id"} {
				if _, present := errObject[key]; !present {
					t.Fatalf("error object missing %q: %#v", key, errObject)
				}
			}
			text, ok := result.Content[0].(*mcp.TextContent)
			if !ok || !strings.Contains(text.Text, body["nl_audit_id"].(string)) {
				t.Fatalf("first text block lacks audit ID: %#v", result.Content)
			}
		})
	}
}
