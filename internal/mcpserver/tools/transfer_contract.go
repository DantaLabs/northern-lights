package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// transferContractInput is deliberately transport-local. It describes the
// public union without coupling the public MCP boundary to the private
// transfer service request types. In particular, confirmation_token and
// idempotency_key are never copied into an output or service request here.
type transferContractInput struct {
	Phase                 string                   `json:"phase"`
	Source                *transferEndpointInput   `json:"source"`
	Target                *transferTargetInput     `json:"target"`
	ConversionPolicyID    string                   `json:"conversion_policy_id"`
	Reason                string                   `json:"reason"`
	TransferID            string                   `json:"transfer_id"`
	ConfirmationToken     string                   `json:"confirmation_token"`
	Observation           string                   `json:"observation"`
	Refreshed             *bool                    `json:"refreshed"`
	RefreshOverrideReason string                   `json:"refresh_override_reason"`
	UILocation            *transferUILocationInput `json:"ui_location"`
	ObservedValue         *transferTypedValueInput `json:"observed_value"`
	Note                  string                   `json:"note"`
	Action                string                   `json:"action"`
	Classification        string                   `json:"classification"`
	Selection             *transferSelectionInput  `json:"selection"`
	TargetTemplateID      string                   `json:"target_template_id"`
	EligibleRowIDs        []string                 `json:"eligible_row_ids"`
	MaxRows               *int                     `json:"max_rows"`
	MaxCells              *int                     `json:"max_cells"`
	IdempotencyKey        string                   `json:"idempotency_key"`
}

type transferEndpointInput struct {
	MappingID  string `json:"mapping_id"`
	ResourceID string `json:"resource_id"`
	Locator    string `json:"locator"`
}

type transferTargetInput struct {
	ResourceID string `json:"resource_id"`
	Locator    string `json:"locator"`
}

type transferUILocationInput struct {
	ResourceID string `json:"resource_id"`
	Locator    string `json:"locator"`
}

type transferTypedValueInput struct {
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	Number    string `json:"number"`
	Boolean   *bool  `json:"boolean"`
	Unit      string `json:"unit"`
	Scale     string `json:"scale"`
	Precision *int   `json:"precision"`
	Formula   *bool  `json:"formula"`
	ErrorCode string `json:"error_code"`
}

type transferSelectionInput struct {
	ReportFieldIDs    []string `json:"report_field_ids"`
	FrozenTransferIDs []string `json:"frozen_transfer_ids"`
	SelectionDigest   string   `json:"selection_digest"`
}

type transferContractError struct {
	Code  string
	Field string
	Msg   string
}

func (e transferContractError) Error() string { return e.Msg }

func transferContractIssue(code, field, message string) error {
	return transferContractError{Code: code, Field: field, Msg: message}
}

