package assurance

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dantalabs/northern-lights/internal/identity"
)

func TestPassAApprovedRelationshipResourcesRequiresReadiness(t *testing.T) {
	store, _ := openTestStore(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, sig := signedBundle(t, private, func(bundle *Bundle) {
		bundle.RelationshipAllowlist = []RelationshipAllowlistEntry{{
			EntryID: "actor-1-resource-1", Revision: 1, ActorID: "actor-1",
			Capability: RelationshipCapabilityRead, ResourceID: "resource-1",
		}}
	})
	validated, err := ValidateBundle(raw, sig, testTenant, public)
	if err != nil {
		t.Fatal(err)
	}
	ctx := assuranceContext()
	if err = store.StageBundle(ctx, validated); err != nil {
		t.Fatal(err)
	}
	if err = store.RequestActivation(ctx, validated.Bundle.BundleID, validated.Bundle.BundleVersion); err != nil {
		t.Fatal(err)
	}
	if err = store.Bootstrap(ctx, testTenant, public); err != nil {
		t.Fatal(err)
	}
	store.setReadiness(false, ErrNotReady)
	_, err = store.ApprovedRelationshipResources(ctx, testTenant, "actor-1", []string{"resource-1"})
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("ApprovedRelationshipResources while not ready = %v, want ErrNotReady", err)
	}
}

func TestPassAAllowlistResourceMustResolveToReportResource(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, sig := signedBundle(t, private, func(bundle *Bundle) {
		bundle.RelationshipAllowlist = []RelationshipAllowlistEntry{{
			EntryID: "unknown", Revision: 1, ActorID: "actor-1",
			Capability: RelationshipCapabilityRead, ResourceID: "not-in-any-report",
		}}
	})
	if _, err = ValidateBundle(raw, sig, testTenant, public); err == nil {
		t.Fatal("signed bundle accepted unresolved relationship allowlist resource")
	}
}

func TestPassARelationshipResourceKindIsResourceKind(t *testing.T) {
	store, _ := openTestStore(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, sig := signedBundle(t, private, func(bundle *Bundle) {
		bundle.RelationshipAllowlist = []RelationshipAllowlistEntry{{
			EntryID: "actor-1-resource-1", Revision: 1, ActorID: "actor-1",
			Capability: RelationshipCapabilityRead, ResourceID: "resource-1",
		}}
	})
	validated, err := ValidateBundle(raw, sig, testTenant, public)
	if err != nil {
		t.Fatal(err)
	}
	ctx := assuranceContext()
	if err = store.StageBundle(ctx, validated); err != nil {
		t.Fatal(err)
	}
	if err = store.RequestActivation(ctx, validated.Bundle.BundleID, validated.Bundle.BundleVersion); err != nil {
		t.Fatal(err)
	}
	if err = store.Bootstrap(ctx, testTenant, public); err != nil {
		t.Fatal(err)
	}
	nodes, err := store.ApprovedRelationshipResources(ctx, testTenant, "actor-1", []string{"resource-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Kind != "spreadsheet" {
		t.Fatalf("relationship resource kind = %#v, want spreadsheet resource kind", nodes)
	}
}

func TestPassADuplicateConversionPolicyRevisionRejected(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, sig := signedBundle(t, private, func(bundle *Bundle) {
		policy := ConversionPolicy{PolicyID: "exact", Revision: 1, Mode: "typed_copy", SourceKind: ValueNumber, TargetKind: ValueNumber}
		bundle.ConversionPolicies = []ConversionPolicy{policy, policy}
	})
	if _, err = ValidateBundle(raw, sig, testTenant, public); err == nil {
		t.Fatal("duplicate conversion policy ID/revision accepted")
	}
}

func TestPassATamperedRouteContentHashRejected(t *testing.T) {
	store, _ := openTestStore(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, sig := signedBundle(t, private, func(bundle *Bundle) {
		bundle.TransferRoutes = []TransferRoute{validTestRoute()}
		bundle.ConversionPolicies = []ConversionPolicy{{PolicyID: "exact", Revision: 1, Mode: "typed_copy", SourceKind: ValueNumber, TargetKind: ValueNumber}}
	})
	validated, err := ValidateBundle(raw, sig, testTenant, public)
	if err != nil {
		t.Fatal(err)
	}
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: testTenant, ObjectID: "actor", Permissions: []identity.Permission{identity.PermissionWorkivaWritePreview}})
	if err = store.StageBundle(ctx, validated); err != nil {
		t.Fatal(err)
	}
	if err = store.RequestActivation(ctx, validated.Bundle.BundleID, validated.Bundle.BundleVersion); err != nil {
		t.Fatal(err)
	}
	if err = store.Bootstrap(ctx, testTenant, public); err != nil {
		t.Fatal(err)
	}
	route := validated.Bundle.TransferRoutes[0]
	route.TargetSheetID = "tampered-sheet"
	changed, err := json.Marshal(route)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec(`UPDATE assurance_transfer_route_revisions SET route_json=? WHERE tenant_id=? AND route_id=?`, string(changed), testTenant, route.RouteID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ResolveTransferRoute(ctx, route.RouteID); err == nil {
		t.Fatal("tampered route resolved")
	}
}
