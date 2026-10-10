package assurance

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dantalabs/northern-lights/internal/identity"
)

const supportedBundleSchemaVersion = 1

const (
	maxBundleBytes       = 4 << 20
	maxBundleIDLength    = 128
	maxReportIDLength    = 128
	maxNameLength        = 256
	maxDescriptionLength = 4096
	maxOwnerLength       = 128
	maxPeriodKeyLength   = 128
	maxPeriodLabelLength = 256
	maxFieldIDLength     = 128
	maxResourceIDLength  = 128
	maxLocatorLength     = 512
	maxUnitLength        = 128
	maxScaleLength       = 64
	maxTimezoneLength    = 64
	maxReferenceLength   = 128
	maxExportProfiles    = 32
	maxFieldCount        = 1000
	maxPeriodCount       = 100
	maxAllowlistEntries  = 5000
	maxPrecision         = 38
	maxRevision          = 1_000_000
)

// Bundle is the signed tenant-bound provisioning unit. It intentionally has no
// provider credentials or runtime secrets.
type Bundle struct {
	SchemaVersion            int                          `json:"schema_version" yaml:"schema_version"`
	BundleID                 string                       `json:"bundle_id" yaml:"bundle_id"`
	BundleVersion            int                          `json:"bundle_version" yaml:"bundle_version"`
	TenantID                 string                       `json:"tenant_id" yaml:"tenant_id"`
	Reports                  []ReportRevision             `json:"reports" yaml:"reports"`
	RuleSets                 []RuleSet                    `json:"rule_sets,omitempty" yaml:"rule_sets,omitempty"`
	MaterialityPolicies      []MaterialityPolicy          `json:"materiality_policies,omitempty" yaml:"materiality_policies,omitempty"`
	ExportProfiles           []ExportProfile              `json:"export_profiles,omitempty" yaml:"export_profiles,omitempty"`
	RetentionPolicies        []RetentionPolicy            `json:"retention_policies,omitempty" yaml:"retention_policies,omitempty"`
	RelationshipAllowlist    []RelationshipAllowlistEntry `json:"relationship_allowlist,omitempty" yaml:"relationship_allowlist,omitempty"`
	TransferRoutes           []TransferRoute              `json:"transfer_routes,omitempty" yaml:"transfer_routes,omitempty"`
	ConversionPolicies       []ConversionPolicy           `json:"conversion_policies,omitempty" yaml:"conversion_policies,omitempty"`
	DestinationProfiles      []DestinationProfile         `json:"destination_profiles,omitempty" yaml:"destination_profiles,omitempty"`
	ContentAccessPolicies    []ContentAccessPolicy        `json:"content_access_policies,omitempty" yaml:"content_access_policies,omitempty"`
	ContentRetentionPolicies []ContentRetentionPolicy     `json:"content_retention_policies,omitempty" yaml:"content_retention_policies,omitempty"`
	ContentResourcePolicies  []ContentResourcePolicy      `json:"content_resource_policies,omitempty" yaml:"content_resource_policies,omitempty"`
	ContentProviderContracts []ContentProviderContract    `json:"content_provider_contracts,omitempty" yaml:"content_provider_contracts,omitempty"`
}

const RelationshipCapabilityRead = "assurance.relationship.read"

// RelationshipAllowlistEntry is one immutable, operator-signed grant revision.
// The active signed bundle is the complete set: an omitted entry remains in
// history but is no longer active.
type RelationshipAllowlistEntry struct {
	EntryID     string `json:"entry_id" yaml:"entry_id"`
	Revision    int    `json:"revision" yaml:"revision"`
	ActorID     string `json:"actor_id" yaml:"actor_id"`
	Capability  string `json:"capability" yaml:"capability"`
	ResourceID  string `json:"resource_id" yaml:"resource_id"`
	ContentHash string `json:"content_hash,omitempty" yaml:"content_hash,omitempty"`
}

// ReportRevision is one immutable server-owned report definition revision.
type ReportRevision struct {
	ReportID            string            `json:"report_id" yaml:"report_id"`
	Revision            int               `json:"revision" yaml:"revision"`
	Name                string            `json:"name" yaml:"name"`
	Description         string            `json:"description,omitempty" yaml:"description,omitempty"`
	Owner               string            `json:"owner" yaml:"owner"`
	Status              string            `json:"status" yaml:"status"`
	RetentionClass      string            `json:"retention_class" yaml:"retention_class"`
	ResourcePolicyHash  string            `json:"resource_policy_hash" yaml:"resource_policy_hash"`
	RuleSetID           string            `json:"rule_set_id,omitempty" yaml:"rule_set_id,omitempty"`
	MaterialityPolicyID string            `json:"materiality_policy_id,omitempty" yaml:"materiality_policy_id,omitempty"`
	ExportProfiles      []string          `json:"export_profiles,omitempty" yaml:"export_profiles,omitempty"`
	EffectiveFrom       string            `json:"effective_from,omitempty" yaml:"effective_from,omitempty"`
	EffectiveTo         string            `json:"effective_to,omitempty" yaml:"effective_to,omitempty"`
	Periods             []Period          `json:"periods" yaml:"periods"`
	Fields              []FieldDefinition `json:"fields" yaml:"fields"`
	ContentHash         string            `json:"content_hash,omitempty" yaml:"content_hash,omitempty"`
}

// ValidatedBundle carries canonical signed bytes into the staging API.
type ValidatedBundle struct {
	Bundle        Bundle
	CanonicalJSON []byte
	Signature     []byte
	ContentHash   string
	verifiedBy    ed25519.PublicKey
}

// ValidateBundle strictly parses JSON or YAML, validates all references and
// bounds, verifies tenant binding and detached Ed25519 signature, and returns
// canonical JSON suitable for atomic staging.
func ValidateBundle(raw, signature []byte, expectedTenant string, publicKey ed25519.PublicKey) (*ValidatedBundle, error) {
	if len(raw) == 0 || len(raw) > maxBundleBytes {
		return nil, domainError("bundle_bounds_invalid", fmt.Sprintf("bundle must be between 1 and %d bytes", maxBundleBytes))
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return nil, domainError("bundle_key_invalid", "configured Ed25519 public key is invalid")
	}
	bundle, err := decodeStrictBundle(raw)
	if err != nil {
		return nil, wrapError("bundle_malformed", "bundle is not strict versioned YAML/JSON", err)
	}
	// Signatures cover the operator-authored semantic bundle. Derived report
	// hashes are populated only after verification and are not silently added
	// to the signed payload.
	canonical, err := CanonicalJSON(bundle)
	if err != nil {
		return nil, wrapError("bundle_canonicalization_failed", "bundle could not be canonicalized", err)
	}
	if !ed25519.Verify(publicKey, canonical, signature) {
		return nil, domainError("bundle_signature_invalid", "bundle signature verification failed")
	}
	if err := validateBundleObject(&bundle, expectedTenant); err != nil {
		return nil, err
	}
	return &ValidatedBundle{Bundle: bundle, CanonicalJSON: canonical, Signature: append([]byte(nil), signature...), ContentHash: digestHex(HashBytes(canonical)), verifiedBy: append(ed25519.PublicKey(nil), publicKey...)}, nil
}

func decodeStrictBundle(raw []byte) (Bundle, error) {
	var node yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&node); err != nil {
		return Bundle{}, err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Bundle{}, errors.New("multiple YAML/JSON documents are not allowed")
		}
		return Bundle{}, err
	}
	if err := rejectBundleReferences(&node); err != nil {
		return Bundle{}, err
	}
	strict := yaml.NewDecoder(bytes.NewReader(raw))
	strict.KnownFields(true)
	var bundle Bundle
	if json.Valid(raw) {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&bundle); err != nil {
			return Bundle{}, err
		}
	} else if err := strict.Decode(&bundle); err != nil {
		return Bundle{}, err
	}
	return bundle, nil
}

func rejectBundleReferences(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.AliasNode || node.Anchor != "" || (node.Value == "<<" && node.Tag == "!!merge") {
		return errors.New("YAML aliases, anchors, and merge keys are not allowed")
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := strings.ToLower(strings.ReplaceAll(node.Content[i].Value, "-", "_"))
			switch key {
			case "secret", "password", "token", "credential", "credentials", "client_id", "client_secret", "api_key", "access_token", "authorization", "private_key", "provider_secret":
				return errors.New("provider credentials and secret fields are not allowed in bundles")
			}
		}
	}
	for _, child := range node.Content {
		if err := rejectBundleReferences(child); err != nil {
			return err
		}
	}
	return nil
}

