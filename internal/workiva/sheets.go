package workiva

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/dantalabs/northern-lights/internal/ratelimit"
)

const maxSheetDataPages = 50
const maxValuesPages = 50
const maxMetadataPageBytes = 8 << 20

// Cell is one entry of the row-major cells array of a sheetdata response.
// Value is the raw cell content. CalculatedValue is the evaluated result
// when the cell contains a formula.
type Cell struct {
	Value           any `json:"value,omitempty"`
	CalculatedValue any `json:"calculatedValue,omitempty"`
	// Presence is tracked separately because omitted fields and explicit nulls
	// carry different meaning for content metadata reads.
	ValuePresent            bool            `json:"-"`
	CalculatedValuePresent  bool            `json:"-"`
	FormatsPresent          bool            `json:"-"`
	EffectiveFormatsPresent bool            `json:"-"`
	Formats                 json.RawMessage `json:"-"`
	EffectiveFormats        json.RawMessage `json:"-"`
}

// SheetData is the data object nested under the official sheetdata response
// envelope. Cells is a row-major two-dimensional array.
type SheetData struct {
	Range          *Range            `json:"range,omitempty"`
	Cells          [][]Cell          `json:"cells,omitempty"`
	Merges         []json.RawMessage `json:"merges,omitempty"`
	ColumnMetadata []json.RawMessage `json:"columnMetadata,omitempty"`
	RowMetadata    []json.RawMessage `json:"rowMetadata,omitempty"`
	// Pages preserves the official range origin for every paginated response.
	// Cells remains concatenated for compatibility with existing callers.
	Pages []SheetData `json:"-"`
}

type sheetDataResponse struct {
	Data     SheetData `json:"data"`
	NextLink string    `json:"@nextLink,omitempty"`
}

// RangeValues is one range result in the official values response.
type RangeValues struct {
	Range  string  `json:"range"`
	Values [][]any `json:"values"`
}

// ValuesResponse is the typed, paginated response from the values endpoint.
type ValuesResponse struct {
	Data     []RangeValues `json:"data"`
	NextLink string        `json:"@nextLink,omitempty"`
}

// GetSheetData reads the sheetdata of one sheet, optionally restricted
// to cellRange (A1 notation) and to the given field names. Pagination
// via "@nextLink" is followed until exhausted and the cells of all pages
// are concatenated in order. cellRange and fields may be empty, in which
// case the corresponding query parameters are omitted.
func (c *Client) GetSheetData(ctx context.Context, spreadsheetID, sheetID, cellRange string, fields []string) (*SheetData, error) {
	return c.getSheetData(ctx, spreadsheetID, sheetID, cellRange, fields, false)
}

// GetSheetDataTyped is the assurance read variant. It preserves JSON number
// text as json.Number so decimal canonicalization never passes through binary
// floating point. Existing GetSheetData callers retain their Phase 2 types.
func (c *Client) GetSheetDataTyped(ctx context.Context, spreadsheetID, sheetID, cellRange string, fields []string) (*SheetData, error) {
	return c.getSheetData(ctx, spreadsheetID, sheetID, cellRange, fields, true)
}

// APIVersion reports the version configured on this client.
func (c *Client) APIVersion() string { return c.apiVersion }

func (c *Client) getSheetData(ctx context.Context, spreadsheetID, sheetID, cellRange string, fields []string, preserveNumbers bool) (*SheetData, error) {
	path := fmt.Sprintf("/spreadsheets/%s/sheets/%s/sheetdata",
		url.PathEscape(spreadsheetID), url.PathEscape(sheetID))

	var qb strings.Builder
	if cellRange != "" {
		qb.WriteString("$cellrange=")
		qb.WriteString(url.QueryEscape(cellRange))
	}
	fields = coordinateFields(fields)
	if len(fields) > 0 {
		if qb.Len() > 0 {
			qb.WriteString("&")
		}
		qb.WriteString("$fields=")
		qb.WriteString(url.QueryEscape(strings.Join(fields, ",")))
	}
	if qb.Len() > 0 {
		path += "?" + qb.String()
	}

	result := &SheetData{}
	nextPath := path
	for pageNum := 1; pageNum <= maxSheetDataPages; pageNum++ {
		resp, err := c.Do(ctx, http.MethodGet, nextPath, nil, ratelimit.CategoryReads)
		if err != nil {
			return nil, err
		}
		capturePresence := requestedContentMetadataFields(fields)
		if !capturePresence {
			var page sheetDataResponse
			decoder := json.NewDecoder(resp.Body)
			if preserveNumbers {
				decoder.UseNumber()
			}
			decodeErr := decoder.Decode(&page)
			closeErr := resp.Body.Close()
			if decodeErr != nil {
				return nil, fmt.Errorf("decode sheetdata response: %w", errors.Join(decodeErr, closeErr))
			}
			if closeErr != nil {
				return nil, fmt.Errorf("close sheetdata response: %w", closeErr)
			}
			if err := appendSheetDataPage(result, page.Data, pageNum); err != nil {
				return nil, err
			}
			if page.NextLink == "" {
				return result, nil
			}
			if pageNum == maxSheetDataPages {
				return nil, fmt.Errorf("sheetdata pagination exceeded maximum of %d pages", maxSheetDataPages)
			}
			nextPath = page.NextLink
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxMetadataPageBytes+1))
		closeErr := resp.Body.Close()
		if readErr != nil {
			if closeErr != nil {
				readErr = errors.Join(readErr, closeErr)
			}
			return nil, fmt.Errorf("read sheetdata response: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close sheetdata response: %w", closeErr)
		}
		if len(body) > maxMetadataPageBytes {
			return nil, fmt.Errorf("sheetdata metadata response exceeded %d bytes", maxMetadataPageBytes)
		}
		var page sheetDataResponse
		decoder := json.NewDecoder(strings.NewReader(string(body)))
		if preserveNumbers {
			decoder.UseNumber()
		}
		decodeErr := decoder.Decode(&page)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode sheetdata response: %w", decodeErr)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return nil, fmt.Errorf("decode sheetdata response: trailing JSON content")
		}
		markCellPresence(body, &page.Data)
		if err := appendSheetDataPage(result, page.Data, pageNum); err != nil {
			return nil, err
		}

		if page.NextLink == "" {
			return result, nil
		}
		if pageNum == maxSheetDataPages {
			return nil, fmt.Errorf("sheetdata pagination exceeded maximum of %d pages", maxSheetDataPages)
		}
		nextPath = page.NextLink
	}
	return nil, fmt.Errorf("sheetdata pagination exceeded maximum of %d pages", maxSheetDataPages)
}

