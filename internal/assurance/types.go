package assurance

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ValueKind is the closed assurance value vocabulary.
type ValueKind string

const (
	ValueBlank    ValueKind = "blank"
	ValueText     ValueKind = "text"
	ValueInteger  ValueKind = "integer"
	ValueNumber   ValueKind = "number"
	ValueBoolean  ValueKind = "boolean"
	ValueDate     ValueKind = "date"
	ValueDateTime ValueKind = "datetime"
	ValueCurrency ValueKind = "currency"
	ValuePercent  ValueKind = "percent"
	ValueError    ValueKind = "error"
)

var decimalPattern = regexp.MustCompile(`^[+-]?[0-9]+(?:\.[0-9]+)?$`)
var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// TypedValue preserves semantic differences that display strings lose.
type TypedValue struct {
	Kind             ValueKind `json:"kind"`
	Text             string    `json:"text,omitempty"`
	Number           string    `json:"number,omitempty"`
	Boolean          bool      `json:"boolean,omitempty"`
	Date             string    `json:"date,omitempty"`
	DateTime         string    `json:"datetime,omitempty"`
	Unit             string    `json:"unit,omitempty"`
	Scale            string    `json:"scale,omitempty"`
	Precision        int       `json:"precision,omitempty"`
	PercentBasis     string    `json:"percent_basis,omitempty"`
	Timezone         string    `json:"timezone,omitempty"`
	Formula          bool      `json:"formula"`
	FormulaText      string    `json:"formula_text,omitempty"`
	Calculated       bool      `json:"calculated,omitempty"`
	CalculatedSource string    `json:"calculated_source,omitempty"`
	ErrorCode        string    `json:"error_code,omitempty"`
}

// ProviderValue is an uncached provider scalar before assurance normalization.
type ProviderValue struct {
	Value           any
	Formula         string
	CalculatedValue any
	ErrorCode       string
}

// ProviderRevision records only provider-returned revisions as verified.
type ProviderRevision struct {
	Value    string           `json:"value,omitempty"`
	Strength RevisionStrength `json:"strength"`
}

// RevisionStrength distinguishes provider evidence from local best effort.
type RevisionStrength string

const (
	RevisionVerified    RevisionStrength = "verified"
	RevisionBestEffort  RevisionStrength = "best_effort"
	RevisionUnavailable RevisionStrength = "unavailable"
)

// Period is a server-approved named reporting period.
type Period struct {
	Key   string `json:"key" yaml:"key"`
	Label string `json:"label,omitempty" yaml:"label"`
	Start string `json:"start,omitempty" yaml:"start"`
	End   string `json:"end,omitempty" yaml:"end"`
}

// FieldDefinition binds a stable field to one exact approved Workiva locator.
type FieldDefinition struct {
	FieldID            string    `json:"field_id" yaml:"field_id"`
	ResourceID         string    `json:"resource_id" yaml:"resource_id"`
	ExternalResourceID string    `json:"external_resource_id" yaml:"external_resource_id"`
	SubresourceID      string    `json:"subresource_id" yaml:"subresource_id"`
	Locator            string    `json:"locator" yaml:"locator"`
	Kind               ValueKind `json:"kind" yaml:"kind"`
	Unit               string    `json:"unit,omitempty" yaml:"unit,omitempty"`
	Scale              string    `json:"scale,omitempty" yaml:"scale,omitempty"`
	Precision          int       `json:"precision,omitempty" yaml:"precision,omitempty"`
	PercentBasis       string    `json:"percent_basis,omitempty" yaml:"percent_basis,omitempty"`
	Timezone           string    `json:"timezone,omitempty" yaml:"timezone,omitempty"`
	Required           bool      `json:"required" yaml:"required"`
	Order              int       `json:"order" yaml:"order"`
	MappingRevision    int       `json:"mapping_revision,omitempty" yaml:"mapping_revision,omitempty"`
}

// CanonicalDecimal returns canonical base-10 text for valid decimal input.
// Invalid input is returned unchanged; callers that accept untrusted values use
// canonicalDecimal, which reports an error.
func CanonicalDecimal(value string) string {
	canonical, err := canonicalDecimal(value)
	if err != nil {
		return value
	}
	return canonical
}

