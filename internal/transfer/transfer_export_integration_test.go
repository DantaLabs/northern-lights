package transfer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

func TestConfirmedTransferExportsThroughTenantScopedProfile(t *testing.T) {
	store, _ := testStore(t)
	var err error
	fixture := &serviceFixture{value: `{"kind":"number","number":"1"}`, op: "https://provider.invalid/operations/export"}
	provider, err := workivaprovider.NewRouterChecked(fixture)
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	route := assurance.TransferRoute{RouteID: "energy-copy", Revision: 1, SourceMappingID: "mapping-energy", SourceResourceID: "src", SourceSheetID: "src-sheet", SourceLocator: "Scope!B2", TargetResourceID: "dst", TargetSheetID: "dst-sheet", TargetLocator: "Results!C4", ConversionPolicyID: "exact", ConversionPolicyVersion: 1, Capabilities: []string{"workiva.write.preview"}, ExportProfiles: []assurance.ExportProfileReference{{ProfileID: "transfer-standard", Revision: 1}}}
	profile := assurance.ExportProfile{ProfileID: "transfer-standard", Revision: 1, Status: "active", PermittedSubjects: []string{"transfer"}, RedactionProfile: "standard", RetentionClass: "long_term", MaxRows: 100, MaxBytes: 1 << 20, DeliveryPolicy: "opaque_reference"}
	bundle := assurance.Bundle{SchemaVersion: 1, BundleID: "bundle-transfer-export", BundleVersion: 1, TenantID: "11111111-1111-1111-1111-111111111111", TransferRoutes: []assurance.TransferRoute{route}, ConversionPolicies: []assurance.ConversionPolicy{{PolicyID: "exact", Revision: 1, Mode: "typed_copy", SourceKind: assurance.ValueNumber, TargetKind: assurance.ValueNumber}}, ExportProfiles: []assurance.ExportProfile{profile}, RetentionPolicies: []assurance.RetentionPolicy{{PolicyID: "retention-long", Revision: 1, TenantID: "11111111-1111-1111-1111-111111111111", RetentionClass: "long_term", DurationSeconds: 3600, Status: "active"}}}
	bundle.Reports = []assurance.ReportRevision{{ReportID: "energy-report", Revision: 1, Name: "Energy", Owner: "operator", Status: "active", RetentionClass: "long_term", ResourcePolicyHash: strings.Repeat("a", 64), Periods: []assurance.Period{{Key: "2026-Q3", Label: "Q3 2026", Start: "2026-07-01", End: "2026-09-30"}}, Fields: []assurance.FieldDefinition{{FieldID: "amount", ResourceID: "src", ExternalResourceID: "src", SubresourceID: "src-sheet", Locator: "Scope!B2", Kind: assurance.ValueNumber, Order: 1}}}}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := assurance.CanonicalJSONBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := assurance.ValidateBundle(raw, ed25519.Sign(private, canonical), bundle.TenantID, public)
	if err != nil {
		t.Fatal(err)
	}
	as, err := assurance.NewWithDB(store.db)
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewWithDB(store.db)
	if err != nil {
		t.Fatal(err)
	}
	as.SetAuditLog(log)
	previewCtx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: bundle.TenantID, ObjectID: "actor-1", Permissions: []identity.Permission{identity.PermissionWorkivaWritePreview}})
	if err = as.StageBundle(previewCtx, validated); err != nil {
		t.Fatal(err)
	}
	if err = as.RequestActivation(previewCtx, bundle.BundleID, bundle.BundleVersion); err != nil {
		t.Fatal(err)
	}
	if err = as.Bootstrap(previewCtx, bundle.TenantID, public); err != nil {
		t.Fatal(err)
	}
	fixture.ctx = previewCtx
	svc, err := NewTestService(store, as, provider, &FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	staged, tenant, actor, permission := stageForConfirm(t, svc, fixture)
	if _, err = svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, IdempotencyDigest: "distinct-confirm-key", RequestDigest: "distinct-confirm-request", Now: time.Now().UTC()}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	exportCtx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: tenant, ObjectID: "actor-1", Permissions: []identity.Permission{identity.PermissionEvidenceExport}})
	storage := assurance.NewMemoryEvidenceStorage()
	response, err := as.ExportEvidence(exportCtx, "caller-controlled-other-actor", "export-audit", assurance.EvidenceRequest{SubjectKind: "transfer", SubjectID: staged.TransferID, Format: "json", RedactionProfile: assurance.RedactionStandard, RetentionClass: "long_term", IdempotencyKey: "export-transfer-1"}, storage)
	if err != nil {
		t.Fatalf("export confirmed transfer: %v", err)
	}
	if response.Status != assurance.EvidenceCompleted {
		t.Fatalf("status=%s", response.Status)
	}
	var exportActor string
	if err := store.db.QueryRow(`SELECT actor_id FROM assurance_idempotency_records WHERE tenant_id=? AND entity_reference=?`, tenant, response.EvidenceManifestID).Scan(&exportActor); err != nil {
		t.Fatal(err)
	}
	if exportActor != tenant+"/actor-1" {
		t.Fatalf("export reservation actor=%q, want trusted identity actor", exportActor)
	}
	var subject []byte
	for _, artifact := range response.Artifacts {
		if artifact.Name == "subject.json" {
			subject, err = storage.Read(exportCtx, artifact.StorageRef)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(subject) == 0 {
		t.Fatal("canonical subject artifact missing")
	}
	for _, want := range []string{`"source"`, `"target"`, `"operation_reference"`, `"uncached_readbacks"`, `"audit_links"`, `"claim_fence_digest"`} {
		if !strings.Contains(string(subject), want) {
			t.Errorf("subject JSON missing %s: %s", want, subject)
		}
	}
	for _, forbidden := range []string{staged.Token, "distinct-confirm-key", "distinct-confirm-request"} {
		if strings.Contains(string(subject), forbidden) {
			t.Errorf("subject JSON leaked %q", forbidden)
		}
	}
	csvStorage := assurance.NewMemoryEvidenceStorage()
	csvResponse, err := as.ExportEvidence(exportCtx, "actor-1", "export-csv-audit", assurance.EvidenceRequest{SubjectKind: "transfer", SubjectID: staged.TransferID, Format: "csv", RedactionProfile: assurance.RedactionStandard, RetentionClass: "long_term", IdempotencyKey: "export-transfer-csv"}, csvStorage)
	if err != nil {
		t.Fatalf("export transfer CSV derivative: %v", err)
	}
	if csvResponse.Manifest.CSV == nil || csvResponse.Manifest.CSV.Authoritative {
		t.Fatalf("CSV metadata=%+v", csvResponse.Manifest.CSV)
	}
	for _, artifact := range csvResponse.Artifacts {
		if artifact.Name == "subject.csv" {
			if _, err := csvStorage.Read(exportCtx, artifact.StorageRef); err != nil {
				t.Fatalf("CSV artifact read-back: %v", err)
			}
		}
	}
}
