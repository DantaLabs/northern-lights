// Package content builds immutable, read-only plans for Wave 4 content
// placement. It does not resolve private references, persist intents, issue
// confirmation tokens, or submit provider mutations.
package content

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var exactJSONDecimalPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?$`)
var boundedJSONNumberPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)
var currencyCodePattern = regexp.MustCompile(`^[A-Z]{3}$`)
var precisionPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]*)$`)

const (
	workivaPinnedAPIVersion = "2026-01-01"
	literalWritePreserves   = "preserves"
)

// Interpretation is the exact semantic context approved for both the source
// literal and destination. Decimal fields are json.Number to avoid float64
// conversion when these values are decoded from signed profile JSON.
type Interpretation struct {
	ValueKind    string          `json:"value_kind"`
	Period       *string         `json:"period"`
	Currency     *string         `json:"currency"`
	Unit         *string         `json:"unit"`
	StoredScale  json.Number     `json:"stored_scale"`
	DisplayScale json.Number     `json:"display_scale"`
	PercentBasis string          `json:"percent_basis"`
	Precision    json.RawMessage `json:"precision"`
}

// PolicyRef identifies an exact immutable policy revision selected by a
// trusted signed-profile resolver.
type PolicyRef struct {
	PolicyID    string `json:"policy_id"`
	Revision    int    `json:"revision"`
	ContentHash string `json:"content_hash"`
}

type RetentionRef struct {
	PolicyRef
	Class string `json:"class"`
}

type MetadataRequirements struct {
	Formula      string `json:"formula"`
	Protection   string `json:"protection"`
	Writability  string `json:"writability"`
	Formatting   string `json:"formatting"`
	Period       string `json:"period"`
	Currency     string `json:"currency"`
	Unit         string `json:"unit"`
	Scale        string `json:"scale"`
	PercentBasis string `json:"percent_basis"`
	Precision    string `json:"precision"`
}

// Candidate must be populated from an owned, immutable candidate lookup after
// trusted delegated identity and content.stage authorization have succeeded.
// The builder independently checks owner binding and the stored text digest.
type Candidate struct {
	Kind                 string          `json:"kind"`
	CandidateOriginKind  string          `json:"origin_kind,omitempty"`
	ID                   string          `json:"id"`
	Revision             int             `json:"revision"`
	TenantID             string          `json:"tenant_id"`
	ActorID              string          `json:"actor_id"`
	Text                 string          `json:"text"`
	TextSHA256           string          `json:"text_sha256"`
	MetadataSHA256       string          `json:"metadata_sha256"`
	LineageSHA256        string          `json:"lineage_sha256"`
	Metadata             json.RawMessage `json:"candidate_metadata"`
	SourceID             string          `json:"source_id"`
	SourceSHA256         string          `json:"source_sha256"`
	SourceMetadataSHA256 string          `json:"source_metadata_sha256"`
	LineageHashes        []string        `json:"lineage_hashes"`
	Value                json.RawMessage `json:"value"`
	Interpretation       Interpretation  `json:"interpretation"`
	Provenance           json.RawMessage `json:"provenance"`
	CreatedAt            time.Time       `json:"created_at"`
	ExpiresAt            time.Time       `json:"expires_at"`
}

