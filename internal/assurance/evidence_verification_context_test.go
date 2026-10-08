package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dantalabs/northern-lights/internal/identity"
)

type verificationContextStorage struct {
	base             *memoryEvidenceStorage
	refs             map[string]string
	reads            map[string]int
	readTenants      []string
	requirePrincipal bool
	cancel           context.CancelFunc
	cancelRef        string
	cancelRead       int
	cancelObserved   bool
}

func newVerificationContextStorage() *verificationContextStorage {
	return &verificationContextStorage{base: NewMemoryEvidenceStorage(), refs: make(map[string]string), reads: make(map[string]int), requirePrincipal: true}
}

func (s *verificationContextStorage) Put(ctx context.Context, name, mediaType string, data []byte) (string, error) {
	ref, err := s.base.Put(ctx, name, mediaType, data)
	if err == nil {
		s.refs[ref] = name
	}
	return ref, err
}

func (s *verificationContextStorage) Read(ctx context.Context, ref string) ([]byte, error) {
	principal, ok := identity.PrincipalFromContext(ctx)
	if s.requirePrincipal && (!ok || principal.TenantID != testTenant || principal.ObjectID != "actor-context" || !principal.HasPermission(identity.PermissionEvidenceExport)) {
		return nil, errors.New("trusted tenant evidence context missing")
	}
	if ok {
		s.readTenants = append(s.readTenants, principal.TenantID)
	} else {
		s.readTenants = append(s.readTenants, "")
	}
	s.reads[ref]++
	if s.cancel != nil && s.refs[ref] == s.cancelRef && s.reads[ref] == s.cancelRead {
		s.cancel()
		s.cancelObserved = ctx.Err() != nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.base.Read(ctx, ref)
}

func (s *verificationContextStorage) Delete(ctx context.Context, ref string) error {
	return s.base.Delete(ctx, ref)
}

func evidenceContextFixture(t *testing.T) (*Store, string, context.Context) {
	t.Helper()
	store, db := openTestStore(t)
	snapshotBundle(t, store, []FieldDefinition{{FieldID: "amount", ResourceID: "resource-1", ExternalResourceID: "sp-1", SubresourceID: "sh-1", Locator: "B3", Kind: ValueNumber, Unit: "kWh", Required: true, Order: 1}})
	seedLongTermRetentionForEvidenceTest(t, db)
	reader := &scriptedReader{results: map[string]ProviderRead{"B3": {Value: ProviderValue{Value: json.Number("12")}, CacheBypassed: true}}, errors: map[string]error{}}
	snapshot, err := (SnapshotService{Store: store, Reader: reader}).Capture(assuranceContext(), "actor-1", "context-snapshot", SnapshotRequest{ReportID: "energy-report", Period: Period{Key: "2026-Q3"}, IdempotencyKey: "context-snapshot-key"})
	if err != nil {
		t.Fatalf("capture export subject: %v", err)
	}
	principal := identity.Principal{TenantID: testTenant, ObjectID: "actor-context", Permissions: []identity.Permission{identity.PermissionEvidenceExport}}
	ctx := identity.ContextWithPrincipal(context.Background(), principal)
	return store, snapshot.SnapshotID, ctx
}

func TestExportEvidenceManifestVerificationRetainsTrustedTenantContextForJSONAndCSV(t *testing.T) {
	for _, format := range []string{"json", "csv"} {
		t.Run(format, func(t *testing.T) {
			store, snapshotID, ctx := evidenceContextFixture(t)
			storage := newVerificationContextStorage()
			result, err := store.ExportEvidence(ctx, "ignored", "context-export-"+format, EvidenceRequest{SubjectKind: "snapshot", SubjectID: snapshotID, Format: format, RedactionProfile: RedactionStandard, RetentionClass: "long_term", IdempotencyKey: "context-export-key-" + format}, storage)
			if err != nil {
				t.Fatalf("export %s with trusted tenant context: %v", format, err)
			}
			if result.Status != EvidenceCompleted || len(storage.readTenants) < 4 {
				t.Fatalf("export status=%q verification reads=%d", result.Status, len(storage.readTenants))
			}
			for i, tenant := range storage.readTenants {
				if tenant != testTenant {
					t.Fatalf("storage read %d received tenant %q, want verified tenant", i+1, tenant)
				}
			}
		})
	}
}

func TestExportEvidenceManifestVerificationPropagatesCancellation(t *testing.T) {
	store, snapshotID, baseCtx := evidenceContextFixture(t)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	principal, _ := identity.PrincipalFromContext(baseCtx)
	ctx := identity.ContextWithPrincipal(base, principal)
	storage := newVerificationContextStorage()
	storage.requirePrincipal = false
	storage.cancel = cancel
	storage.cancelRef = "manifest.json"
	storage.cancelRead = 2 // first read is VerifyStorageHash; second is manifest verification
	_, err := store.ExportEvidence(ctx, "ignored", "context-cancel-export", EvidenceRequest{SubjectKind: "snapshot", SubjectID: snapshotID, Format: "json", RedactionProfile: RedactionStandard, RetentionClass: "long_term", IdempotencyKey: "context-cancel-export-key"}, storage)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled manifest verification error=%v, want context.Canceled", err)
	}
	if !storage.cancelObserved {
		t.Fatal("manifest verification read did not receive the canceled export context")
	}
}

func TestExportEvidenceRejectsMissingTenantBeforeStorageAccess(t *testing.T) {
	store, snapshotID, _ := evidenceContextFixture(t)
	storage := newVerificationContextStorage()
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{ObjectID: "actor-context", Permissions: []identity.Permission{identity.PermissionEvidenceExport}})
	_, err := store.ExportEvidence(ctx, "ignored", "missing-tenant-export", EvidenceRequest{SubjectKind: "snapshot", SubjectID: snapshotID, Format: "json", RedactionProfile: RedactionStandard, RetentionClass: "long_term", IdempotencyKey: "missing-tenant-export-key"}, storage)
	if err == nil {
		t.Fatal("export without tenant identity was accepted")
	}
	if len(storage.readTenants) != 0 || len(storage.refs) != 0 {
		t.Fatalf("missing-tenant request reached storage: reads=%d writes=%d", len(storage.readTenants), len(storage.refs))
	}
}
