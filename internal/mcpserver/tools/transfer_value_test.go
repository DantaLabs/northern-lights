package tools

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestTransferToolDiscoveredBoundedAndUnavailableWithoutProviderCall(t *testing.T) {
	env := newTestEnv(t, func(w http.ResponseWriter, _ *http.Request) { t.Errorf("unexpected provider call") })
	tools := listAllTools(t)
	var discovered bool
	for _, tool := range tools {
		if tool.Name == "workiva_transfer_value" {
			discovered = true
			verifyClosedBoundedSchema(t, "transfer.input", tool.InputSchema)
			verifyClosedBoundedSchema(t, "transfer.output", tool.OutputSchema)
		}
	}
	if !discovered {
		t.Fatal("transfer tool missing from tools/list")
	}
	if len(tools) != 12 {
		t.Fatalf("discovered %d tools, want isolated branch count 12", len(tools))
	}
	for _, phase := range []string{"stage", "confirm", "acknowledge", "reconcile", "bulk_stage", "bulk_confirm"} {
		result := callTool(t, env.deps, TransferValue(), map[string]any{"phase": phase, "idempotency_key": "test-key"})
		if result.IsError {
			t.Fatalf("phase %s returned tool error: %v", phase, result)
		}
		if len(result.Content) == 0 {
			t.Fatalf("phase %s returned no typed result", phase)
		}
		text, ok := result.Content[0].(*mcp.TextContent)
		if !ok {
			t.Fatalf("phase %s content is %T", phase, result.Content[0])
		}
		var output map[string]any
		if err := json.Unmarshal([]byte(text.Text), &output); err != nil {
			t.Fatalf("decode %s response: %v", phase, err)
		}
		want := "unavailable"
		if phase == "bulk_stage" || phase == "bulk_confirm" {
			want = "feature_disabled"
		}
		if output["status"] != want {
			t.Fatalf("phase %s status=%v want %s", phase, output["status"], want)
		}
		if output["no_mutation_submitted"] != true {
			t.Fatalf("phase %s may have submitted mutation: %#v", phase, output)
		}
	}
	if got := env.apiCalls.Load(); got != 0 {
		t.Fatalf("unavailable transfer called provider %d times", got)
	}
}