func validateBundleObject(bundle *Bundle, expectedTenant string) error {
	if bundle.SchemaVersion != supportedBundleSchemaVersion {
		return domainError("bundle_version_unsupported", fmt.Sprintf("unsupported bundle schema version %d", bundle.SchemaVersion))
	}
	if err := validateBundleString(bundle.BundleID, "bundle_id", maxBundleIDLength, true); err != nil || bundle.BundleVersion <= 0 || bundle.BundleVersion > maxRevision {
		return domainError("bundle_invalid", "bundle_id and bounded positive bundle_version are required")
	}
	if err := validateBundleString(bundle.TenantID, "tenant_id", maxReferenceLength, true); err != nil || bundle.TenantID != expectedTenant {
		return domainError("bundle_tenant_mismatch", "bundle tenant does not match configured tenant")
	}
	if len(bundle.Reports) == 0 || len(bundle.Reports) > 100 {
		return domainError("bundle_invalid", "bundle must contain 1 through 100 report revisions")
	}
	reports := make(map[string]bool, len(bundle.Reports))
	reportResources := make(map[string]bool)
	lastRevisionByReport := make(map[string]int)
	byReport := make(map[string][]ReportRevision)
	for reportIndex := range bundle.Reports {
		report := &bundle.Reports[reportIndex]
		key := fmt.Sprintf("%s/%d", report.ReportID, report.Revision)
		if err := validateBundleString(report.ReportID, "report_id", maxReportIDLength, true); err != nil || report.Revision <= 0 || report.Revision > maxRevision || reports[key] {
			return domainError("bundle_duplicate_or_invalid_report", "report IDs and bounded positive revisions must be unique")
		}
		if previousRevision, ok := lastRevisionByReport[report.ReportID]; ok && report.Revision <= previousRevision {
			return domainError("bundle_revision_non_monotonic", fmt.Sprintf("report %s revisions must increase in declared order", report.ReportID))
		}
		lastRevisionByReport[report.ReportID] = report.Revision
		reports[key] = true
		if err := validateBundleString(report.Name, "name", maxNameLength, true); err != nil {
			return domainError("bundle_invalid_report", fmt.Sprintf("report %s has invalid bounded metadata", report.ReportID))
		}
		if err := validateBundleString(report.Description, "description", maxDescriptionLength, false); err != nil {
			return domainError("bundle_invalid_report", fmt.Sprintf("report %s has invalid bounded metadata", report.ReportID))
		}
		if err := validateBundleString(report.Owner, "owner", maxOwnerLength, true); err != nil {
			return domainError("bundle_invalid_report", fmt.Sprintf("report %s has invalid bounded metadata", report.ReportID))
		}
		if report.Status != "active" {
			return domainError("bundle_invalid_report", fmt.Sprintf("report %s must be active", report.ReportID))
		}
		if report.RetentionClass != "standard" && report.RetentionClass != "long_term" {
			return domainError("bundle_invalid_retention_class", fmt.Sprintf("report %s has unsupported retention class %q", report.ReportID, report.RetentionClass))
		}
		if len(report.ResourcePolicyHash) != 64 {
			return domainError("bundle_invalid_report", fmt.Sprintf("report %s resource policy hash is invalid", report.ReportID))
		}
		if _, err := hex.DecodeString(report.ResourcePolicyHash); err != nil {
			return domainError("bundle_invalid_report", fmt.Sprintf("report %s resource policy hash is invalid", report.ReportID))
		}
		if len(report.ExportProfiles) > maxExportProfiles {
			return domainError("bundle_collection_bound", fmt.Sprintf("report %s has too many export profiles", report.ReportID))
		}
		// References are checked against the complete candidate below, after all
		// server-owned objects have been validated and indexed.
		if len(report.Periods) == 0 || len(report.Periods) > maxPeriodCount || len(report.Fields) == 0 || len(report.Fields) > maxFieldCount {
			return domainError("bundle_invalid_report", fmt.Sprintf("report %s has invalid period or field count", report.ReportID))
		}
		if err := validateEffectiveInterval(report.EffectiveFrom, report.EffectiveTo); err != nil {
			return domainError("bundle_invalid_effective_interval", fmt.Sprintf("report %s: %v", report.ReportID, err))
		}
		periods := make(map[string]bool, len(report.Periods))
		for _, period := range report.Periods {
			if err := validateBundleString(period.Key, "period.key", maxPeriodKeyLength, true); err != nil {
				return domainError("bundle_invalid_period", fmt.Sprintf("report %s contains an invalid, unbounded, or duplicate period", report.ReportID))
			}
			if err := validateBundleString(period.Label, "period.label", maxPeriodLabelLength, true); err != nil {
				return domainError("bundle_invalid_period", fmt.Sprintf("report %s contains an invalid, unbounded, or duplicate period", report.ReportID))
			}
			if len(period.Start) != len("2006-01-02") || len(period.End) != len("2006-01-02") || periods[period.Key] || !validDateRange(period.Start, period.End) {
				return domainError("bundle_invalid_period", fmt.Sprintf("report %s contains an invalid, unbounded, or duplicate period", report.ReportID))
			}
			periods[period.Key] = true
		}
		fields := make(map[string]bool, len(report.Fields))
		orders := make(map[int]bool, len(report.Fields))
		for _, field := range report.Fields {
			reportResources[field.ResourceID] = true
			for _, item := range []struct {
				value    string
				name     string
				max      int
				required bool
			}{
				{field.FieldID, "field_id", maxFieldIDLength, true},
				{field.ResourceID, "resource_id", maxResourceIDLength, true},
				{field.ExternalResourceID, "external_resource_id", maxResourceIDLength, true},
				{field.SubresourceID, "subresource_id", maxResourceIDLength, true},
				{field.Locator, "locator", maxLocatorLength, true},
				{field.Unit, "unit", maxUnitLength, false},
				{field.Scale, "scale", maxScaleLength, false},
				{field.PercentBasis, "percent_basis", maxScaleLength, false},
				{field.Timezone, "timezone", maxTimezoneLength, false},
			} {
				if err := validateBundleString(item.value, item.name, item.max, item.required); err != nil {
					return domainError("bundle_invalid_field", fmt.Sprintf("report %s field %s: %v", report.ReportID, field.FieldID, err))
				}
			}
			if field.Order <= 0 || field.Order > maxFieldCount || fields[field.FieldID] || orders[field.Order] {
				return domainError("bundle_duplicate_or_invalid_field", fmt.Sprintf("report %s contains an invalid or duplicate field", report.ReportID))
			}
			if field.Precision < 0 || field.Precision > maxPrecision || field.MappingRevision < 0 || field.MappingRevision > maxRevision {
				return domainError("bundle_invalid_field_bounds", fmt.Sprintf("field %s has an out-of-bounds precision or mapping revision", field.FieldID))
			}
			if !knownValueKind(field.Kind) {
				return domainError("bundle_invalid_field_kind", fmt.Sprintf("field %s has unsupported kind %q", field.FieldID, field.Kind))
			}
			if field.Scale != "" && field.Scale != "ones" {
				return domainError("bundle_invalid_field_scale", fmt.Sprintf("field %s has unsupported scale %q", field.FieldID, field.Scale))
			}
			if field.Kind == ValueCurrency && !currencyPattern.MatchString(field.Unit) {
				return domainError("bundle_invalid_field_unit", fmt.Sprintf("field %s currency unit is invalid", field.FieldID))
			}
			if field.Kind == ValuePercent && field.PercentBasis != "0..1" && field.PercentBasis != "0..100" {
				return domainError("bundle_invalid_percent_basis", fmt.Sprintf("field %s percent basis is invalid", field.FieldID))
			}
			fields[field.FieldID] = true
			orders[field.Order] = true
		}
		suppliedContentHash := report.ContentHash
		report.ContentHash = ""
		canonical, err := CanonicalJSON(*report)
		if err != nil {
			return wrapError("bundle_canonicalization_failed", "report revision could not be canonicalized", err)
		}
		calculatedContentHash := digestHex(HashBytes(canonical))
		if suppliedContentHash != "" && suppliedContentHash != calculatedContentHash {
			return domainError("bundle_hash_invalid", fmt.Sprintf("report %s content hash does not match", report.ReportID))
		}
		report.ContentHash = calculatedContentHash
		byReport[report.ReportID] = append(byReport[report.ReportID], *report)
	}
	for reportID, revisions := range byReport {
		sort.Slice(revisions, func(i, j int) bool { return revisions[i].Revision < revisions[j].Revision })
		for i := 1; i < len(revisions); i++ {
			previous, current := revisions[i-1], revisions[i]
			if previous.EffectiveTo != "" && current.EffectiveFrom != "" && current.EffectiveFrom <= previous.EffectiveTo {
				return domainError("bundle_effective_interval_overlap", fmt.Sprintf("report %s revisions have overlapping effective intervals", reportID))
			}
		}
	}
	ruleSets := make(map[string]RuleSet, len(bundle.RuleSets))
	for index := range bundle.RuleSets {
		set := &bundle.RuleSets[index]
		if err := ValidateRuleSet(*set); err != nil {
			return domainError("bundle_invalid_rule_set", err.Error())
		}
		key := fmt.Sprintf("%s/%d", set.RuleSetID, set.Revision)
		if _, exists := ruleSets[key]; exists {
			return domainError("bundle_duplicate_rule_set", "rule set revisions must be unique")
		}
		if set.ContentHash == "" {
			copySet := *set
			copySet.ContentHash = ""
			canonical, _ := CanonicalJSON(copySet)
			set.ContentHash = digestHex(HashBytes(canonical))
		} else {
			copySet := *set
			supplied := copySet.ContentHash
			copySet.ContentHash = ""
			canonical, _ := CanonicalJSON(copySet)
			if supplied != digestHex(HashBytes(canonical)) {
				return domainError("bundle_hash_invalid", "rule set content hash does not match")
			}
		}
		ruleSets[key] = *set
	}
	policies := make(map[string]MaterialityPolicy, len(bundle.MaterialityPolicies))
	for index := range bundle.MaterialityPolicies {
		policy := &bundle.MaterialityPolicies[index]
		if err := ValidateMaterialityPolicy(*policy); err != nil {
			return domainError("bundle_invalid_materiality_policy", err.Error())
		}
		key := fmt.Sprintf("%s/%d", policy.PolicyID, policy.Revision)
		if _, exists := policies[key]; exists {
			return domainError("bundle_duplicate_materiality_policy", "materiality policy revisions must be unique")
		}
		if policy.Revision > maxRevision {
			return domainError("bundle_invalid_materiality_policy", "materiality policy revision is out of bounds")
		}
		policies[key] = *policy
	}
	profiles := make(map[string]ExportProfile, len(bundle.ExportProfiles))
	for index := range bundle.ExportProfiles {
		profile := &bundle.ExportProfiles[index]
		if err := ValidateExportProfile(*profile); err != nil {
			return domainError("bundle_invalid_export_profile", err.Error())
		}
		key := fmt.Sprintf("%s/%d", profile.ProfileID, profile.Revision)
		if _, exists := profiles[key]; exists {
			return domainError("bundle_duplicate_export_profile", "export profile revisions must be unique")
		}
		profiles[key] = *profile
	}
	retentions := make(map[string]RetentionPolicy, len(bundle.RetentionPolicies))
	for index := range bundle.RetentionPolicies {
		policy := &bundle.RetentionPolicies[index]
		if err := ValidateRetentionPolicy(*policy); err != nil {
			return domainError("bundle_invalid_retention_policy", err.Error())
		}
		if policy.TenantID != "" && policy.TenantID != bundle.TenantID {
			return domainError("bundle_tenant_mismatch", "retention policy tenant does not match bundle tenant")
		}
		if _, exists := retentions[policy.RetentionClass]; exists {
			return domainError("bundle_duplicate_retention_policy", "retention classes must be unique")
		}
		retentions[policy.RetentionClass] = *policy
	}
	if len(bundle.RelationshipAllowlist) > maxAllowlistEntries {
		return domainError("bundle_collection_bound", "relationship allowlist contains too many entries")
	}
	allowlistKeys := make(map[string]bool, len(bundle.RelationshipAllowlist))
	lastAllowlistRevision := make(map[string]int, len(bundle.RelationshipAllowlist))
	latestAllowlist := make(map[string]RelationshipAllowlistEntry, len(bundle.RelationshipAllowlist))
	for index := range bundle.RelationshipAllowlist {
		entry := &bundle.RelationshipAllowlist[index]
		for _, item := range []struct {
			value string
			name  string
			max   int
		}{
			{entry.EntryID, "relationship_allowlist.entry_id", maxReferenceLength},
			{entry.ActorID, "relationship_allowlist.actor_id", maxReferenceLength},
			{entry.ResourceID, "relationship_allowlist.resource_id", maxResourceIDLength},
		} {
			if err := validateBundleString(item.value, item.name, item.max, true); err != nil {
				return domainError("bundle_invalid_relationship_allowlist", err.Error())
			}
		}
		if entry.Capability != RelationshipCapabilityRead {
			return domainError("bundle_invalid_relationship_allowlist", "relationship allowlist capability is unsupported")
		}
		if !reportResources[entry.ResourceID] {
			return domainError("bundle_reference_unresolved", fmt.Sprintf("relationship allowlist %s references an unavailable report resource", entry.EntryID))
		}
		key := fmt.Sprintf("%s/%d", entry.EntryID, entry.Revision)
		if entry.Revision <= 0 || entry.Revision > maxRevision || allowlistKeys[key] {
			return domainError("bundle_duplicate_relationship_allowlist", "relationship allowlist IDs and bounded positive revisions must be unique")
		}
		if previous, exists := lastAllowlistRevision[entry.EntryID]; exists && entry.Revision <= previous {
			return domainError("bundle_revision_non_monotonic", fmt.Sprintf("relationship allowlist %s revisions must increase in declared order", entry.EntryID))
		}
		allowlistKeys[key] = true
		lastAllowlistRevision[entry.EntryID] = entry.Revision
		suppliedHash := entry.ContentHash
		entry.ContentHash = ""
		canonical, err := CanonicalJSON(*entry)
		if err != nil {
			return wrapError("bundle_canonicalization_failed", "relationship allowlist revision could not be canonicalized", err)
		}
		calculatedHash := digestHex(HashBytes(canonical))
		if suppliedHash != "" && suppliedHash != calculatedHash {
			return domainError("bundle_hash_invalid", fmt.Sprintf("relationship allowlist %s content hash does not match", entry.EntryID))
		}
		entry.ContentHash = calculatedHash
		latestAllowlist[entry.EntryID] = *entry
	}
	activeGrantTuples := make(map[string]bool, len(latestAllowlist))
	for _, entry := range latestAllowlist {
		tuple := entry.ActorID + "\x00" + entry.Capability + "\x00" + entry.ResourceID
		if activeGrantTuples[tuple] {
			return domainError("bundle_duplicate_relationship_allowlist", "active relationship allowlist grants must be unique")
		}
		activeGrantTuples[tuple] = true
	}
	for _, report := range bundle.Reports {
		if report.RuleSetID != "" && !hasLatestRuleSet(ruleSets, report.RuleSetID) {
			return domainError("bundle_reference_unresolved", fmt.Sprintf("report %s references an unavailable rule set", report.ReportID))
		}
		if report.RuleSetID != "" {
			approved := make(map[string]bool, len(report.Fields))
			for _, field := range report.Fields {
				approved[field.FieldID] = true
			}
			for _, set := range ruleSets {
				if set.RuleSetID != report.RuleSetID {
					continue
				}
				for _, rule := range set.Rules {
					for _, fieldID := range rule.FieldIDs {
						if !approved[fieldID] {
							return domainError("bundle_reference_unresolved", fmt.Sprintf("rule %s references an unapproved field", rule.RuleID))
						}
					}
					if rule.TargetFieldID != "" && !approved[rule.TargetFieldID] {
						return domainError("bundle_reference_unresolved", fmt.Sprintf("rule %s references an unapproved target field", rule.RuleID))
					}
				}
			}
		}
		if report.MaterialityPolicyID != "" && !hasLatestMaterialityPolicy(policies, report.MaterialityPolicyID) {
			return domainError("bundle_reference_unresolved", fmt.Sprintf("report %s references an unavailable materiality policy", report.ReportID))
		}
		for _, profileID := range report.ExportProfiles {
			if !hasLatestExportProfile(profiles, profileID) {
				return domainError("bundle_reference_unresolved", fmt.Sprintf("report %s references an unavailable export profile", report.ReportID))
			}
		}
		if len(bundle.RetentionPolicies) > 0 && retentions[report.RetentionClass].RetentionClass == "" {
			return domainError("bundle_reference_unresolved", fmt.Sprintf("report %s references an unavailable retention policy", report.ReportID))
		}
	}
	conversionPolicies := map[string]ConversionPolicy{}
	for i := range bundle.ConversionPolicies {
		p := &bundle.ConversionPolicies[i]
		if err := validateConversionPolicy(*p); err != nil {
			return domainError("bundle_invalid_conversion_policy", err.Error())
		}
		key := fmt.Sprintf("%s/%d", p.PolicyID, p.Revision)
		if _, ok := conversionPolicies[key]; ok {
			return domainError("bundle_duplicate_conversion_policy", "conversion policy revisions must be unique")
		}
		canonical, err := CanonicalJSON(conversionPolicyContent(*p))
		if err != nil {
			return err
		}
		hash := digestHex(HashBytes(canonical))
		if p.ContentHash != "" && p.ContentHash != hash {
			return domainError("bundle_hash_invalid", "conversion policy content hash does not match")
		}
		p.ContentHash = hash
		conversionPolicies[key] = *p
	}
	seenRoutes := map[string]bool{}
	for i := range bundle.TransferRoutes {
		r := &bundle.TransferRoutes[i]
		if err := validateTransferRoute(*r); err != nil {
			return domainError("bundle_invalid_transfer_route", err.Error())
		}
		key := fmt.Sprintf("%s/%d", r.RouteID, r.Revision)
		if seenRoutes[key] {
			return domainError("bundle_duplicate_transfer_route", "transfer route revisions must be unique")
		}
		seenRoutes[key] = true
		if _, ok := conversionPolicies[fmt.Sprintf("%s/%d", r.ConversionPolicyID, r.ConversionPolicyVersion)]; !ok {
			return domainError("bundle_reference_unresolved", "transfer route conversion policy exact revision is missing")
		}
		for _, reference := range r.ExportProfiles {
			approved, exists := profiles[fmt.Sprintf("%s/%d", reference.ProfileID, reference.Revision)]
			if !exists {
				return domainError("bundle_reference_unresolved", "transfer route references an unavailable export profile")
			}
			permitted := false
			for _, subject := range approved.PermittedSubjects {
				if subject == "transfer" {
					permitted = true
					break
				}
			}
			if !permitted {
				return domainError("bundle_reference_unresolved", "transfer route export profile does not permit transfer subjects")
			}
		}
		canonical, err := CanonicalJSON(transferRouteContent(*r))
		if err != nil {
			return err
		}
		hash := digestHex(HashBytes(canonical))
		if r.ContentHash != "" && r.ContentHash != hash {
			return domainError("bundle_hash_invalid", "transfer route content hash does not match")
		}
		r.ContentHash = hash
	}
	if len(bundle.DestinationProfiles) > 256 || len(bundle.ContentAccessPolicies) > 256 || len(bundle.ContentRetentionPolicies) > 64 || len(bundle.ContentResourcePolicies) > 256 || len(bundle.ContentProviderContracts) > 64 {
		return domainError("bundle_collection_bound", "content policy collection exceeds its bound")
	}
	accessKeys := map[string]bool{}
	lastAccessRevision := map[string]int{}
	for i := range bundle.ContentAccessPolicies {
		p := &bundle.ContentAccessPolicies[i]
		if err := validateContentAccessPolicy(p, bundle.TenantID); err != nil {
			return domainError("bundle_invalid_content_access_policy", err.Error())
		}
		key := fmt.Sprintf("%s/%d", p.PolicyID, p.Revision)
		if accessKeys[key] {
			return domainError("bundle_duplicate_content_access_policy", "content access policy revisions must be unique")
		}
		accessKeys[key] = true
		if p.Revision <= lastAccessRevision[p.PolicyID] {
			return domainError("bundle_revision_non_monotonic", "content access policy revisions must increase in declared order")
		}
		lastAccessRevision[p.PolicyID] = p.Revision
	}
	retentionKeys := map[string]bool{}
	lastRetentionRevision := map[string]int{}
	for i := range bundle.ContentRetentionPolicies {
		p := &bundle.ContentRetentionPolicies[i]
		if err := validateContentRetentionPolicy(p); err != nil {
			return domainError("bundle_invalid_content_retention_policy", err.Error())
		}
		key := fmt.Sprintf("%s/%d", p.PolicyID, p.Revision)
		if retentionKeys[key] {
			return domainError("bundle_duplicate_content_retention_policy", "content retention policy revisions must be unique")
		}
		retentionKeys[key] = true
		if p.Revision <= lastRetentionRevision[p.PolicyID] {
			return domainError("bundle_revision_non_monotonic", "content retention policy revisions must increase in declared order")
		}
		lastRetentionRevision[p.PolicyID] = p.Revision
	}
	resourcePolicies := map[string]ContentResourcePolicy{}
	lastResourceRevision := map[string]int{}
	for i := range bundle.ContentResourcePolicies {
		p := &bundle.ContentResourcePolicies[i]
		if err := validateContentResourcePolicy(p); err != nil {
			return domainError("bundle_invalid_content_resource_policy", err.Error())
		}
		key := fmt.Sprintf("%s/%d", p.PolicyID, p.Revision)
		if _, exists := resourcePolicies[key]; exists {
			return domainError("bundle_duplicate_content_resource_policy", "content resource policy revisions must be unique")
		}
		resourcePolicies[key] = *p
		if p.Revision <= lastResourceRevision[p.PolicyID] {
			return domainError("bundle_revision_non_monotonic", "content resource policy revisions must increase in declared order")
		}
		lastResourceRevision[p.PolicyID] = p.Revision
		authorized := false
		for _, a := range bundle.ContentAccessPolicies {
			if a.PolicyID == p.AccessPolicyID && a.Revision == p.AccessPolicyRevision && a.ContentHash == p.AccessPolicyContentHash && a.State == "active" && contentPolicyContains(a.Capabilities, p.Capability) && contentPolicyContains(a.ResourceIDs, p.ResourceID) {
				authorized = true
			}
		}
		if !authorized {
			return domainError("bundle_reference_unresolved", "content resource policy access scope is unresolved")
		}
	}
	providerContracts := map[string]ContentProviderContract{}
	lastProviderRevision := map[string]int{}
	for i := range bundle.ContentProviderContracts {
		p := &bundle.ContentProviderContracts[i]
		if err := validateContentProviderContract(p); err != nil {
			return domainError("bundle_invalid_content_provider_contract", err.Error())
		}
		key := fmt.Sprintf("%s/%d", p.PolicyID, p.Revision)
		if _, exists := providerContracts[key]; exists {
			return domainError("bundle_duplicate_content_provider_contract", "provider contract revisions must be unique")
		}
		providerContracts[key] = *p
		if p.Revision <= lastProviderRevision[p.PolicyID] {
			return domainError("bundle_revision_non_monotonic", "provider contract revisions must increase in declared order")
		}
		lastProviderRevision[p.PolicyID] = p.Revision
	}
	profileRevisions := map[string]bool{}
	lastProfileRevision := map[string]int{}
	for i := range bundle.DestinationProfiles {
		p := &bundle.DestinationProfiles[i]
		if err := validateDestinationProfile(p); err != nil {
			return domainError("bundle_invalid_destination_profile", err.Error())
		}
		key := fmt.Sprintf("%s/%d", *p.DestinationProfileID, *p.Revision)
		if profileRevisions[key] {
			return domainError("bundle_duplicate_destination_profile", "destination profile revisions must be unique")
		}
		profileRevisions[key] = true
		if *p.Revision <= lastProfileRevision[*p.DestinationProfileID] {
			return domainError("bundle_revision_non_monotonic", "destination profile revisions must increase in declared order")
		}
		lastProfileRevision[*p.DestinationProfileID] = *p.Revision
		cp := resourcePolicies[fmt.Sprintf("%s/%d", p.ResourcePolicy.PolicyID, p.ResourcePolicy.Revision)]
		provider := providerContracts[fmt.Sprintf("%s/%d", p.ProviderContract.PolicyID, p.ProviderContract.Revision)]
		convFound := false
		for _, conv := range bundle.ConversionPolicies {
			if conv.PolicyID == p.ConversionPolicy.PolicyID && conv.Revision == p.ConversionPolicy.Revision && conv.ContentHash == p.ConversionPolicy.ContentHash {
				convFound = true
			}
		}
		if cp.ContentHash != p.ResourcePolicy.ContentHash || cp.State != "active" || cp.ResourceID != *p.ResourceID || cp.SheetID != *p.SheetID || cp.Cell != *p.Cell || provider.ContentHash != p.ProviderContract.ContentHash || provider.State != "active" || !contentPolicyContains(provider.SupportedOperations, *p.Operation) || !convFound {
			return domainError("bundle_reference_unresolved", "destination profile exact resource, conversion, or provider policy reference is unresolved")
		}
		metadata := p.MetadataRequirements
		for _, requirement := range []struct {
			name  string
			value *string
		}{{"formula", metadata.Formula}, {"protection", metadata.Protection}, {"writability", metadata.Writability}, {"formatting", metadata.Formatting}, {"period", metadata.Period}, {"currency", metadata.Currency}, {"unit", metadata.Unit}, {"scale", metadata.Scale}, {"percent_basis", metadata.PercentBasis}, {"precision", metadata.Precision}} {
			if *requirement.value == "provider_verified" && !contentPolicyContains(provider.RequiredFacts, requirement.name) {
				return domainError("bundle_reference_unresolved", fmt.Sprintf("provider contract does not declare required fact %q", requirement.name))
			}
		}
		retentionFound := false
		for j := range bundle.ContentRetentionPolicies {
			rp := &bundle.ContentRetentionPolicies[j]
			if rp.PolicyID == p.RetentionPolicy.PolicyID && rp.Revision == p.RetentionPolicy.Revision && rp.ContentHash == p.RetentionPolicy.ContentHash && rp.State == "active" {
				retentionFound = true
			}
		}
		if !retentionFound {
			return domainError("bundle_reference_unresolved", "destination profile retention reference is unresolved")
		}
	}
	return nil
}