// DestinationProfile is the relevant, already-resolved projection of the
// signed active profile. Construction and signature/active-bundle validation
// belong to the trusted policy resolver, never to an ingress request.
type DestinationProfile struct {
	TenantID                   string               `json:"tenant_id"`
	ID                         string               `json:"destination_profile_id"`
	Revision                   int                  `json:"revision"`
	ContentHash                string               `json:"content_hash"`
	State                      string               `json:"state"`
	ResourceID                 string               `json:"resource_id"`
	SheetID                    string               `json:"sheet_id"`
	Cell                       string               `json:"cell"`
	Operation                  string               `json:"operation"`
	ExpectedInterpretation     Interpretation       `json:"expected_interpretation"`
	AllowedConversions         []string             `json:"allowed_conversions"`
	ResourcePolicy             PolicyRef            `json:"resource_policy"`
	ConversionPolicy           PolicyRef            `json:"conversion_policy"`
	ProviderContract           PolicyRef            `json:"provider_contract"`
	RetentionPolicy            RetentionRef         `json:"retention_policy"`
	PreviewTTLSeconds          int                  `json:"preview_ttl_seconds"`
	ProvenanceRequirement      string               `json:"provenance_requirement"`
	MetadataRequirements       MetadataRequirements `json:"metadata_requirements"`
	IntentLifetimeSeconds      int64                `json:"intent_lifetime_seconds"`
	IdempotencyLifetimeSeconds int64                `json:"idempotency_lifetime_seconds"`
	ProviderFamily             string               `json:"provider_family"`
	ProviderAPIVersion         string               `json:"provider_api_version"`
}

// BuildInput contains resolved records and one uncached provider observation.
// It intentionally has no caller-supplied identity or authorization fields;
// those come from ctx. Candidate/profile resolution must happen only after
// the caller is authorized by the service that invokes this pure builder.
type BuildInput struct {
	Candidate           Candidate
	Profile             DestinationProfile
	RequestedProfileID  string
	RequestedProfileRev int
	ObservationID       string
	ObservedAt          time.Time
	Metadata            workivaprovider.ContentMetadata
	PlacementIntentID   string
	CreatedAt           time.Time
	PolicyBinding       PolicyBinding
}

// PolicyBinding carries the trusted resolver's active signed-bundle and
// access-policy evidence into the immutable preview.
type PolicyBinding struct {
	ActiveBundleHash    string    `json:"active_bundle_hash"`
	ActiveBundleVersion int       `json:"active_bundle_version"`
	AccessPolicyRef     PolicyRef `json:"access_policy_ref"`
	AccessExpiresAt     time.Time `json:"access_expires_at"`
}

// PreviewPlan is a deterministic read-only report. A plan with blockers is
// never a staged intent and must not be persisted or returned as a successful
// stage response. IntendedValue preserves the exact source JSON token bytes.
type PreviewPlan struct {
	PlacementIntentID      string                          `json:"placement_intent_id"`
	ContentHash            string                          `json:"content_hash"`
	ActorBindingID         string                          `json:"actor_binding_id"`
	CandidateKind          string                          `json:"candidate_kind"`
	CandidateOriginKind    string                          `json:"origin_kind,omitempty"`
	CandidateID            string                          `json:"candidate_id"`
	CandidateRevision      int                             `json:"candidate_revision"`
	CandidateHash          string                          `json:"candidate_hash"`
	CandidateMetadataHash  string                          `json:"candidate_metadata_hash"`
	CandidateLineageHash   string                          `json:"candidate_lineage_hash"`
	SourceID               string                          `json:"source_id"`
	SourceHash             string                          `json:"source_hash"`
	SourceMetadataHash     string                          `json:"source_metadata_hash"`
	LineageHashes          []string                        `json:"lineage_hashes"`
	CandidateProvenance    json.RawMessage                 `json:"candidate_provenance"`
	OriginalText           string                          `json:"original_text"`
	OriginalValue          json.RawMessage                 `json:"original_value"`
	OriginalInterpretation Interpretation                  `json:"original_interpretation"`
	IntendedValue          json.RawMessage                 `json:"intended_value"`
	TargetInterpretation   Interpretation                  `json:"target_interpretation"`
	CurrentValue           json.RawMessage                 `json:"current_value"`
	CurrentValueHash       string                          `json:"current_value_content_hash"`
	Destination            DestinationProfile              `json:"destination_profile"`
	PolicyBinding          PolicyBinding                   `json:"policy_binding"`
	Conversion             string                          `json:"conversion"`
	ProviderObservationID  string                          `json:"provider_observation_id"`
	ProviderObservedAt     time.Time                       `json:"provider_observed_at"`
	ProviderMetadata       workivaprovider.ContentMetadata `json:"provider_metadata"`
	CreatedAt              time.Time                       `json:"created_at"`
	ExpiresAt              time.Time                       `json:"expires_at"`
	RetentionPolicy        RetentionRef                    `json:"retention_policy"`
	Blockers               []string                        `json:"blockers"`
}

