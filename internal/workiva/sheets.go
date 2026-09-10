package workiva

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/dantalabs/northern-lights/internal/ratelimit"
)

const maxSheetDataPages = 50
const maxValuesPages = 50

// Cell is one entry of the row-major cells array of a sheetdata response.
// Value is the raw cell content. CalculatedValue is the evaluated result
// when the cell contains a formula.
type Cell struct {
	Value           any `json:"value,omitempty"`
	CalculatedValue any `json:"calculatedValue,omitempty"`
}

// SheetData is the data object nested under the official sheetdata response
// envelope. Cells is a row-major two-dimensional array.
type SheetData struct {
	Range          *Range            `json:"range,omitempty"`
	Cells          [][]Cell          `json:"cells,omitempty"`
	Merges         []json.RawMessage `json:"merges,omitempty"`
	ColumnMetadata []json.RawMessage `json:"columnMetadata,omitempty"`
	RowMetadata    []json.RawMessage `json:"rowMetadata,omitempty"`
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
	path := fmt.Sprintf("/spreadsheets/%s/sheets/%s/sheetdata",
		url.PathEscape(spreadsheetID), url.PathEscape(sheetID))

	var qb strings.Builder
	if cellRange != "" {
		qb.WriteString("$cellrange=")
		qb.WriteString(url.QueryEscape(cellRange))
	}
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
		var page sheetDataResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&page)
		closeErr := resp.Body.Close()
		if decodeErr != nil {
			if closeErr != nil {
				decodeErr = errors.Join(decodeErr, closeErr)
			}
			return nil, fmt.Errorf("decode sheetdata response: %w", decodeErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close sheetdata response: %w", closeErr)
		}

		if result.Range == nil && page.Data.Range != nil {
			result.Range = page.Data.Range
		}
		result.Cells = append(result.Cells, page.Data.Cells...)
		result.Merges = append(result.Merges, page.Data.Merges...)
		result.ColumnMetadata = append(result.ColumnMetadata, page.Data.ColumnMetadata...)
		result.RowMetadata = append(result.RowMetadata, page.Data.RowMetadata...)

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
