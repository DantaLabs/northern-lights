package assurance

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

var contentPolicyID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var contentPolicyHash = regexp.MustCompile(`^[a-f0-9]{64}$`)
var contentPolicyDecimalBound = big.NewRat(1_000_000_000_000, 1)

// ExplicitNullableString records whether a nullable policy member appeared.
// A nil Value with Set=true is an explicit JSON/YAML null.
type ExplicitNullableString struct {
	Set   bool    `json:"-" yaml:"-"`
	Value *string `json:"-" yaml:"-"`
}

func (n *ExplicitNullableString) UnmarshalJSON(raw []byte) error {
	n.Set = true
	if string(raw) == "null" {
		n.Value = nil
		return nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	n.Value = &value
	return nil
}
func (n ExplicitNullableString) MarshalJSON() ([]byte, error) {
	if n.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*n.Value)
}
func (n *ExplicitNullableString) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag == "!!null" {
		n.Set = true
		n.Value = nil
		return nil
	}
	var value string
	if err := node.Decode(&value); err != nil {
		return err
	}
	n.Set = true
	n.Value = &value
	return nil
}

type DestinationInterpretation struct {
	ValueKind    *string                `json:"value_kind" yaml:"value_kind"`
	Period       ExplicitNullableString `json:"period" yaml:"period"`
	Currency     ExplicitNullableString `json:"currency" yaml:"currency"`
	Unit         ExplicitNullableString `json:"unit" yaml:"unit"`
	StoredScale  *json.Number           `json:"stored_scale" yaml:"stored_scale"`
	DisplayScale *json.Number           `json:"display_scale" yaml:"display_scale"`
	PercentBasis *string                `json:"percent_basis" yaml:"percent_basis"`
	Precision    NullableInt            `json:"precision" yaml:"precision"`
}

func (i *DestinationInterpretation) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("expected_interpretation must be an object")
	}
	allowed := map[string]bool{"value_kind": true, "period": true, "currency": true, "unit": true, "stored_scale": true, "display_scale": true, "percent_basis": true, "precision": true}
	seen := map[string]bool{}
	for index := 0; index+1 < len(node.Content); index += 2 {
		key, value := node.Content[index].Value, node.Content[index+1]
		if !allowed[key] || seen[key] {
			return fmt.Errorf("expected_interpretation has unknown or duplicate member %q", key)
		}
		seen[key] = true
		switch key {
		case "value_kind":
			if err := value.Decode(&i.ValueKind); err != nil {
				return err
			}
		case "period", "currency", "unit":
			var target *ExplicitNullableString
			switch key {
			case "period":
				target = &i.Period
			case "currency":
				target = &i.Currency
			default:
				target = &i.Unit
			}
			target.Set = true
			if value.Tag != "!!null" {
				var text string
				if err := value.Decode(&text); err != nil {
					return err
				}
				target.Value = &text
			}
		case "stored_scale":
			scale, err := exactYAMLNumber(value)
			if err != nil {
				return err
			}
			i.StoredScale = scale
		case "display_scale":
			scale, err := exactYAMLNumber(value)
			if err != nil {
				return err
			}
			i.DisplayScale = scale
		case "percent_basis":
			if err := value.Decode(&i.PercentBasis); err != nil {
				return err
			}
		case "precision":
			i.Precision.Set = true
			if value.Tag != "!!null" {
				var n int
				if err := value.Decode(&n); err != nil {
					return err
				}
				i.Precision.Value = &n
			}
		}
	}
	return nil
}

func exactYAMLNumber(node *yaml.Node) (*json.Number, error) {
	if node.Tag != "!!int" && node.Tag != "!!float" {
		return nil, fmt.Errorf("scale must be a JSON-compatible number")
	}
	n := json.Number(node.Value)
	if !validPositiveContentScale(&n) {
		return nil, fmt.Errorf("scale must be a positive bounded exact decimal")
	}
	return &n, nil
}

// NullableInt distinguishes an explicit null precision from an omitted member.
type NullableInt struct {
	Set   bool `json:"-" yaml:"-"`
	Value *int `json:"-" yaml:"-"`
}

func (n *NullableInt) UnmarshalJSON(raw []byte) error {
	n.Set = true
	if string(raw) == "null" {
		n.Value = nil
		return nil
	}
	var v int
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	n.Value = &v
	return nil
}
func (n NullableInt) MarshalJSON() ([]byte, error) {
	if n.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*n.Value)
}
func (n *NullableInt) UnmarshalYAML(node *yaml.Node) error {
	n.Set = true
	if node.Tag == "!!null" {
		n.Value = nil
		return nil
	}
	var v int
	if err := node.Decode(&v); err != nil {
		return err
	}
	n.Value = &v
	return nil
}