// BuildPreviewPlan applies only exact typed copy and never reads or writes
// storage. It returns a useful blocked plan when provider facts are unknown.
func BuildPreviewPlan(ctx context.Context, input BuildInput) (PreviewPlan, error) {
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok || principal.TokenType != identity.TokenTypeDelegated || !isCanonicalUUID(principal.TenantID) || !isCanonicalUUID(principal.ObjectID) || principal.UsedSubjectFallback || !principal.HasPermission(identity.PermissionContentStage) {
		return PreviewPlan{}, errors.New("content: trusted delegated content.stage principal required")
	}
	actor := principal.TenantID + "/" + principal.ObjectID
	if input.Candidate.TenantID != principal.TenantID || input.Candidate.ActorID != actor {
		return PreviewPlan{}, errors.New("content: candidate is unavailable")
	}
	if err := validateCandidate(input.Candidate, input.CreatedAt); err != nil {
		return PreviewPlan{}, err
	}
	profile := input.Profile
	if profile.TenantID != principal.TenantID || profile.ID != input.RequestedProfileID || profile.Revision != input.RequestedProfileRev || profile.State != "active" || profile.ID == "" || profile.Revision < 1 || !sha256Pattern.MatchString(profile.ContentHash) {
		return PreviewPlan{}, errors.New("content: requested active profile revision is unavailable")
	}
	if profile.Operation != "single_cell_literal" || profile.Cell == "" || profile.ResourceID == "" || profile.SheetID == "" || profile.ProviderFamily != "workiva" || profile.ProviderAPIVersion != workivaPinnedAPIVersion {
		return PreviewPlan{}, errors.New("content: profile destination is invalid")
	}
	if profile.PreviewTTLSeconds < 1 || profile.PreviewTTLSeconds > 900 || profile.IntentLifetimeSeconds < int64(profile.PreviewTTLSeconds) || profile.IdempotencyLifetimeSeconds < profile.IntentLifetimeSeconds {
		return PreviewPlan{}, errors.New("content: signed profile preview lifetime is invalid")
	}
	if profile.ProvenanceRequirement != "source_or_verified_evidence" && profile.ProvenanceRequirement != "fine_grained_available" {
		return PreviewPlan{}, errors.New("content: signed provenance requirement is invalid")
	}
	requirements := profile.MetadataRequirements
	if requirements.Formula != "provider_verified" || requirements.Protection != "provider_verified" || requirements.Writability != "provider_verified" || requirements.Formatting != "provider_verified" {
		return PreviewPlan{}, errors.New("content: signed critical metadata requirements are invalid")
	}
	for _, requirement := range []string{requirements.Period, requirements.Currency, requirements.Unit, requirements.Scale, requirements.PercentBasis, requirements.Precision} {
		if requirement != "provider_verified" && requirement != "operator_declared_allowed" {
			return PreviewPlan{}, errors.New("content: signed semantic metadata requirement is invalid")
		}
		if requirement == "provider_verified" {
			return PreviewPlan{}, errors.New("content: required provider-verified semantic metadata is unavailable")
		}
	}
	if profile.ProvenanceRequirement == "fine_grained_available" && !candidateHasFineGrainedProvenance(input.Candidate.Provenance) {
		return PreviewPlan{}, errors.New("content: fine-grained source provenance is unavailable")
	}
	if !contains(profile.AllowedConversions, "typed_copy") {
		return PreviewPlan{}, errors.New("content: exact typed_copy is not approved by profile")
	}
	if !validPolicyRef(profile.ResourcePolicy) || !validPolicyRef(profile.ConversionPolicy) || !validPolicyRef(profile.ProviderContract) || !validPolicyRef(profile.RetentionPolicy.PolicyRef) || profile.RetentionPolicy.Class != "workflow" {
		return PreviewPlan{}, errors.New("content: exact signed policy references are required")
	}
	binding := input.PolicyBinding
	if !sha256Pattern.MatchString(binding.ActiveBundleHash) || binding.ActiveBundleVersion < 1 || !validPolicyRef(binding.AccessPolicyRef) || binding.AccessExpiresAt.IsZero() || !binding.AccessExpiresAt.After(input.CreatedAt) {
		return PreviewPlan{}, errors.New("content: active bundle and access policy bindings are required")
	}
	if err := equalInterpretations(input.Candidate.Interpretation, profile.ExpectedInterpretation); err != nil {
		return PreviewPlan{}, fmt.Errorf("content: candidate interpretation does not match profile: %w", err)
	}
	if err := validateInterpretation(input.Candidate.Interpretation); err != nil {
		return PreviewPlan{}, fmt.Errorf("content: candidate interpretation is incomplete: %w", err)
	}
	if err := validateInterpretation(profile.ExpectedInterpretation); err != nil {
		return PreviewPlan{}, fmt.Errorf("content: profile interpretation is incomplete: %w", err)
	}
	if err := validateLiteral(input.Candidate.Value, input.Candidate.Interpretation.ValueKind); err != nil {
		return PreviewPlan{}, err
	}
	if err := bindLiteralToText(input.Candidate.Value, input.Candidate.Text, input.Candidate.Interpretation.ValueKind); err != nil {
		return PreviewPlan{}, err
	}
	if looksLikeFormula(input.Candidate.Value) {
		return PreviewPlan{}, errors.New("content: formula-looking source literal is blocked")
	}
	metadata := input.Metadata
	if metadata.SpreadsheetID != profile.ResourceID || metadata.SheetID != profile.SheetID || metadata.Locator != profile.Cell || metadata.Provenance.QueryRange != profile.Cell || !providerBelongsToFamily(metadata.Provenance.Provider, profile.ProviderFamily) || metadata.Provenance.APIVersion != profile.ProviderAPIVersion || metadata.Provenance.Endpoint == "" || metadata.Provenance.Cache != "bypassed" {
		return PreviewPlan{}, errors.New("content: uncached provider observation does not match approved destination")
	}
	if err := workivaprovider.ValidateContentMetadata(metadata); err != nil {
		return PreviewPlan{}, fmt.Errorf("content: invalid provider observation: %w", err)
	}
	if !metadata.ValuePresent || !validRawScalar(metadata.RawValue) || !metadata.FormatsPresent || !metadata.EffectiveFormatsPresent {
		return PreviewPlan{}, errors.New("content: provider observation is missing critical value or format metadata")
	}
	if looksLikeFormula(metadata.RawValue) {
		return PreviewPlan{}, errors.New("content: formula target is blocked")
	}
	if metadata.Protection != "unprotected" || metadata.Writable != "writable" {
		return PreviewPlan{}, errors.New("content: target protection or writability is unknown or disallowed")
	}
	blockers := []string(nil)
	if metadata.LiteralWriteFormatPreservation != literalWritePreserves {
		blockers = append(blockers, "provider_format_preservation_unverified")
	}
	if input.ObservationID == "" || input.ObservedAt.IsZero() || input.ObservedAt.After(input.CreatedAt) {
		return PreviewPlan{}, errors.New("content: provider observation identity and time are required")
	}
	if input.PlacementIntentID == "" || input.CreatedAt.IsZero() {
		return PreviewPlan{}, errors.New("content: intent identity and creation time are required")
	}
	if !input.CreatedAt.Before(input.Candidate.ExpiresAt) {
		return PreviewPlan{}, errors.New("content: candidate has expired")
	}
	currentHash, err := hashCurrentObservation(metadata)
	if err != nil {
		return PreviewPlan{}, err
	}
	created := input.CreatedAt.UTC()
	expires := created.Add(time.Duration(profile.PreviewTTLSeconds) * time.Second)
	if expires.After(input.Candidate.ExpiresAt) || expires.After(binding.AccessExpiresAt) {
		return PreviewPlan{}, errors.New("content: signed preview lifetime exceeds candidate retention")
	}
	plan := PreviewPlan{
		PlacementIntentID: input.PlacementIntentID, ActorBindingID: actor,
		CandidateKind: input.Candidate.Kind, CandidateOriginKind: input.Candidate.CandidateOriginKind, CandidateID: input.Candidate.ID,
		CandidateRevision: input.Candidate.Revision, CandidateHash: input.Candidate.TextSHA256,
		CandidateMetadataHash: input.Candidate.MetadataSHA256, CandidateLineageHash: input.Candidate.LineageSHA256,
		SourceID: input.Candidate.SourceID, SourceHash: input.Candidate.SourceSHA256, SourceMetadataHash: input.Candidate.SourceMetadataSHA256,
		LineageHashes: append([]string(nil), input.Candidate.LineageHashes...), CandidateProvenance: cloneRaw(input.Candidate.Provenance),
		OriginalText: input.Candidate.Text, OriginalValue: cloneRaw(input.Candidate.Value),
		OriginalInterpretation: cloneInterpretation(input.Candidate.Interpretation),
		IntendedValue:          cloneRaw(input.Candidate.Value), TargetInterpretation: cloneInterpretation(profile.ExpectedInterpretation),
		CurrentValue: cloneRaw(metadata.RawValue), CurrentValueHash: currentHash,
		Destination: cloneProfile(profile), PolicyBinding: binding, Conversion: "typed_copy", ProviderObservationID: input.ObservationID,
		ProviderObservedAt: input.ObservedAt.UTC(), ProviderMetadata: cloneMetadata(metadata),
		CreatedAt: created, ExpiresAt: expires, RetentionPolicy: profile.RetentionPolicy,
		Blockers: blockers,
	}
	plan.ContentHash, err = hashPlan(plan)
	if err != nil {
		return PreviewPlan{}, err
	}
	return plan, nil
}

