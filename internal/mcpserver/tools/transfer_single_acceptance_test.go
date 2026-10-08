package tools

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/transfer"
	"github.com/dantalabs/northern-lights/internal/workiva"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

type singleTransferProvider struct {
	posts   atomic.Int32
	reads   atomic.Int32
	waitErr error
}

func (*singleTransferProvider) ListSpreadsheets(context.Context) ([]workiva.Spreadsheet, error) {
	return nil, nil
}
func (*singleTransferProvider) ListSheets(context.Context, string) ([]workiva.Sheet, error) {
	return nil, nil
}
func (*singleTransferProvider) GetSheetData(context.Context, string, string, string, []string) (*workiva.SheetData, error) {
	return nil, errors.New("unused")
}
func (p *singleTransferProvider) ReadUncached(_ context.Context, r assurance.SourceRequest) (assurance.ProviderRead, error) {
	p.reads.Add(1)
	n := json.Number("1")
	if r.ExternalResourceID == "dst" {
		n = "0"
		if p.posts.Load() > 0 {
			n = "1"
		}
	}
	return assurance.ProviderRead{Value: assurance.ProviderValue{Value: n}, CacheBypassed: true}, nil
}
func (p *singleTransferProvider) UpdateSheetWithRetryAfter(_ context.Context, resource, sheet string, update workiva.SheetUpdate) (string, time.Duration, error) {
	if resource != "dst" || sheet != "dst-sheet" {
		return "", 0, errors.New("wrong target")
	}
	p.posts.Add(1)
	return "https://provider.invalid/operations/1", 0, nil
}

func (p *singleTransferProvider) WaitOperationWithInitialRetryAfter(context.Context, string, time.Duration) (string, error) {
	if p.waitErr != nil {
		return "", p.waitErr
	}
	return "completed", nil
}

