package workivaprovider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

// ContentMetadata is a direct, single-cell observation. Presence flags are
// authoritative: a missing value never implies a blank cell or no formula.
type ContentMetadata struct {
	SpreadsheetID                  string                    `json:"spreadsheet_id"`
	SheetID                        string                    `json:"sheet_id"`
	Locator                        string                    `json:"locator"`
	RawValue                       json.RawMessage           `json:"raw_value,omitempty"`
	ValuePresent                   bool                      `json:"value_present"`
	CalculatedValue                json.RawMessage           `json:"calculated_value,omitempty"`
	CalculatedValuePresent         bool                      `json:"calculated_value_present"`
	RawFormats                     json.RawMessage           `json:"formats,omitempty"`
	FormatsPresent                 bool                      `json:"formats_present"`
	EffectiveFormats               json.RawMessage           `json:"effective_formats,omitempty"`
	EffectiveFormatsPresent        bool                      `json:"effective_formats_present"`
	FormattingSHA256               string                    `json:"formatting_sha256"`
	Provenance                     ContentMetadataProvenance `json:"provenance"`
	Protection                     string                    `json:"protection"`
	Writable                       string                    `json:"writable"`
	LiteralWriteFormatPreservation string                    `json:"literal_write_format_preservation"`
	LiteralWriteAPIVersion         string                    `json:"literal_write_api_version,omitempty"`
	LiteralWriteEndpoint           string                    `json:"literal_write_endpoint,omitempty"`
}

const contentMetadataValueLimit = 1 << 20
const literalWriteUpdateEndpoint = "POST /spreadsheets/{spreadsheetId}/sheets/{sheetId}/update"

type ContentMetadataProvenance struct {
	Provider   string `json:"provider"`
	APIVersion string `json:"api_version"`
	Endpoint   string `json:"endpoint"`
	QueryRange string `json:"query_range"`
	Cache      string `json:"cache"`
}

// ContentMetadataReader is an additive optional capability. Callers can
// probe it without widening the required Reader interface.
type ContentMetadataReader interface {
	ReadContentMetadata(context.Context, string, string, string) (ContentMetadata, error)
}