func validateCandidate(candidate Candidate, now time.Time) error {
	if (candidate.Kind != "extracted_item" && candidate.Kind != "draft_artifact") || candidate.ID == "" || candidate.Revision < 1 || candidate.Text == "" || len([]byte(candidate.Text)) > 32768 || candidate.SourceID == "" || !sha256Pattern.MatchString(candidate.SourceSHA256) || !sha256Pattern.MatchString(candidate.SourceMetadataSHA256) || !sha256Pattern.MatchString(candidate.TextSHA256) || !sha256Pattern.MatchString(hashBytes([]byte(candidate.Text))) || candidate.TextSHA256 != hashBytes([]byte(candidate.Text)) || !sha256Pattern.MatchString(candidate.MetadataSHA256) || !sha256Pattern.MatchString(candidate.LineageSHA256) {
		return errors.New("content: owned candidate is invalid")
	}
	if !candidate.ExpiresAt.After(now) || candidate.ExpiresAt.IsZero() || candidate.CreatedAt.IsZero() || candidate.CreatedAt.After(now) || !candidate.CreatedAt.Before(candidate.ExpiresAt) {
		return errors.New("content: candidate is unavailable or expired")
	}
	if candidate.Kind == "draft_artifact" {
		switch candidate.CandidateOriginKind {
		case "analyst", "agent_flow", "copilot", "other":
		default:
			return errors.New("content: immutable draft origin kind is invalid")
		}
	} else if candidate.CandidateOriginKind != "" {
		return errors.New("content: extracted item cannot carry a draft origin kind")
	}
	metadataCanonical, err := canonicalJSON(candidate.Metadata)
	if err != nil || !bytes.Equal(metadataCanonical, candidate.Metadata) || hashBytes(candidate.Metadata) != candidate.MetadataSHA256 {
		return errors.New("content: immutable candidate metadata digest is invalid")
	}
	provenanceCanonical, err := canonicalJSON(candidate.Provenance)
	if err != nil || !bytes.Equal(provenanceCanonical, candidate.Provenance) || hashBytes(candidate.Provenance) != candidate.SourceMetadataSHA256 {
		return errors.New("content: immutable source metadata digest is invalid")
	}
	if len(candidate.LineageHashes) == 0 {
		return errors.New("content: candidate lineage is incomplete")
	}
	for _, digest := range candidate.LineageHashes {
		if !sha256Pattern.MatchString(digest) {
			return errors.New("content: candidate lineage hash is invalid")
		}
	}
	if !contains(candidate.LineageHashes, candidate.SourceSHA256) {
		return errors.New("content: candidate source hash is absent from immutable lineage")
	}
	return nil
}

