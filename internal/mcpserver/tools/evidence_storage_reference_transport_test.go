package tools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/identity"
)

// This fixture emits the same opaque reference layout as AzureEvidenceStorage
// without contacting Azure or reading a persisted evidence database.
type azureShapedEvidenceStorage struct {
	mu    sync.RWMutex
	items map[string][]byte
}

func newAzureShapedEvidenceStorage() *azureShapedEvidenceStorage {
	return &azureShapedEvidenceStorage{items: make(map[string][]byte)}
}

func (s *azureShapedEvidenceStorage) Put(ctx context.Context, _ string, _ string, data []byte) (string, error) {
	if !fixtureEvidenceExportAuthorized(ctx) {
		return "", fmt.Errorf("fixture requires trusted evidence-export principal")
	}
	hash := sha256.Sum256(data)
	etagHash := sha256.Sum256([]byte(`"fixture-etag"`))
	tenantHash := sha256.Sum256([]byte(identity.LegacyTenantID))
	environmentHash := sha256.Sum256([]byte("fixture-evidence-environment"))
	payload := strings.Join([]string{
		hex.EncodeToString(tenantHash[:]), hex.EncodeToString(environmentHash[:]),
		"01234567-89ab-cdef-0123-456789abcdef", hex.EncodeToString(hash[:]), hex.EncodeToString(etagHash[:]),
	}, ".")
	ref := "azev1." + base64.RawURLEncoding.EncodeToString([]byte(payload))
	s.mu.Lock()
	s.items[ref] = append([]byte(nil), data...)
	s.mu.Unlock()
	return ref, nil
}

func (s *azureShapedEvidenceStorage) Read(ctx context.Context, ref string) ([]byte, error) {
	if !fixtureEvidenceExportAuthorized(ctx) {
		return nil, fmt.Errorf("fixture requires trusted evidence-export principal")
	}
	s.mu.RLock()
	data, ok := s.items[ref]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("fixture evidence reference not found")
	}
	return append([]byte(nil), data...), nil
}

func (s *azureShapedEvidenceStorage) Delete(ctx context.Context, ref string) error {
	if !fixtureEvidenceExportAuthorized(ctx) {
		return fmt.Errorf("fixture requires trusted evidence-export principal")
	}
	s.mu.Lock()
	delete(s.items, ref)
	s.mu.Unlock()
	return nil
}

func fixtureEvidenceExportAuthorized(ctx context.Context) bool {
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok || principal.TenantID != identity.LegacyTenantID || principal.ObjectID != "actor-a" {
		return false
	}
	for _, permission := range principal.Permissions {
		if permission == identity.PermissionEvidenceExport {
			return true
		}
	}
	return false
}