type DestinationMetadataRequirements struct {
	Formula      *string `json:"formula" yaml:"formula"`
	Protection   *string `json:"protection" yaml:"protection"`
	Writability  *string `json:"writability" yaml:"writability"`
	Formatting   *string `json:"formatting" yaml:"formatting"`
	Period       *string `json:"period" yaml:"period"`
	Currency     *string `json:"currency" yaml:"currency"`
	Unit         *string `json:"unit" yaml:"unit"`
	Scale        *string `json:"scale" yaml:"scale"`
	PercentBasis *string `json:"percent_basis" yaml:"percent_basis"`
	Precision    *string `json:"precision" yaml:"precision"`
}
type ContentPolicyReference struct {
	PolicyID    string `json:"policy_id" yaml:"policy_id"`
	Revision    int    `json:"revision" yaml:"revision"`
	ContentHash string `json:"content_hash" yaml:"content_hash"`
}
type ContentRetentionReference struct {
	PolicyID    string `json:"policy_id" yaml:"policy_id"`
	Revision    int    `json:"revision" yaml:"revision"`
	ContentHash string `json:"content_hash" yaml:"content_hash"`
	Class       string `json:"class" yaml:"class"`
}

// ContentResourcePolicy binds one signed profile to exact destination coordinates.
type ContentResourcePolicy struct {
	PolicyID                string `json:"policy_id" yaml:"policy_id"`
	Revision                int    `json:"revision" yaml:"revision"`
	ContentHash             string `json:"content_hash,omitempty" yaml:"content_hash,omitempty"`
	State                   string `json:"state" yaml:"state"`
	ResourceID              string `json:"resource_id" yaml:"resource_id"`
	SheetID                 string `json:"sheet_id" yaml:"sheet_id"`
	Cell                    string `json:"cell" yaml:"cell"`
	AccessPolicyID          string `json:"access_policy_id" yaml:"access_policy_id"`
	AccessPolicyRevision    int    `json:"access_policy_revision" yaml:"access_policy_revision"`
	AccessPolicyContentHash string `json:"access_policy_content_hash" yaml:"access_policy_content_hash"`
	Capability              string `json:"capability" yaml:"capability"`
}

// ContentProviderContract describes declared provider behavior, not verified observations.
type ContentProviderContract struct {
	PolicyID            string   `json:"policy_id" yaml:"policy_id"`
	Revision            int      `json:"revision" yaml:"revision"`
	ContentHash         string   `json:"content_hash,omitempty" yaml:"content_hash,omitempty"`
	State               string   `json:"state" yaml:"state"`
	Provider            string   `json:"provider" yaml:"provider"`
	APIVersion          string   `json:"api_version" yaml:"api_version"`
	SupportedOperations []string `json:"supported_operations" yaml:"supported_operations"`
	RequiredFacts       []string `json:"required_facts" yaml:"required_facts"`
}

func contentResourcePolicyContent(p ContentResourcePolicy) any     { p.ContentHash = ""; return p }
func contentProviderContractContent(p ContentProviderContract) any { p.ContentHash = ""; return p }

// DestinationProfile is the signed, closed destination contract. Pointer
// members preserve the distinction between a missing required member and its zero value.
type DestinationProfile struct {
	SchemaVersion          *int                             `json:"schema_version" yaml:"schema_version"`
	DestinationProfileID   *string                          `json:"destination_profile_id" yaml:"destination_profile_id"`
	Revision               *int                             `json:"revision" yaml:"revision"`
	ContentHash            *string                          `json:"content_hash" yaml:"content_hash"`
	State                  *string                          `json:"state" yaml:"state"`
	ResourceID             *string                          `json:"resource_id" yaml:"resource_id"`
	SheetID                *string                          `json:"sheet_id" yaml:"sheet_id"`
	Cell                   *string                          `json:"cell" yaml:"cell"`
	Operation              *string                          `json:"operation" yaml:"operation"`
	ExpectedInterpretation *DestinationInterpretation       `json:"expected_interpretation" yaml:"expected_interpretation"`
	FormulaPolicy          *string                          `json:"formula_policy" yaml:"formula_policy"`
	LiteralPolicy          *string                          `json:"literal_policy" yaml:"literal_policy"`
	ProtectionPolicy       *string                          `json:"protection_policy" yaml:"protection_policy"`
	FormatPolicy           *string                          `json:"format_policy" yaml:"format_policy"`
	UnknownCriticalFacts   *string                          `json:"unknown_critical_facts" yaml:"unknown_critical_facts"`
	ProvenanceRequirement  *string                          `json:"provenance_requirement" yaml:"provenance_requirement"`
	MetadataRequirements   *DestinationMetadataRequirements `json:"metadata_requirements" yaml:"metadata_requirements"`
	AllowedConversions     *[]string                        `json:"allowed_conversions" yaml:"allowed_conversions"`
	ResourcePolicy         *ContentPolicyReference          `json:"resource_policy" yaml:"resource_policy"`
	ConversionPolicy       *ContentPolicyReference          `json:"conversion_policy" yaml:"conversion_policy"`
	ProviderContract       *ContentPolicyReference          `json:"provider_contract" yaml:"provider_contract"`
	RetentionPolicy        *ContentRetentionReference       `json:"retention_policy" yaml:"retention_policy"`
	PreviewTTLSeconds      *int                             `json:"preview_ttl_seconds" yaml:"preview_ttl_seconds"`
}