func singleTransferFixture(t *testing.T) (rawWave2Fixture, *singleTransferProvider, *httptest.Server, ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	f := newRawWave2Fixture(t)
	provider := &singleTransferProvider{}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bundle := assurance.Bundle{SchemaVersion: 1, BundleID: "single-transfer-test", BundleVersion: 2, TenantID: identity.LegacyTenantID,
		Reports:            []assurance.ReportRevision{{ReportID: "transfer-reference", Revision: 1, Name: "Transfer reference", Owner: "fixture", Status: "active", RetentionClass: "standard", ResourcePolicyHash: strings.Repeat("a", 64), Periods: []assurance.Period{{Key: "2026-Q3", Label: "Q3", Start: "2026-07-01", End: "2026-09-30"}}, Fields: []assurance.FieldDefinition{{FieldID: "transfer-field", ResourceID: "resource-transfer", ExternalResourceID: "src", SubresourceID: "src-sheet", Locator: "Scope!B2", Kind: assurance.ValueNumber, Required: true, Order: 1}}}},
		TransferRoutes:     []assurance.TransferRoute{{RouteID: "route-approved", Revision: 1, SourceMappingID: "mapping-approved", SourceResourceID: "src", SourceSheetID: "src-sheet", SourceLocator: "Scope!B2", TargetResourceID: "dst", TargetSheetID: "dst-sheet", TargetLocator: "Results!C4", ConversionPolicyID: "exact", ConversionPolicyVersion: 1, Capabilities: []string{"workiva.write.preview"}, ExportProfiles: []assurance.ExportProfileReference{{ProfileID: "transfer-evidence-standard", Revision: 1}}}},
		ConversionPolicies: []assurance.ConversionPolicy{{PolicyID: "exact", Revision: 1, Mode: "typed_copy", SourceKind: assurance.ValueNumber, TargetKind: assurance.ValueNumber}},
		ExportProfiles:     []assurance.ExportProfile{{ProfileID: "transfer-evidence-standard", Revision: 1, Status: "active", PermittedSubjects: []string{"transfer"}, RedactionProfile: "standard", RetentionClass: "long_term", MaxRows: 1000, MaxBytes: 1 << 20, DeliveryPolicy: "opaque_reference"}},
		RetentionPolicies:  []assurance.RetentionPolicy{{Revision: 1, TenantID: identity.LegacyTenantID, RetentionClass: "long_term", DurationSeconds: 86400, Status: "active"}, {Revision: 1, TenantID: identity.LegacyTenantID, RetentionClass: "standard", DurationSeconds: 3600, Status: "active"}}}
	raw, err := assurance.CanonicalJSON(bundle)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := assurance.ValidateBundle(raw, ed25519.Sign(private, raw), identity.LegacyTenantID, public)
	if err != nil {
		t.Fatal(err)
	}
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: identity.LegacyTenantID, ObjectID: "fixture-admin", Permissions: identity.AllPermissions()})
	if err = f.store.StageBundle(ctx, validated); err != nil {
		t.Fatal(err)
	}
	if err = f.store.RequestActivation(ctx, bundle.BundleID, bundle.BundleVersion); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Bootstrap(ctx, identity.LegacyTenantID, public); err != nil {
		t.Fatal(err)
	}
	ts, err := transfer.NewWithDB(f.db)
	if err != nil {
		t.Fatal(err)
	}
	router, err := workivaprovider.NewRouterChecked(provider)
	if err != nil {
		t.Fatal(err)
	}
	service, err := transfer.NewTestService(ts, f.store, router, &transfer.FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	f.env.deps.Transfer = service
	reg := mcpserver.NewRegistry()
	for _, tool := range All() {
		reg.Register(tool)
	}
	principals := map[string]identity.Principal{"actor-a-token": {TenantID: identity.LegacyTenantID, ObjectID: "actor-a", Permissions: identity.AllPermissions()}, "preview-token": {TenantID: identity.LegacyTenantID, ObjectID: "preview", Permissions: []identity.Permission{identity.PermissionWorkivaWritePreview}}}
	handler, err := mcpserver.New(f.env.deps, reg, &mcpserver.Options{AuthMode: config.AuthModeEntra, TokenVerifier: rawTokenVerifier{principals: principals}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return f, provider, server, private, public
}

func TestInjectedSingleTransferRawSuccessAndDenial(t *testing.T) {
	f, p, srv, _, _ := singleTransferFixture(t)
	c := &rawMCPClient{t: t, url: srv.URL + "/mcp", authToken: "actor-a-token"}
	initializeRaw(t, c)
	schema := rawToolSchemas(t, c)["workiva_transfer_value"]
	stage := map[string]any{"phase": "stage", "source": map[string]any{"mapping_id": "mapping-approved"}, "target": map[string]any{"resource_id": "dst", "locator": "Results!C4"}, "idempotency_key": "stage-key"}
	bad := map[string]any{"phase": "stage", "source": map[string]any{"mapping_id": "mapping-approved"}, "target": map[string]any{"resource_id": "dst", "locator": "Results!C5"}, "idempotency_key": "bad-key"}
	denied := assertRawPayload(t, "route denial", schema, rawCall(t, c, "workiva_transfer_value", bad), false)
	if denied["status"] == "staged" || p.posts.Load() != 0 || p.reads.Load() != 0 {
		t.Fatalf("denial reached provider: %#v", denied)
	}
	staged := assertRawPayload(t, "stage", schema, rawCall(t, c, "workiva_transfer_value", stage), false)
	token, _ := staged["confirmation_token"].(string)
	id, _ := staged["transfer_id"].(string)
	if staged["status"] != "staged" || token == "" || id == "" || p.posts.Load() != 0 {
		t.Fatalf("invalid stage %#v posts=%d", staged, p.posts.Load())
	}
	var storedKey, storedRequest string
	if err := f.db.QueryRow(`SELECT idempotency_digest, request_digest FROM transfer_intents WHERE transfer_id=?`, id).Scan(&storedKey, &storedRequest); err != nil {
		t.Fatal(err)
	}
	validated, _, err := validateTransferContract(stage)
	if err != nil {
		t.Fatal(err)
	}
	wantRequest, err := transferRequestDigest(validated)
	if err != nil {
		t.Fatal(err)
	}
	if storedKey != ingressDigest("stage-key") || storedRequest != wantRequest {
		t.Fatalf("ingress digest mismatch key=%q request=%q", storedKey, storedRequest)
	}
	reads := p.reads.Load()
	var signedRouteJSON string
	if err := f.db.QueryRow(`SELECT route_json FROM assurance_transfer_route_revisions WHERE route_id='route-approved'`).Scan(&signedRouteJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE assurance_transfer_route_revisions SET route_json='{}' WHERE route_id='route-approved'`); err != nil {
		t.Fatal(err)
	}
	replayed := assertRawPayload(t, "stage replay", schema, rawCall(t, c, "workiva_transfer_value", stage), false)
	if replayed["status"] != "idempotency_replay" || replayed["confirmation_token"] != "" || p.reads.Load() != reads {
		t.Fatalf("stage replay %#v reads=%d", replayed, p.reads.Load())
	}
	if _, err := f.db.Exec(`UPDATE assurance_transfer_route_revisions SET route_json=? WHERE route_id='route-approved'`, signedRouteJSON); err != nil {
		t.Fatal(err)
	}
	preview := &rawMCPClient{t: t, url: srv.URL + "/mcp", authToken: "preview-token"}
	initializeRaw(t, preview)
	args := map[string]any{"phase": "confirm", "transfer_id": id, "confirmation_token": token, "idempotency_key": "confirm-key"}
	resp, _ := preview.post("tools/call", map[string]any{"name": "workiva_transfer_value", "arguments": args})
	if resp.StatusCode == 200 {
		t.Fatal("preview-only principal confirmed")
	}
	if p.posts.Load() != 0 {
		t.Fatal("denied confirm wrote")
	}
	confirmed := assertRawPayload(t, "confirm", schema, rawCall(t, c, "workiva_transfer_value", args), false)
	if confirmed["status"] != "machine_verified_visual_ack_pending" || confirmed["machine_outcome"] != "api_verified" || p.posts.Load() != 1 {
		t.Fatalf("confirm %#v posts=%d", confirmed, p.posts.Load())
	}
	if !strings.Contains(confirmed["operation_reference"].(string), "/operations/") {
		t.Fatal("missing operation")
	}
	auditLink(t, f.db, confirmed["nl_audit_id"].(string))
	var confirmKeyDigest string
	if err := f.db.QueryRow(`SELECT idempotency_digest FROM assurance_idempotency_records WHERE action='confirm' AND entity_reference=?`, id).Scan(&confirmKeyDigest); err != nil {
		t.Fatal(err)
	}
	if confirmKeyDigest != ingressDigest("confirm-key") {
		t.Fatalf("confirm ingress digest=%q", confirmKeyDigest)
	}
	args["confirmation_token"] = "different-token"
	r := assertRawPayload(t, "confirm replay", schema, rawCall(t, c, "workiva_transfer_value", args), false)
	if r["status"] != "idempotency_replay" || p.posts.Load() != 1 {
		t.Fatalf("confirm replay %#v posts=%d", r, p.posts.Load())
	}
}

func TestInjectedSingleTransferEvidenceExportRawJSONAndCSV(t *testing.T) {
	// The fixture explicitly uses memory storage for local acceptance only; this
	// does not claim production-durable artifact storage.
	fixture, _, server, _, _ := singleTransferFixture(t)
	client := &rawMCPClient{t: t, url: server.URL + "/mcp", authToken: "actor-a-token"}
	initializeRaw(t, client)
	schemas := rawToolSchemas(t, client)
	transferSchema := schemas["workiva_transfer_value"]
	stage := assertRawPayload(t, "evidence stage", transferSchema, rawCall(t, client, "workiva_transfer_value", map[string]any{"phase": "stage", "source": map[string]any{"mapping_id": "mapping-approved"}, "target": map[string]any{"resource_id": "dst", "locator": "Results!C4"}, "idempotency_key": "export-stage-key"}), false)
	transferID := stage["transfer_id"].(string)
	token := stage["confirmation_token"].(string)
	confirm := assertRawPayload(t, "evidence confirm", transferSchema, rawCall(t, client, "workiva_transfer_value", map[string]any{"phase": "confirm", "transfer_id": transferID, "confirmation_token": token, "idempotency_key": "export-confirm-key"}), false)
	if confirm["status"] != "machine_verified_visual_ack_pending" {
		t.Fatalf("transfer did not confirm: %#v", confirm)
	}

	for _, format := range []string{"json", "csv"} {
		export := assertRawPayload(t, "transfer evidence "+format, schemas["workiva_export_evidence"], rawCall(t, client, "workiva_export_evidence", map[string]any{"subject_kind": "transfer", "subject_id": transferID, "format": format, "redaction_profile": "standard", "include_audit_chain": false, "retention_class": "long_term", "idempotency_key": "export-transfer-" + format}), false)
		if export["status"] != "completed" || export["package_hash"] == "" {
			t.Fatalf("transfer export %s response=%#v", format, export)
		}
		artifacts, ok := export["artifacts"].([]any)
		if !ok {
			t.Fatalf("artifacts=%#v", export["artifacts"])
		}
		wantCount := 2
		if format == "csv" {
			wantCount = 3
		}
		if len(artifacts) != wantCount {
			t.Fatalf("%s artifacts=%d want=%d: %#v", format, len(artifacts), wantCount, artifacts)
		}
		var subject []byte
		for _, item := range artifacts {
			artifact := item.(map[string]any)
			data, err := fixture.storage.Read(context.Background(), artifact["storage_ref"].(string))
			if err != nil {
				t.Fatalf("artifact readback %s: %v", artifact["name"], err)
			}
			digest := assurance.HashBytes(data)
			if artifact["sha256"] != hex.EncodeToString(digest[:]) || int(artifact["byte_count"].(float64)) != len(data) {
				t.Fatalf("artifact hash/size mismatch: %#v", artifact)
			}
			if artifact["name"] == "subject.json" {
				subject = data
			}
		}
		if len(subject) == 0 {
			t.Fatal("subject JSON artifact missing")
		}
		var decoded map[string]any
		if err := json.Unmarshal(subject, &decoded); err != nil {
			t.Fatalf("subject JSON invalid: %v", err)
		}
		intent, ok := decoded["intent"].(map[string]any)
		if !ok || decoded["transfer_id"] != transferID || intent["route_id"] != "route-approved" {
			t.Fatalf("unexpected exported transfer subject: %s", subject)
		}
		for _, secret := range []string{token, "export-stage-key", "export-confirm-key"} {
			if strings.Contains(string(subject), secret) {
				t.Fatalf("transfer export leaked %q", secret)
			}
		}
		var actor, state, responseStatus string
		if err := fixture.db.QueryRow(`SELECT actor_id,state,response_status FROM assurance_idempotency_records WHERE tenant_id=? AND entity_reference=?`, identity.LegacyTenantID, export["evidence_manifest_id"]).Scan(&actor, &state, &responseStatus); err != nil {
			t.Fatal(err)
		}
		if actor != identity.LegacyTenantID+"/actor-a" {
			t.Fatalf("export reservation actor=%q", actor)
		}
		if state != "sealed" || responseStatus != "completed" {
			t.Fatalf("export reservation was not sealed: state=%q status=%q", state, responseStatus)
		}
		var persistedArtifacts int
		if err := fixture.db.QueryRow(`SELECT count(*) FROM assurance_evidence_artifacts WHERE tenant_id=? AND manifest_id=?`, identity.LegacyTenantID, export["evidence_manifest_id"]).Scan(&persistedArtifacts); err != nil {
			t.Fatal(err)
		}
		if persistedArtifacts != wantCount {
			t.Fatalf("persisted artifacts=%d want=%d", persistedArtifacts, wantCount)
		}
	}
}

type countingExportStorage struct {
	assurance.EvidenceStorage
	puts atomic.Int32
}

func (s *countingExportStorage) Put(ctx context.Context, name, mediaType string, data []byte) (string, error) {
	s.puts.Add(1)
	return s.EvidenceStorage.Put(ctx, name, mediaType, data)
}

func TestTransferExportProfileRevocationBlocksNewKeyButNotSealedReplay(t *testing.T) {
	fixture, _, server, private, public := singleTransferFixture(t)
	storage := &countingExportStorage{EvidenceStorage: fixture.storage}
	fixture.store.SetEvidenceStorage(storage)
	client := &rawMCPClient{t: t, url: server.URL + "/mcp", authToken: "actor-a-token"}
	initializeRaw(t, client)
	schemas := rawToolSchemas(t, client)
	transferSchema := schemas["workiva_transfer_value"]
	stage := assertRawPayload(t, "revocation stage", transferSchema, rawCall(t, client, "workiva_transfer_value", map[string]any{"phase": "stage", "source": map[string]any{"mapping_id": "mapping-approved"}, "target": map[string]any{"resource_id": "dst", "locator": "Results!C4"}, "idempotency_key": "revocation-stage"}), false)
	id := stage["transfer_id"].(string)
	confirm := assertRawPayload(t, "revocation confirm", transferSchema, rawCall(t, client, "workiva_transfer_value", map[string]any{"phase": "confirm", "transfer_id": id, "confirmation_token": stage["confirmation_token"], "idempotency_key": "revocation-confirm"}), false)
	if confirm["status"] != "machine_verified_visual_ack_pending" {
		t.Fatalf("confirm=%#v", confirm)
	}
	jsonArgs := map[string]any{"subject_kind": "transfer", "subject_id": id, "format": "json", "redaction_profile": "standard", "include_audit_chain": false, "retention_class": "long_term", "idempotency_key": "revocation-export-json"}
	first := assertRawPayload(t, "initial transfer export", schemas["workiva_export_evidence"], rawCall(t, client, "workiva_export_evidence", jsonArgs), false)
	if first["status"] != "completed" || storage.puts.Load() != 2 {
		t.Fatalf("initial export=%#v puts=%d", first, storage.puts.Load())
	}

	var activeRaw string
	if err := fixture.db.QueryRow(`SELECT bundle_json FROM assurance_active_bundles WHERE tenant_id=?`, identity.LegacyTenantID).Scan(&activeRaw); err != nil {
		t.Fatal(err)
	}
	var successor assurance.Bundle
	if err := json.Unmarshal([]byte(activeRaw), &successor); err != nil {
		t.Fatal(err)
	}
	successor.BundleID = "single-transfer-profile-revoked"
	successor.BundleVersion++
	// Omitting both route and profile deactivates the complete active set while
	// immutable route/profile history and the staged transfer remain intact.
	successor.TransferRoutes = nil
	successor.ExportProfiles = nil
	successor.ConversionPolicies = nil
	successorRaw, err := assurance.CanonicalJSON(successor)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := assurance.ValidateBundle(successorRaw, ed25519.Sign(private, successorRaw), identity.LegacyTenantID, public)
	if err != nil {
		t.Fatalf("successor bundle: %v", err)
	}
	admin := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: identity.LegacyTenantID, ObjectID: "fixture-admin", Permissions: identity.AllPermissions()})
	if err = fixture.store.StageBundle(admin, validated); err != nil {
		t.Fatal(err)
	}
	if err = fixture.store.RequestActivation(admin, successor.BundleID, successor.BundleVersion); err != nil {
		t.Fatal(err)
	}
	if err = fixture.store.Bootstrap(admin, identity.LegacyTenantID, public); err != nil {
		t.Fatal(err)
	}
	if err = fixture.store.Ready(); err != nil {
		t.Fatalf("successor active bundle not ready: %v", err)
	}
	if _, err = fixture.db.Exec(`SELECT route_json FROM assurance_transfer_route_revisions WHERE tenant_id=? AND route_id='route-approved'`, identity.LegacyTenantID); err != nil {
		t.Fatal(err)
	}

	putsBeforeReplay := storage.puts.Load()
	replay := assertRawPayload(t, "sealed transfer export replay", schemas["workiva_export_evidence"], rawCall(t, client, "workiva_export_evidence", jsonArgs), false)
	if replay["status"] != "idempotency_replay" || replay["evidence_manifest_id"] != first["evidence_manifest_id"] || storage.puts.Load() != putsBeforeReplay {
		t.Fatalf("sealed replay=%#v puts=%d before=%d", replay, storage.puts.Load(), putsBeforeReplay)
	}

	newArgs := map[string]any{"subject_kind": "transfer", "subject_id": id, "format": "json", "redaction_profile": "standard", "include_audit_chain": false, "retention_class": "long_term", "idempotency_key": "revocation-export-new-key"}
	newExport := assertRawPayload(t, "new export after profile revocation", schemas["workiva_export_evidence"], rawCall(t, client, "workiva_export_evidence", newArgs), true)
	errObject, _ := newExport["error"].(map[string]any)
	if errObject["code"] != "export_profile_not_approved" || storage.puts.Load() != putsBeforeReplay {
		t.Fatalf("revoked-profile export=%#v puts=%d want no storage work", newExport, storage.puts.Load())
	}
}

func TestInjectedSingleTransferRawAcknowledgeAndReconcileReplay(t *testing.T) {
	_, provider, srv, _, _ := singleTransferFixture(t)
	client := &rawMCPClient{t: t, url: srv.URL + "/mcp", authToken: "actor-a-token"}
	initializeRaw(t, client)
	schema := rawToolSchemas(t, client)["workiva_transfer_value"]

	stage := map[string]any{"phase": "stage", "source": map[string]any{"mapping_id": "mapping-approved"}, "target": map[string]any{"resource_id": "dst", "locator": "Results!C4"}, "idempotency_key": "ack-stage-key"}
	staged := assertRawPayload(t, "ack stage", schema, rawCall(t, client, "workiva_transfer_value", stage), false)
	transferID := staged["transfer_id"].(string)
	confirm := map[string]any{"phase": "confirm", "transfer_id": transferID, "confirmation_token": staged["confirmation_token"], "idempotency_key": "ack-confirm-key"}
	confirmed := assertRawPayload(t, "ack confirm", schema, rawCall(t, client, "workiva_transfer_value", confirm), false)
	if confirmed["status"] != "machine_verified_visual_ack_pending" {
		t.Fatalf("confirm status=%v", confirmed["status"])
	}
	ack := map[string]any{"phase": "acknowledge", "transfer_id": transferID, "observation": "matches", "refreshed": true, "ui_location": map[string]any{"resource_id": "dst", "locator": "Results!C4"}, "idempotency_key": "ack-key"}
	acknowledged := assertRawPayload(t, "acknowledge", schema, rawCall(t, client, "workiva_transfer_value", ack), false)
	if acknowledged["status"] != "visually_acknowledged" || provider.posts.Load() != 1 {
		t.Fatalf("acknowledgement=%#v posts=%d", acknowledged, provider.posts.Load())
	}
	replayedAck := assertRawPayload(t, "acknowledge replay", schema, rawCall(t, client, "workiva_transfer_value", ack), false)
	if replayedAck["status"] != "idempotency_replay" || provider.posts.Load() != 1 {
		t.Fatalf("acknowledgement replay=%#v posts=%d", replayedAck, provider.posts.Load())
	}

	// A separate ambiguous transfer exercises reconciliation without allowing a
	// second provider mutation. The read-back is intentionally uncached.
	_, provider2, srv2, _, _ := singleTransferFixture(t)
	provider2.waitErr = errors.New("operation status unavailable")
	client2 := &rawMCPClient{t: t, url: srv2.URL + "/mcp", authToken: "actor-a-token"}
	initializeRaw(t, client2)
	stage2 := map[string]any{"phase": "stage", "source": map[string]any{"mapping_id": "mapping-approved"}, "target": map[string]any{"resource_id": "dst", "locator": "Results!C4"}, "idempotency_key": "reconcile-stage-key"}
	staged2 := assertRawPayload(t, "reconcile stage", schema, rawCall(t, client2, "workiva_transfer_value", stage2), false)
	confirm2 := map[string]any{"phase": "confirm", "transfer_id": staged2["transfer_id"], "confirmation_token": staged2["confirmation_token"], "idempotency_key": "reconcile-confirm-key"}
	unknown := assertRawPayload(t, "ambiguous confirm", schema, rawCall(t, client2, "workiva_transfer_value", confirm2), false)
	if unknown["status"] != "reconciliation_required" || provider2.posts.Load() != 1 {
		t.Fatalf("ambiguous confirm=%#v posts=%d", unknown, provider2.posts.Load())
	}
	readBack := map[string]any{"phase": "reconcile", "transfer_id": staged2["transfer_id"], "action": "read_back", "idempotency_key": "read-back-key"}
	readBackResult := assertRawPayload(t, "reconcile read_back", schema, rawCall(t, client2, "workiva_transfer_value", readBack), false)
	if readBackResult["status"] != "reconciliation_required" || readBackResult["readback_cache_bypassed"] != true || provider2.posts.Load() != 1 {
		t.Fatalf("read-back=%#v posts=%d", readBackResult, provider2.posts.Load())
	}
	replayedReadBack := assertRawPayload(t, "reconcile read_back replay", schema, rawCall(t, client2, "workiva_transfer_value", readBack), false)
	if replayedReadBack["status"] != "idempotency_replay" || provider2.posts.Load() != 1 {
		t.Fatalf("read-back replay=%#v posts=%d", replayedReadBack, provider2.posts.Load())
	}
}

func TestInjectedSingleTransferHundredConcurrentConfirmOnePost(t *testing.T) {
	f, p, _, _, _ := singleTransferFixture(t)
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: identity.LegacyTenantID, ObjectID: "actor-a", Permissions: identity.AllPermissions()})
	staged, err := f.env.deps.Transfer.Stage(ctx, transfer.StageRequest{RouteID: "route-approved", IdempotencyDigest: ingressDigest("stage-concurrent"), RequestDigest: ingressDigest("request-concurrent")})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.env.deps.Transfer.Confirm(ctx, transfer.ConfirmRequest{TransferID: staged.TransferID, Token: staged.Token, IdempotencyDigest: ingressDigest("confirm-concurrent"), RequestDigest: ingressDigest("confirm-request")})
		}()
	}
	wg.Wait()
	if p.posts.Load() != 1 {
		t.Fatalf("concurrent confirm posts=%d", p.posts.Load())
	}
}