func canonicalDecimal(value string) (string, error) {
	if !decimalPattern.MatchString(value) {
		return "", fmt.Errorf("invalid canonical decimal %q", value)
	}
	negative := strings.HasPrefix(value, "-")
	value = strings.TrimPrefix(strings.TrimPrefix(value, "+"), "-")
	parts := strings.SplitN(value, ".", 2)
	whole := strings.TrimLeft(parts[0], "0")
	if whole == "" {
		whole = "0"
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = strings.TrimRight(parts[1], "0")
	}
	if fraction != "" {
		whole += "." + fraction
	}
	if negative && whole != "0" {
		whole = "-" + whole
	}
	return whole, nil
}

// NormalizeProviderValue converts a provider scalar according to the approved
// field definition without locale inference or binary floating point.
func NormalizeProviderValue(input ProviderValue, field FieldDefinition) (TypedValue, error) {
	out := TypedValue{Kind: field.Kind, Unit: field.Unit, Scale: field.Scale, Precision: field.Precision, PercentBasis: field.PercentBasis, Timezone: field.Timezone}
	value := input.Value
	if input.ErrorCode != "" {
		if field.Kind != ValueError {
			return TypedValue{}, fmt.Errorf("provider error %q for non-error field", input.ErrorCode)
		}
		out.ErrorCode = input.ErrorCode
		return out, nil
	}
	if input.Formula != "" {
		if !strings.HasPrefix(input.Formula, "=") {
			return TypedValue{}, fmt.Errorf("formula must begin with =")
		}
		if input.CalculatedValue == nil {
			return TypedValue{}, fmt.Errorf("formula has no calculated value")
		}
		out.Formula = true
		out.FormulaText = input.Formula
		out.Calculated = true
		out.CalculatedSource = "provider"
		value = input.CalculatedValue
	}

	switch field.Kind {
	case ValueBlank:
		if value != nil && value != "" {
			return TypedValue{}, fmt.Errorf("nonblank value for blank field")
		}
		return out, nil
	case ValueText:
		text, ok := value.(string)
		if !ok {
			return TypedValue{}, fmt.Errorf("text field received %T", value)
		}
		out.Text = text
		return out, nil
	case ValueBoolean:
		boolean, ok := value.(bool)
		if !ok {
			return TypedValue{}, fmt.Errorf("boolean field received %T", value)
		}
		out.Boolean = boolean
		return out, nil
	case ValueInteger, ValueNumber, ValueCurrency, ValuePercent:
		decimal, err := scalarDecimal(value)
		if err != nil {
			return TypedValue{}, err
		}
		if field.Kind == ValueInteger && strings.Contains(decimal, ".") {
			return TypedValue{}, fmt.Errorf("integer field received non-integer %q", decimal)
		}
		if field.Kind == ValueCurrency && !currencyPattern.MatchString(field.Unit) {
			return TypedValue{}, fmt.Errorf("currency requires an uppercase ISO 4217 unit")
		}
		if field.Kind == ValuePercent && field.PercentBasis != "0..1" && field.PercentBasis != "0..100" {
			return TypedValue{}, fmt.Errorf("percent requires basis 0..1 or 0..100")
		}
		out.Number = decimal
		return out, nil
	case ValueDate:
		text, ok := value.(string)
		if !ok {
			return TypedValue{}, fmt.Errorf("date field received %T", value)
		}
		parsed, err := time.Parse("2006-01-02", text)
		if err != nil || parsed.Format("2006-01-02") != text {
			return TypedValue{}, fmt.Errorf("date must be a valid ISO calendar date")
		}
		out.Date = text
		return out, nil
	case ValueDateTime:
		text, ok := value.(string)
		if !ok {
			return TypedValue{}, fmt.Errorf("datetime field received %T", value)
		}
		parsed, err := time.Parse(time.RFC3339, text)
		if err != nil {
			return TypedValue{}, fmt.Errorf("datetime must be RFC3339 with an explicit timezone")
		}
		out.DateTime = parsed.UTC().Format(time.RFC3339Nano)
		out.Timezone = "UTC"
		return out, nil
	case ValueError:
		return TypedValue{}, fmt.Errorf("error field requires provider error_code")
	default:
		return TypedValue{}, fmt.Errorf("unsupported value kind %q", field.Kind)
	}
}

func scalarDecimal(value any) (string, error) {
	var text string
	switch typed := value.(type) {
	case json.Number:
		text = typed.String()
	case string:
		text = typed
	case int:
		text = strconv.Itoa(typed)
	case int64:
		text = strconv.FormatInt(typed, 10)
	case nil:
		return "", fmt.Errorf("missing numeric value")
	default:
		return "", fmt.Errorf("numeric field received %T; provider must preserve JSON number text", value)
	}
	return canonicalDecimal(text)
}
