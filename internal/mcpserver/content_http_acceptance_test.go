package mcpserver_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/content"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/mcpserver/tools"
	"github.com/go-jose/go-jose/v4"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"
)

const (
	contentHTTPTestTenant = "11111111-1111-4111-8111-111111111111"
	contentHTTPTestActor  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	contentHTTPForeignID  = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	contentHTTPClientID   = "33333333-3333-4333-8333-333333333333"
	contentHTTPAudience   = "44444444-4444-4444-8444-444444444444"
	contentHTTPIssuer     = "https://login.microsoftonline.com/11111111-1111-4111-8111-111111111111/v2.0"
)

type contentHTTPKeySet struct {
	kid string
	key *rsa.PublicKey
}

func (s contentHTTPKeySet) VerifySignature(_ context.Context, raw string) ([]byte, error) {
	parsed, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil || len(parsed.Signatures) != 1 || parsed.Signatures[0].Header.KeyID != s.kid {
		return nil, errors.New("invalid test signing key")
	}
	return parsed.Verify(s.key)
}

func TestAuthenticatedHTTPByteIntakePersistsSignedPasteAndPDFFixtures(t *testing.T) {
	store, db, log, gate, resolver, verifier, privateKey := newContentHTTPAcceptanceRuntime(t)
	runtime, err := content.NewRuntime(content.RuntimeConfig{Store: store, Resolver: resolver, Audit: log, Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := mcpserver.New(mcpserver.Deps{Audit: log, Assurance: store, Content: runtime, DrainGate: gate, Cfg: &config.Config{}}, contentAcceptanceRegistry(), &mcpserver.Options{AuthMode: config.AuthModeEntra, TokenVerifier: verifier})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	for _, kind := range []string{"paste", "pdf"} {
		t.Run(kind, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "wave4", "fixtures", "byte-intake-"+kind+".json"))
			if err != nil {
				t.Fatal(err)
			}
			projection, err := content.DecodeContentIngest(raw)
			if err != nil {
				t.Fatal(err)
			}
			arguments := make(map[string]any)
			if err := json.Unmarshal(raw, &arguments); err != nil {
				t.Fatal(err)
			}
			result := callContentHTTP(t, server.URL, signContentHTTPToken(t, privateKey, contentHTTPTestActor, "delegated"), arguments)
			if result.IsError {
				t.Fatalf("authenticated %s ingest failed: %#v", kind, result.StructuredContent)
			}
			var output content.PublicPlacementOutput
			encoded, err := json.Marshal(result.StructuredContent)
			if err != nil || json.Unmarshal(encoded, &output) != nil {
				t.Fatalf("decode public ingest response %s: %s (%v)", kind, encoded, err)
			}
			validateContentHTTPOutputSchema(t, result.StructuredContent)
			var projectionOut struct {
				Source struct {
					SourceArtifactID string `json:"source_artifact_id"`
					ContentHash      string `json:"content_hash"`
					ByteLength       int    `json:"byte_length"`
				} `json:"source"`
			}
			publicResult, err := json.Marshal(output.Result)
			if err != nil || json.Unmarshal(publicResult, &projectionOut) != nil {
				t.Fatalf("decode saved source projection %s: %s (%v)", kind, publicResult, err)
			}
			sourceCtx := identity.ContextWithPrincipal(context.Background(), contentHTTPPrincipal(contentHTTPTestActor, true))
			saved, err := store.GetContentSource(sourceCtx, projectionOut.Source.SourceArtifactID)
			if err != nil {
				t.Fatalf("read saved %s through owner principal: %v", kind, err)
			}
			wantHash := sha256.Sum256(projection.Input.SourceBytes)
			if !bytes.Equal(saved.SourceBytes, projection.Input.SourceBytes) || saved.SourceSHA256 != hex.EncodeToString(wantHash[:]) || projectionOut.Source.ContentHash != saved.SourceSHA256 || projectionOut.Source.ByteLength != len(projection.Input.SourceBytes) {
				t.Fatalf("saved %s bytes/hash differ from submitted fixture", kind)
			}
			var encrypted []byte
			if err := db.QueryRow(`SELECT source_bytes FROM assurance_content_sources WHERE tenant_id=? AND source_artifact_id=?`, contentHTTPTestTenant, saved.ID).Scan(&encrypted); err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(encrypted, projection.Input.SourceBytes) || bytes.Contains(encrypted, projection.Input.SourceBytes) {
				t.Fatalf("%s source bytes were not encrypted at rest", kind)
			}
			if output.Phase != "ingest" || output.Status != "ok" || output.NLAuditID == "" {
				t.Fatalf("public ingest result lacks actual phase/status/audit proof: %+v", output)
			}
			var linkedAudit int
			if err := db.QueryRow(`SELECT count(*) FROM audit_log WHERE audit_id=?`, output.NLAuditID).Scan(&linkedAudit); err != nil || linkedAudit != 1 {
				t.Fatalf("sealed ingest audit ID has %d audit rows (err=%v)", linkedAudit, err)
			}
			if kind == "paste" {
				var auditRowsBeforeReplay int
				if err := db.QueryRow(`SELECT count(*) FROM audit_log`).Scan(&auditRowsBeforeReplay); err != nil {
					t.Fatal(err)
				}
				replay := callContentHTTP(t, server.URL, signContentHTTPToken(t, privateKey, contentHTTPTestActor, "delegated"), arguments)
				var replayOutput content.PublicPlacementOutput
				replayJSON, err := json.Marshal(replay.StructuredContent)
				if err != nil || json.Unmarshal(replayJSON, &replayOutput) != nil {
					t.Fatalf("decode sealed replay: %s (%v)", replayJSON, err)
				}
				validateContentHTTPOutputSchema(t, replay.StructuredContent)
				var replaySource struct {
					Source struct {
						SourceArtifactID string `json:"source_artifact_id"`
					} `json:"source"`
				}
				replayProjection, err := json.Marshal(replayOutput.Result)
				if err != nil || json.Unmarshal(replayProjection, &replaySource) != nil {
					t.Fatalf("decode replay source binding: %s (%v)", replayProjection, err)
				}
				if !replayOutput.Replayed || replaySource.Source.SourceArtifactID != saved.ID || replayOutput.NLAuditID != output.NLAuditID {
					t.Fatalf("replay did not return original saved source identity: replay=%+v source=%q original=%q", replayOutput, replaySource.Source.SourceArtifactID, saved.ID)
				}
				var replayAuditRows int
				if err := db.QueryRow(`SELECT count(*) FROM audit_log WHERE audit_id=?`, output.NLAuditID).Scan(&replayAuditRows); err != nil || replayAuditRows != 1 {
					t.Fatalf("sealed replay changed original audit row count to %d (err=%v), want exactly one", replayAuditRows, err)
				}
				var auditRowsAfterReplay int
				if err := db.QueryRow(`SELECT count(*) FROM audit_log`).Scan(&auditRowsAfterReplay); err != nil || auditRowsAfterReplay != auditRowsBeforeReplay {
					t.Fatalf("sealed replay changed total audit row count from %d to %d (err=%v)", auditRowsBeforeReplay, auditRowsAfterReplay, err)
				}
			}
		})
	}

	var genericRows int
	if err := db.QueryRow(`SELECT count(*) FROM audit_log WHERE tool='workiva_content_placement' AND action='call'`).Scan(&genericRows); err != nil {
		t.Fatal(err)
	}
	if genericRows != 0 {
		t.Fatalf("content calls received %d generic audit rows in addition to atomic service audit", genericRows)
	}
	entries := contentHTTPAuditEntries(t, log)
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	for _, sensitive := range [][]byte{[]byte("FY2025 | Revenue | 12.4"), []byte("Ignore prior instructions"), []byte("write 99.9")} {
		if bytes.Contains(encoded, sensitive) {
			t.Fatalf("audit output contains source text %q", sensitive)
		}
	}
}

