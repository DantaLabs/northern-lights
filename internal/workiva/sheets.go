package workiva

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/dantalabs/northern-lights/internal/ratelimit"
)

// maxSheetDataPages is the maximum number of sheetdata pages
// GetSheetData will follow via @nextLink before returning an error.
const maxSheetDataPages = 50

// Cell is one entry of the row-major cells array of a sheetdata
// response. Value is the raw cell content (a formula when it starts
// with "="); CalculatedValue is the evaluated result when the cell
// contains a formula.
type Cell struct {
	Value           *string `json:"value,omitempty"`
	CalculatedValue any     `json:"calculatedValue,omitempty"`
}

// SheetData is the decoded body of the sheetdata endpoint. Cells is a
// row-major two-dimensional array. NextLink carries the "@nextLink"
// pagination URL of the response and is empty on the final page.
type SheetData struct {
	Range          *Range            `json:"range,omitempty"`
	Cells          [][]Cell          `json:"cells,omitempty"`
	Merges         []json.RawMessage `json:"merges,omitempty"`
	ColumnMetadata []json.RawMessage `json:"columnMetadata,omitempty"`
	RowMetadata    []json.RawMessage `json:"rowMetadata,omitempty"`
	NextLink       string            `json:"@nextLink,omitempty"`
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
		var page SheetData
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decode sheetdata response: %w", err)
		}
		resp.Body.Close()

		if result.Range == nil && page.Range != nil {
			result.Range = page.Range
		}
		result.Cells = append(result.Cells, page.Cells...)
		result.Merges = append(result.Merges, page.Merges...)
		result.ColumnMetadata = append(result.ColumnMetadata, page.ColumnMetadata...)
		result.RowMetadata = append(result.RowMetadata, page.RowMetadata...)

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

// GetRangeValues reads the evaluated values of a range in A1 notation
// from the values endpoint and returns the decoded JSON body (an array
// of rows, each an array of values).
func (c *Client) GetRangeValues(ctx context.Context, spreadsheetID, sheetID, a1 string) (any, error) {
	path := fmt.Sprintf("/spreadsheets/%s/sheets/%s/values/%s",
		url.PathEscape(spreadsheetID), url.PathEscape(sheetID), url.PathEscape(a1))

	resp, err := c.Do(ctx, http.MethodGet, path, nil, ratelimit.CategoryReads)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var values any
	if err := json.NewDecoder(resp.Body).Decode(&values); err != nil {
		return nil, fmt.Errorf("decode values response: %w", err)
	}
	return values, nil
}