func providerBelongsToFamily(provider, family string) bool {
	return family == "workiva" && provider == "workiva_rest"
}

func validateLiteral(raw json.RawMessage, kind string) error {
	if !validRawScalar(raw) {
		return errors.New("content: candidate does not contain an exact JSON scalar literal")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return errors.New("content: candidate literal is invalid")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("content: candidate literal has trailing data")
	}
	valid := false
	switch kind {
	case "text", "date":
		_, valid = value.(string)
	case "number":
		number, isNumber := value.(json.Number)
		valid = isNumber && exactJSONDecimalPattern.MatchString(number.String()) && len(number.String()) <= 128
	case "boolean":
		_, valid = value.(bool)
	}
	if !valid {
		return errors.New("content: candidate literal kind does not match its interpretation")
	}
	return nil
}

func bindLiteralToText(raw json.RawMessage, text, kind string) error {
	switch kind {
	case "number", "boolean":
		if string(raw) != text {
			return errors.New("content: typed literal is not byte-identical to owned candidate text")
		}
	case "text", "date":
		var value string
		if json.Unmarshal(raw, &value) != nil || value != text {
			return errors.New("content: text literal is not byte-identical to owned candidate text")
		}
	}
	return nil
}

func validateInterpretation(i Interpretation) error {
	if i.ValueKind != "text" && i.ValueKind != "number" && i.ValueKind != "date" && i.ValueKind != "boolean" {
		return errors.New("value kind is missing or unsupported")
	}
	if !validOptionalString(i.Period, 64, false) || !validOptionalString(i.Unit, 64, false) || !validOptionalString(i.Currency, 3, false) {
		return errors.New("period, currency, or unit exceeds its bound")
	}
	if i.Currency != nil && !currencyCodePattern.MatchString(*i.Currency) {
		return errors.New("currency must be an uppercase three-letter code")
	}
	if !boundedPositiveDecimal(i.StoredScale) || !boundedPositiveDecimal(i.DisplayScale) {
		return errors.New("stored and display scales must be positive bounded exact decimals")
	}
	if i.PercentBasis != "not_percentage" && i.PercentBasis != "fraction" && i.PercentBasis != "whole_percent" && i.PercentBasis != "basis_points" {
		return errors.New("percent basis is missing or unsupported")
	}
	var precision any
	decoder := json.NewDecoder(bytes.NewReader(i.Precision))
	decoder.UseNumber()
	if len(i.Precision) == 0 || decoder.Decode(&precision) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("precision must be explicitly present")
	}
	if precision == nil {
		return nil
	}
	n, ok := precision.(json.Number)
	if !ok || !precisionPattern.MatchString(n.String()) {
		return errors.New("precision must be null or an integer from 0 through 15")
	}
	parsed, ok := new(big.Int).SetString(n.String(), 10)
	if !ok || !parsed.IsInt64() || parsed.Int64() > 15 {
		return errors.New("precision must be null or an integer from 0 through 15")
	}
	return nil
}

