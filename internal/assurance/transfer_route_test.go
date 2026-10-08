package assurance

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/dantalabs/northern-lights/internal/identity"
)

func validTestRoute() TransferRoute {
	return TransferRoute{RouteID: "energy-copy", Revision: 1, SourceMappingID: "mapping-energy", SourceResourceID: "source-file", SourceSheetID: "source-sheet", SourceLocator: "Scope!B2", TargetResourceID: "target-file", TargetSheetID: "target-sheet", TargetLocator: "Results!C4", ConversionPolicyID: "exact", ConversionPolicyVersion: 1, Capabilities: []string{"workiva.write.preview"}}
}

func TestTransferRouteMustBeInSignedBundle(t *testing.T) {
	public, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, sig := signedBundle(t, priv, func(b *Bundle) {
		b.TransferRoutes = []TransferRoute{validTestRoute()}
		b.ConversionPolicies = []ConversionPolicy{{PolicyID: "exact", Revision: 1, Mode: "typed_copy", SourceKind: ValueNumber, TargetKind: ValueNumber}}
		b.ConversionPolicies = []ConversionPolicy{{PolicyID: "exact", Revision: 1, Mode: "typed_copy", SourceKind: ValueNumber, TargetKind: ValueNumber}}
	})
	if _, err := ValidateBundle(raw, sig, testTenant, public); err != nil {
		t.Fatalf("valid signed route rejected: %v", err)
	}
	if _, err := ValidateBundle(raw, []byte("unsigned"), testTenant, public); err == nil {
		t.Fatal("unsigned/ad-hoc route accepted")
	}
	r := validTestRoute()
	r.TargetLocator = "Results!C4:D4"
	if err := validateTransferRoute(r); err == nil {
		t.Fatal("multi-cell target locator accepted")
	}
}

// Privileged bundle signing is exercised through the existing operator test helper in integration tests.
func TestSignedActiveTransferRouteResolvesAndOmissionDeactivates(t *testing.T) {
	store, _ := openTestStore(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: testTenant, ObjectID: "actor", Permissions: []identity.Permission{identity.PermissionWorkivaWritePreview}})
	raw, sig := signedBundle(t, priv, func(b *Bundle) {
		b.TransferRoutes = []TransferRoute{validTestRoute()}
		b.ConversionPolicies = []ConversionPolicy{{PolicyID: "exact", Revision: 1, Mode: "typed_copy", SourceKind: ValueNumber, TargetKind: ValueNumber}}
		b.ConversionPolicies = []ConversionPolicy{{PolicyID: "exact", Revision: 1, Mode: "typed_copy", SourceKind: ValueNumber, TargetKind: ValueNumber}}
	})
	validated, err := ValidateBundle(raw, sig, testTenant, pub)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.StageBundle(ctx, validated); err != nil {
		t.Fatal(err)
	}
	if err = store.RequestActivation(ctx, "bundle-1", 1); err != nil {
		t.Fatal(err)
	}
	if err = store.Bootstrap(ctx, testTenant, pub); err != nil {
		t.Fatal(err)
	}
	got, err := store.ResolveTransferRoute(ctx, "energy-copy")
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceResourceID != "source-file" || got.TargetResourceID != "target-file" || got.TargetSheetID != "target-sheet" || got.TargetLocator != "Results!C4" {
		t.Fatalf("wrong route resolved: %+v", got)
	}
	noCap := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: testTenant, ObjectID: "actor", Permissions: nil})
	if _, err := store.ResolveTransferRoute(noCap, "energy-copy"); err == nil {
		t.Fatal("capability omission granted route")
	}
	raw, sig = signedBundle(t, priv, func(b *Bundle) { b.BundleID = "bundle-2"; b.BundleVersion = 2; b.Reports[0].Revision = 2 })
	validated, err = ValidateBundle(raw, sig, testTenant, pub)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.StageBundle(ctx, validated); err != nil {
		t.Fatal(err)
	}
	if err = store.RequestActivation(ctx, "bundle-2", 2); err != nil {
		t.Fatal(err)
	}
	if err = store.Bootstrap(ctx, testTenant, pub); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ResolveTransferRoute(ctx, "energy-copy"); err == nil {
		t.Fatal("omitted route remained active")
	}
}

