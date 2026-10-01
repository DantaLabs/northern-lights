package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
)

type countingEvidenceStorage struct {
	inner   assurance.EvidenceStorage
	puts    atomic.Int32
	reads   atomic.Int32
	deletes atomic.Int32
}

func (s *countingEvidenceStorage) Put(ctx context.Context, name, mediaType string, data []byte) (string, error) {
	s.puts.Add(1)
	return s.inner.Put(ctx, name, mediaType, data)
}
func (s *countingEvidenceStorage) Read(ctx context.Context, ref string) ([]byte, error) {
	s.reads.Add(1)
	return s.inner.Read(ctx, ref)
}
func (s *countingEvidenceStorage) Delete(ctx context.Context, ref string) error {
	s.deletes.Add(1)
	return s.inner.Delete(ctx, ref)
}

func allToolsRegistry() *mcpserver.Registry {
	reg := mcpserver.NewRegistry()
	for _, tool := range All() {
		reg.Register(tool)
	}
	return reg
}

func startRawAPIKeyServer(t *testing.T, fixture rawWave2Fixture) *httptest.Server {
	t.Helper()
	handler, err := mcpserver.New(fixture.env.deps, allToolsRegistry(), &mcpserver.Options{APIToken: "api-key"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func TestRawHTTPAPIKeyAssuranceDenialsHappenBeforeReservationStorageOrProvider(t *testing.T) {
	fixture := newRawWave2Fixture(t)
	storage := &countingEvidenceStorage{inner: fixture.storage}
	fixture.store.SetEvidenceStorage(storage)
	server := startRawAPIKeyServer(t, fixture)
	client := &rawMCPClient{t: t, url: server.URL + "/mcp", authToken: "api-key", actor: "caller-asserted@example.com"}
	initializeRaw(t, client)
	schemas := rawToolSchemas(t, client)
	before := countAssuranceReservations(t, fixture)
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"workiva_validate_report", map[string]any{"snapshot_id": "missing", "rule_set_id": "rules-energy", "idempotency_key": "denied-validation"}},
		{"workiva_compare_periods", map[string]any{"current_snapshot_id": "missing", "prior_snapshot_id": "missing", "materiality_policy_id": "mat-energy", "idempotency_key": "denied-comparison"}},
		{"workiva_export_evidence", map[string]any{"subject_kind": "comparison", "subject_id": "missing", "format": "json", "redaction_profile": "standard", "include_audit_chain": false, "retention_class": "long_term", "idempotency_key": "denied-export"}},
	} {
		result := rawCall(t, client, tc.name, tc.args)
		body := assertRawPayload(t, tc.name, schemas[tc.name], result, true)
		if body["status"] != "denied" || body["error"].(map[string]any)["code"] != "strong_identity_required" {
			t.Fatalf("%s denial = %#v", tc.name, body)
		}
	}
	if got := countAssuranceReservations(t, fixture); got != before {
		t.Fatalf("denied assurance calls changed reservation count from %d to %d", before, got)
	}
	if fixture.env.apiCalls.Load() != 0 {
		t.Fatalf("denied assurance calls reached Workiva provider: %d calls", fixture.env.apiCalls.Load())
	}
	if storage.puts.Load() != 0 || storage.reads.Load() != 0 || storage.deletes.Load() != 0 {
		t.Fatalf("API-key evidence denial touched storage: puts=%d reads=%d deletes=%d", storage.puts.Load(), storage.reads.Load(), storage.deletes.Load())
	}
}

func countAssuranceReservations(t *testing.T, fixture rawWave2Fixture) int {
	t.Helper()
	var count int
	if err := fixture.db.QueryRow(`SELECT count(*) FROM assurance_idempotency_records`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestRawHTTPAPIKeyCompatibilityProfileAllowsCallerAssertedLegacyValidationAndComparison(t *testing.T) {
	fixture := newRawWave2Fixture(t)
	fixture.env.deps.Cfg.AssuranceLegacyAPIKeyProfile = true
	server := startRawAPIKeyServer(t, fixture)
	client := &rawMCPClient{t: t, url: server.URL + "/mcp", authToken: "api-key", actor: "caller-asserted@example.com"}
	initializeRaw(t, client)
	schemas := rawToolSchemas(t, client)
	call := func(name string, args map[string]any) map[string]any {
		result := rawCall(t, client, name, args)
		return assertRawPayload(t, name, schemas[name], result, false)
	}
	prior := call("workiva_snapshot_report", map[string]any{"report_id": "energy-report", "period": map[string]any{"key": "2026-Q2"}, "idempotency_key": "legacy-prior"})
	current := call("workiva_snapshot_report", map[string]any{"report_id": "energy-report", "period": map[string]any{"key": "2026-Q3"}, "idempotency_key": "legacy-current"})
	validation := call("workiva_validate_report", map[string]any{"snapshot_id": current["snapshot_id"], "rule_set_id": "rules-energy", "idempotency_key": "legacy-validation"})
	comparison := call("workiva_compare_periods", map[string]any{"current_snapshot_id": current["snapshot_id"], "prior_snapshot_id": prior["snapshot_id"], "materiality_policy_id": "mat-energy", "idempotency_key": "legacy-comparison"})
	if prior["status"] != "completed" || current["status"] != "completed" || validation["status"] != "passed" || comparison["status"] != "completed" {
		t.Fatalf("legacy compatibility statuses: prior=%#v current=%#v validation=%#v comparison=%#v", prior, current, validation, comparison)
	}
	var actor, tenant string
	if err := fixture.db.QueryRow(`SELECT tenant_id, actor FROM audit_log WHERE audit_id=?`, validation["nl_audit_id"].(string)).Scan(&tenant, &actor); err != nil {
		t.Fatal(err)
	}
	if tenant != identity.LegacyTenantID || actor != "caller-asserted@example.com" {
		t.Fatalf("legacy audit attribution is not tenant-isolated/caller-asserted: tenant=%q actor=%q", tenant, actor)
	}
}

func TestRawHTTPEntraCapabilityDenialsAndCredentialFailuresAreBoundedAndSideEffectFree(t *testing.T) {
	fixture := newRawWave2Fixture(t)
	readOnly := identity.Principal{TenantID: identity.LegacyTenantID, ObjectID: "read-only", Permissions: []identity.Permission{identity.PermissionWorkivaRead}}
	missingObjectID := identity.Principal{TenantID: identity.LegacyTenantID, Permissions: []identity.Permission{identity.PermissionEvidenceExport}}
	principals := map[string]identity.Principal{"read-only": readOnly, "missing-object-id": missingObjectID}
	handler, err := mcpserver.New(fixture.env.deps, allToolsRegistry(), &mcpserver.Options{AuthMode: config.AuthModeEntra, TokenVerifier: rawTokenVerifier{principals: principals}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	before := countAssuranceReservations(t, fixture)
	for _, name := range []string{"workiva_snapshot_report", "workiva_validate_report", "workiva_compare_periods", "workiva_export_evidence"} {
		client := &rawMCPClient{t: t, url: server.URL + "/mcp", authToken: "read-only"}
		resp, body := client.post("tools/call", map[string]any{"name": name, "arguments": map[string]any{}})
		if resp.StatusCode != http.StatusForbidden || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("workiva.read must not grant %s: status=%d content-type=%q cache=%q body=%s", name, resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"), body)
		}
		var denial map[string]any
		if err := json.Unmarshal(body, &denial); err != nil {
			t.Fatal(err)
		}
		if denial["status"] != "denied" || denial["nl_audit_id"] == "" || denial["error"].(map[string]any)["nl_audit_id"] != denial["nl_audit_id"] {
			t.Fatalf("bounded capability denial = %#v", denial)
		}
	}
	deniedReservations := countAssuranceReservations(t, fixture)
	if deniedReservations != before {
		t.Fatalf("workiva.read capability denials changed reservation count from %d to %d", before, deniedReservations)
	}
	exactCases := []struct {
		name       string
		permission identity.Permission
		args       map[string]any
	}{
		{"workiva_snapshot_report", identity.PermissionAssuranceSnapshot, map[string]any{"report_id": "missing", "period": map[string]any{"key": "missing"}, "idempotency_key": "exact-snapshot"}},
		{"workiva_validate_report", identity.PermissionAssuranceValidate, map[string]any{"snapshot_id": "missing", "rule_set_id": "missing", "idempotency_key": "exact-validation"}},
		{"workiva_compare_periods", identity.PermissionAssuranceCompare, map[string]any{"current_snapshot_id": "missing", "prior_snapshot_id": "missing", "materiality_policy_id": "missing", "idempotency_key": "exact-comparison"}},
		{"workiva_export_evidence", identity.PermissionEvidenceExport, map[string]any{"subject_kind": "comparison", "subject_id": "missing", "format": "json", "redaction_profile": "standard", "include_audit_chain": false, "retention_class": "long_term", "idempotency_key": "exact-export"}},
	}
	for index, tc := range exactCases {
		token := fmt.Sprintf("exact-%d", index)
		principals[token] = identity.Principal{TenantID: identity.LegacyTenantID, ObjectID: "exact-" + tc.name, Permissions: []identity.Permission{tc.permission}}
		client := &rawMCPClient{t: t, url: server.URL + "/mcp", authToken: token}
		initializeRaw(t, client)
		schemas := rawToolSchemas(t, client)
		result := rawCall(t, client, tc.name, tc.args)
		assertRawPayload(t, tc.name+"/exact-capability", schemas[tc.name], result, true)
	}
	denialsBeforeCredential := countAssuranceReservations(t, fixture)
	missingIdentityClient := &rawMCPClient{t: t, url: server.URL + "/mcp", authToken: "missing-object-id"}
	result := rawCall(t, missingIdentityClient, "workiva_export_evidence", map[string]any{"subject_kind": "comparison", "subject_id": "missing", "format": "json", "redaction_profile": "standard", "include_audit_chain": false, "retention_class": "long_term", "idempotency_key": "missing-identity"})
	// Permission is present, but the service must still reject an untrusted tid/oid.
	body := assertRawPayload(t, "workiva_export_evidence/missing-object-id", exportEvidenceSchemaForTest(t, fixture, server.URL), result, true)
	if body["status"] != "denied" {
		t.Fatalf("missing trusted tid/oid was accepted: %#v", body)
	}
	code := body["error"].(map[string]any)["code"]
	if code != "strong_identity_required" && code != "authorization_denied" {
		t.Fatalf("missing trusted tid/oid denial code=%v: %#v", code, body)
	}
	unknown := &rawMCPClient{t: t, url: server.URL + "/mcp", authToken: "expired-or-malformed"}
	resp, bodyBytes := unknown.post("tools/call", map[string]any{"name": "workiva_snapshot_report", "arguments": map[string]any{}})
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("expired credential denial status/headers=%d/%q/%q body=%s", resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"), bodyBytes)
	}
	if got := countAssuranceReservations(t, fixture); got != denialsBeforeCredential {
		t.Fatalf("capability/credential denials changed reservation count from %d to %d", denialsBeforeCredential, got)
	}
	if fixture.env.apiCalls.Load() != 0 {
		t.Fatalf("capability/credential denials reached provider: %d calls", fixture.env.apiCalls.Load())
	}
}

func exportEvidenceSchemaForTest(t *testing.T, fixture rawWave2Fixture, endpoint string) map[string]any {
	t.Helper()
	client := &rawMCPClient{t: t, url: endpoint + "/mcp", authToken: "missing-object-id"}
	initializeRaw(t, client)
	return rawToolSchemas(t, client)["workiva_export_evidence"]
}
