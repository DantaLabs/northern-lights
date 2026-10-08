package tools

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/relationships"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

// validateJSONSchema is deliberately restricted to the JSON Schema vocabulary
// emitted by this repository. It validates decoded wire values, not Go structs,
// so nulls, omitted required fields, and unadvertised response properties are
// caught at the raw MCP transport boundary.
func validateJSONSchema(schema map[string]any, value any) error {
	return validateJSONSchemaAt("$", schema, value)
}

func validateJSONSchemaAt(path string, schema map[string]any, value any) error {
	if value == nil {
		return fmt.Errorf("%s: JSON null is not permitted", path)
	}
	if enum, ok := schema["enum"].([]any); ok {
		matched := false
		for _, candidate := range enum {
			if candidate == value {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s: %v is not in enum", path, value)
		}
	}
	typ, _ := schema["type"].(string)
	switch typ {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: got %T, want object", path, value)
		}
		properties, _ := schema["properties"].(map[string]any)
		required := schemaStrings(schema["required"])
		for _, name := range required {
			if _, ok := object[name]; !ok {
				return fmt.Errorf("%s: missing required property %q", path, name)
			}
		}
		additional, hasAdditional := schema["additionalProperties"]
		if hasAdditional && additional == false {
			for name := range object {
				if _, ok := properties[name]; !ok {
					return fmt.Errorf("%s: unknown property %q", path, name)
				}
			}
		}
		for name, child := range object {
			propertySchema, advertised := properties[name].(map[string]any)
			if advertised {
				if err := validateJSONSchemaAt(path+"."+name, propertySchema, child); err != nil {
					return err
				}
			}
		}
	case "array":
		array, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s: got %T, want array", path, value)
		}
		if min, ok := schemaInt(schema["minItems"]); ok && len(array) < min {
			return fmt.Errorf("%s: %d items is below minItems %d", path, len(array), min)
		}
		if max, ok := schemaInt(schema["maxItems"]); ok && len(array) > max {
			return fmt.Errorf("%s: %d items exceeds maxItems %d", path, len(array), max)
		}
		if unique, _ := schema["uniqueItems"].(bool); unique {
			seen := map[string]struct{}{}
			for index, item := range array {
				encoded, err := json.Marshal(item)
				if err != nil {
					return fmt.Errorf("%s[%d]: cannot encode unique item: %w", path, index, err)
				}
				if _, exists := seen[string(encoded)]; exists {
					return fmt.Errorf("%s: duplicate item at index %d", path, index)
				}
				seen[string(encoded)] = struct{}{}
			}
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for index, item := range array {
				if err := validateJSONSchemaAt(fmt.Sprintf("%s[%d]", path, index), items, item); err != nil {
					return err
				}
			}
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s: got %T, want string", path, value)
		}
		length := len([]rune(text))
		if min, ok := schemaInt(schema["minLength"]); ok && length < min {
			return fmt.Errorf("%s: string length %d is below minLength %d", path, length, min)
		}
		if max, ok := schemaInt(schema["maxLength"]); ok && length > max {
			return fmt.Errorf("%s: string length %d exceeds maxLength %d", path, length, max)
		}
	case "integer":
		number, ok := schemaNumber(value)
		if !ok || math.Trunc(number) != number {
			return fmt.Errorf("%s: got %T, want integer", path, value)
		}
		if min, ok := schemaNumber(schema["minimum"]); ok && number < min {
			return fmt.Errorf("%s: %v is below minimum %v", path, number, min)
		}
		if max, ok := schemaNumber(schema["maximum"]); ok && number > max {
			return fmt.Errorf("%s: %v exceeds maximum %v", path, number, max)
		}
	case "number":
		if _, ok := schemaNumber(value); !ok {
			return fmt.Errorf("%s: got %T, want number", path, value)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: got %T, want boolean", path, value)
		}
	default:
		return fmt.Errorf("%s: unsupported schema type %q", path, typ)
	}
	return nil
}

func schemaStrings(value any) []string {
	if items, ok := value.([]string); ok {
		return items
	}
	items, _ := value.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	if result == nil {
		if items, ok := value.([]string); ok {
			return items
		}
	}
	return result
}

func schemaInt(value any) (int, bool) {
	number, ok := schemaNumber(value)
	return int(number), ok && math.Trunc(number) == number
}

func schemaNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func TestRestrictedJSONSchemaValidatorRejectsNullAndUnknownProperties(t *testing.T) {
	schema := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{"name": map[string]any{"type": "string", "minLength": 1}},
		"required":   []string{"name"},
	}
	for name, value := range map[string]any{
		"null":    map[string]any{"name": nil},
		"unknown": map[string]any{"name": "ok", "extra": true},
		"missing": map[string]any{},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateJSONSchema(schema, value); err == nil {
				t.Fatal("validator accepted invalid response")
			}
		})
	}
}

type rawRPCResult struct {
	IsError           bool           `json:"isError"`
	StructuredContent map[string]any `json:"structuredContent"`
	Content           []struct {
		Text string `json:"text"`
	} `json:"content"`
}

type rawRPCResponse struct {
	Result rawRPCResult    `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func rawToolSchemas(t *testing.T, c *rawMCPClient) map[string]map[string]any {
	t.Helper()
	resp, body := c.post("tools/list", map[string]any{})
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("tools/list status/content-type %d/%q: %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	var list struct {
		Result struct {
			Tools []struct {
				Name         string         `json:"name"`
				OutputSchema map[string]any `json:"outputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("tools/list JSON: %v", err)
	}
	if len(list.Result.Tools) != 13 {
		t.Fatalf("tools/list returned %d tools, want exactly 13", len(list.Result.Tools))
	}
	result := make(map[string]map[string]any, len(list.Result.Tools))
	for _, tool := range list.Result.Tools {
		if tool.Name == "" || tool.OutputSchema == nil {
			t.Fatalf("tools/list tool lacks name/outputSchema: %#v", tool)
		}
		result[tool.Name] = tool.OutputSchema
	}
	return result
}

func rawCall(t *testing.T, c *rawMCPClient, name string, args map[string]any) rawRPCResult {
	t.Helper()
	resp, body := c.post("tools/call", map[string]any{"name": name, "arguments": args})
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("%s status/content-type %d/%q: %s", name, resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	var rpc rawRPCResponse
	if err := json.Unmarshal(body, &rpc); err != nil {
		t.Fatalf("%s response is not JSON-RPC: %v: %s", name, err, body)
	}
	if len(rpc.Error) != 0 && string(rpc.Error) != "null" {
		t.Fatalf("%s returned JSON-RPC error instead of typed result: %s", name, body)
	}
	return rpc.Result
}

func assertRawPayload(t *testing.T, name string, schema map[string]any, result rawRPCResult, wantError bool) map[string]any {
	t.Helper()
	if result.IsError != wantError {
		t.Fatalf("%s IsError=%v, want %v; content=%#v", name, result.IsError, wantError, result.StructuredContent)
	}
	if err := validateJSONSchema(schema, result.StructuredContent); err != nil {
		t.Fatalf("%s structuredContent violates advertised output schema: %v\ncontent=%#v", name, err, result.StructuredContent)
	}
	auditID, _ := result.StructuredContent["nl_audit_id"].(string)
	if auditID == "" || len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, auditID) {
		t.Fatalf("%s lacks audit/correlation in structured content and first text block: %#v", name, result)
	}
	return result.StructuredContent
}

type rawTokenVerifier struct {
	principals map[string]identity.Principal
}

func (v rawTokenVerifier) Verify(_ context.Context, token string) (identity.Principal, error) {
	principal, ok := v.principals[token]
	if !ok {
		return identity.Principal{}, fmt.Errorf("unknown test token")
	}
	return principal, nil
}

type rawWave2Fixture struct {
	env     testEnv
	db      *sql.DB
	store   *assurance.Store
	storage assurance.EvidenceStorage
	state   *rawProviderState
}

type rawProviderState struct {
	typedReads atomic.Int32
}

func newRawWave2Fixture(t *testing.T) rawWave2Fixture {
	t.Helper()
	state := &rawProviderState{}
	env := newTestEnv(t, rawWave2WorkivaMock(t, state))
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store, err := assurance.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	sharedAudit, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	store.SetAuditLog(sharedAudit)
	storage := assurance.NewMemoryEvidenceStorage()
	store.SetEvidenceStorage(storage)
	env.deps.Audit = sharedAudit
	env.deps.Assurance = store
	env.deps.Cfg.AssuranceEnabled = true
	env.deps.Cfg.AssuranceLegacyAPIKeyProfile = false
	env.deps.Client = workivaprovider.NewRouter(env.deps.Client)
	seedField(t, env)
	provisionRawWave2Bundle(t, store)
	return rawWave2Fixture{env: env, db: db, store: store, storage: storage, state: state}
}