func TestExportEvidenceAzureShapedReferencesPassSDKOutputValidation(t *testing.T) {
	fixture := newRawWave2Fixture(t)
	storage := newAzureShapedEvidenceStorage()
	fixture.store.SetEvidenceStorage(storage)
	principal := tenantPrincipal(identity.LegacyTenantID, "actor-a", identity.PermissionAssuranceSnapshot, identity.PermissionEvidenceExport)
	snapshot := callEntraTool(t, fixture.env.deps, SnapshotReport(), principal, map[string]any{
		"report_id": "energy-report", "period": map[string]any{"key": "2026-Q3"},
		"consistency": "best_effort", "idempotency_key": "long-ref-snapshot",
	})
	if snapshot.IsError {
		t.Fatalf("local snapshot setup failed: %#v", snapshot.Content)
	}
	snapshotID, _ := structuredContent(t, snapshot)["snapshot_id"].(string)
	if snapshotID == "" {
		t.Fatalf("snapshot response lacks ID: %#v", snapshot.StructuredContent)
	}
	outputSchema := exportEvidenceOutputSchema()
	for _, format := range []string{"json", "csv"} {
		t.Run(format, func(t *testing.T) {
			result := callEntraTool(t, fixture.env.deps, ExportEvidence(), principal, map[string]any{
				"subject_kind": "snapshot", "subject_id": snapshotID, "format": format,
				"redaction_profile": "standard", "include_audit_chain": false,
				"retention_class": "long_term", "idempotency_key": "long-ref-" + format,
			})
			if result.IsError {
				t.Fatalf("SDK rejected successful evidence export response: %#v", result.Content)
			}
			body := structuredContent(t, result)
			if body["status"] != "completed" {
				t.Fatalf("export did not complete: %#v", body)
			}
			if err := validateJSONSchema(outputSchema, body); err != nil {
				t.Fatalf("emitted SDK structuredContent violates advertised schema: %v", err)
			}
			artifacts, ok := body["artifacts"].([]any)
			if !ok || len(artifacts) == 0 {
				t.Fatalf("export lacks artifact descriptors: %#v", body["artifacts"])
			}
			for _, rawArtifact := range artifacts {
				artifact, ok := rawArtifact.(map[string]any)
				if !ok {
					t.Fatalf("artifact has unexpected type %T", rawArtifact)
				}
				ref, _ := artifact["storage_ref"].(string)
				if !strings.HasPrefix(ref, "azev1.") || len(ref) <= 256 || len(ref) > 512 {
					t.Fatalf("reference length/prefix = %d/%q, want Azure shape within [257,512]", len(ref), ref[:min(len(ref), 16)])
				}
			}
		})
	}
}

func TestExportEvidenceStorageReferenceSchemaBoundIs512AndClosed(t *testing.T) {
	artifactSchema := exportEvidenceOutputSchema()["properties"].(map[string]any)["artifacts"].(map[string]any)["items"].(map[string]any)
	properties := artifactSchema["properties"].(map[string]any)
	refSchema := properties["storage_ref"].(map[string]any)
	if refSchema["minLength"] != 1 || refSchema["maxLength"] != 512 || artifactSchema["additionalProperties"] != false {
		t.Fatalf("reference contract changed unexpectedly: storage_ref=%#v artifact=%#v", refSchema, artifactSchema)
	}
	response := map[string]any{
		"nl_audit_id": "audit", "status": "completed",
		"artifacts": []any{map[string]any{
			"artifact_id": "id", "name": "name", "media_type": "application/json",
			"byte_count": float64(0), "sha256": strings.Repeat("a", 64), "storage_ref": strings.Repeat("x", 512),
		}},
	}
	response["artifacts"].([]any)[0].(map[string]any)["storage_ref"] = "x"
	if err := validateJSONSchema(exportEvidenceOutputSchema(), response); err != nil {
		t.Fatalf("schema rejected minimum-length reference: %v", err)
	}
	response["artifacts"].([]any)[0].(map[string]any)["storage_ref"] = ""
	if err := validateJSONSchema(exportEvidenceOutputSchema(), response); err == nil {
		t.Fatal("schema accepted empty reference")
	}
	response["artifacts"].([]any)[0].(map[string]any)["storage_ref"] = strings.Repeat("x", 512)
	if err := validateJSONSchema(exportEvidenceOutputSchema(), response); err != nil {
		t.Fatalf("schema rejected maximum-length reference: %v", err)
	}
	response["artifacts"].([]any)[0].(map[string]any)["storage_ref"] = strings.Repeat("x", 513)
	if err := validateJSONSchema(exportEvidenceOutputSchema(), response); err == nil {
		t.Fatal("schema accepted reference above Azure adapter's 512-character bound")
	}
	response["artifacts"].([]any)[0].(map[string]any)["storage_ref"] = strings.Repeat("x", 512)
	response["artifacts"].([]any)[0].(map[string]any)["unexpected"] = true
	if err := validateJSONSchema(exportEvidenceOutputSchema(), response); err == nil {
		t.Fatal("schema accepted an unknown artifact property")
	}
}

var _ assurance.EvidenceStorage = (*azureShapedEvidenceStorage)(nil)