func hasLatestRuleSet(values map[string]RuleSet, id string) bool {
	for key, value := range values {
		if value.RuleSetID == id && strings.HasPrefix(key, id+"/") {
			return true
		}
	}
	return false
}
func hasLatestMaterialityPolicy(values map[string]MaterialityPolicy, id string) bool {
	for _, value := range values {
		if value.PolicyID == id {
			return true
		}
	}
	return false
}
func hasLatestExportProfile(values map[string]ExportProfile, id string) bool {
	for _, value := range values {
		if value.ProfileID == id {
			return true
		}
	}
	return false
}

func validateBundleString(value, name string, max int, required bool) error {
	if required && value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > max {
		return fmt.Errorf("%s exceeds %d bytes", name, max)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s contains a control character", name)
		}
	}
	return nil
}

func validateEffectiveInterval(from, to string) error {
	if from == "" && to == "" {
		return nil
	}
	if from == "" || to == "" {
		return errors.New("effective_from and effective_to must be supplied together")
	}
	fromDate, fromErr := time.Parse("2006-01-02", from)
	toDate, toErr := time.Parse("2006-01-02", to)
	if fromErr != nil || toErr != nil || toDate.Before(fromDate) {
		return errors.New("effective interval must be an ordered ISO date range")
	}
	return nil
}