func rawWave2WorkivaMock(t *testing.T, state *rawProviderState) http.HandlerFunc {
	t.Helper()
	const mapperBody = `{"data":{"range":{"startRow":1,"startColumn":0,"stopRow":1,"stopColumn":1},"cells":[[{"value":"Scope 2 Energy (kWh)"},{"value":"1234"}]]}}`
	return tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body string
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/spreadsheets":
			body = `{"data":[{"id":"sp-1","name":"VSME 2026"}]}`
		case r.Method == http.MethodGet && r.URL.Path == "/spreadsheets/sp-1/sheets":
			body = `{"data":[{"id":"sh-1","name":"B3 Energy","index":0}]}`
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sheetdata") && strings.Contains(r.URL.Query().Get("$fields"), "calculatedValue"):
			value := 10
			if state.typedReads.Add(1) > 1 {
				value = 12
			}
			body = fmt.Sprintf(`{"data":{"range":{"startRow":2,"startColumn":1,"stopRow":2,"stopColumn":1},"cells":[[{"value":%d}]]}}`, value)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sheetdata"):
			body = mapperBody
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/update"):
			w.WriteHeader(http.StatusAccepted)
			body = `{"operationLocation":"/operations/op-1"}`
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/operations/"):
			body = `{"id":"op-1","status":"completed","resourceUrl":"/spreadsheets/sp-1/sheets/sh-1"}`
		default:
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, body)
	})
}

func provisionRawWave2Bundle(t *testing.T, store *assurance.Store) {
	t.Helper()
	const tenant = identity.LegacyTenantID
	rules := assurance.RuleSet{RuleSetID: "rules-energy", Revision: 1, Status: "active", Rules: []assurance.RuleDefinition{{RuleID: "required-energy", Revision: 1, Order: 1, Kind: assurance.RuleRequired, FieldIDs: []string{"scope2-kwh"}}}}
	policy := assurance.MaterialityPolicy{PolicyID: "mat-energy", Revision: 1, Status: "active", AbsoluteThreshold: "1", RelativeThreshold: "0.05", Direction: "absolute_or_relative", ZeroBaseline: "not_comparable", MissingBehavior: "not_comparable", TypeChangeBehavior: "not_comparable", Rounding: "half_even"}
	profile := assurance.ExportProfile{ProfileID: "profile-evidence", Revision: 1, Status: "active", PermittedSubjects: []string{"snapshot", "validation_run", "comparison"}, RedactionProfile: "standard", RetentionClass: "long_term", MaxRows: 100000, MaxBytes: 64 << 20, DeliveryPolicy: "opaque_reference"}
	bundle := assurance.Bundle{
		SchemaVersion: 1, BundleID: "bundle-raw-wave2", BundleVersion: 1, TenantID: tenant,
		RuleSets: []assurance.RuleSet{rules}, MaterialityPolicies: []assurance.MaterialityPolicy{policy}, ExportProfiles: []assurance.ExportProfile{profile},
		RetentionPolicies: []assurance.RetentionPolicy{{TenantID: tenant, Revision: 1, RetentionClass: "standard", DurationSeconds: 3600, Status: "active"}, {TenantID: tenant, Revision: 1, RetentionClass: "long_term", DurationSeconds: 86400, Status: "active"}},
		Reports: []assurance.ReportRevision{
			{ReportID: "energy-report", Revision: 1, Name: "Energy", Owner: "owner", Status: "active", RetentionClass: "standard", ResourcePolicyHash: strings.Repeat("a", 64), RuleSetID: rules.RuleSetID, MaterialityPolicyID: policy.PolicyID, ExportProfiles: []string{profile.ProfileID}, Periods: []assurance.Period{{Key: "2026-Q2", Label: "Q2 2026", Start: "2026-04-01", End: "2026-06-30"}, {Key: "2026-Q3", Label: "Q3 2026", Start: "2026-07-01", End: "2026-09-30"}}, Fields: []assurance.FieldDefinition{{FieldID: "scope2-kwh", ResourceID: "resource-1", ExternalResourceID: "sp-1", SubresourceID: "sh-1", Locator: "B3", Kind: assurance.ValueNumber, Unit: "kWh", Scale: "ones", Required: true, Order: 1}}},
			{ReportID: "relationship-reference-report", Revision: 1, Name: "Relationship reference", Owner: "owner", Status: "active", RetentionClass: "standard", ResourcePolicyHash: strings.Repeat("a", 64), Periods: []assurance.Period{{Key: "2026-Q3", Label: "Q3 2026", Start: "2026-07-01", End: "2026-09-30"}}, Fields: []assurance.FieldDefinition{{FieldID: "relationship-resource-2", ResourceID: "resource-2", ExternalResourceID: "sp-2", SubresourceID: "sh-2", Locator: "A1", Kind: assurance.ValueText, Required: true, Order: 1}}},
		},
		RelationshipAllowlist: []assurance.RelationshipAllowlistEntry{
			{EntryID: "actor-a-resource-1", Revision: 1, ActorID: "actor-a", Capability: assurance.RelationshipCapabilityRead, ResourceID: "resource-1"},
			{EntryID: "actor-a-resource-2", Revision: 1, ActorID: "actor-a", Capability: assurance.RelationshipCapabilityRead, ResourceID: "resource-2"},
		},
	}
	if err := assurance.ValidateRuleSet(rules); err != nil {
		t.Fatal(err)
	}
	raw, err := assurance.CanonicalJSON(bundle)
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := assurance.ValidateBundle(raw, ed25519.Sign(private, raw), tenant, public)
	if err != nil {
		t.Fatalf("ValidateBundle: %v", err)
	}
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: tenant, ObjectID: "fixture-admin", Permissions: identity.AllPermissions()})
	if err := store.StageBundle(ctx, validated); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestActivation(ctx, bundle.BundleID, bundle.BundleVersion); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(ctx, tenant, public); err != nil {
		t.Fatal(err)
	}
}