func validOptionalString(value *string, max int, allowEmpty bool) bool {
	return value == nil || len([]byte(*value)) <= max && (allowEmpty || *value != "")
}

func boundedPositiveDecimal(value json.Number) bool {
	if len(value.String()) == 0 || len(value.String()) > 128 || !boundedJSONNumberPattern.MatchString(value.String()) {
		return false
	}
	rational, ok := new(big.Rat).SetString(value.String())
	return ok && rational.Sign() > 0 && rational.Cmp(big.NewRat(1_000_000_000_000, 1)) <= 0
}

func validRawScalar(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return false
	}
	switch value.(type) {
	case nil, string, bool, json.Number:
		return true
	default:
		return false
	}
}

func looksLikeFormula(raw json.RawMessage) bool {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimLeft(value, " \t\r\n"), "=")
}

func equalInterpretations(a, b Interpretation) error {
	left, err := json.Marshal(a)
	if err != nil {
		return err
	}
	right, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if !bytes.Equal(left, right) {
		return errors.New("exact kind, period, unit, scale, percentage basis, and precision are required")
	}
	return nil
}

func validPolicyRef(ref PolicyRef) bool {
	return ref.PolicyID != "" && ref.Revision > 0 && sha256Pattern.MatchString(ref.ContentHash)
}