// UnmarshalYAML routes profiles through the JSON-typed struct decoder so YAML
// scalar coercions cannot make an invalid schema value appear well typed.
func (p *DestinationProfile) UnmarshalYAML(node *yaml.Node) error {
	var raw bytes.Buffer
	if err := writeExactYAMLJSON(&raw, node); err != nil {
		return err
	}
	type profileAlias DestinationProfile
	decoder := json.NewDecoder(bytes.NewReader(raw.Bytes()))
	decoder.DisallowUnknownFields()
	var parsed profileAlias
	if err := decoder.Decode(&parsed); err != nil {
		return err
	}
	*p = DestinationProfile(parsed)
	return nil
}

// writeExactYAMLJSON bridges the YAML profile form to the strict JSON-typed
// decoder without routing numeric scalars through float64. That preserves
// exact scale lexemes such as 1.00000000000000000001 for signed hashing.
func writeExactYAMLJSON(out *bytes.Buffer, node *yaml.Node) error {
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) != 1 {
			return fmt.Errorf("profile YAML document must contain one value")
		}
		return writeExactYAMLJSON(out, node.Content[0])
	case yaml.MappingNode:
		out.WriteByte('{')
		seen := make(map[string]bool, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
				return fmt.Errorf("profile YAML keys must be unique strings")
			}
			seen[key.Value] = true
			if i > 0 {
				out.WriteByte(',')
			}
			encodedKey, _ := json.Marshal(key.Value)
			out.Write(encodedKey)
			out.WriteByte(':')
			if err := writeExactYAMLJSON(out, node.Content[i+1]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
		return nil
	case yaml.SequenceNode:
		out.WriteByte('[')
		for i, item := range node.Content {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := writeExactYAMLJSON(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
		return nil
	case yaml.AliasNode:
		return fmt.Errorf("profile YAML aliases are unsupported")
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!str", "!!timestamp":
			encoded, _ := json.Marshal(node.Value)
			out.Write(encoded)
		case "!!null":
			out.WriteString("null")
		case "!!bool":
			if node.Value != "true" && node.Value != "false" {
				return fmt.Errorf("profile YAML boolean is invalid")
			}
			out.WriteString(node.Value)
		case "!!int", "!!float":
			if !json.Valid([]byte(node.Value)) {
				return fmt.Errorf("profile YAML numeric scalar is not JSON-compatible")
			}
			out.WriteString(node.Value)
		default:
			return fmt.Errorf("profile YAML scalar type %q is unsupported", node.Tag)
		}
		return nil
	default:
		return fmt.Errorf("profile YAML node kind is unsupported")
	}
}

// ContentAccessPolicy grants an explicit capability scope to a trusted actor.
type ContentAccessPolicy struct {
	PolicyID      string   `json:"policy_id" yaml:"policy_id"`
	Revision      int      `json:"revision" yaml:"revision"`
	ContentHash   string   `json:"content_hash,omitempty" yaml:"content_hash,omitempty"`
	State         string   `json:"state" yaml:"state"`
	TenantID      string   `json:"tenant_id" yaml:"tenant_id"`
	ActorObjectID string   `json:"actor_object_id" yaml:"actor_object_id"`
	Capabilities  []string `json:"capabilities" yaml:"capabilities"`
	ResourceIDs   []string `json:"resource_ids" yaml:"resource_ids"`
	ValidUntil    string   `json:"valid_until" yaml:"valid_until"`
}