func startRawEntraServer(t *testing.T, fixture rawWave2Fixture) (*httptest.Server, map[string]identity.Principal) {
	t.Helper()
	principal := identity.Principal{TenantID: identity.LegacyTenantID, ObjectID: "actor-a", Permissions: identity.AllPermissions()}
	principals := map[string]identity.Principal{"actor-a-token": principal}
	handler, err := mcpserver.New(fixture.env.deps, func() *mcpserver.Registry {
		reg := mcpserver.NewRegistry()
		for _, tool := range All() {
			reg.Register(tool)
		}
		return reg
	}(), &mcpserver.Options{AuthMode: config.AuthModeEntra, TokenVerifier: rawTokenVerifier{principals: principals}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server, principals
}

func initializeRaw(t *testing.T, c *rawMCPClient) {
	t.Helper()
	resp, body := c.post("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "raw-wave2", "version": "test"}})
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("initialize status/content-type %d/%q: %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if resp, _ := c.post("notifications/initialized", nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notifications/initialized status=%d, want 202", resp.StatusCode)
	}
}

func auditLink(t *testing.T, db *sql.DB, auditID string) {
	t.Helper()
	var requestID, correlationID string
	if err := db.QueryRow(`SELECT request_id, correlation_id FROM assurance_audit_links WHERE tenant_id=? AND audit_id=? LIMIT 1`, identity.LegacyTenantID, auditID).Scan(&requestID, &correlationID); err != nil {
		t.Fatalf("audit link for %s: %v", auditID, err)
	}
	if requestID == "" || correlationID == "" {
		t.Fatalf("audit link for %s lacks request/correlation IDs: request=%q correlation=%q", auditID, requestID, correlationID)
	}
}

func auditRecord(t *testing.T, db *sql.DB, auditID string) {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM audit_log WHERE tenant_id=? AND audit_id=?`, identity.LegacyTenantID, auditID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatalf("audit log has no record for %s", auditID)
	}
}

func TestRawRelationshipToolsListSchemaMatchesContract(t *testing.T) {
	fixture := newRawWave2Fixture(t)
	server, _ := startRawEntraServer(t, fixture)
	client := &rawMCPClient{t: t, url: server.URL + "/mcp", authToken: "actor-a-token"}
	initializeRaw(t, client)
	resp, body := client.post("tools/list", map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list status %d: %s", resp.StatusCode, body)
	}
	var listed struct {
		Result struct {
			Tools []struct {
				Name         string         `json:"name"`
				InputSchema  map[string]any `json:"inputSchema"`
				OutputSchema map[string]any `json:"outputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	var relationship *struct {
		InputSchema  map[string]any `json:"inputSchema"`
		OutputSchema map[string]any `json:"outputSchema"`
	}
	for _, tool := range listed.Result.Tools {
		if tool.Name == "workiva_discover_relationships" {
			relationship = &struct {
				InputSchema  map[string]any `json:"inputSchema"`
				OutputSchema map[string]any `json:"outputSchema"`
			}{tool.InputSchema, tool.OutputSchema}
		}
	}
	if relationship == nil {
		t.Fatal("relationship tool absent from tools/list")
	}
	for label, schema := range map[string]map[string]any{"input": relationship.InputSchema, "output": relationship.OutputSchema} {
		if err := assertSchemaRecursivelyClosedAndBounded("$", schema); err != nil {
			t.Errorf("%s schema: %v", label, err)
		}
		fixtureBytes, err := os.ReadFile("testdata/mcp-schemas/workiva_discover_relationships." + label + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var expected map[string]any
		if err := json.Unmarshal(fixtureBytes, &expected); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(schema, expected) {
			t.Errorf("advertised %s schema differs from §4.6 fixture", label)
		}
	}
}

func assertSchemaRecursivelyClosedAndBounded(path string, schema map[string]any) error {
	switch schema["type"] {
	case "object":
		if schema["additionalProperties"] != false {
			return fmt.Errorf("%s object is not closed", path)
		}
		props, _ := schema["properties"].(map[string]any)
		for name, raw := range props {
			child, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("%s.%s has invalid schema", path, name)
			}
			if err := assertSchemaRecursivelyClosedAndBounded(path+"."+name, child); err != nil {
				return err
			}
		}
	case "array":
		if _, ok := schema["maxItems"]; !ok {
			return fmt.Errorf("%s array has no maxItems", path)
		}
		if child, ok := schema["items"].(map[string]any); ok {
			return assertSchemaRecursivelyClosedAndBounded(path+"[]", child)
		}
	}
	return nil
}

func TestRawRelationshipCallRejectsAllowlistMutationInputWithoutWrites(t *testing.T) {
	fixture := newRawWave2Fixture(t)
	graph, err := relationships.NewStore(fixture.db)
	if err != nil {
		t.Fatal(err)
	}
	fixture.env.deps.Relationships = graph
	server, _ := startRawEntraServer(t, fixture)
	client := &rawMCPClient{t: t, url: server.URL + "/mcp", authToken: "actor-a-token"}
	initializeRaw(t, client)
	var before int
	if err := fixture.db.QueryRow(`SELECT count(*) FROM assurance_relationship_allowlist_revisions`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	resp, body := client.post("tools/call", map[string]any{"name": "workiva_discover_relationships", "arguments": map[string]any{
		"operation": "lineage", "root": map[string]any{"resource_id": "resource-1"}, "idempotency_key": "no-mutation",
		"relationship_allowlist": []any{map[string]any{"actor_id": "actor-a", "resource_id": "resource-3"}},
	}})
	lowerBody := strings.ToLower(string(body))
	if resp.StatusCode != http.StatusOK || !strings.Contains(lowerBody, "relationship_allowlist") || !strings.Contains(lowerBody, "additional") {
		t.Fatalf("relationship allowlist mutation input was not rejected as an additional property: status=%d body=%s", resp.StatusCode, body)
	}
	var after int
	if err := fixture.db.QueryRow(`SELECT count(*) FROM assurance_relationship_allowlist_revisions`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before || fixture.env.apiCalls.Load() != 0 {
		t.Fatalf("rejected MCP mutation changed state or called provider: before=%d after=%d provider_calls=%d", before, after, fixture.env.apiCalls.Load())
	}
}

func TestRawRelationshipCallSchemasAndDenialAreClosed(t *testing.T) {
	fixture := newRawWave2Fixture(t)
	graph, err := relationships.NewStore(fixture.db)
	if err != nil {
		t.Fatal(err)
	}
	fixture.env.deps.Relationships = graph
	_, err = graph.Refresh(context.Background(), identity.LegacyTenantID, relationships.Scope{RootID: "resource-1"}, relationships.Discovery{Complete: true, EndOfScope: true, Nodes: []relationships.Node{{ResourceID: "resource-1", Kind: "report", ExternalID: "external-1", Provenance: "observed", Confidence: "high"}, {ResourceID: "resource-2", Kind: "spreadsheet", ExternalID: "external-2", Provenance: "observed", Confidence: "high"}}, Edges: []relationships.Edge{{From: "resource-1", To: "resource-2", Relation: "derived_from", Provenance: "observed", Confidence: "high"}}}, "actor-a", "seed-audit")
	if err != nil {
		t.Fatal(err)
	}
	server, _ := startRawEntraServer(t, fixture)
	client := &rawMCPClient{t: t, url: server.URL + "/mcp", authToken: "actor-a-token"}
	initializeRaw(t, client)
	schemas := rawToolSchemas(t, client)
	args := map[string]any{"operation": "lineage", "root": map[string]any{"resource_id": "resource-1"}, "idempotency_key": "relationship-lineage-test"}
	result := rawCall(t, client, "workiva_discover_relationships", args)
	body := assertRawPayload(t, "workiva_discover_relationships", schemas["workiva_discover_relationships"], result, false)
	if body["status"] != "completed" || len(body["nodes"].([]any)) != 2 || len(body["edges"].([]any)) != 1 {
		t.Fatalf("lineage response should contain persisted path, got %#v", body)
	}
	auditRecord(t, fixture.db, body["nl_audit_id"].(string))
	if fixture.env.apiCalls.Load() != 0 {
		t.Fatalf("lineage reached provider: %d calls", fixture.env.apiCalls.Load())
	}
}

func TestRawRelationshipAuthorizationDenialHasArraysAndNoProviderCalls(t *testing.T) {
	fixture := newRawWave2Fixture(t)
	principal := identity.Principal{TenantID: identity.LegacyTenantID, ObjectID: "read-denied", Permissions: []identity.Permission{identity.PermissionAssuranceSnapshot}}
	principals := map[string]identity.Principal{"denied-token": principal}
	reg := mcpserver.NewRegistry()
	for _, tool := range All() {
		reg.Register(tool)
	}
	handler, err := mcpserver.New(fixture.env.deps, reg, &mcpserver.Options{AuthMode: config.AuthModeEntra, TokenVerifier: rawTokenVerifier{principals: principals}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := &rawMCPClient{t: t, url: server.URL + "/mcp", authToken: "denied-token"}
	initializeRaw(t, client)
	resp, body := client.post("tools/call", map[string]any{"name": "workiva_discover_relationships", "arguments": map[string]any{"operation": "lineage", "root": map[string]any{"resource_id": "resource-1"}, "idempotency_key": "denied"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("transport authorization status = %d, want 403: %s", resp.StatusCode, body)
	}
	var denial struct {
		Status string `json:"status"`
		Error  struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &denial); err != nil {
		t.Fatalf("transport authorization body is not JSON: %v: %s", err, body)
	}
	if denial.Status != "denied" || denial.Error.Code != "permission_denied" || denial.Error.Message == "" {
		t.Fatalf("unexpected generic transport denial: %s", body)
	}
	if fixture.env.apiCalls.Load() != 0 {
		t.Fatalf("denied call reached provider: %d calls", fixture.env.apiCalls.Load())
	}
}

func TestRawJSONRPCWave2SuccessFlowValidatesEveryRegisteredOutput(t *testing.T) {
	fixture := newRawWave2Fixture(t)
	server, principals := startRawEntraServer(t, fixture)
	client := &rawMCPClient{t: t, url: server.URL + "/mcp", authToken: "actor-a-token"}
	initializeRaw(t, client)
	schemas := rawToolSchemas(t, client)

	call := func(name string, args map[string]any, wantError bool) map[string]any {
		result := rawCall(t, client, name, args)
		body := assertRawPayload(t, name, schemas[name], result, wantError)
		auditID := body["nl_audit_id"].(string)
		auditRecord(t, fixture.db, auditID)
		return body
	}

	list := call("workiva_list_spreadsheets", map[string]any{}, false)
	var actor string
	if err := fixture.db.QueryRow(`SELECT actor FROM audit_log WHERE tenant_id=? AND audit_id=?`, identity.LegacyTenantID, list["nl_audit_id"].(string)).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	if actor != identity.LegacyTenantID+"/actor-a" {
		t.Fatalf("Entra actor header spoof was accepted: audit actor=%q", actor)
	}
	call("workiva_read_range", map[string]any{"spreadsheet_id": "sp-1", "sheet_id": "sh-1", "range": "B3"}, false)
	call("workiva_search_fields", map[string]any{"query": "scope"}, false)
	call("workiva_get_field", map[string]any{"name": "scope2_energy_kwh"}, false)
	staged := call("workiva_update_field", map[string]any{"name": "scope2_energy_kwh", "value": "9999"}, false)
	token, ok := staged["confirm_token"].(string)
	if !ok || token == "" {
		t.Fatalf("raw update preview lacks confirmation token: %#v", staged)
	}
	call("workiva_update_field", map[string]any{"name": "scope2_energy_kwh", "value": "9999", "confirm_token": token}, false)
	call("workiva_sync_mapping", map[string]any{"spreadsheet_id": "sp-1", "sheet_id": "sh-1"}, false)
	call("workiva_audit_trail", map[string]any{}, false)

	prior := call("workiva_snapshot_report", map[string]any{"report_id": "energy-report", "period": map[string]any{"key": "2026-Q2"}, "consistency": "best_effort", "idempotency_key": "raw-prior"}, false)
	if prior["status"] != "completed" || prior["snapshot_id"] == "" {
		t.Fatalf("prior snapshot response = %#v", prior)
	}
	currentArgs := map[string]any{"report_id": "energy-report", "period": map[string]any{"key": "2026-Q3"}, "consistency": "best_effort", "idempotency_key": "raw-current"}
	current := call("workiva_snapshot_report", currentArgs, false)
	if current["status"] != "completed" || current["snapshot_id"] == "" {
		t.Fatalf("current snapshot response = %#v", current)
	}
	validateArgs := map[string]any{"snapshot_id": current["snapshot_id"], "rule_set_id": "rules-energy", "idempotency_key": "raw-validation"}
	validation := call("workiva_validate_report", validateArgs, false)
	if validation["status"] != "passed" {
		t.Fatalf("validation response = %#v", validation)
	}
	compareArgs := map[string]any{"current_snapshot_id": current["snapshot_id"], "prior_snapshot_id": prior["snapshot_id"], "materiality_policy_id": "mat-energy", "idempotency_key": "raw-comparison"}
	comparison := call("workiva_compare_periods", compareArgs, false)
	if comparison["status"] != "completed" || comparison["comparison_id"] == "" {
		t.Fatalf("comparison response = %#v", comparison)
	}
	export := call("workiva_export_evidence", map[string]any{"subject_kind": "comparison", "subject_id": comparison["comparison_id"], "format": "json", "redaction_profile": "standard", "include_audit_chain": false, "retention_class": "long_term", "idempotency_key": "raw-export"}, false)
	if export["status"] != "completed" || export["package_hash"] == "" {
		t.Fatalf("evidence response = %#v", export)
	}
	artifacts, ok := export["artifacts"].([]any)
	if !ok || len(artifacts) != 2 {
		t.Fatalf("evidence artifacts = %#v, want subject and manifest", export["artifacts"])
	}
	for _, item := range artifacts {
		artifact := item.(map[string]any)
		data, err := fixture.storage.Read(context.Background(), artifact["storage_ref"].(string))
		if err != nil {
			t.Fatalf("storage read-back %s: %v", artifact["name"], err)
		}
		digest := assurance.HashBytes(data)
		want := hex.EncodeToString(digest[:])
		if artifact["sha256"] != want || int(artifact["byte_count"].(float64)) != len(data) {
			t.Fatalf("storage hash/read-back mismatch for %#v", artifact)
		}
	}
	for _, id := range []any{prior["nl_audit_id"], current["nl_audit_id"], validation["nl_audit_id"], comparison["nl_audit_id"], export["nl_audit_id"]} {
		auditLink(t, fixture.db, id.(string))
	}

	replay := call("workiva_snapshot_report", currentArgs, false)
	if replay["status"] != "idempotency_replay" || replay["snapshot_id"] != current["snapshot_id"] {
		t.Fatalf("snapshot replay = %#v", replay)
	}
	principals["actor-b-token"] = identity.Principal{TenantID: identity.LegacyTenantID, ObjectID: "actor-b", Permissions: identity.AllPermissions()}
	clientB := &rawMCPClient{t: t, url: server.URL + "/mcp", authToken: "actor-b-token", actor: "spoofed-actor@example.com"}
	initializeRaw(t, clientB)
	schemasB := rawToolSchemas(t, clientB)
	actorBResult := rawCall(t, clientB, "workiva_snapshot_report", currentArgs)
	actorB := assertRawPayload(t, "workiva_snapshot_report/actor-b", schemasB["workiva_snapshot_report"], actorBResult, false)
	if actorB["status"] != "completed" || actorB["snapshot_id"] == current["snapshot_id"] {
		t.Fatalf("Actor B replay isolation failed: %#v", actorB)
	}
	var actorBLog string
	if err := fixture.db.QueryRow(`SELECT actor FROM audit_log WHERE tenant_id=? AND audit_id=?`, identity.LegacyTenantID, actorB["nl_audit_id"].(string)).Scan(&actorBLog); err != nil {
		t.Fatal(err)
	}
	if actorBLog != identity.LegacyTenantID+"/actor-b" {
		t.Fatalf("Actor B header spoof was accepted: audit actor=%q", actorBLog)
	}
	conflict := call("workiva_snapshot_report", map[string]any{"report_id": "energy-report", "period": map[string]any{"key": "2026-Q2"}, "consistency": "best_effort", "idempotency_key": "raw-current"}, true)
	if conflict["status"] != "idempotency_conflict" || conflict["error"].(map[string]any)["code"] != "idempotency_conflict" {
		t.Fatalf("idempotency conflict = %#v", conflict)
	}

	busyKey := "raw-busy"
	period := assurance.Period{Key: "2026-Q2", Label: "Q2 2026", Start: "2026-04-01", End: "2026-06-30"}
	canonical, err := assurance.CanonicalJSON(struct {
		ReportID             string                    `json:"report_id"`
		Period               assurance.Period          `json:"period"`
		FieldIDs             []string                  `json:"field_ids"`
		AllowPartial         bool                      `json:"allow_partial"`
		IncludeRelationships bool                      `json:"include_relationships"`
		Consistency          assurance.ConsistencyMode `json:"consistency"`
		RetentionClass       string                    `json:"retention_class"`
	}{"energy-report", period, []string{"scope2-kwh"}, false, false, assurance.ConsistencyBestEffort, "standard"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: identity.LegacyTenantID, ObjectID: "actor-a", Permissions: identity.AllPermissions()})
	if reservation, err := fixture.store.Reserve(ctx, assurance.ReservationRequest{ActorID: identity.LegacyTenantID + "/actor-a", Tool: "workiva_snapshot_report", Action: "capture", IdempotencyDigest: assurance.DigestIdempotencyKey(busyKey), RequestDigest: assurance.HashBytes(canonical), RetentionClass: "standard"}, time.Now().UTC()); err != nil || reservation.Disposition != assurance.ReservationOwned {
		t.Fatalf("seed in-progress reservation = %#v, %v", reservation, err)
	}
	busy := call("workiva_snapshot_report", map[string]any{"report_id": "energy-report", "period": map[string]any{"key": "2026-Q2"}, "consistency": "best_effort", "idempotency_key": busyKey}, true)
	if busy["status"] != "idempotency_in_progress" || busy["error"].(map[string]any)["code"] != "idempotency_in_progress" {
		t.Fatalf("in-progress response = %#v", busy)
	}

	var profileJSON string
	if err := fixture.db.QueryRow(`SELECT profile_json FROM assurance_export_profiles WHERE tenant_id=? AND profile_id=? AND revision=1`, identity.LegacyTenantID, "profile-evidence").Scan(&profileJSON); err != nil {
		t.Fatalf("export profile lookup: %v", err)
	}
	var profile assurance.ExportProfile
	if err := json.Unmarshal([]byte(profileJSON), &profile); err != nil {
		t.Fatal(err)
	}
	profile.MaxBytes = 1
	profileJSONBytes, err := assurance.CanonicalJSON(profile)
	if err != nil {
		t.Fatal(err)
	}
	profileDigest := assurance.HashBytes(profileJSONBytes)
	if _, err := fixture.db.Exec(`UPDATE assurance_export_profiles SET profile_json=?, content_hash=? WHERE tenant_id=? AND profile_id=? AND revision=1`, string(profileJSONBytes), hex.EncodeToString(profileDigest[:]), identity.LegacyTenantID, "profile-evidence"); err != nil {
		t.Fatal(err)
	}
	tooLarge := call("workiva_export_evidence", map[string]any{"subject_kind": "comparison", "subject_id": comparison["comparison_id"], "format": "json", "redaction_profile": "standard", "include_audit_chain": false, "retention_class": "long_term", "idempotency_key": "raw-too-large"}, false)
	if tooLarge["status"] != "too_large" {
		t.Fatalf("too_large response = %#v", tooLarge)
	}
}