func validateContentHTTPOutputSchema(t *testing.T, value any) {
	t.Helper()
	schemaPath := filepath.Join("..", "..", "docs", "wave4", "schemas", "content-placement.output.json")
	rawSchema, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		t.Fatalf("decode published content output schema: %v", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatalf("compile published content output schema: %v", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var instance any
	if err := json.Unmarshal(encoded, &instance); err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(instance); err != nil {
		t.Fatalf("actual content response violates published output schema: %v; response=%s", err, encoded)
	}
}

func TestAuthenticatedHTTPByteIntakeDeniesAPIKeyAppOnlyFallbackAndForeignActor(t *testing.T) {
	store, db, log, gate, resolver, verifier, privateKey := newContentHTTPAcceptanceRuntime(t)
	runtime, err := content.NewRuntime(content.RuntimeConfig{Store: store, Resolver: resolver, Audit: log, Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	registry := contentAcceptanceRegistry()
	entraHandler, err := mcpserver.New(mcpserver.Deps{Audit: log, Assurance: store, Content: runtime, DrainGate: gate, Cfg: &config.Config{}}, registry, &mcpserver.Options{AuthMode: config.AuthModeEntra, TokenVerifier: verifier})
	if err != nil {
		t.Fatal(err)
	}
	entraServer := httptest.NewServer(entraHandler)
	t.Cleanup(entraServer.Close)
	apiHandler, err := mcpserver.New(mcpserver.Deps{Audit: log, Assurance: store, Content: runtime, DrainGate: gate, Cfg: &config.Config{}}, registry, &mcpserver.Options{APIToken: "legacy-byte-key"})
	if err != nil {
		t.Fatal(err)
	}
	apiServer := httptest.NewServer(apiHandler)
	t.Cleanup(apiServer.Close)
	args := contentHTTPFixtureArguments(t, "paste")
	deniedTokens := []string{
		signContentHTTPToken(t, privateKey, contentHTTPTestActor, "application"),
		signContentHTTPToken(t, privateKey, "", "delegated"),
	}
	for _, token := range deniedTokens {
		result, err := callContentHTTPExpectError(t, entraServer.URL, token, args)
		if err == nil && (result == nil || !result.IsError) {
			t.Fatal("app-only or fallback JWT passed content authorization")
		}
	}
	if result, err := callContentHTTPExpectError(t, apiServer.URL, "legacy-byte-key", args); err == nil && (result == nil || !result.IsError) {
		t.Fatal("API-key caller passed content authorization")
	}
	var deniedAuditRows int
	if err := db.QueryRow(`SELECT count(*) FROM audit_log WHERE tool='workiva_content_placement' AND action='authorization_denied' AND target='ingest'`).Scan(&deniedAuditRows); err != nil || deniedAuditRows != 3 {
		t.Fatalf("API-key/app-only/fallback denial audit rows=%d (err=%v), want 3 safe phase rows", deniedAuditRows, err)
	}
	deniedAuditJSON, err := json.Marshal(contentHTTPAuditEntries(t, log))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"FY2025 | Revenue | 12.4", "JVBERi0x", "PRIVATE PASTE"} {
		if bytes.Contains(deniedAuditJSON, []byte(secret)) {
			t.Fatalf("auth denial audit leaked request bytes %q", secret)
		}
	}

	foreign := signContentHTTPToken(t, privateKey, contentHTTPForeignID, "delegated")
	result, err := callContentHTTPExpectError(t, entraServer.URL, foreign, args)
	if err != nil {
		t.Fatalf("foreign actor HTTP exchange failed before service denial: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("signed intake policy did not deny foreign actor: %#v", result)
	}
	for _, table := range []string{"assurance_content_sources", "assurance_content_items", "assurance_content_drafts", "assurance_idempotency_records"} {
		var count int
		query := `SELECT count(*) FROM ` + table + ` WHERE tenant_id=?`
		if err := db.QueryRow(query, contentHTTPTestTenant).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("denied intake created %d private rows in %s", count, table)
		}
	}
	duplicateRaw := contentHTTPFixtureRaw(t, "paste")
	duplicateRaw = []byte(strings.Replace(string(duplicateRaw), `"phase": "ingest",`, `"phase": "ingest", "phase": "ingest",`, 1))
	duplicateResult := postContentHTTPRaw(t, entraServer.URL, signContentHTTPToken(t, privateKey, contentHTTPTestActor, "delegated"), duplicateRaw)
	if !duplicateResult.IsError {
		t.Fatalf("actual ContentPlacement adapter accepted duplicate JSON members: %+v", duplicateResult)
	}
	var duplicateOutput map[string]any
	if err := json.Unmarshal(duplicateResult.StructuredContent, &duplicateOutput); err != nil {
		t.Fatal(err)
	}
	if duplicateOutput["status"] == "ok" || duplicateOutput["result"] != nil {
		t.Fatalf("duplicate JSON members were not rejected before result projection: %#v", duplicateOutput)
	}
	for _, table := range []string{"assurance_content_sources", "assurance_content_items", "assurance_content_drafts", "assurance_idempotency_records"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE tenant_id=?`, contentHTTPTestTenant).Scan(&count); err != nil || count != 0 {
			t.Fatalf("duplicate JSON created %d private rows in %s (err=%v)", count, table, err)
		}
	}
}

func contentAcceptanceRegistry() *mcpserver.Registry {
	registry := mcpserver.NewRegistry()
	registry.Register(tools.ContentPlacement())
	return registry
}

func callContentHTTP(t *testing.T, endpoint, token string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := callContentHTTPExpectError(t, endpoint, token, args)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func callContentHTTPExpectError(t *testing.T, endpoint, token string, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "content-http-acceptance", Version: "test"}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: endpoint + "/mcp", HTTPClient: contentBearerHTTPClient(token)}
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := session.Close(); closeErr != nil {
			t.Errorf("close content MCP session: %v", closeErr)
		}
	}()
	return session.CallTool(context.Background(), &mcp.CallToolParams{Name: "workiva_content_placement", Arguments: args})
}

func contentBearerHTTPClient(token string) *http.Client {
	return &http.Client{Transport: contentBearerRoundTripper{token: token, base: http.DefaultTransport}}
}

type contentBearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (rt contentBearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+rt.token)
	return rt.base.RoundTrip(clone)
}

func contentHTTPFixtureArguments(t *testing.T, kind string) map[string]any {
	t.Helper()
	raw := contentHTTPFixtureRaw(t, kind)
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatal(err)
	}
	return args
}

func contentHTTPFixtureRaw(t *testing.T, kind string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "wave4", "fixtures", "byte-intake-"+kind+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type contentHTTPRawResult struct {
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent"`
}

func postContentHTTPRaw(t *testing.T, endpoint, token string, arguments []byte) contentHTTPRawResult {
	t.Helper()
	body := append([]byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"workiva_content_placement","arguments":`), arguments...)
	body = append(body, []byte(`}}`)...)
	request, err := http.NewRequest(http.MethodPost, endpoint+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close raw MCP response: %v", err)
		}
	}()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("raw tools/call status=%d body=%s", response.StatusCode, data)
	}
	var envelope struct {
		Result contentHTTPRawResult `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Result
}

func contentHTTPAuditEntries(t *testing.T, log *audit.Log) []audit.Entry {
	t.Helper()
	var exported bytes.Buffer
	if err := log.Export(&exported); err != nil {
		t.Fatal(err)
	}
	var entries []audit.Entry
	for line := range bytes.Lines(exported.Bytes()) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry audit.Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode audit entry: %v", err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func newContentHTTPAcceptanceRuntime(t *testing.T) (*assurance.Store, *sql.DB, *audit.Log, *assurance.DrainGate, *assurance.ContentPolicyResolver, *identity.EntraVerifier, *rsa.PrivateKey) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	store, err := assurance.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetContentStorageKey("http-acceptance-key", bytes.Repeat([]byte{0x4a}, 32)); err != nil {
		t.Fatal(err)
	}
	publicKey, err := activateHTTPContentPolicy(t, store)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := assurance.NewContentPolicyResolver(store, publicKey, "http-content-access")
	if err != nil {
		t.Fatal(err)
	}
	store.SetAuditLog(log)
	verifier, private, err := newContentHTTPVerifier(t, true)
	if err != nil {
		t.Fatal(err)
	}
	gate := assurance.NewDrainGate()
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close acceptance database: %v", err)
		}
		if err := log.Close(); err != nil {
			t.Errorf("close acceptance audit log: %v", err)
		}
	})
	return store, db, log, gate, resolver, verifier, private
}