// ContentRetentionPolicy defines explicit source, draft, intent, idempotency
// and recovery lifetimes. Zero values are rejected and are never defaulted.
type ContentRetentionPolicy struct {
	PolicyID                   string `json:"policy_id" yaml:"policy_id"`
	Revision                   int    `json:"revision" yaml:"revision"`
	ContentHash                string `json:"content_hash,omitempty" yaml:"content_hash,omitempty"`
	State                      string `json:"state" yaml:"state"`
	SourceLifetimeSeconds      int64  `json:"source_lifetime_seconds" yaml:"source_lifetime_seconds"`
	DraftLifetimeSeconds       int64  `json:"draft_lifetime_seconds" yaml:"draft_lifetime_seconds"`
	IntentLifetimeSeconds      int64  `json:"intent_lifetime_seconds" yaml:"intent_lifetime_seconds"`
	IdempotencyLifetimeSeconds int64  `json:"idempotency_lifetime_seconds" yaml:"idempotency_lifetime_seconds"`
	RecoveryLifetimeSeconds    int64  `json:"recovery_lifetime_seconds" yaml:"recovery_lifetime_seconds"`
}

func destinationProfileContent(p DestinationProfile) any {
	raw, _ := json.Marshal(p)
	var object map[string]json.RawMessage
	_ = json.Unmarshal(raw, &object)
	delete(object, "content_hash")
	return object
}
func contentAccessPolicyContent(p ContentAccessPolicy) any       { p.ContentHash = ""; return p }
func contentRetentionPolicyContent(p ContentRetentionPolicy) any { p.ContentHash = ""; return p }

func validateDestinationProfile(p *DestinationProfile) error {
	if p.SchemaVersion == nil || *p.SchemaVersion != 1 || p.DestinationProfileID == nil || !contentPolicyID.MatchString(*p.DestinationProfileID) || p.Revision == nil || *p.Revision < 1 || *p.Revision > maxRevision || p.ContentHash == nil || !contentPolicyHash.MatchString(*p.ContentHash) || p.State == nil || (*p.State != "active" && *p.State != "retired") || p.ResourceID == nil || !contentPolicyID.MatchString(*p.ResourceID) || p.SheetID == nil || !contentPolicyID.MatchString(*p.SheetID) || p.Cell == nil || !regexp.MustCompile(`^[A-Z]{1,3}[1-9][0-9]{0,6}$`).MatchString(*p.Cell) {
		return fmt.Errorf("required profile identity, state, or destination member is invalid")
	}
	if p.Operation == nil || *p.Operation != "single_cell_literal" || p.FormulaPolicy == nil || *p.FormulaPolicy != "reject_formula_target" || p.LiteralPolicy == nil || *p.LiteralPolicy != "literal_only" || p.ProtectionPolicy == nil || *p.ProtectionPolicy != "reject_protected" || p.FormatPolicy == nil || *p.FormatPolicy != "preserve" || p.UnknownCriticalFacts == nil || *p.UnknownCriticalFacts != "block" {
		return fmt.Errorf("profile operation policy is unsupported")
	}
	i := p.ExpectedInterpretation
	if i == nil || i.ValueKind == nil || !oneOf(*i.ValueKind, "text", "number", "date", "boolean") || !i.Period.Set || !i.Currency.Set || !i.Unit.Set || !validPositiveContentScale(i.StoredScale) || !validPositiveContentScale(i.DisplayScale) || i.PercentBasis == nil || !oneOf(*i.PercentBasis, "not_percentage", "fraction", "whole_percent", "basis_points") || !i.Precision.Set {
		return fmt.Errorf("expected_interpretation has missing or invalid required members")
	}
	if !nullableBound(i.Period, 64, false) || !nullableCurrency(i.Currency) || !nullableBound(i.Unit, 64, false) || (i.Precision.Value != nil && (*i.Precision.Value < 0 || *i.Precision.Value > 15)) {
		return fmt.Errorf("expected_interpretation nullable member is invalid")
	}
	if p.ProvenanceRequirement == nil || !oneOf(*p.ProvenanceRequirement, "source_or_verified_evidence", "fine_grained_available") || p.MetadataRequirements == nil || p.AllowedConversions == nil || len(*p.AllowedConversions) < 1 || len(*p.AllowedConversions) > 3 || p.ResourcePolicy == nil || p.ConversionPolicy == nil || p.ProviderContract == nil || p.RetentionPolicy == nil || p.PreviewTTLSeconds == nil || *p.PreviewTTLSeconds < 1 || *p.PreviewTTLSeconds > 900 {
		return fmt.Errorf("required policy reference or bounded profile member is missing")
	}
	m := p.MetadataRequirements
	for _, v := range []*string{m.Formula, m.Protection, m.Writability, m.Formatting} {
		if v == nil || *v != "provider_verified" {
			return fmt.Errorf("metadata protection requirements are invalid")
		}
	}
	for _, v := range []*string{m.Period, m.Currency, m.Unit, m.Scale, m.PercentBasis, m.Precision} {
		if v == nil || !oneOf(*v, "provider_verified", "operator_declared_allowed") {
			return fmt.Errorf("metadata requirements are invalid")
		}
	}
	seen := map[string]bool{}
	for _, v := range *p.AllowedConversions {
		if !oneOf(v, "typed_copy", "same_currency_scale", "verified_percentage") || seen[v] {
			return fmt.Errorf("allowed conversions are invalid")
		}
		seen[v] = true
	}
	for _, r := range []*ContentPolicyReference{p.ResourcePolicy, p.ConversionPolicy, p.ProviderContract} {
		if !validPolicyRef(*r) {
			return fmt.Errorf("policy reference is invalid")
		}
	}
	ret := p.RetentionPolicy
	if !validPolicyRef(ContentPolicyReference{ret.PolicyID, ret.Revision, ret.ContentHash}) || ret.Class != "workflow" {
		return fmt.Errorf("workflow retention reference is invalid")
	}
	canonical, err := CanonicalJSON(destinationProfileContent(*p))
	if err != nil {
		return err
	}
	if digestHex(HashBytes(canonical)) != *p.ContentHash {
		return fmt.Errorf("profile content_hash mismatch")
	}
	return nil
}