func decodeTransferContract(in map[string]any) (transferContractInput, error) {
	var out transferContractInput
	raw, err := json.Marshal(in)
	if err != nil {
		return out, transferContractIssue("invalid_request", "", "transfer arguments are not valid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		return out, transferContractIssue("invalid_request", "", "transfer arguments do not match the registered input schema")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return out, transferContractIssue("invalid_request", "", "transfer arguments contain trailing data")
	}
	return out, nil
}

func hasTransferField(in map[string]any, name string) bool {
	_, ok := in[name]
	return ok
}

func hasTransferPath(in map[string]any, path string) bool {
	parts := strings.Split(path, ".")
	var current any = in
	for _, part := range parts {
		object, ok := current.(map[string]any)
		if !ok {
			return false
		}
		current, ok = object[part]
		if !ok {
			return false
		}
	}
	return true
}

func validateTransferContract(in map[string]any) (transferContractInput, []string, error) {
	input, err := decodeTransferContract(in)
	if err != nil {
		return input, nil, err
	}
	if !hasTransferField(in, "phase") || input.Phase == "" {
		return input, nil, transferContractIssue("missing_required_field", "phase", "phase is required")
	}
	if !hasTransferString(input.Phase, 32) {
		return input, nil, transferContractIssue("invalid_phase", "phase", "phase is not supported")
	}
	if !hasTransferField(in, "idempotency_key") || input.IdempotencyKey == "" {
		return input, nil, transferContractIssue("missing_required_field", "idempotency_key", "idempotency_key is required")
	}
	if err := validateBoundedString("idempotency_key", input.IdempotencyKey, 256, false); err != nil {
		return input, nil, err
	}

	if err := validateTransferCommonFields(in, input); err != nil {
		return input, nil, err
	}

	ignored := []string{}
	switch input.Phase {
	case "stage":
		if err := rejectTransferFields(in, "stage", "transfer_id", "confirmation_token", "observation", "refreshed", "refresh_override_reason", "ui_location", "observed_value", "note", "action", "classification", "selection", "target_template_id", "eligible_row_ids", "max_rows", "max_cells"); err != nil {
			return input, nil, err
		}
		if input.Source == nil || !hasTransferField(in, "source") {
			return input, nil, transferContractIssue("missing_required_field", "source", "source is required")
		}
		if input.Target == nil || !hasTransferField(in, "target") {
			return input, nil, transferContractIssue("missing_required_field", "target", "target is required")
		}
		if err := validateTransferSource(in, input.Source); err != nil {
			return input, nil, err
		}
		if err := validateTransferTarget(input.Target); err != nil {
			return input, nil, err
		}
	case "confirm":
		if err := rejectTransferFields(in, "confirm", "source", "target", "conversion_policy_id", "reason", "observation", "refreshed", "refresh_override_reason", "ui_location", "observed_value", "note", "action", "classification", "selection", "target_template_id", "eligible_row_ids", "max_rows", "max_cells"); err != nil {
			return input, nil, err
		}
		if err := requireTransferOpaqueID(in, "transfer_id", input.TransferID, 128); err != nil {
			return input, nil, err
		}
		if !hasTransferField(in, "confirmation_token") || input.ConfirmationToken == "" {
			return input, nil, transferContractIssue("missing_required_field", "confirmation_token", "confirmation_token is required")
		}
		if err := validateBoundedString("confirmation_token", input.ConfirmationToken, 4096, false); err != nil {
			return input, nil, err
		}
	case "acknowledge":
		if err := rejectTransferFields(in, "acknowledge", "source", "target", "conversion_policy_id", "reason", "confirmation_token", "action", "classification", "selection", "target_template_id", "eligible_row_ids", "max_rows", "max_cells"); err != nil {
			return input, nil, err
		}
		if err := requireTransferOpaqueID(in, "transfer_id", input.TransferID, 128); err != nil {
			return input, nil, err
		}
		if !hasTransferField(in, "observation") || input.Observation == "" {
			return input, nil, transferContractIssue("missing_required_field", "observation", "observation is required")
		}
		if err := validateBoundedString("observation", input.Observation, 32, false); err != nil {
			return input, nil, err
		}
		if input.Observation != "matches" && input.Observation != "mismatch" && input.Observation != "not_visible" && input.Observation != "not_checked" {
			return input, nil, transferContractIssue("invalid_value", "observation", "observation is not supported")
		}
		if input.Refreshed == nil || !hasTransferField(in, "refreshed") {
			return input, nil, transferContractIssue("missing_required_field", "refreshed", "refreshed is required")
		}
		if (input.Observation == "matches" || input.Observation == "mismatch") && input.UILocation == nil {
			return input, nil, transferContractIssue("missing_required_field", "ui_location", "ui_location is required for this observation")
		}
		if input.UILocation != nil {
			if err := validateTransferUILocation(input.UILocation); err != nil {
				return input, nil, err
			}
		}
		if input.Observation == "matches" && !*input.Refreshed && input.RefreshOverrideReason == "" {
			return input, nil, transferContractIssue("missing_required_field", "refresh_override_reason", "refresh_override_reason is required when matches is not refreshed")
		}
	case "reconcile":
		if err := rejectTransferFields(in, "reconcile", "source", "target", "conversion_policy_id", "reason", "confirmation_token", "observation", "refreshed", "refresh_override_reason", "ui_location", "observed_value", "selection", "target_template_id", "eligible_row_ids", "max_rows", "max_cells"); err != nil {
			return input, nil, err
		}
		if err := requireTransferOpaqueID(in, "transfer_id", input.TransferID, 128); err != nil {
			return input, nil, err
		}
		if !hasTransferField(in, "action") || input.Action == "" {
			return input, nil, transferContractIssue("missing_required_field", "action", "action is required")
		}
		if input.Action != "inspect_operation" && input.Action != "read_back" && input.Action != "classify" && input.Action != "close" {
			return input, nil, transferContractIssue("invalid_value", "action", "action is not supported")
		}
		if input.Action == "inspect_operation" || input.Action == "read_back" {
			if hasTransferField(in, "classification") {
				return input, nil, transferContractIssue("forbidden_field", "classification", "classification is not valid for this reconciliation action")
			}
			if hasTransferField(in, "note") {
				if err := validateBoundedString("note", input.Note, 2000, true); err != nil {
					return input, nil, err
				}
				ignored = append(ignored, "note")
			}
		} else {
			if !hasTransferField(in, "classification") || input.Classification == "" {
				return input, nil, transferContractIssue("missing_required_field", "classification", "classification is required")
			}
			if err := validateBoundedString("classification", input.Classification, 64, false); err != nil {
				return input, nil, err
			}
			if input.Classification != "confirmed_applied" && input.Classification != "confirmed_not_applied" && input.Classification != "conflicting_value" && input.Classification != "still_unknown" && input.Classification != "provider_evidence_inconsistent" {
				return input, nil, transferContractIssue("invalid_value", "classification", "classification is not supported")
			}
			if !hasTransferField(in, "note") || input.Note == "" {
				return input, nil, transferContractIssue("missing_required_field", "note", "note is required for this reconciliation action")
			}
			if err := validateBoundedString("note", input.Note, 2000, false); err != nil {
				return input, nil, err
			}
		}
	case "bulk_stage":
		if err := rejectTransferFields(in, "bulk_stage", "source", "target", "conversion_policy_id", "reason", "transfer_id", "confirmation_token", "observation", "refreshed", "refresh_override_reason", "ui_location", "observed_value", "note", "action", "classification", "selection.frozen_transfer_ids", "selection.selection_digest", "eligible_row_ids"); err != nil {
			return input, nil, err
		}
		if input.Selection == nil || !hasTransferField(in, "selection") || len(input.Selection.ReportFieldIDs) == 0 {
			return input, nil, transferContractIssue("missing_required_field", "selection.report_field_ids", "selection.report_field_ids is required")
		}
		if err := validateTransferIDs("selection.report_field_ids", input.Selection.ReportFieldIDs, 1000, 128); err != nil {
			return input, nil, err
		}
		if err := requireTransferOpaqueID(in, "target_template_id", input.TargetTemplateID, 128); err != nil {
			return input, nil, err
		}
	case "bulk_confirm":
		if err := rejectTransferFields(in, "bulk_confirm", "source", "target", "conversion_policy_id", "reason", "transfer_id", "confirmation_token", "observation", "refreshed", "refresh_override_reason", "ui_location", "observed_value", "note", "action", "classification", "selection.report_field_ids", "target_template_id", "max_rows", "max_cells"); err != nil {
			return input, nil, err
		}
		if input.Selection == nil || !hasTransferField(in, "selection") {
			return input, nil, transferContractIssue("missing_required_field", "selection", "selection is required")
		}
		if err := validateTransferIDs("selection.frozen_transfer_ids", input.Selection.FrozenTransferIDs, 100, 128); err != nil {
			return input, nil, err
		}
		if len(input.Selection.FrozenTransferIDs) == 0 {
			return input, nil, transferContractIssue("missing_required_field", "selection.frozen_transfer_ids", "selection.frozen_transfer_ids is required")
		}
		if err := requireTransferOpaqueID(in, "selection.selection_digest", input.Selection.SelectionDigest, 128); err != nil {
			return input, nil, err
		}
		if err := validateTransferIDs("eligible_row_ids", input.EligibleRowIDs, 100, 128); err != nil {
			return input, nil, err
		}
		if len(input.EligibleRowIDs) == 0 {
			return input, nil, transferContractIssue("missing_required_field", "eligible_row_ids", "eligible_row_ids is required")
		}
	default:
		return input, nil, transferContractIssue("invalid_phase", "phase", "phase is not supported")
	}
	return input, ignored, nil
}

func validateTransferCommonFields(in map[string]any, input transferContractInput) error {
	for field, value := range map[string]string{
		"conversion_policy_id": input.ConversionPolicyID,
		"transfer_id":          input.TransferID,
		"target_template_id":   input.TargetTemplateID,
	} {
		if hasTransferField(in, field) {
			if err := validateBoundedString(field, value, 128, false); err != nil {
				return err
			}
		}
	}
	if hasTransferField(in, "reason") {
		if err := validateBoundedString("reason", input.Reason, 1000, false); err != nil {
			return err
		}
	}
	if hasTransferField(in, "note") {
		if err := validateBoundedString("note", input.Note, 2000, true); err != nil {
			return err
		}
	}
	if hasTransferField(in, "refresh_override_reason") {
		if err := validateBoundedString("refresh_override_reason", input.RefreshOverrideReason, 256, false); err != nil {
			return err
		}
	}
	if hasTransferField(in, "action") {
		if input.Action != "" {
			if err := validateBoundedString("action", input.Action, 32, false); err != nil {
				return err
			}
		}
	}
	if hasTransferField(in, "observation") {
		if input.Observation != "" {
			if err := validateBoundedString("observation", input.Observation, 32, false); err != nil {
				return err
			}
		}
	}
	if hasTransferField(in, "classification") {
		if input.Classification != "" {
			if err := validateBoundedString("classification", input.Classification, 64, false); err != nil {
				return err
			}
		}
	}
	if hasTransferField(in, "max_rows") && (input.MaxRows == nil || *input.MaxRows < 1 || *input.MaxRows > 100) {
		return transferContractIssue("invalid_bounds", "max_rows", "max_rows must be between 1 and 100")
	}
	if hasTransferField(in, "max_cells") && (input.MaxCells == nil || *input.MaxCells < 1 || *input.MaxCells > 100) {
		return transferContractIssue("invalid_bounds", "max_cells", "max_cells must be between 1 and 100")
	}
	if hasTransferField(in, "eligible_row_ids") {
		if err := validateTransferIDs("eligible_row_ids", input.EligibleRowIDs, 100, 128); err != nil {
			return err
		}
	}
	if hasTransferField(in, "observed_value") {
		if input.ObservedValue == nil {
			return transferContractIssue("invalid_request", "observed_value", "observed_value must be an object")
		}
		if err := validateTransferTypedValue(input.ObservedValue); err != nil {
			return err
		}
	}
	return nil
}

func rejectTransferFields(in map[string]any, phase string, fields ...string) error {
	for _, field := range fields {
		if hasTransferPath(in, field) {
			return transferContractIssue("forbidden_field", field, fmt.Sprintf("%s is not accepted during %s", field, phase))
		}
	}
	return nil
}

func validateTransferSource(in map[string]any, source *transferEndpointInput) error {
	if hasTransferPath(in, "source.mapping_id") && source.MappingID == "" {
		return transferContractIssue("opaque_id_invalid", "source.mapping_id", "source.mapping_id must not be empty")
	}
	if hasTransferPath(in, "source.resource_id") && source.ResourceID == "" {
		return transferContractIssue("opaque_id_invalid", "source.resource_id", "source.resource_id must not be empty")
	}
	if hasTransferPath(in, "source.locator") && source.Locator == "" {
		return transferContractIssue("invalid_value", "source.locator", "source.locator must not be empty")
	}
	hasMapping := source.MappingID != ""
	hasResource := source.ResourceID != "" || source.Locator != ""
	if hasMapping == hasResource || (hasResource && (source.ResourceID == "" || source.Locator == "")) {
		return transferContractIssue("invalid_source", "source", "source must contain mapping_id or resource_id plus locator")
	}
	if hasMapping {
		return validateOpaqueTransferID("source.mapping_id", source.MappingID, 128)
	}
	if err := validateOpaqueTransferID("source.resource_id", source.ResourceID, 128); err != nil {
		return err
	}
	return validateBoundedString("source.locator", source.Locator, 512, false)
}

func validateTransferTarget(target *transferTargetInput) error {
	if err := validateOpaqueTransferID("target.resource_id", target.ResourceID, 128); err != nil {
		return err
	}
	return validateBoundedString("target.locator", target.Locator, 512, false)
}

func validateTransferUILocation(location *transferUILocationInput) error {
	if err := validateOpaqueTransferID("ui_location.resource_id", location.ResourceID, 128); err != nil {
		return err
	}
	return validateBoundedString("ui_location.locator", location.Locator, 512, false)
}

func requireTransferOpaqueID(in map[string]any, field, value string, max int) error {
	if !hasTransferField(in, strings.SplitN(field, ".", 2)[0]) || value == "" {
		return transferContractIssue("missing_required_field", field, field+" is required")
	}
	return validateOpaqueTransferID(field, value, max)
}

func validateTransferIDs(field string, values []string, maxItems, maxLength int) error {
	if len(values) > maxItems {
		return transferContractIssue("invalid_bounds", field, fmt.Sprintf("%s must contain no more than %d items", field, maxItems))
	}
	for _, value := range values {
		if err := validateOpaqueTransferID(field, value, maxLength); err != nil {
			return err
		}
	}
	return nil
}

func validateOpaqueTransferID(field, value string, max int) error {
	if err := validateBoundedString(field, value, max, false); err != nil {
		if issue, ok := err.(transferContractError); ok && issue.Code == "invalid_value" {
			issue.Code = "opaque_id_invalid"
			return issue
		}
		return err
	}
	return nil
}

func validateBoundedString(field, value string, max int, allowEmpty bool) error {
	if !allowEmpty && value == "" {
		return transferContractIssue("invalid_value", field, field+" must not be empty")
	}
	if utf8.RuneCountInString(value) > max {
		return transferContractIssue("invalid_bounds", field, fmt.Sprintf("%s exceeds its maximum length of %d", field, max))
	}
	if strings.TrimSpace(value) != value {
		return transferContractIssue("opaque_id_invalid", field, field+" must not have surrounding whitespace")
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return transferContractIssue("invalid_value", field, field+" contains a control character")
		}
	}
	return nil
}