func activateHTTPContentPolicy(t *testing.T, store *assurance.Store) (ed25519.PublicKey, error) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	access := assurance.ContentAccessPolicy{PolicyID: "http-content-access", Revision: 1, State: "active", TenantID: contentHTTPTestTenant, ActorObjectID: contentHTTPTestActor, Capabilities: []string{"content.ingest"}, ResourceIDs: []string{"http-resource"}, ValidUntil: "2027-01-01T00:00:00Z"}
	retention := assurance.ContentRetentionPolicy{PolicyID: "http-content-retention", Revision: 1, State: "active", SourceLifetimeSeconds: 86400, DraftLifetimeSeconds: 43200, IntentLifetimeSeconds: 3600, IdempotencyLifetimeSeconds: 7200, RecoveryLifetimeSeconds: 3600}
	bundle := assurance.Bundle{
		SchemaVersion: 1, BundleID: "http-byte-intake-policy", BundleVersion: 1, TenantID: contentHTTPTestTenant,
		Reports:               []assurance.ReportRevision{{ReportID: "http-byte-intake", Revision: 1, Name: "HTTP intake acceptance", Owner: "test", Status: "active", RetentionClass: "standard", ResourcePolicyHash: strings.Repeat("a", 64), Periods: []assurance.Period{{Key: "2026-Q4", Label: "Q4 2026", Start: "2026-10-01", End: "2026-12-31"}}, Fields: []assurance.FieldDefinition{{FieldID: "intake-value", ResourceID: "http-resource", ExternalResourceID: "external-http-resource", SubresourceID: "sheet-1", Locator: "A1", Kind: assurance.ValueText, Required: true, Order: 1}}}},
		ContentAccessPolicies: []assurance.ContentAccessPolicy{access}, ContentRetentionPolicies: []assurance.ContentRetentionPolicy{retention},
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		return nil, err
	}
	canonical, err := assurance.CanonicalJSONBytes(raw)
	if err != nil {
		return nil, err
	}
	validated, err := assurance.ValidateBundle(canonical, ed25519.Sign(privateKey, canonical), contentHTTPTestTenant, publicKey)
	if err != nil {
		return nil, err
	}
	ctx := identity.ContextWithPrincipal(context.Background(), contentHTTPPrincipal(contentHTTPTestActor, true))
	if err := store.StageBundle(ctx, validated); err != nil {
		return nil, err
	}
	if err := store.RequestActivation(ctx, bundle.BundleID, bundle.BundleVersion); err != nil {
		return nil, err
	}
	if err := store.Bootstrap(ctx, contentHTTPTestTenant, publicKey); err != nil {
		return nil, err
	}
	return append(ed25519.PublicKey(nil), publicKey...), nil
}