// Keep scale values as their exact signed JSON number lexemes. In particular,
// never pass policy decimals through float64 during validation or projection.
func validPositiveContentScale(value *json.Number) bool {
	if value == nil || len(value.String()) == 0 || len(value.String()) > 128 || !json.Valid([]byte(value.String())) {
		return false
	}
	rat, ok := new(big.Rat).SetString(value.String())
	return ok && rat.Sign() > 0 && rat.Cmp(contentPolicyDecimalBound) <= 0
}
func oneOf(v string, choices ...string) bool {
	for _, c := range choices {
		if v == c {
			return true
		}
	}
	return false
}
func validPolicyRef(r ContentPolicyReference) bool {
	return contentPolicyID.MatchString(r.PolicyID) && r.Revision > 0 && r.Revision <= maxRevision && contentPolicyHash.MatchString(r.ContentHash)
}
func nullableBound(v ExplicitNullableString, max int, allowEmpty bool) bool {
	return v.Set && (v.Value == nil || (len([]byte(*v.Value)) <= max && (allowEmpty || *v.Value != "")))
}
func nullableCurrency(v ExplicitNullableString) bool {
	return v.Set && (v.Value == nil || regexp.MustCompile(`^[A-Z]{3}$`).MatchString(*v.Value))
}

func validateContentAccessPolicy(p *ContentAccessPolicy, tenant string) error {
	if !contentPolicyID.MatchString(p.PolicyID) || p.Revision < 1 || p.Revision > maxRevision || p.State != "active" && p.State != "retired" || p.TenantID != tenant || !uuidIsCanonical(p.ActorObjectID) || len(p.Capabilities) == 0 || len(p.Capabilities) > 5 || len(p.ResourceIDs) == 0 || len(p.ResourceIDs) > 256 {
		return fmt.Errorf("content access policy fields are invalid")
	}
	allowedCaps := map[string]bool{}
	for _, capability := range p.Capabilities {
		if !oneOf(capability, "content.ingest", "content.stage", "content.confirm", "content.acknowledge", "content.reconcile") || allowedCaps[capability] {
			return fmt.Errorf("content access capabilities are invalid")
		}
		allowedCaps[capability] = true
	}
	if _, err := time.Parse(time.RFC3339, p.ValidUntil); err != nil {
		return fmt.Errorf("content access policy valid_until is required")
	}
	seen := map[string]bool{}
	for _, id := range p.ResourceIDs {
		if !contentPolicyID.MatchString(id) || seen[id] {
			return fmt.Errorf("content access resources are invalid")
		}
		seen[id] = true
	}
	canonical, err := CanonicalJSON(contentAccessPolicyContent(*p))
	if err != nil {
		return err
	}
	h := digestHex(HashBytes(canonical))
	if p.ContentHash != "" && p.ContentHash != h {
		return fmt.Errorf("content access policy hash mismatch")
	}
	p.ContentHash = h
	return nil
}
func validateContentRetentionPolicy(p *ContentRetentionPolicy) error {
	if !contentPolicyID.MatchString(p.PolicyID) || p.Revision < 1 || p.Revision > maxRevision || p.State != "active" && p.State != "retired" {
		return fmt.Errorf("content retention policy fields are invalid")
	}
	for _, v := range []int64{p.SourceLifetimeSeconds, p.DraftLifetimeSeconds, p.IntentLifetimeSeconds, p.IdempotencyLifetimeSeconds, p.RecoveryLifetimeSeconds} {
		if v < 1 || v > 315360000 {
			return fmt.Errorf("content retention lifetime must be explicit and bounded")
		}
	}
	if p.IdempotencyLifetimeSeconds < p.IntentLifetimeSeconds || p.RecoveryLifetimeSeconds < p.IntentLifetimeSeconds || p.DraftLifetimeSeconds > p.SourceLifetimeSeconds {
		return fmt.Errorf("content retention horizons are inconsistent")
	}
	canonical, err := CanonicalJSON(contentRetentionPolicyContent(*p))
	if err != nil {
		return err
	}
	h := digestHex(HashBytes(canonical))
	if p.ContentHash != "" && p.ContentHash != h {
		return fmt.Errorf("content retention policy hash mismatch")
	}
	p.ContentHash = h
	return nil
}