func requestedContentMetadataFields(fields []string) bool {
	for _, field := range fields {
		if field == "cells.formats" || field == "cells.effectiveFormats" {
			return true
		}
	}
	return false
}

func appendSheetDataPage(result *SheetData, page SheetData, pageNum int) error {
	if len(page.Cells) > 0 && page.Range == nil {
		return fmt.Errorf("sheetdata page %d has cells but no range metadata", pageNum)
	}
	if result.Range == nil && page.Range != nil {
		result.Range = page.Range
	}
	result.Pages = append(result.Pages, page)
	result.Cells = append(result.Cells, page.Cells...)
	result.Merges = append(result.Merges, page.Merges...)
	result.ColumnMetadata = append(result.ColumnMetadata, page.ColumnMetadata...)
	result.RowMetadata = append(result.RowMetadata, page.RowMetadata...)
	return nil
}

// markCellPresence records field presence without changing the decoder's
// historical scalar types (float64 for GetSheetData and json.Number for the
// typed variant).
func markCellPresence(body []byte, data *SheetData) {
	var envelope struct {
		Data struct {
			Cells [][]map[string]json.RawMessage `json:"cells"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return
	}
	for row := range data.Cells {
		for col := range data.Cells[row] {
			if row >= len(envelope.Data.Cells) || col >= len(envelope.Data.Cells[row]) {
				continue
			}
			fields := envelope.Data.Cells[row][col]
			cell := &data.Cells[row][col]
			_, cell.ValuePresent = fields["value"]
			_, cell.CalculatedValuePresent = fields["calculatedValue"]
			cell.Formats, cell.FormatsPresent = fields["formats"]
			cell.EffectiveFormats, cell.EffectiveFormatsPresent = fields["effectiveFormats"]
		}
	}
}

// coordinateFields ensures every sheetdata request that selects fields also
// selects range metadata. Cell coordinates cannot be derived without it.
func coordinateFields(fields []string) []string {
	if len(fields) == 0 {
		return nil
	}
	result := append([]string(nil), fields...)
	for _, field := range result {
		if field == "range" {
			return result
		}
	}
	return append(result, "range")
}

// GetRangeValues reads the values for an A1 range. It follows the official
// paginated values response until exhausted, up to a finite page cap.
func (c *Client) GetRangeValues(ctx context.Context, spreadsheetID, sheetID, a1 string) (*ValuesResponse, error) {
	path := fmt.Sprintf("/spreadsheets/%s/sheets/%s/values/%s",
		url.PathEscape(spreadsheetID), url.PathEscape(sheetID), url.PathEscape(a1))

	result := &ValuesResponse{}
	nextPath := path
	for pageNum := 1; pageNum <= maxValuesPages; pageNum++ {
		resp, err := c.Do(ctx, http.MethodGet, nextPath, nil, ratelimit.CategoryReads)
		if err != nil {
			return nil, err
		}
		var page ValuesResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&page)
		closeErr := resp.Body.Close()
		if decodeErr != nil {
			if closeErr != nil {
				decodeErr = errors.Join(decodeErr, closeErr)
			}
			return nil, fmt.Errorf("decode values response: %w", decodeErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close values response: %w", closeErr)
		}
		result.Data = append(result.Data, page.Data...)
		if page.NextLink == "" {
			return result, nil
		}
		if pageNum == maxValuesPages {
			return nil, fmt.Errorf("values pagination exceeded maximum of %d pages", maxValuesPages)
		}
		nextPath = page.NextLink
	}
	return nil, fmt.Errorf("values pagination exceeded maximum of %d pages", maxValuesPages)
}
