package tools

import (
	"encoding/json"
	"net/http"
	"strings"
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
	if len(tools) != 13 {
		t.Fatalf("discovered %d tools, want integrated count 13", len(tools))
	}
	for _, phase := range []string{"stage", "confirm", "acknowledge", "reconcile", "bulk_stage", "bulk_confirm"} {
		result := callTool(t, env.deps, TransferValue(), transferValidArguments(phase))
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
		switch phase {
		case "confirm", "acknowledge", "reconcile":
			want = "denied"
		case "bulk_stage", "bulk_confirm":
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

func transferValidArguments(phase string) map[string]any {
	base := map[string]any{"phase": phase, "idempotency_key": "test-key-" + phase}
	switch phase {
	case "stage":
		base["source"] = map[string]any{"mapping_id": "mapping-opaque"}
		base["target"] = map[string]any{"resource_id": "resource-opaque", "locator": "Sheet 1!B2"}
	case "confirm":
		base["transfer_id"] = "transfer-opaque"
		base["confirmation_token"] = "one-time-secret"
	case "acknowledge":
		base["transfer_id"] = "transfer-opaque"
		base["observation"] = "not_checked"
		base["refreshed"] = true
	case "reconcile":
		base["transfer_id"] = "transfer-opaque"
		base["action"] = "inspect_operation"
	case "bulk_stage":
		base["selection"] = map[string]any{"report_field_ids": []any{"field-opaque"}}
		base["target_template_id"] = "template-opaque"
	case "bulk_confirm":
		base["selection"] = map[string]any{"frozen_transfer_ids": []any{"transfer-opaque"}, "selection_digest": "digest-opaque"}
		base["eligible_row_ids"] = []any{"row-opaque"}
	}
	return base
}

func TestRawTransferPhaseValidationIsStructuredAndSideEffectFree(t *testing.T) {
	fixture := newRawWave2Fixture(t)
	server := startRawAPIKeyServer(t, fixture)
	client := &rawMCPClient{t: t, url: server.URL + "/mcp", authToken: "api-key", actor: "caller-asserted@example.com"}
	initializeRaw(t, client)
	schemas := rawToolSchemas(t, client)
	before := countAssuranceReservations(t, fixture)

	cases := []struct {
		phase  string
		mutate func(map[string]any)
		code   string
	}{
		{phase: "stage", mutate: func(args map[string]any) { delete(args, "source") }, code: "missing_required_field"},
		{phase: "confirm", mutate: func(args map[string]any) { delete(args, "confirmation_token") }, code: "missing_required_field"},
		{phase: "acknowledge", mutate: func(args map[string]any) { delete(args, "refreshed") }, code: "missing_required_field"},
		{phase: "reconcile", mutate: func(args map[string]any) { delete(args, "action") }, code: "missing_required_field"},
		{phase: "bulk_stage", mutate: func(args map[string]any) { delete(args, "selection") }, code: "missing_required_field"},
		{phase: "bulk_confirm", mutate: func(args map[string]any) { delete(args, "eligible_row_ids") }, code: "missing_required_field"},
	}
	for _, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			args := transferValidArguments(tc.phase)
			tc.mutate(args)
			result := rawCall(t, client, "workiva_transfer_value", args)
			body := assertRawPayload(t, "transfer/"+tc.phase, schemas["workiva_transfer_value"], result, false)
			if body["status"] != "error" {
				t.Fatalf("status=%v, want error: %#v", body["status"], body)
			}
			errors, ok := body["errors"].([]any)
			if !ok || len(errors) != 1 || errors[0].(map[string]any)["code"] != tc.code {
				t.Fatalf("structured errors=%#v, want %s", body["errors"], tc.code)
			}
		})
	}

	// The advertised schema rejects values above its maximum before the handler
	// can emit a phase-specific response. This is a distinct transport claim.
	oversized := transferValidArguments("bulk_stage")
	oversized["max_rows"] = 101
	result := rawCall(t, client, "workiva_transfer_value", oversized)
	if !result.IsError {
		t.Fatal("SDK accepted max_rows above the advertised maximum")
	}

	secretArgs := transferValidArguments("confirm")
	secretArgs["confirmation_token"] = "raw-confirmation-token-must-not-return"
	delete(secretArgs, "transfer_id")
	resp, rawBody := client.post("tools/call", map[string]any{"name": "workiva_transfer_value", "arguments": secretArgs})
	if resp.StatusCode != http.StatusOK || strings.Contains(string(rawBody), "raw-confirmation-token-must-not-return") {
		t.Fatalf("raw token was returned or transport failed: status=%d body=%s", resp.StatusCode, rawBody)
	}

	if got := countAssuranceReservations(t, fixture); got != before {
		t.Fatalf("validation changed reservation count from %d to %d", before, got)
	}
	if got := fixture.env.apiCalls.Load(); got != 0 {
		t.Fatalf("validation reached provider %d times", got)
	}
}

func TestRawTransferHighImpactAPIKeyDenialAndBulkGate(t *testing.T) {
	fixture := newRawWave2Fixture(t)
	server := startRawAPIKeyServer(t, fixture)
	client := &rawMCPClient{t: t, url: server.URL + "/mcp", authToken: "api-key", actor: "caller-asserted@example.com"}
	initializeRaw(t, client)
	schemas := rawToolSchemas(t, client)
	for _, phase := range []string{"stage", "confirm", "acknowledge", "reconcile", "bulk_stage", "bulk_confirm"} {
		result := rawCall(t, client, "workiva_transfer_value", transferValidArguments(phase))
		body := assertRawPayload(t, "transfer/gate/"+phase, schemas["workiva_transfer_value"], result, false)
		want := "unavailable"
		wantCode := "transfer_service_unavailable"
		switch phase {
		case "confirm", "acknowledge", "reconcile":
			want, wantCode = "denied", "strong_identity_required"
		case "bulk_stage", "bulk_confirm":
			want, wantCode = "feature_disabled", "feature_disabled"
		}
		if body["status"] != want || body["errors"].([]any)[0].(map[string]any)["code"] != wantCode {
			t.Fatalf("phase %s gate response=%#v, want status=%s code=%s", phase, body, want, wantCode)
		}
	}
	if got := countAssuranceReservations(t, fixture); got != 0 {
		t.Fatalf("gated transfer calls created %d reservations", got)
	}
	if got := fixture.env.apiCalls.Load(); got != 0 {
		t.Fatalf("gated transfer calls reached provider %d times", got)
	}
}