func validateContentResourcePolicy(p *ContentResourcePolicy) error {
	if !contentPolicyID.MatchString(p.PolicyID) || p.Revision < 1 || p.Revision > maxRevision || p.State != "active" && p.State != "retired" || !contentPolicyID.MatchString(p.ResourceID) || !contentPolicyID.MatchString(p.SheetID) || !regexp.MustCompile(`^[A-Z]{1,3}[1-9][0-9]{0,6}$`).MatchString(p.Cell) || !contentPolicyID.MatchString(p.AccessPolicyID) || p.AccessPolicyRevision < 1 || p.AccessPolicyRevision > maxRevision || !contentPolicyHash.MatchString(p.AccessPolicyContentHash) || !isContentDestinationCapability(identity.Permission(p.Capability)) {
		return fmt.Errorf("content resource policy fields are invalid")
	}
	canonical, err := CanonicalJSON(contentResourcePolicyContent(*p))
	if err != nil {
		return err
	}
	hash := digestHex(HashBytes(canonical))
	if p.ContentHash != "" && p.ContentHash != hash {
		return fmt.Errorf("content resource policy hash mismatch")
	}
	p.ContentHash = hash
	return nil
}
func validateContentProviderContract(p *ContentProviderContract) error {
	if !contentPolicyID.MatchString(p.PolicyID) || p.Revision < 1 || p.Revision > maxRevision || p.State != "active" && p.State != "retired" || p.Provider != "workiva" || p.APIVersion != "2026-01-01" || len(p.SupportedOperations) == 0 || len(p.SupportedOperations) > 16 || len(p.RequiredFacts) > 32 {
		return fmt.Errorf("content provider contract fields are invalid")
	}
	allowedOperations := map[string]bool{"single_cell_literal": true}
	allowedFacts := map[string]bool{"formula": true, "protection": true, "writability": true, "formatting": true, "period": true, "currency": true, "unit": true, "scale": true, "percent_basis": true, "precision": true}
	for index, list := range [][]string{p.SupportedOperations, p.RequiredFacts} {
		seen := map[string]bool{}
		for _, item := range list {
			if !contentPolicyID.MatchString(item) || seen[item] || index == 0 && !allowedOperations[item] || index == 1 && !allowedFacts[item] {
				return fmt.Errorf("content provider contract lists are invalid")
			}
			seen[item] = true
		}
	}
	for _, required := range []string{"formula", "protection", "writability", "formatting"} {
		if !contentPolicyContains(p.RequiredFacts, required) {
			return fmt.Errorf("provider contract omits required fact %q", required)
		}
	}
	canonical, err := CanonicalJSON(contentProviderContractContent(*p))
	if err != nil {
		return err
	}
	hash := digestHex(HashBytes(canonical))
	if p.ContentHash != "" && p.ContentHash != hash {
		return fmt.Errorf("content provider contract hash mismatch")
	}
	p.ContentHash = hash
	return nil
}

// ContentPolicyResolver always rechecks the active signed bundle. The key is
// trusted configuration supplied at construction, never read from bundle data.
type ContentPolicyResolver struct {
	store          *Store
	publicKey      ed25519.PublicKey
	accessPolicyID string
}

func NewContentPolicyResolver(store *Store, trustedKey ed25519.PublicKey, accessPolicyID string) (*ContentPolicyResolver, error) {
	if store == nil || store.db == nil || len(trustedKey) != ed25519.PublicKeySize || !contentPolicyID.MatchString(accessPolicyID) {
		return nil, domainError("content_policy_configuration_invalid", "store, trusted Ed25519 key, and explicit access policy selector are required")
	}
	return &ContentPolicyResolver{store: store, publicKey: append(ed25519.PublicKey(nil), trustedKey...), accessPolicyID: accessPolicyID}, nil
}