func (r *Router) ReadContentMetadata(ctx context.Context, spreadsheetID, sheetID, locator string) (ContentMetadata, error) {
	if r == nil || r.reader == nil || isNilInterface(r.reader) {
		return ContentMetadata{}, errors.New("workivaprovider: content metadata provider unavailable")
	}
	locator = strings.TrimSpace(locator)
	if locator == "" || strings.Contains(locator, ":") {
		return ContentMetadata{}, errors.New("workivaprovider: content metadata requires one exact cell locator")
	}
	if capability, ok := r.reader.(ContentMetadataReader); ok {
		if _, recursive := r.reader.(*Router); recursive {
			return ContentMetadata{}, errors.New("workivaprovider: recursive content metadata provider rejected")
		}
		metadata, err := capability.ReadContentMetadata(ctx, spreadsheetID, sheetID, locator)
		if err != nil {
			return ContentMetadata{}, err
		}
		if err := validateContentMetadata(metadata, spreadsheetID, sheetID, locator); err != nil {
			return ContentMetadata{}, err
		}
		return metadata, nil
	}
	typed, ok := r.reader.(interface {
		GetSheetDataTyped(context.Context, string, string, string, []string) (*workiva.SheetData, error)
	})
	if !ok {
		return ContentMetadata{}, errors.New("workivaprovider: typed content metadata provider unavailable")
	}
	fields := []string{"cells.value", "cells.calculatedValue", "cells.formats", "cells.effectiveFormats"}
	data, err := typed.GetSheetDataTyped(ctx, spreadsheetID, sheetID, locator, fields)
	if err != nil {
		return ContentMetadata{}, fmt.Errorf("workivaprovider: read content metadata: %w", err)
	}
	if data == nil || len(data.Cells) != 1 || len(data.Cells[0]) != 1 || len(data.Pages) != 1 {
		return ContentMetadata{}, errors.New("workivaprovider: content metadata response did not resolve to exactly one cell")
	}
	wantRange, err := workiva.A1ToRange(locator)
	if err != nil {
		return ContentMetadata{}, fmt.Errorf("workivaprovider: invalid cell locator: %w", err)
	}
	if wantRange.StartRow < 0 || wantRange.StartCol < 0 || wantRange.StartRow != wantRange.StopRow || wantRange.StartCol != wantRange.StopCol {
		return ContentMetadata{}, errors.New("workivaprovider: content metadata requires one bounded cell locator")
	}
	gotRange := data.Pages[0].Range
	if gotRange == nil || *gotRange != wantRange {
		return ContentMetadata{}, errors.New("workivaprovider: content metadata response range does not match requested cell")
	}
	cell := data.Cells[0][0]
	valueRaw, err := marshalPresentValue(cell.Value, cell.ValuePresent)
	if err != nil {
		return ContentMetadata{}, err
	}
	calculatedRaw, err := marshalPresentValue(cell.CalculatedValue, cell.CalculatedValuePresent)
	if err != nil {
		return ContentMetadata{}, err
	}
	formats, err := canonicalRaw(cell.Formats, cell.FormatsPresent)
	if err != nil {
		return ContentMetadata{}, fmt.Errorf("canonicalize cell formats: %w", err)
	}
	effective, err := canonicalRaw(cell.EffectiveFormats, cell.EffectiveFormatsPresent)
	if err != nil {
		return ContentMetadata{}, fmt.Errorf("canonicalize effective formats: %w", err)
	}
	formatHash, err := ContentFormattingHash(formats, cell.FormatsPresent, effective, cell.EffectiveFormatsPresent)
	if err != nil {
		return ContentMetadata{}, fmt.Errorf("workivaprovider: hash cell formats: %w", err)
	}
	return ContentMetadata{
		SpreadsheetID: spreadsheetID, SheetID: sheetID, Locator: locator,
		RawValue: valueRaw, ValuePresent: cell.ValuePresent,
		CalculatedValue: calculatedRaw, CalculatedValuePresent: cell.CalculatedValuePresent,
		RawFormats: formats, FormatsPresent: cell.FormatsPresent,
		EffectiveFormats: effective, EffectiveFormatsPresent: cell.EffectiveFormatsPresent,
		FormattingSHA256: formatHash,
		Provenance:       ContentMetadataProvenance{Provider: "workiva_rest", APIVersion: clientAPIVersion(r.reader), Endpoint: "GET /spreadsheets/{spreadsheetId}/sheets/{sheetId}/sheetdata", QueryRange: locator, Cache: "bypassed"},
		Protection:       "unknown", Writable: "unknown", LiteralWriteFormatPreservation: "unknown",
	}, nil
}