func hasTransferString(value string, max int) bool {
	return value != "" && utf8.RuneCountInString(value) <= max
}

func validateTransferTypedValue(value *transferTypedValueInput) error {
	if value.Kind == "" || utf8.RuneCountInString(value.Kind) > 32 {
		return transferContractIssue("invalid_value", "observed_value.kind", "observed_value.kind is required and bounded")
	}
	hasText, hasNumber, hasBoolean := value.Text != "", value.Number != "", value.Boolean != nil
	hasUnit, hasScale, hasPrecision := value.Unit != "", value.Scale != "", value.Precision != nil
	hasError := value.ErrorCode != ""
	switch value.Kind {
	case "text", "date", "datetime":
		if !hasText || hasNumber || hasBoolean || hasUnit || hasScale || hasPrecision || hasError {
			return transferContractIssue("invalid_typed_value", "observed_value", "observed_value members do not match its kind")
		}
		if err := validateBoundedString("observed_value.text", value.Text, 4096, false); err != nil {
			return err
		}
	case "number", "integer", "currency", "percent":
		if !hasNumber || hasText || hasBoolean || hasError {
			return transferContractIssue("invalid_typed_value", "observed_value", "observed_value members do not match its kind")
		}
		if err := validateBoundedString("observed_value.number", value.Number, 256, false); err != nil {
			return err
		}
	case "boolean":
		if !hasBoolean || hasText || hasNumber || hasUnit || hasScale || hasPrecision || hasError {
			return transferContractIssue("invalid_typed_value", "observed_value", "observed_value members do not match its kind")
		}
	case "error":
		if !hasError || hasText || hasNumber || hasBoolean || hasUnit || hasScale || hasPrecision {
			return transferContractIssue("invalid_typed_value", "observed_value", "observed_value members do not match its kind")
		}
		if err := validateBoundedString("observed_value.error_code", value.ErrorCode, 128, false); err != nil {
			return err
		}
	case "blank":
		if hasText || hasNumber || hasBoolean || hasUnit || hasScale || hasPrecision || hasError {
			return transferContractIssue("invalid_typed_value", "observed_value", "observed_value members do not match its kind")
		}
	default:
		return transferContractIssue("invalid_typed_value", "observed_value.kind", "observed_value.kind is not supported")
	}
	if value.Precision != nil && (*value.Precision < 0 || *value.Precision > 38) {
		return transferContractIssue("invalid_bounds", "observed_value.precision", "observed_value.precision must be between 0 and 38")
	}
	for field, text := range map[string]string{"observed_value.unit": value.Unit, "observed_value.scale": value.Scale} {
		if text != "" {
			if err := validateBoundedString(field, text, map[string]int{"observed_value.unit": 128, "observed_value.scale": 64}[field], false); err != nil {
				return err
			}
		}
	}
	return nil
}