func (r *ContentPolicyResolver) active(ctx context.Context, capability identity.Permission) (*ValidatedBundle, string, error) {
	if r == nil || r.store == nil {
		return nil, "", domainError("content_policy_unavailable", "content policy resolver is unavailable")
	}
	p, ok := identity.PrincipalFromContext(ctx)
	if !ok || p.TenantID == "" || p.TokenType != identity.TokenTypeDelegated || p.TenantID == identity.LegacyTenantID || p.ObjectID == "" || p.UsedSubjectFallback || !uuidIsCanonical(p.TenantID) || !uuidIsCanonical(p.ObjectID) || !p.HasPermission(capability) {
		return nil, "", domainError("strong_identity_required", "trusted delegated tenant and actor UUID are required")
	}
	if err := r.store.Ready(); err != nil {
		return nil, "", domainError("content_policy_unavailable", "assurance store is not ready to serve signed policies")
	}
	var raw, sigHex, stored string
	err := r.store.db.QueryRowContext(ctx, `SELECT bundle_json,signature_hex,content_hash FROM assurance_active_bundles WHERE tenant_id=? AND singleton=1`, p.TenantID).Scan(&raw, &sigHex, &stored)
	if err != nil {
		return nil, "", domainError("content_policy_unavailable", "active signed policy bundle is unavailable")
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return nil, "", domainError("content_policy_integrity_failed", "active signed bundle signature is invalid")
	}
	valid, err := ValidateBundle([]byte(raw), sig, p.TenantID, r.publicKey)
	if err != nil || valid.ContentHash != stored {
		return nil, "", domainError("content_policy_integrity_failed", "active signed bundle failed integrity validation")
	}
	return valid, p.TenantID, nil
}
func uuidIsCanonical(s string) bool {
	parsed, err := uuid.Parse(s)
	return err == nil && parsed.String() == s
}

type ContentDestinationBindings struct {
	Resource            ContentResourcePolicy
	Conversion          ConversionPolicy
	ProviderContract    ContentProviderContract
	Retention           ContentRetentionPolicy
	AccessPolicy        ContentAccessPolicy
	ActiveBundleHash    string
	ActiveBundleVersion int
}

func (r *ContentPolicyResolver) ResolveContentDestination(ctx context.Context, id string, revision int, now time.Time) (DestinationProfile, ContentDestinationBindings, error) {
	return r.ResolveContentDestinationForCapability(ctx, id, revision, now, identity.PermissionContentStage)
}

func isContentDestinationCapability(capability identity.Permission) bool {
	switch capability {
	case identity.PermissionContentStage, identity.PermissionContentConfirm, identity.PermissionContentAcknowledge, identity.PermissionContentReconcile:
		return true
	default:
		return false
	}
}