func TestSameTransferRouteRevisionCannotChangeContent(t *testing.T) {
	store, _ := openTestStore(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ctx := assuranceContext()
	raw, sig := signedBundle(t, priv, func(b *Bundle) {
		b.TransferRoutes = []TransferRoute{validTestRoute()}
		b.ConversionPolicies = []ConversionPolicy{{PolicyID: "exact", Revision: 1, Mode: "typed_copy", SourceKind: ValueNumber, TargetKind: ValueNumber}}
		b.ConversionPolicies = []ConversionPolicy{{PolicyID: "exact", Revision: 1, Mode: "typed_copy", SourceKind: ValueNumber, TargetKind: ValueNumber}}
	})
	validated, err := ValidateBundle(raw, sig, testTenant, pub)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.StageBundle(ctx, validated); err != nil {
		t.Fatal(err)
	}
	if err = store.RequestActivation(ctx, "bundle-1", 1); err != nil {
		t.Fatal(err)
	}
	if err = store.Bootstrap(ctx, testTenant, pub); err != nil {
		t.Fatal(err)
	}
	raw, sig = signedBundle(t, priv, func(b *Bundle) {
		b.BundleID = "bundle-2"
		b.BundleVersion = 2
		r := validTestRoute()
		r.TargetLocator = "Results!D4"
		b.TransferRoutes = []TransferRoute{r}
		b.ConversionPolicies = []ConversionPolicy{{PolicyID: "exact", Revision: 1, Mode: "typed_copy", SourceKind: ValueNumber, TargetKind: ValueNumber}}
	})
	validated, err = ValidateBundle(raw, sig, testTenant, pub)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.StageBundle(ctx, validated); err == nil {
		t.Fatal("same route revision accepted changed content")
	}
}

func TestTransferRouteResolutionRequiresTrustedSameTenantActor(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: "other-tenant", ObjectID: "actor", Permissions: []identity.Permission{identity.PermissionWorkivaWritePreview}})
	if _, err := store.ResolveTransferRoute(ctx, "route"); err == nil {
		t.Fatal("cross-tenant route access succeeded")
	}
}

func TestConversionPolicySignedActivationExactRevisionAndTenantIsolation(t *testing.T) {
	store, _ := openTestStore(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: testTenant, ObjectID: "actor", Permissions: []identity.Permission{identity.PermissionWorkivaWritePreview}})
	policy := ConversionPolicy{PolicyID: "copy", Revision: 1, Mode: "typed_copy", SourceKind: ValueNumber, TargetKind: ValueNumber}
	raw, sig := signedBundle(t, priv, func(b *Bundle) {
		b.ConversionPolicies = []ConversionPolicy{policy}
		b.TransferRoutes = []TransferRoute{func() TransferRoute { r := validTestRoute(); r.ConversionPolicyID = "copy"; return r }()}
	})
	validated, err := ValidateBundle(raw, sig, testTenant, pub)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.StageBundle(ctx, validated); err != nil {
		t.Fatal(err)
	}
	if err = store.RequestActivation(ctx, "bundle-1", 1); err != nil {
		t.Fatal(err)
	}
	if err = store.Bootstrap(ctx, testTenant, pub); err != nil {
		t.Fatal(err)
	}
	got, err := store.ResolveConversionPolicy(ctx, "copy", 1)
	if err != nil || got.ContentHash == "" {
		t.Fatalf("active policy=%+v err=%v", got, err)
	}
	other := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: "other", ObjectID: "actor"})
	if _, err = store.ResolveConversionPolicy(other, "copy", 1); err == nil {
		t.Fatal("cross-tenant policy resolved")
	}
	if _, err = store.ResolveConversionPolicy(ctx, "copy", 2); err == nil {
		t.Fatal("inexact revision resolved")
	}
}

func TestConversionPolicyRejectsMissingReferenceAndUnsupportedConversion(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	raw, sig := signedBundle(t, priv, func(b *Bundle) { r := validTestRoute(); b.TransferRoutes = []TransferRoute{r} })
	if _, err := ValidateBundle(raw, sig, testTenant, pub); err == nil {
		t.Fatal("route with missing policy accepted")
	}
	raw, sig = signedBundle(t, priv, func(b *Bundle) {
		b.ConversionPolicies = []ConversionPolicy{{PolicyID: "convert", Revision: 1, Mode: "currency_exchange", SourceKind: ValueCurrency, TargetKind: ValueCurrency}}
	})
	if _, err := ValidateBundle(raw, sig, testTenant, pub); err == nil {
		t.Fatal("unsupported arbitrary conversion accepted")
	}
}

func TestTypedCopyRequiresExactKindUnitAndFormulaFreeValue(t *testing.T) {
	p := ConversionPolicy{PolicyID: "copy", Revision: 1, Mode: "typed_copy", SourceKind: ValueNumber, TargetKind: ValueNumber, SourceUnit: "kg", TargetUnit: "kg"}
	v := TypedValue{Kind: ValueNumber, Number: "12.5", Unit: "kg"}
	got, err := p.ConvertTypedValue(v)
	if err != nil || got.Number != "12.5" {
		t.Fatalf("copy=%+v err=%v", got, err)
	}
	for name, bad := range map[string]TypedValue{
		"unit conversion": {Kind: ValueNumber, Number: "12.5", Unit: "g"},
		"kind conversion": {Kind: ValueCurrency, Number: "12.5", Unit: "kg"},
		"formula marker":  {Kind: ValueNumber, Number: "12.5", Unit: "kg", Formula: true},
		"formula text":    {Kind: ValueNumber, Number: "12.5", Unit: "kg", FormulaText: "=1+1"},
		"calculated":      {Kind: ValueNumber, Number: "12.5", Unit: "kg", Calculated: true},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := p.ConvertTypedValue(bad); err == nil {
				t.Fatal("unsupported value converted")
			}
		})
	}
}
