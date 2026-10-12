package tools

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/content"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/google/jsonschema-go/jsonschema"
)

func TestContentPlacementRawSDKRejectsDuplicateJSONAndFailsConfirmClosed(t *testing.T) {
	fixture := newRawWave2Fixture(t)
	if err := fixture.store.SetContentStorageKey("content-placement-runtime-test", make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := assurance.NewContentPolicyResolver(fixture.store, publicKey, "not-activated-content-access")
	if err != nil {
		t.Fatal(err)
	}
	gate := assurance.NewDrainGate()
	runtime, err := content.NewRuntime(content.RuntimeConfig{Store: fixture.store, Resolver: resolver, Audit: fixture.env.deps.Audit, Gate: gate, Fence: runtimeTestDurableContentFence{}})
	if err != nil {
		t.Fatal(err)
	}
	permissions := []identity.Permission{identity.PermissionContentIngest, identity.PermissionContentConfirm, identity.PermissionContentReconcile}
	principal := identity.Principal{TenantID: "11111111-1111-4111-8111-111111111111", ObjectID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", TokenType: identity.TokenTypeDelegated, Permissions: permissions}
	verifier := rawTokenVerifier{principals: map[string]identity.Principal{"content-placement-token": principal}}
	registry := mcpserver.NewRegistry()
	registry.Register(ContentPlacement())
	handler, err := mcpserver.New(mcpserver.Deps{Audit: fixture.env.deps.Audit, Assurance: fixture.store, DrainGate: gate, Content: runtime, Cfg: &config.Config{AssuranceEnabled: true}}, registry, &mcpserver.Options{AuthMode: config.AuthModeEntra, TokenVerifier: verifier})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := &rawMCPClient{t: t, url: server.URL + "/mcp", authToken: "content-placement-token"}
	initializeRaw(t, client)
	outputSchema := contentPlacementOutputSchema(t, client)
	duplicate := json.RawMessage(`{"phase":"ingest","phase":"ingest","idempotency_key":"content-duplicate-key-01","origin":{"kind":"analyst"},"source":{"source_text":"PRIVATE SOURCE","media_type":"text/plain"},"extracted_items":[],"draft_artifacts":[]}`)
	resp, body := client.post("tools/call", map[string]any{"name": "workiva_content_placement", "arguments": duplicate})
	if resp.StatusCode != 200 {
		t.Fatalf("duplicate JSON request status=%d: %s", resp.StatusCode, body)
	}
	var rejected rawRPCResponse
	if err := json.Unmarshal(body, &rejected); err != nil {
		t.Fatal(err)
	}
	assertContentPlacementSchema(t, outputSchema, rejected.Result)
	if rejected.Result.StructuredContent == nil || !rejected.Result.IsError {
		t.Fatalf("duplicate JSON was not rejected as a structured tool error: %s", body)
	}
	var errorOutput content.PublicPlacementOutput
	encoded, _ := json.Marshal(rejected.Result.StructuredContent)
	if err := json.Unmarshal(encoded, &errorOutput); err != nil {
		t.Fatal(err)
	}
	if errorOutput.Status != "invalid" || len(errorOutput.Errors) != 1 || errorOutput.Errors[0].Code != "invalid_request" || errorOutput.NLAuditID == "" {
		t.Fatalf("duplicate JSON response violates public contract: %+v", errorOutput)
	}
	if got := string(body); containsString(got, "PRIVATE SOURCE") {
		t.Fatal("raw request body leaked into tool response")
	}
	var privateRows int
	if err := fixture.db.QueryRow(`SELECT count(*) FROM assurance_content_sources`).Scan(&privateRows); err != nil {
		t.Fatal(err)
	}
	if privateRows != 0 {
		t.Fatalf("malformed request persisted %d content source rows", privateRows)
	}

	confirm := json.RawMessage(`{"phase":"confirm","idempotency_key":"content-confirm-key-001","placement_intent_id":"intent-placeholder"}`)
	resp, body = client.post("tools/call", map[string]any{"name": "workiva_content_placement", "arguments": confirm})
	if resp.StatusCode != 200 {
		t.Fatalf("confirm response status=%d: %s", resp.StatusCode, body)
	}
	var blocked rawRPCResponse
	if err := json.Unmarshal(body, &blocked); err != nil {
		t.Fatal(err)
	}
	if !blocked.Result.IsError {
		t.Fatalf("unadmitted confirmation was not blocked: %s", body)
	}
	encoded, _ = json.Marshal(blocked.Result.StructuredContent)
	if err := json.Unmarshal(encoded, &errorOutput); err != nil {
		t.Fatal(err)
	}
	if errorOutput.Phase != "confirm" || errorOutput.Status != "unavailable" || len(errorOutput.Errors) != 1 || errorOutput.Errors[0].Code != "feature_disabled" || !containsString(errorOutput.Errors[0].Message, "capability_unverified") || !errorOutput.NoMutationSubmitted {
		t.Fatalf("confirmation gate response does not clearly fail closed: %+v", errorOutput)
	}
	assertContentPlacementSchema(t, outputSchema, blocked.Result)

	reconcile := json.RawMessage(`{"phase":"reconcile","idempotency_key":"content-reconcile-key-1","placement_intent_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","action":"read_back"}`)
	resp, body = client.post("tools/call", map[string]any{"name": contentPlacementToolName, "arguments": reconcile})
	if resp.StatusCode != 200 {
		t.Fatalf("reconcile response status=%d: %s", resp.StatusCode, body)
	}
	var reconcileBlocked rawRPCResponse
	if err := json.Unmarshal(body, &reconcileBlocked); err != nil {
		t.Fatal(err)
	}
	assertContentPlacementSchema(t, outputSchema, reconcileBlocked.Result)
	encoded, _ = json.Marshal(reconcileBlocked.Result.StructuredContent)
	if err := json.Unmarshal(encoded, &errorOutput); err != nil {
		t.Fatal(err)
	}
	if errorOutput.Phase != "reconcile" || errorOutput.Status != "unavailable" || len(errorOutput.Errors) != 1 || errorOutput.Errors[0].Code != "feature_disabled" || !containsString(errorOutput.Errors[0].Message, "capability_unverified") {
		t.Fatalf("reconciliation gate response does not clearly fail closed: %+v", errorOutput)
	}
}

func containsString(value, part string) bool { return strings.Contains(value, part) }

func contentPlacementOutputSchema(t *testing.T, client *rawMCPClient) map[string]any {
	t.Helper()
	resp, body := client.post("tools/list", map[string]any{})
	if resp.StatusCode != 200 {
		t.Fatalf("tools/list status=%d: %s", resp.StatusCode, body)
	}
	var listing struct {
		Result struct {
			Tools []struct {
				Name         string         `json:"name"`
				OutputSchema map[string]any `json:"outputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		t.Fatal(err)
	}
	for _, tool := range listing.Result.Tools {
		if tool.Name == contentPlacementToolName {
			if tool.OutputSchema == nil {
				t.Fatal("content tool omitted output schema")
			}
			return tool.OutputSchema
		}
	}
	t.Fatal("content placement tool is absent from tools/list")
	return nil
}

func assertContentPlacementSchema(t *testing.T, schemaMap map[string]any, result rawRPCResult) {
	t.Helper()
	if !result.IsError {
		t.Fatal("expected structured tool error")
	}
	encodedSchema, err := json.Marshal(schemaMap)
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(encodedSchema, &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve advertised response schema: %v", err)
	}
	instanceBytes, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var instance any
	if err := json.Unmarshal(instanceBytes, &instance); err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(instance); err != nil {
		t.Fatalf("structured content violates advertised output schema: %v\n%s", err, instanceBytes)
	}
}

type runtimeTestDurableContentFence struct{}

func (runtimeTestDurableContentFence) Identity(string) (string, string, error) {
	return "env", "tenant", nil
}
func (runtimeTestDurableContentFence) Durable() bool { return true }
func (runtimeTestDurableContentFence) CreateClaim(context.Context, content.ContentClaim) (string, error) {
	panic("content runtime must not claim provider writes")
}
func (runtimeTestDurableContentFence) VerifyClaim(context.Context, content.ContentClaim, string) error {
	panic("content runtime must not verify mutation claims")
}
func (runtimeTestDurableContentFence) CreateTerminal(context.Context, content.ContentClaim, content.ContentTerminal) (string, error) {
	panic("content runtime must not write terminal proofs")
}
func (runtimeTestDurableContentFence) VerifyTerminal(context.Context, content.ContentClaim, content.ContentTerminal, string) error {
	panic("content runtime must not verify terminal proofs")
}
func (runtimeTestDurableContentFence) ReadVerifiedTerminal(context.Context, content.ContentClaim, string) (content.ContentTerminal, string, error) {
	panic("content runtime must not read mutation terminals")
}
func (runtimeTestDurableContentFence) CaptureHighWater(context.Context, string) (content.ContentFenceHighWater, error) {
	panic("content runtime must not admit from fence durability alone")
}
func (runtimeTestDurableContentFence) Scan(context.Context, content.ContentFenceScanRequest) ([]content.ContentFenceObject, error) {
	panic("content runtime must not admit from fence durability alone")
}