// ResolveContentDestinationForCapability requires the caller and signed scope to grant the same phase.
func (r *ContentPolicyResolver) ResolveContentDestinationForCapability(ctx context.Context, id string, revision int, now time.Time, capability identity.Permission) (DestinationProfile, ContentDestinationBindings, error) {
	if !isContentDestinationCapability(capability) {
		return DestinationProfile{}, ContentDestinationBindings{}, domainError("content_policy_capability_invalid", "content destination capability is not a supported server phase")
	}
	b, tenant, err := r.active(ctx, capability)
	if err != nil {
		return DestinationProfile{}, ContentDestinationBindings{}, err
	}
	var profile *DestinationProfile
	for i := range b.Bundle.DestinationProfiles {
		p := &b.Bundle.DestinationProfiles[i]
		if p.DestinationProfileID != nil && *p.DestinationProfileID == id && p.Revision != nil && *p.Revision == revision {
			profile = p
			break
		}
	}
	if profile == nil || *profile.State != "active" {
		return DestinationProfile{}, ContentDestinationBindings{}, domainError("content_policy_not_found", "active destination profile was not found")
	}
	var access *ContentAccessPolicy
	principal, _ := identity.PrincipalFromContext(ctx)
	for i := range b.Bundle.ContentAccessPolicies {
		p := &b.Bundle.ContentAccessPolicies[i]
		if p.PolicyID == r.accessPolicyID && p.State == "active" && p.TenantID == tenant && p.ActorObjectID == principal.ObjectID && contentPolicyContains(p.Capabilities, string(capability)) && contentPolicyContains(p.ResourceIDs, *profile.ResourceID) {
			access = p
			break
		}
	}
	if access == nil || expired(access.ValidUntil, now) {
		return DestinationProfile{}, ContentDestinationBindings{}, domainError("content_policy_denied", "active content access policy is missing, stale, or out of scope for the requested capability")
	}
	bindings := ContentDestinationBindings{ActiveBundleHash: b.ContentHash, ActiveBundleVersion: b.Bundle.BundleVersion}
	for _, p := range b.Bundle.ContentResourcePolicies {
		if p.PolicyID == profile.ResourcePolicy.PolicyID && p.Revision == profile.ResourcePolicy.Revision && p.ContentHash == profile.ResourcePolicy.ContentHash {
			bindings.Resource = p
		}
	}
	for _, p := range b.Bundle.ContentProviderContracts {
		if p.PolicyID == profile.ProviderContract.PolicyID && p.Revision == profile.ProviderContract.Revision && p.ContentHash == profile.ProviderContract.ContentHash {
			bindings.ProviderContract = p
		}
	}
	bindings.AccessPolicy = *access
	foundConversion := false
	for _, p := range b.Bundle.ConversionPolicies {
		if p.PolicyID == profile.ConversionPolicy.PolicyID && p.Revision == profile.ConversionPolicy.Revision && p.ContentHash == profile.ConversionPolicy.ContentHash {
			bindings.Conversion = p
			foundConversion = true
		}
	}
	var retention *ContentRetentionPolicy
	for i := range b.Bundle.ContentRetentionPolicies {
		p := &b.Bundle.ContentRetentionPolicies[i]
		if p.PolicyID == profile.RetentionPolicy.PolicyID && p.Revision == profile.RetentionPolicy.Revision && p.ContentHash == profile.RetentionPolicy.ContentHash && p.State == "active" {
			retention = p
		}
	}
	if !foundConversion || retention == nil || bindings.Resource.PolicyID == "" || bindings.ProviderContract.PolicyID == "" || bindings.Resource.AccessPolicyID != access.PolicyID || bindings.Resource.AccessPolicyRevision != access.Revision || bindings.Resource.AccessPolicyContentHash != access.ContentHash || !contentPolicyContains(access.Capabilities, string(capability)) || !contentPolicyContains(bindings.ProviderContract.SupportedOperations, *profile.Operation) {
		return DestinationProfile{}, ContentDestinationBindings{}, domainError("content_policy_reference_invalid", "destination profile references unresolved signed policies")
	}
	if bindings.Resource.ResourceID != *profile.ResourceID || bindings.Resource.SheetID != *profile.SheetID || bindings.Resource.Cell != *profile.Cell || bindings.Resource.State != "active" || bindings.ProviderContract.State != "active" || bindings.ProviderContract.APIVersion != "2026-01-01" {
		return DestinationProfile{}, ContentDestinationBindings{}, domainError("content_policy_reference_invalid", "resolved resource or provider contract no longer matches the destination profile")
	}
	bindings.Retention = *retention
	return *profile, bindings, nil
}
func contentPolicyContains(items []string, v string) bool {
	for _, item := range items {
		if item == v {
			return true
		}
	}
	return false
}
func expired(value string, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, value)
	return err != nil || !now.Before(t)
}

func (r *ContentPolicyResolver) ResolveContentIntakePolicy(ctx context.Context, now time.Time) (ContentAccessPolicy, ContentRetentionPolicy, error) {
	b, tenant, err := r.active(ctx, identity.PermissionContentIngest)
	if err != nil {
		return ContentAccessPolicy{}, ContentRetentionPolicy{}, err
	}
	principal, _ := identity.PrincipalFromContext(ctx)
	var access *ContentAccessPolicy
	for i := range b.Bundle.ContentAccessPolicies {
		p := &b.Bundle.ContentAccessPolicies[i]
		if p.PolicyID == r.accessPolicyID && p.State == "active" && p.TenantID == tenant && p.ActorObjectID == principal.ObjectID && contentPolicyContains(p.Capabilities, "content.ingest") {
			access = p
			break
		}
	}
	if access == nil || expired(access.ValidUntil, now) {
		return ContentAccessPolicy{}, ContentRetentionPolicy{}, domainError("content_policy_denied", "active content ingest policy is missing or expired")
	}
	var retention *ContentRetentionPolicy
	for i := range b.Bundle.ContentRetentionPolicies {
		p := &b.Bundle.ContentRetentionPolicies[i]
		if p.State == "active" {
			if retention != nil {
				return ContentAccessPolicy{}, ContentRetentionPolicy{}, domainError("content_policy_ambiguous", "an explicit content retention policy selector is required")
			}
			retention = p
		}
	}
	if retention == nil {
		return ContentAccessPolicy{}, ContentRetentionPolicy{}, domainError("content_policy_missing", "active content retention policy is missing")
	}
	return *access, *retention, nil
}