func newContentHTTPVerifier(t *testing.T, allowFallback bool) (*identity.EntraVerifier, *rsa.PrivateKey, error) {
	t.Helper()
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	verifier, err := identity.NewEntraVerifier(identity.EntraVerifierConfig{
		Authority: contentHTTPIssuer, Audience: contentHTTPAudience, TenantID: contentHTTPTestTenant, AllowSubjectFallback: allowFallback,
		Policy: identity.PermissionPolicy{
			DelegatedScopes:    map[string][]identity.Permission{"nl.content.ingest": {identity.PermissionContentIngest}},
			ApplicationRoles:   map[string][]identity.Permission{"nl.content.ingest": {identity.PermissionContentIngest}},
			ApplicationClients: map[string][]identity.Permission{contentHTTPClientID: {identity.PermissionContentIngest}},
		},
	}, contentHTTPKeySet{kid: "http-intake-kid", key: &private.PublicKey}, func() time.Time { return time.Now().UTC() })
	return verifier, private, err
}

func signContentHTTPToken(t *testing.T, private *rsa.PrivateKey, actorID, kind string) string {
	t.Helper()
	now := time.Now().UTC()
	claims := map[string]any{"iss": contentHTTPIssuer, "aud": contentHTTPAudience, "exp": now.Add(5 * time.Minute).Unix(), "iat": now.Add(-time.Minute).Unix(), "nbf": now.Add(-time.Minute).Unix(), "tid": contentHTTPTestTenant, "sub": "http-subject", "azp": contentHTTPClientID}
	switch kind {
	case "delegated":
		claims["scp"] = "nl.content.ingest"
		if actorID != "" {
			claims["oid"] = actorID
		}
	case "application":
		claims["idtyp"] = "app"
		claims["roles"] = []string{"nl.content.ingest"}
		claims["oid"] = contentHTTPTestActor
	default:
		t.Fatalf("unknown test token kind %q", kind)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: private}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "http-intake-kid"))
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func contentHTTPPrincipal(actorID string, ingest bool) identity.Principal {
	permissions := []identity.Permission{}
	if ingest {
		permissions = append(permissions, identity.PermissionContentIngest)
	}
	return identity.Principal{TenantID: contentHTTPTestTenant, ObjectID: actorID, TokenType: identity.TokenTypeDelegated, Permissions: permissions}
}
