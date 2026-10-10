package content

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
)

// projectResolvedProfile is the shared, exact projection from a signature-
// verified active assurance bundle into the content engine. Callers must first
// resolve the bundle for their own required capability.
func projectResolvedProfile(p assurance.DestinationProfile, b assurance.ContentDestinationBindings) (ResolvedProfile, error) {
	if p.ProvenanceRequirement == nil || p.MetadataRequirements == nil || p.ExpectedInterpretation == nil || p.ResourcePolicy == nil || p.ConversionPolicy == nil || p.ProviderContract == nil || b.ProviderContract.Provider == "" || b.ProviderContract.APIVersion == "" || p.RetentionPolicy == nil || p.PreviewTTLSeconds == nil || p.DestinationProfileID == nil || p.Revision == nil || p.ContentHash == nil || p.State == nil || p.ResourceID == nil || p.SheetID == nil || p.Cell == nil || p.Operation == nil || p.AllowedConversions == nil {
		return ResolvedProfile{}, errors.New("content: signed destination profile projection is incomplete")
	}
	expiry, err := time.Parse(time.RFC3339, b.AccessPolicy.ValidUntil)
	if err != nil {
		return ResolvedProfile{}, errors.New("content: signed access policy expiry is invalid")
	}
	interpretation, err := projectInterpretation(*p.ExpectedInterpretation)
	if err != nil {
		return ResolvedProfile{}, err
	}
	ref := func(id string, revision int, hash string) PolicyRef {
		return PolicyRef{PolicyID: id, Revision: revision, ContentHash: hash}
	}
	m := p.MetadataRequirements
	profile := DestinationProfile{TenantID: b.AccessPolicy.TenantID, ID: *p.DestinationProfileID, Revision: *p.Revision, ContentHash: *p.ContentHash, State: *p.State, ResourceID: *p.ResourceID, SheetID: *p.SheetID, Cell: *p.Cell, Operation: *p.Operation, ExpectedInterpretation: interpretation, AllowedConversions: append([]string(nil), (*p.AllowedConversions)...),
		ResourcePolicy: ref(p.ResourcePolicy.PolicyID, p.ResourcePolicy.Revision, p.ResourcePolicy.ContentHash), ConversionPolicy: ref(p.ConversionPolicy.PolicyID, p.ConversionPolicy.Revision, p.ConversionPolicy.ContentHash), ProviderContract: ref(p.ProviderContract.PolicyID, p.ProviderContract.Revision, p.ProviderContract.ContentHash),
		RetentionPolicy: RetentionRef{PolicyRef: ref(p.RetentionPolicy.PolicyID, p.RetentionPolicy.Revision, p.RetentionPolicy.ContentHash), Class: p.RetentionPolicy.Class}, PreviewTTLSeconds: *p.PreviewTTLSeconds, ProvenanceRequirement: *p.ProvenanceRequirement,
		MetadataRequirements: MetadataRequirements{Formula: valueOrEmpty(m.Formula), Protection: valueOrEmpty(m.Protection), Writability: valueOrEmpty(m.Writability), Formatting: valueOrEmpty(m.Formatting), Period: valueOrEmpty(m.Period), Currency: valueOrEmpty(m.Currency), Unit: valueOrEmpty(m.Unit), Scale: valueOrEmpty(m.Scale), PercentBasis: valueOrEmpty(m.PercentBasis), Precision: valueOrEmpty(m.Precision)}, IntentLifetimeSeconds: b.Retention.IntentLifetimeSeconds, IdempotencyLifetimeSeconds: b.Retention.IdempotencyLifetimeSeconds, ProviderFamily: b.ProviderContract.Provider, ProviderAPIVersion: b.ProviderContract.APIVersion}
	return ResolvedProfile{Profile: profile, Binding: PolicyBinding{ActiveBundleHash: b.ActiveBundleHash, ActiveBundleVersion: b.ActiveBundleVersion, AccessPolicyRef: ref(b.AccessPolicy.PolicyID, b.AccessPolicy.Revision, b.AccessPolicy.ContentHash), AccessExpiresAt: expiry}, IdempotencyRetentionSeconds: b.Retention.IdempotencyLifetimeSeconds}, nil
}

func projectInterpretation(src assurance.DestinationInterpretation) (Interpretation, error) {
	if src.ValueKind == nil || src.StoredScale == nil || src.DisplayScale == nil || src.PercentBasis == nil || !src.Period.Set || !src.Currency.Set || !src.Unit.Set || !src.Precision.Set {
		return Interpretation{}, errors.New("content: signed interpretation is incomplete")
	}
	var precision json.RawMessage
	if src.Precision.Value == nil {
		precision = json.RawMessage("null")
	} else {
		raw, err := json.Marshal(*src.Precision.Value)
		if err != nil {
			return Interpretation{}, err
		}
		precision = raw
	}
	return Interpretation{ValueKind: *src.ValueKind, Period: cloneString(src.Period.Value), Currency: cloneString(src.Currency.Value), Unit: cloneString(src.Unit.Value), StoredScale: json.Number(*src.StoredScale), DisplayScale: json.Number(*src.DisplayScale), PercentBasis: *src.PercentBasis, Precision: precision}, nil
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