func knownValueKind(kind ValueKind) bool {
	switch kind {
	case ValueBlank, ValueText, ValueInteger, ValueNumber, ValueBoolean, ValueDate, ValueDateTime, ValueCurrency, ValuePercent, ValueError:
		return true
	default:
		return false
	}
}

func validDateRange(start, end string) bool {
	startTime, startErr := time.Parse("2006-01-02", start)
	endTime, endErr := time.Parse("2006-01-02", end)
	return startErr == nil && endErr == nil && !endTime.Before(startTime)
}

// StageBundle atomically stores a fully validated candidate without requesting activation.
func (s *Store) StageBundle(ctx context.Context, bundle *ValidatedBundle) error {
	if bundle == nil {
		return domainError("bundle_invalid", "validated bundle is required")
	}
	if len(bundle.verifiedBy) != ed25519.PublicKeySize {
		return domainError("bundle_signature_invalid", "staging requires a bundle returned by signature validation")
	}
	tenant := identity.StorageTenant(ctx)
	// ValidatedBundle exposes mutable fields for compatibility. Rebuild the
	// authoritative projection from the signed canonical bytes before any
	// database lookup, and reject callers that changed either representation.
	verified, err := ValidateBundle(bundle.CanonicalJSON, bundle.Signature, tenant, bundle.verifiedBy)
	if err != nil || verified.ContentHash != bundle.ContentHash || !bytes.Equal(verified.CanonicalJSON, bundle.CanonicalJSON) {
		return domainError("bundle_signature_invalid", "staging bundle bytes, signature, or stored hash failed revalidation")
	}
	providedProjection, err := CanonicalJSON(bundle.Bundle)
	if err != nil {
		return domainError("bundle_invalid", "staging bundle projection cannot be canonicalized")
	}
	verifiedProjection, err := CanonicalJSON(verified.Bundle)
	if err != nil || !bytes.Equal(providedProjection, verifiedProjection) {
		return domainError("bundle_invalid", "staging bundle projection does not match its signed canonical bytes")
	}
	bundle = verified
	if bundle.Bundle.TenantID != tenant {
		return domainError("bundle_tenant_mismatch", "bundle tenant does not match trusted tenant")
	}
	var candidateHash string
	candidateErr := s.db.QueryRowContext(ctx, `SELECT content_hash FROM assurance_bundle_candidates WHERE tenant_id=? AND bundle_id=? AND bundle_version=?`, tenant, bundle.Bundle.BundleID, bundle.Bundle.BundleVersion).Scan(&candidateHash)
	if candidateErr == nil {
		if candidateHash != bundle.ContentHash {
			return domainError("bundle_candidate_conflict", "reused bundle candidate revision has different content")
		}
		return nil
	}
	if !errors.Is(candidateErr, sql.ErrNoRows) {
		return fmt.Errorf("assurance: inspect bundle candidate: %w", candidateErr)
	}
	var activeVersion sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT bundle_version FROM assurance_active_bundles WHERE tenant_id=? AND singleton=1`, tenant).Scan(&activeVersion); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("assurance: read active bundle: %w", err)
	}
	if activeVersion.Valid && int64(bundle.Bundle.BundleVersion) <= activeVersion.Int64 {
		return domainError("bundle_revision_non_monotonic", "candidate bundle version must exceed active version")
	}
	for _, report := range bundle.Bundle.Reports {
		var existingRevision int
		var existingHash string
		err := s.db.QueryRowContext(ctx, `SELECT revision, content_hash FROM assurance_report_revisions
 WHERE tenant_id=? AND report_id=? ORDER BY revision DESC LIMIT 1`, tenant, report.ReportID).Scan(&existingRevision, &existingHash)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("assurance: inspect report revision: %w", err)
		}
		if err == nil && (report.Revision < existingRevision || report.Revision == existingRevision && report.ContentHash != existingHash) {
			return domainError("bundle_revision_non_monotonic", fmt.Sprintf("report %s revision is not a valid immutable successor", report.ReportID))
		}
	}
	for _, policy := range bundle.Bundle.ConversionPolicies {
		var latest int
		var hash string
		err := s.db.QueryRowContext(ctx, `SELECT revision,content_hash FROM assurance_conversion_policy_revisions WHERE tenant_id=? AND policy_id=? ORDER BY revision DESC LIMIT 1`, tenant, policy.PolicyID).Scan(&latest, &hash)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && (policy.Revision < latest || policy.Revision == latest && policy.ContentHash != hash) {
			return domainError("bundle_revision_non_monotonic", "conversion policy revision is not a valid immutable successor")
		}
	}
	for _, route := range bundle.Bundle.TransferRoutes {
		var latestRevision int
		var existing string
		err := s.db.QueryRowContext(ctx, `SELECT revision, content_hash FROM assurance_transfer_route_revisions WHERE tenant_id=? AND route_id=? ORDER BY revision DESC LIMIT 1`, tenant, route.RouteID).Scan(&latestRevision, &existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && (route.Revision < latestRevision || (route.Revision == latestRevision && route.ContentHash != existing)) {
			return domainError("bundle_revision_non_monotonic", "transfer route revision is not a valid immutable successor")
		}
	}
	checkRevision := func(table, idColumn string, id string, revision int, contentHash string) error {
		var existingRevision int
		var existingHash string
		err := s.db.QueryRowContext(ctx, `SELECT revision, content_hash FROM `+table+` WHERE tenant_id=? AND `+idColumn+`=? ORDER BY revision DESC LIMIT 1`, tenant, id).Scan(&existingRevision, &existingHash)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if revision < existingRevision || (revision == existingRevision && existingHash != contentHash) {
			return domainError("bundle_revision_non_monotonic", fmt.Sprintf("%s revision is not a valid immutable successor", id))
		}
		return nil
	}
	for _, set := range bundle.Bundle.RuleSets {
		if err := checkRevision("assurance_rule_sets", "rule_set_id", set.RuleSetID, set.Revision, set.ContentHash); err != nil {
			return err
		}
	}
	for _, policy := range bundle.Bundle.MaterialityPolicies {
		raw, _ := marshalCanonical(policy)
		if err := checkRevision("assurance_materiality_policies", "policy_id", policy.PolicyID, policy.Revision, digestHex(HashBytes([]byte(raw)))); err != nil {
			return err
		}
	}
	for _, profile := range bundle.Bundle.ExportProfiles {
		raw, _ := marshalCanonical(profile)
		if err := checkRevision("assurance_export_profiles", "profile_id", profile.ProfileID, profile.Revision, digestHex(HashBytes([]byte(raw)))); err != nil {
			return err
		}
	}
	for _, policy := range bundle.Bundle.RetentionPolicies {
		copyPolicy := policy
		copyPolicy.TenantID = tenant
		raw, _ := marshalCanonical(copyPolicy)
		incomingHash := digestHex(HashBytes([]byte(raw)))
		var existingRevision, existingDuration int
		var existingHash string
		err := s.db.QueryRowContext(ctx, `SELECT revision, duration_seconds, content_hash FROM assurance_retention_policy_revisions WHERE tenant_id=? AND retention_class=? ORDER BY revision DESC LIMIT 1`, tenant, policy.RetentionClass).Scan(&existingRevision, &existingDuration, &existingHash)
		if err == nil && retentionRevisionConflict(existingRevision, existingHash, incomingHash, policy.Revision) {
			return domainError("bundle_revision_non_monotonic", "retention policy revision is not a valid immutable successor")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var reusedRevision int
		if err := s.db.QueryRowContext(ctx, `SELECT revision FROM assurance_retention_policy_revisions WHERE tenant_id=? AND retention_class=? AND content_hash=? AND revision<>? LIMIT 1`, tenant, policy.RetentionClass, incomingHash, policy.Revision).Scan(&reusedRevision); err == nil {
			return domainError("bundle_revision_content_reused", "retention policy content cannot be reused under a different revision")
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	for _, entry := range bundle.Bundle.RelationshipAllowlist {
		if err := checkRevision("assurance_relationship_allowlist_revisions", "entry_id", entry.EntryID, entry.Revision, entry.ContentHash); err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var stagedHash string
	err = tx.QueryRowContext(ctx, `SELECT content_hash FROM assurance_bundle_candidates WHERE tenant_id=? AND bundle_id=? AND bundle_version=?`, tenant, bundle.Bundle.BundleID, bundle.Bundle.BundleVersion).Scan(&stagedHash)
	if err == nil {
		if stagedHash != bundle.ContentHash {
			return domainError("bundle_candidate_conflict", "reused bundle candidate revision has different content")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for _, p := range bundle.Bundle.DestinationProfiles {
		if err := checkContentPolicyRevision(ctx, tx, tenant, bundle.verifiedBy, "destination_profile", *p.DestinationProfileID, *p.Revision, *p.ContentHash); err != nil {
			return err
		}
	}
	for _, p := range bundle.Bundle.ContentAccessPolicies {
		if err := checkContentPolicyRevision(ctx, tx, tenant, bundle.verifiedBy, "content_access", p.PolicyID, p.Revision, p.ContentHash); err != nil {
			return err
		}
	}
	for _, p := range bundle.Bundle.ContentRetentionPolicies {
		if err := checkContentPolicyRevision(ctx, tx, tenant, bundle.verifiedBy, "content_retention", p.PolicyID, p.Revision, p.ContentHash); err != nil {
			return err
		}
	}
	for _, p := range bundle.Bundle.ContentResourcePolicies {
		if err := checkContentPolicyRevision(ctx, tx, tenant, bundle.verifiedBy, "content_resource", p.PolicyID, p.Revision, p.ContentHash); err != nil {
			return err
		}
	}
	for _, p := range bundle.Bundle.ContentProviderContracts {
		if err := checkContentPolicyRevision(ctx, tx, tenant, bundle.verifiedBy, "content_provider", p.PolicyID, p.Revision, p.ContentHash); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO assurance_bundle_candidates
 (tenant_id, bundle_id, bundle_version, schema_version, bundle_json, signature_hex, content_hash, activation_requested, staged_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?)
		ON CONFLICT(tenant_id, bundle_id, bundle_version) DO NOTHING`, tenant, bundle.Bundle.BundleID, bundle.Bundle.BundleVersion,
		bundle.Bundle.SchemaVersion, string(bundle.CanonicalJSON), hex.EncodeToString(bundle.Signature), bundle.ContentHash, formatTimestamp(time.Now()))
	if err != nil {
		return fmt.Errorf("assurance: stage bundle: %w", err)
	}
	var committedHash string
	if err := tx.QueryRowContext(ctx, `SELECT content_hash FROM assurance_bundle_candidates WHERE tenant_id=? AND bundle_id=? AND bundle_version=?`, tenant, bundle.Bundle.BundleID, bundle.Bundle.BundleVersion).Scan(&committedHash); err != nil {
		return err
	}
	if committedHash != bundle.ContentHash {
		return domainError("bundle_candidate_conflict", "reused bundle candidate revision has different content")
	}
	return tx.Commit()
}

type contentPolicyHistoryReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func checkContentPolicyRevision(ctx context.Context, reader contentPolicyHistoryReader, tenant string, trustedKey ed25519.PublicKey, kind, id string, revision int, hash string) (returnErr error) {
	rows, err := reader.QueryContext(ctx, `SELECT bundle_json,signature_hex,content_hash FROM assurance_bundle_candidates WHERE tenant_id=? UNION ALL SELECT bundle_json,signature_hex,content_hash FROM assurance_active_bundles WHERE tenant_id=? AND singleton=1`, tenant, tenant)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && returnErr == nil {
			returnErr = closeErr
		}
	}()
	for rows.Next() {
		var raw, signatureHex, storedHash string
		if err := rows.Scan(&raw, &signatureHex, &storedHash); err != nil {
			return err
		}
		signature, err := hex.DecodeString(signatureHex)
		if err != nil {
			return domainError("content_policy_history_corrupt", "stored signed policy history signature is malformed")
		}
		validated, err := ValidateBundle([]byte(raw), signature, tenant, trustedKey)
		if err != nil || validated.ContentHash != storedHash {
			return domainError("content_policy_history_corrupt", "stored signed policy history failed signature or hash verification")
		}
		prior := validated.Bundle
		check := func(priorID string, priorRevision int, priorHash string) error {
			if priorID != id {
				return nil
			}
			if priorRevision > revision || priorRevision == revision && priorHash != hash {
				return domainError("bundle_revision_hash_conflict", "content policy revision is not a monotonic immutable successor")
			}
			if priorRevision != revision && priorHash == hash {
				return domainError("bundle_revision_content_reused", "content policy hash cannot be reused under another revision")
			}
			return nil
		}
		switch kind {
		case "destination_profile":
			for _, p := range prior.DestinationProfiles {
				if p.DestinationProfileID != nil && p.Revision != nil && p.ContentHash != nil {
					if err := check(*p.DestinationProfileID, *p.Revision, *p.ContentHash); err != nil {
						return err
					}
				}
			}
		case "content_access":
			for _, p := range prior.ContentAccessPolicies {
				raw, _ := CanonicalJSON(contentAccessPolicyContent(p))
				h := digestHex(HashBytes(raw))
				if p.ContentHash != "" && p.ContentHash != h {
					return domainError("content_policy_history_corrupt", "stored access policy hash is inconsistent")
				}
				if err := check(p.PolicyID, p.Revision, h); err != nil {
					return err
				}
			}
		case "content_retention":
			for _, p := range prior.ContentRetentionPolicies {
				raw, _ := CanonicalJSON(contentRetentionPolicyContent(p))
				h := digestHex(HashBytes(raw))
				if p.ContentHash != "" && p.ContentHash != h {
					return domainError("content_policy_history_corrupt", "stored retention policy hash is inconsistent")
				}
				if err := check(p.PolicyID, p.Revision, h); err != nil {
					return err
				}
			}
		case "content_resource":
			for _, p := range prior.ContentResourcePolicies {
				raw, _ := CanonicalJSON(contentResourcePolicyContent(p))
				h := digestHex(HashBytes(raw))
				if p.ContentHash != "" && p.ContentHash != h {
					return domainError("content_policy_history_corrupt", "stored resource policy hash is inconsistent")
				}
				if err := check(p.PolicyID, p.Revision, h); err != nil {
					return err
				}
			}
		case "content_provider":
			for _, p := range prior.ContentProviderContracts {
				raw, _ := CanonicalJSON(contentProviderContractContent(p))
				h := digestHex(HashBytes(raw))
				if p.ContentHash != "" && p.ContentHash != h {
					return domainError("content_policy_history_corrupt", "stored provider contract hash is inconsistent")
				}
				if err := check(p.PolicyID, p.Revision, h); err != nil {
					return err
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

// RequestActivation atomically marks one complete staged candidate for next-start activation.
func (s *Store) RequestActivation(ctx context.Context, bundleID string, bundleVersion int) error {
	tenant := identity.StorageTenant(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE assurance_bundle_candidates SET activation_requested=0, requested_at='' WHERE tenant_id=?`, tenant); err != nil {
		return fmt.Errorf("assurance: clear activation request: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE assurance_bundle_candidates SET activation_requested=1, requested_at=?
 WHERE tenant_id=? AND bundle_id=? AND bundle_version=?`, formatTimestamp(time.Now()), tenant, bundleID, bundleVersion)
	if err != nil {
		return fmt.Errorf("assurance: request activation: %w", err)
	}
	if err := requireOneTransition(result); err != nil {
		return domainError("bundle_candidate_missing", "staged candidate was not found")
	}
	return tx.Commit()
}

// Bootstrap verifies and atomically activates a requested candidate. Invalid
// requested candidates force readiness false even when an older bundle exists.
func (s *Store) Bootstrap(ctx context.Context, expectedTenant string, publicKey ed25519.PublicKey) error {
	tenant := identity.StorageTenant(ctx)
	if tenant != expectedTenant {
		err := domainError("bundle_tenant_mismatch", "trusted startup tenant does not match configured tenant")
		s.setReadiness(false, errors.Join(ErrNotReady, err))
		return errors.Join(ErrNotReady, err)
	}
	if _, err := s.ExpireStaleReservations(ctx, time.Now().UTC()); err != nil {
		return s.failReadiness(fmt.Errorf("assurance: startup reservation recovery: %w", err))
	}
	type candidate struct {
		id           string
		version      int
		raw          string
		signatureHex string
		hash         string
	}
	rows, err := s.db.QueryContext(ctx, `SELECT bundle_id, bundle_version, bundle_json, signature_hex, content_hash
 FROM assurance_bundle_candidates WHERE tenant_id=? AND activation_requested=1 ORDER BY requested_at`, tenant)
	if err != nil {
		return s.failReadiness(err)
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.version, &item.raw, &item.signatureHex, &item.hash); err != nil {
			_ = rows.Close()
			return s.failReadiness(err)
		}
		candidates = append(candidates, item)
	}
	if err := rows.Close(); err != nil {
		return s.failReadiness(err)
	}
	if len(candidates) > 1 {
		return s.failReadiness(domainError("bundle_activation_ambiguous", "multiple activation requests exist"))
	}
	if len(candidates) == 0 {
		return s.bootstrapPriorActive(ctx, tenant, publicKey)
	}
	item := candidates[0]
	signature, err := hex.DecodeString(item.signatureHex)
	if err != nil {
		return s.failReadiness(domainError("bundle_signature_invalid", "candidate signature encoding is invalid"))
	}
	validated, err := ValidateBundle([]byte(item.raw), signature, expectedTenant, publicKey)
	if err != nil || validated.ContentHash != item.hash || validated.Bundle.BundleID != item.id || validated.Bundle.BundleVersion != item.version {
		if err == nil {
			err = domainError("bundle_hash_invalid", "candidate metadata or hash does not match signed bundle")
		}
		return s.failReadiness(err)
	}
	if err := s.activateBundle(ctx, validated); err != nil {
		return s.failReadiness(err)
	}
	s.setReadiness(true, nil)
	return nil
}

func (s *Store) bootstrapPriorActive(ctx context.Context, tenant string, publicKey ed25519.PublicKey) error {
	var raw, signatureHex, storedHash string
	err := s.db.QueryRowContext(ctx, `SELECT bundle_json, signature_hex, content_hash FROM assurance_active_bundles
 WHERE tenant_id=? AND singleton=1`, tenant).Scan(&raw, &signatureHex, &storedHash)
	if errors.Is(err, sql.ErrNoRows) {
		return s.failReadiness(domainError("active_bundle_missing", "no valid active bundle is available"))
	}
	if err != nil {
		return s.failReadiness(err)
	}
	signature, err := hex.DecodeString(signatureHex)
	if err != nil {
		return s.failReadiness(err)
	}
	validated, err := ValidateBundle([]byte(raw), signature, tenant, publicKey)
	if err != nil || validated.ContentHash != storedHash {
		if err == nil {
			err = domainError("bundle_hash_invalid", "active bundle hash does not match")
		}
		return s.failReadiness(err)
	}
	if err := s.activateBundle(ctx, validated); err != nil {
		return s.failReadiness(err)
	}
	s.setReadiness(true, nil)
	return nil
}

func (s *Store) failReadiness(cause error) error {
	err := errors.Join(ErrNotReady, cause)
	s.setReadiness(false, err)
	return err
}

func (s *Store) activateBundle(ctx context.Context, bundle *ValidatedBundle) error {
	tenant := identity.StorageTenant(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE assurance_report_definitions SET status='inactive' WHERE tenant_id=?`, tenant); err != nil {
		return fmt.Errorf("assurance: deactivate omitted reports: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE assurance_report_revisions SET status='inactive' WHERE tenant_id=?`, tenant); err != nil {
		return fmt.Errorf("assurance: deactivate historical revisions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM assurance_active_bundle_objects WHERE tenant_id=?`, tenant); err != nil {
		return fmt.Errorf("assurance: clear active bundle object set: %w", err)
	}
	addActiveObject := func(kind, id string, revision int, version int) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO assurance_active_bundle_objects (tenant_id, object_kind, object_id, object_revision, bundle_version) VALUES (?, ?, ?, ?, ?)`, tenant, kind, id, revision, version)
		return err
	}
	for _, policy := range bundle.Bundle.ConversionPolicies {
		raw, err := marshalCanonical(policy)
		if err != nil {
			return err
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT content_hash FROM assurance_conversion_policy_revisions WHERE tenant_id=? AND policy_id=? AND revision=?`, tenant, policy.PolicyID, policy.Revision).Scan(&existing)
		if err == nil && existing != policy.ContentHash {
			return domainError("bundle_revision_hash_conflict", "conversion policy revision content changed")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_conversion_policy_revisions (tenant_id,policy_id,revision,content_hash,policy_json) VALUES (?,?,?,?,?)`, tenant, policy.PolicyID, policy.Revision, policy.ContentHash, raw); err != nil {
				return err
			}
		}
		if err := addActiveObject("conversion_policy", policy.PolicyID, policy.Revision, bundle.Bundle.BundleVersion); err != nil {
			return err
		}
	}
	for _, route := range bundle.Bundle.TransferRoutes {
		raw, err := marshalCanonical(route)
		if err != nil {
			return err
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT content_hash FROM assurance_transfer_route_revisions WHERE tenant_id=? AND route_id=? AND revision=?`, tenant, route.RouteID, route.Revision).Scan(&existing)
		if err == nil && existing != route.ContentHash {
			return domainError("bundle_revision_hash_conflict", "transfer route revision content changed")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_transfer_route_revisions (tenant_id,route_id,revision,content_hash,route_json) VALUES (?,?,?,?,?)`, tenant, route.RouteID, route.Revision, route.ContentHash, raw); err != nil {
				return err
			}
		}
		if err := addActiveObject("transfer_route", route.RouteID, route.Revision, bundle.Bundle.BundleVersion); err != nil {
			return err
		}
	}
	for _, ruleSet := range bundle.Bundle.RuleSets {
		definitionJSON, err := marshalCanonical(ruleSet)
		if err != nil {
			return err
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT content_hash FROM assurance_rule_sets WHERE tenant_id=? AND rule_set_id=? AND revision=?`, tenant, ruleSet.RuleSetID, ruleSet.Revision).Scan(&existing)
		if err == nil && existing != ruleSet.ContentHash {
			return domainError("bundle_revision_hash_conflict", "rule set revision content changed")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_rule_sets (tenant_id, rule_set_id, revision, content_hash, status, definition_json) VALUES (?, ?, ?, ?, ?, ?)`, tenant, ruleSet.RuleSetID, ruleSet.Revision, ruleSet.ContentHash, ruleSet.Status, definitionJSON); err != nil {
				return err
			}
			for index, rule := range ruleOrder(ruleSet) {
				ruleJSON, err := marshalCanonical(rule)
				if err != nil {
					return err
				}
				order := rule.Order
				if order == 0 {
					order = index + 1
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_rule_set_rules (tenant_id, rule_set_id, revision, rule_id, rule_order, rule_json) VALUES (?, ?, ?, ?, ?, ?)`, tenant, ruleSet.RuleSetID, ruleSet.Revision, rule.RuleID, order, ruleJSON); err != nil {
					return err
				}
			}
		}
		if err := addActiveObject("rule_set", ruleSet.RuleSetID, ruleSet.Revision, bundle.Bundle.BundleVersion); err != nil {
			return err
		}
	}
	for _, policy := range bundle.Bundle.MaterialityPolicies {
		policyJSON, err := marshalCanonical(policy)
		if err != nil {
			return err
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT content_hash FROM assurance_materiality_policies WHERE tenant_id=? AND policy_id=? AND revision=?`, tenant, policy.PolicyID, policy.Revision).Scan(&existing)
		contentHash := digestHex(HashBytes([]byte(policyJSON)))
		if err == nil && existing != contentHash {
			return domainError("bundle_revision_hash_conflict", "materiality policy revision content changed")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_materiality_policies (tenant_id, policy_id, revision, status, policy_json, content_hash) VALUES (?, ?, ?, ?, ?, ?)`, tenant, policy.PolicyID, policy.Revision, policy.Status, policyJSON, contentHash); err != nil {
				return err
			}
		}
		if err := addActiveObject("materiality_policy", policy.PolicyID, policy.Revision, bundle.Bundle.BundleVersion); err != nil {
			return err
		}
	}
	for _, profile := range bundle.Bundle.ExportProfiles {
		profileJSON, err := marshalCanonical(profile)
		if err != nil {
			return err
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT content_hash FROM assurance_export_profiles WHERE tenant_id=? AND profile_id=? AND revision=?`, tenant, profile.ProfileID, profile.Revision).Scan(&existing)
		contentHash := digestHex(HashBytes([]byte(profileJSON)))
		if err == nil && existing != contentHash {
			return domainError("bundle_revision_hash_conflict", "export profile revision content changed")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_export_profiles (tenant_id, profile_id, revision, status, profile_json, content_hash) VALUES (?, ?, ?, ?, ?, ?)`, tenant, profile.ProfileID, profile.Revision, profile.Status, profileJSON, contentHash); err != nil {
				return err
			}
		}
		if err := addActiveObject("export_profile", profile.ProfileID, profile.Revision, bundle.Bundle.BundleVersion); err != nil {
			return err
		}
	}
	for _, policy := range bundle.Bundle.RetentionPolicies {
		policy.TenantID = tenant
		policyJSON, err := marshalCanonical(policy)
		if err != nil {
			return err
		}
		contentHash := digestHex(HashBytes([]byte(policyJSON)))
		// The v8 singleton is compatibility history only. Revision/hash
		// monotonicity is checked against assurance_retention_policy_revisions.
		if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_retention_policies (tenant_id, retention_class, duration_seconds, policy_json, content_hash) VALUES (?, ?, ?, ?, ?) ON CONFLICT(tenant_id, retention_class) DO NOTHING`, tenant, policy.RetentionClass, policy.DurationSeconds, policyJSON, contentHash); err != nil {
			return err
		}
		var existingRevisionHash string
		err = tx.QueryRowContext(ctx, `SELECT content_hash FROM assurance_retention_policy_revisions WHERE tenant_id=? AND retention_class=? AND revision=?`, tenant, policy.RetentionClass, policy.Revision).Scan(&existingRevisionHash)
		var latestRevision int
		var latestHash string
		latestErr := tx.QueryRowContext(ctx, `SELECT revision, content_hash FROM assurance_retention_policy_revisions WHERE tenant_id=? AND retention_class=? ORDER BY revision DESC LIMIT 1`, tenant, policy.RetentionClass).Scan(&latestRevision, &latestHash)
		if latestErr != nil && !errors.Is(latestErr, sql.ErrNoRows) {
			return latestErr
		}
		if latestErr == nil && retentionRevisionConflict(latestRevision, latestHash, contentHash, policy.Revision) {
			return domainError("bundle_revision_non_monotonic", "retention policy revision is not a valid immutable successor")
		}
		var reusedRevision int
		if reuseErr := tx.QueryRowContext(ctx, `SELECT revision FROM assurance_retention_policy_revisions WHERE tenant_id=? AND retention_class=? AND content_hash=? AND revision<>? LIMIT 1`, tenant, policy.RetentionClass, contentHash, policy.Revision).Scan(&reusedRevision); reuseErr == nil {
			return domainError("bundle_revision_content_reused", "retention policy content cannot be reused under a different revision")
		} else if !errors.Is(reuseErr, sql.ErrNoRows) {
			return reuseErr
		}
		if err == nil && existingRevisionHash != contentHash {
			return domainError("bundle_revision_hash_conflict", "retention policy revision content changed")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_retention_policy_revisions (tenant_id, retention_class, revision, duration_seconds, policy_json, content_hash) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(tenant_id, retention_class, revision) DO NOTHING`, tenant, policy.RetentionClass, policy.Revision, policy.DurationSeconds, policyJSON, contentHash); err != nil {
			return err
		}
		if err := addActiveObject("retention_policy", policy.RetentionClass, policy.Revision, bundle.Bundle.BundleVersion); err != nil {
			return err
		}
	}
	latestAllowlistRevision := make(map[string]int, len(bundle.Bundle.RelationshipAllowlist))
	latestAllowlistHash := make(map[string]string, len(bundle.Bundle.RelationshipAllowlist))
	for _, entry := range bundle.Bundle.RelationshipAllowlist {
		if entry.Revision > latestAllowlistRevision[entry.EntryID] {
			latestAllowlistRevision[entry.EntryID] = entry.Revision
			latestAllowlistHash[entry.EntryID] = entry.ContentHash
		}
	}
	for entryID, incomingRevision := range latestAllowlistRevision {
		var storedRevision int
		var storedHash string
		err := tx.QueryRowContext(ctx, `SELECT revision, content_hash FROM assurance_relationship_allowlist_revisions
 WHERE tenant_id=? AND entry_id=? ORDER BY revision DESC LIMIT 1`, tenant, entryID).Scan(&storedRevision, &storedHash)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && (incomingRevision < storedRevision || incomingRevision == storedRevision && latestAllowlistHash[entryID] != storedHash) {
			return domainError("bundle_revision_non_monotonic", fmt.Sprintf("relationship allowlist %s revision is not a valid immutable successor", entryID))
		}
	}
	for _, entry := range bundle.Bundle.RelationshipAllowlist {
		definitionJSON, err := marshalCanonical(entry)
		if err != nil {
			return err
		}
		var existingHash string
		err = tx.QueryRowContext(ctx, `SELECT content_hash FROM assurance_relationship_allowlist_revisions WHERE tenant_id=? AND entry_id=? AND revision=?`, tenant, entry.EntryID, entry.Revision).Scan(&existingHash)
		if err == nil && existingHash != entry.ContentHash {
			return domainError("bundle_revision_hash_conflict", "relationship allowlist revision content changed")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_relationship_allowlist_revisions
 (tenant_id, entry_id, revision, actor_id, capability, resource_id, definition_json, content_hash, provisioned_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, tenant, entry.EntryID, entry.Revision, entry.ActorID, entry.Capability,
				entry.ResourceID, definitionJSON, entry.ContentHash, formatTimestamp(time.Now())); err != nil {
				return err
			}
		}
		if entry.Revision == latestAllowlistRevision[entry.EntryID] {
			if err := addActiveObject("relationship_allowlist", entry.EntryID, entry.Revision, bundle.Bundle.BundleVersion); err != nil {
				return err
			}
		}
	}
	latestByReport := make(map[string]int, len(bundle.Bundle.Reports))
	for _, report := range bundle.Bundle.Reports {
		if report.Revision > latestByReport[report.ReportID] {
			latestByReport[report.ReportID] = report.Revision
		}
	}
	for _, report := range bundle.Bundle.Reports {
		active := report.Revision == latestByReport[report.ReportID]
		status := "inactive"
		if active {
			if err := addActiveObject("report", report.ReportID, report.Revision, bundle.Bundle.BundleVersion); err != nil {
				return err
			}
			status = "active"
		}
		var existingHash string
		err := tx.QueryRowContext(ctx, `SELECT content_hash FROM assurance_report_revisions
 WHERE tenant_id=? AND report_id=? AND revision=?`, tenant, report.ReportID, report.Revision).Scan(&existingHash)
		existing := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("assurance: inspect report revision %s/%d: %w", report.ReportID, report.Revision, err)
		}
		if existing && existingHash != report.ContentHash {
			return domainError("bundle_revision_hash_conflict", fmt.Sprintf("report %s revision %d has a different content hash", report.ReportID, report.Revision))
		}
		if !existing {
			periodsJSON, _ := marshalCanonical(report.Periods)
			exportsJSON, _ := marshalCanonical(report.ExportProfiles)
			if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_report_revisions
 (tenant_id, report_id, revision, name, description, owner, periods_json, resource_policy_hash, rule_set_id,
  materiality_policy_id, export_profiles_json, retention_class, effective_from, effective_to, content_hash, status)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, tenant, report.ReportID, report.Revision, report.Name,
				report.Description, report.Owner, periodsJSON, report.ResourcePolicyHash, report.RuleSetID, report.MaterialityPolicyID,
				exportsJSON, report.RetentionClass, report.EffectiveFrom, report.EffectiveTo, report.ContentHash, status); err != nil {
				return fmt.Errorf("assurance: insert report revision %s/%d: %w", report.ReportID, report.Revision, err)
			}
			fields := append([]FieldDefinition(nil), report.Fields...)
			sort.Slice(fields, func(i, j int) bool { return fields[i].Order < fields[j].Order })
			for _, field := range fields {
				if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_report_fields
 (tenant_id, report_id, revision, field_id, field_order, required, resource_id, external_resource_id, subresource_id,
  locator, value_kind, unit, scale, precision_value, percent_basis, timezone, mapping_revision)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, tenant, report.ReportID, report.Revision, field.FieldID,
					field.Order, field.Required, field.ResourceID, field.ExternalResourceID, field.SubresourceID, field.Locator,
					field.Kind, field.Unit, field.Scale, field.Precision, field.PercentBasis, field.Timezone, field.MappingRevision); err != nil {
					return fmt.Errorf("assurance: insert report field %s: %w", field.FieldID, err)
				}
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE assurance_report_revisions SET status=? WHERE tenant_id=? AND report_id=? AND revision=?`, status, tenant, report.ReportID, report.Revision); err != nil {
			return fmt.Errorf("assurance: reactivate unchanged report %s/%d: %w", report.ReportID, report.Revision, err)
		}
		if active {
			if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_report_definitions
 (tenant_id, report_id, name, description, owner, active_revision, status) VALUES (?, ?, ?, ?, ?, ?, 'active')
 ON CONFLICT(tenant_id, report_id) DO UPDATE SET name=excluded.name, description=excluded.description,
 owner=excluded.owner, active_revision=excluded.active_revision, status='active'`, tenant, report.ReportID, report.Name,
				report.Description, report.Owner, report.Revision); err != nil {
				return fmt.Errorf("assurance: activate report %s: %w", report.ReportID, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_active_bundles
 (tenant_id, singleton, bundle_id, bundle_version, schema_version, bundle_json, signature_hex, content_hash, activated_at)
 VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT(tenant_id, singleton) DO UPDATE SET bundle_id=excluded.bundle_id, bundle_version=excluded.bundle_version,
 schema_version=excluded.schema_version, bundle_json=excluded.bundle_json, signature_hex=excluded.signature_hex,
 content_hash=excluded.content_hash, activated_at=excluded.activated_at`, tenant, bundle.Bundle.BundleID, bundle.Bundle.BundleVersion,
		bundle.Bundle.SchemaVersion, string(bundle.CanonicalJSON), hex.EncodeToString(bundle.Signature), bundle.ContentHash, formatTimestamp(time.Now())); err != nil {
		return fmt.Errorf("assurance: activate bundle pointer: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE assurance_bundle_candidates SET activation_requested=0 WHERE tenant_id=? AND bundle_id=? AND bundle_version=?`, tenant, bundle.Bundle.BundleID, bundle.Bundle.BundleVersion); err != nil {
		return err
	}
	return tx.Commit()
}