func marshalPresentValue(value any, present bool) (json.RawMessage, error) {
	if !present {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	return json.RawMessage(raw), err
}

func canonicalRaw(raw json.RawMessage, present bool) (json.RawMessage, error) {
	if !present {
		return nil, nil
	}
	canonical, err := assurance.CanonicalJSONBytes(raw)
	return json.RawMessage(canonical), err
}

// ContentFormattingHash hashes the canonical raw and effective format payloads
// together with their presence bits. Omitted and explicit-null formats differ.
func ContentFormattingHash(formats json.RawMessage, formatsPresent bool, effectiveFormats json.RawMessage, effectiveFormatsPresent bool) (string, error) {
	canonicalFormats, err := canonicalFormatJSON(formats, formatsPresent)
	if err != nil {
		return "", fmt.Errorf("formats: %w", err)
	}
	canonicalEffective, err := canonicalFormatJSON(effectiveFormats, effectiveFormatsPresent)
	if err != nil {
		return "", fmt.Errorf("effective formats: %w", err)
	}
	input, err := json.Marshal(struct {
		Formats          json.RawMessage `json:"formats"`
		FormatsPresent   bool            `json:"formats_present"`
		Effective        json.RawMessage `json:"effective_formats"`
		EffectivePresent bool            `json:"effective_formats_present"`
	}{canonicalFormats, formatsPresent, canonicalEffective, effectiveFormatsPresent})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(input)
	return hex.EncodeToString(digest[:]), nil
}

func canonicalFormatJSON(raw json.RawMessage, present bool) (json.RawMessage, error) {
	if !present {
		if len(raw) != 0 {
			return nil, errors.New("format bytes supplied while presence is false")
		}
		return nil, nil
	}
	if len(raw) == 0 || len(raw) > contentMetadataValueLimit {
		return nil, errors.New("format payload is empty or exceeds size limit")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("format payload contains trailing JSON")
	}
	switch value.(type) {
	case nil, map[string]any:
	default:
		return nil, errors.New("format payload must be an object or null")
	}
	canonical, err := assurance.CanonicalJSONBytes(raw)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(canonical), nil
}

func clientAPIVersion(reader Reader) string {
	if client, ok := reader.(interface{ APIVersion() string }); ok {
		return client.APIVersion()
	}
	return ""
}

func validateContentMetadata(metadata ContentMetadata, spreadsheetID, sheetID, locator string) error {
	if metadata.SpreadsheetID != spreadsheetID || metadata.SheetID != sheetID || metadata.Locator != locator || metadata.Provenance.QueryRange != locator {
		return errors.New("workivaprovider: metadata provider returned mismatched resource coordinates")
	}
	return ValidateContentMetadata(metadata)
}

// ValidateContentMetadata verifies provider provenance, field shape and
// presence, formatting checksum, and any asserted literal-write capability.
// It is intended for downstream staging code that consumes this evidence.
func ValidateContentMetadata(metadata ContentMetadata) error {
	if metadata.SpreadsheetID == "" || metadata.SheetID == "" || metadata.Locator == "" || metadata.Provenance.QueryRange != metadata.Locator || metadata.Provenance.Provider == "" || metadata.Provenance.APIVersion != workiva.DefaultAPIVersion || metadata.Provenance.Endpoint == "" || metadata.Provenance.Cache != "bypassed" {
		return errors.New("workivaprovider: metadata provider omitted verified API provenance")
	}
	if len(metadata.RawValue) > contentMetadataValueLimit || len(metadata.CalculatedValue) > contentMetadataValueLimit || !validScalarJSON(metadata.RawValue, metadata.ValuePresent) || !validScalarJSON(metadata.CalculatedValue, metadata.CalculatedValuePresent) || !validFormatJSON(metadata.RawFormats, metadata.FormatsPresent) || !validFormatJSON(metadata.EffectiveFormats, metadata.EffectiveFormatsPresent) {
		return errors.New("workivaprovider: metadata provider returned inconsistent or malformed field presence")
	}
	if (metadata.Protection != "unknown" && metadata.Protection != "protected" && metadata.Protection != "unprotected") || (metadata.Writable != "unknown" && metadata.Writable != "writable" && metadata.Writable != "not_writable") {
		return errors.New("workivaprovider: metadata provider returned an unsupported protection or writability state")
	}
	formatHash, err := ContentFormattingHash(metadata.RawFormats, metadata.FormatsPresent, metadata.EffectiveFormats, metadata.EffectiveFormatsPresent)
	if err != nil {
		return fmt.Errorf("workivaprovider: invalid metadata formats: %w", err)
	}
	if metadata.FormattingSHA256 == "" || metadata.FormattingSHA256 != formatHash {
		return errors.New("workivaprovider: metadata formatting hash does not match returned formats")
	}
	switch metadata.LiteralWriteFormatPreservation {
	case "unknown":
		if metadata.LiteralWriteAPIVersion != "" || metadata.LiteralWriteEndpoint != "" {
			return errors.New("workivaprovider: unknown literal-write capability cannot include write proof")
		}
	case "preserves":
		if metadata.LiteralWriteAPIVersion != workiva.DefaultAPIVersion || metadata.LiteralWriteEndpoint != literalWriteUpdateEndpoint {
			return errors.New("workivaprovider: literal-write preservation proof is not bound to the pinned update endpoint")
		}
	default:
		return errors.New("workivaprovider: unsupported literal-write format preservation value")
	}
	return nil
}

func decodeOptionalJSON(raw json.RawMessage, present bool) (any, bool) {
	if !present {
		return nil, len(raw) == 0
	}
	if len(raw) == 0 {
		return nil, false
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	var trailing any
	return value, decoder.Decode(&trailing) == io.EOF
}

func validScalarJSON(raw json.RawMessage, present bool) bool {
	value, ok := decodeOptionalJSON(raw, present)
	if !ok || !present {
		return ok
	}
	switch value.(type) {
	case nil, string, bool, json.Number:
		canonical, err := assurance.CanonicalJSONBytes(raw)
		return err == nil && bytes.Equal(raw, canonical)
	default:
		return false
	}
}

func validFormatJSON(raw json.RawMessage, present bool) bool {
	value, ok := decodeOptionalJSON(raw, present)
	if !ok || !present {
		return ok
	}
	switch value.(type) {
	case nil, map[string]any:
		canonical, err := canonicalFormatJSON(raw, true)
		return err == nil && bytes.Equal(raw, canonical)
	default:
		return false
	}
}