func candidateHasFineGrainedProvenance(raw json.RawMessage) bool {
	var source struct {
		Provenance struct {
			Availability string  `json:"availability"`
			Page         *int    `json:"page"`
			Table        *string `json:"table"`
			Segment      *string `json:"segment"`
			StartByte    *int    `json:"start_byte"`
			EndByte      *int    `json:"end_byte"`
		} `json:"provenance"`
	}
	if json.Unmarshal(raw, &source) != nil || source.Provenance.Availability != "available" {
		return false
	}
	p := source.Provenance
	return p.Page != nil && *p.Page > 0 || p.Table != nil && strings.TrimSpace(*p.Table) != "" || p.Segment != nil && strings.TrimSpace(*p.Segment) != "" || p.StartByte != nil && *p.StartByte >= 0 && p.EndByte != nil && *p.EndByte > *p.StartByte
}

func hashCurrentObservation(metadata workivaprovider.ContentMetadata) (string, error) {
	value, err := canonicalJSON(metadata.RawValue)
	if err != nil {
		return "", err
	}
	canonical := struct {
		SpreadsheetID string          `json:"spreadsheet_id"`
		SheetID       string          `json:"sheet_id"`
		Locator       string          `json:"locator"`
		Value         json.RawMessage `json:"value"`
		Formatting    string          `json:"formatting_sha256"`
	}{metadata.SpreadsheetID, metadata.SheetID, metadata.Locator, value, metadata.FormattingSHA256}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return hashBytes(encoded), nil
}

func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("content: provider value is not valid JSON")
	}
	encoded, err := json.Marshal(value)
	return encoded, err
}

func hashPlan(plan PreviewPlan) (string, error) {
	plan.ContentHash = ""
	encoded, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	return hashBytes(encoded), nil
}

func hashBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func cloneRaw(raw json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), raw...) }

func cloneInterpretation(i Interpretation) Interpretation {
	if i.Period != nil {
		value := *i.Period
		i.Period = &value
	}
	if i.Currency != nil {
		value := *i.Currency
		i.Currency = &value
	}
	if i.Unit != nil {
		value := *i.Unit
		i.Unit = &value
	}
	i.Precision = cloneRaw(i.Precision)
	return i
}

func cloneProfile(p DestinationProfile) DestinationProfile {
	p.ExpectedInterpretation = cloneInterpretation(p.ExpectedInterpretation)
	p.AllowedConversions = append([]string(nil), p.AllowedConversions...)
	return p
}

func cloneMetadata(m workivaprovider.ContentMetadata) workivaprovider.ContentMetadata {
	m.RawValue = cloneRaw(m.RawValue)
	m.CalculatedValue = cloneRaw(m.CalculatedValue)
	m.RawFormats = cloneRaw(m.RawFormats)
	m.EffectiveFormats = cloneRaw(m.EffectiveFormats)
	return m
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func isCanonicalUUID(value string) bool {
	if len(value) != 36 || strings.ToLower(value) != value {
		return false
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if char != '-' {
				return false
			}
			continue
		}
		if char < '0' || char > '9' {
			if char < 'a' || char > 'f' {
				return false
			}
		}
	}
	return true
}