// ResolveReport resolves one active revision, exact named period, and an
// approved field subset. Empty fieldIDs selects all required fields.
func (s *Store) ResolveReport(ctx context.Context, reportID string, requested Period, fieldIDs []string) (ReportRevision, error) {
	if err := s.Ready(); err != nil {
		return ReportRevision{}, err
	}
	tenant := identity.StorageTenant(ctx)
	var report ReportRevision
	var periodsJSON, exportsJSON string
	err := s.db.QueryRowContext(ctx, `SELECT r.report_id, r.revision, r.name, r.description, r.owner, r.status,
 r.retention_class, r.resource_policy_hash, r.rule_set_id, r.materiality_policy_id, r.export_profiles_json,
 r.effective_from, r.effective_to, r.periods_json, r.content_hash
 FROM assurance_report_definitions d JOIN assurance_report_revisions r
 ON r.tenant_id=d.tenant_id AND r.report_id=d.report_id AND r.revision=d.active_revision
 WHERE d.tenant_id=? AND d.report_id=? AND d.status='active' AND r.status='active'`, tenant, reportID).Scan(&report.ReportID, &report.Revision,
		&report.Name, &report.Description, &report.Owner, &report.Status, &report.RetentionClass, &report.ResourcePolicyHash,
		&report.RuleSetID, &report.MaterialityPolicyID, &exportsJSON, &report.EffectiveFrom, &report.EffectiveTo, &periodsJSON, &report.ContentHash)
	if errors.Is(err, sql.ErrNoRows) {
		return ReportRevision{}, domainError("report_not_found", "approved report was not found")
	}
	if err != nil {
		return ReportRevision{}, wrapError("report_resolution_failed", "approved report could not be resolved", err)
	}
	if err := json.Unmarshal([]byte(periodsJSON), &report.Periods); err != nil {
		return ReportRevision{}, wrapError("report_integrity_failed", "stored period policy is invalid", err)
	}
	if err := json.Unmarshal([]byte(exportsJSON), &report.ExportProfiles); err != nil {
		return ReportRevision{}, wrapError("report_integrity_failed", "stored export profiles are invalid", err)
	}
	period, err := resolvePeriod(report.Periods, requested)
	if err != nil {
		return ReportRevision{}, err
	}
	report.Periods = []Period{period}
	rows, err := s.db.QueryContext(ctx, `SELECT field_id, resource_id, external_resource_id, subresource_id, locator,
 value_kind, unit, scale, precision_value, percent_basis, timezone, required, field_order, mapping_revision
 FROM assurance_report_fields WHERE tenant_id=? AND report_id=? AND revision=? ORDER BY field_order`, tenant, report.ReportID, report.Revision)
	if err != nil {
		return ReportRevision{}, err
	}
	defer func() { _ = rows.Close() }()
	all := make(map[string]FieldDefinition)
	var order []string
	for rows.Next() {
		var field FieldDefinition
		if err := rows.Scan(&field.FieldID, &field.ResourceID, &field.ExternalResourceID, &field.SubresourceID, &field.Locator,
			&field.Kind, &field.Unit, &field.Scale, &field.Precision, &field.PercentBasis, &field.Timezone, &field.Required, &field.Order, &field.MappingRevision); err != nil {
			return ReportRevision{}, err
		}
		all[field.FieldID] = field
		order = append(order, field.FieldID)
	}
	selected := make(map[string]bool)
	if len(fieldIDs) == 0 {
		for _, id := range order {
			if all[id].Required {
				selected[id] = true
			}
		}
	} else {
		for _, id := range fieldIDs {
			if selected[id] {
				return ReportRevision{}, domainError("field_selection_invalid", "field_ids contains a duplicate")
			}
			if _, ok := all[id]; !ok {
				return ReportRevision{}, domainError("field_not_approved", "requested field is not in the approved report revision")
			}
			selected[id] = true
		}
	}
	for _, id := range order {
		if selected[id] {
			report.Fields = append(report.Fields, all[id])
		}
	}
	if len(report.Fields) == 0 {
		return ReportRevision{}, domainError("field_selection_empty", "report selection contains no fields")
	}
	return report, nil
}

func resolvePeriod(periods []Period, requested Period) (Period, error) {
	if requested.Key == "" {
		return Period{}, domainError("period_required", "period.key is required")
	}
	for _, period := range periods {
		if period.Key != requested.Key {
			continue
		}
		if (requested.Label != "" && requested.Label != period.Label) || (requested.Start != "" && requested.Start != period.Start) || (requested.End != "" && requested.End != period.End) {
			return Period{}, domainError("period_mismatch", "period metadata does not match the approved named period")
		}
		return period, nil
	}
	return Period{}, domainError("period_not_found", "approved named period was not found")
}

// ParsePublicKey accepts a hex-encoded detached Ed25519 public key.
func ParsePublicKey(value string) (ed25519.PublicKey, error) {
	decoded, err := hex.DecodeString(strings.TrimSpace(value))
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid Ed25519 public key")
	}
	return ed25519.PublicKey(decoded), nil
}
