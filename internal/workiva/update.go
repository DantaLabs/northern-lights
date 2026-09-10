package workiva

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/dantalabs/northern-lights/internal/ratelimit"
)

// CellEdit is one entry of an editCells update. Workiva uses zero-based
// column and row indexes for each individual cell.
type CellEdit struct {
	Column int `json:"column"`
	Row    int `json:"row"`
	Value  any `json:"value"`
}

type editCellsUpdate struct {
	Cells []CellEdit `json:"cells"`
}

// EditRangeOp writes a contiguous block of values starting at Range.
// Values is a row-major array whose shape must fit inside Range.
type EditRangeOp struct {
	Range  Range   `json:"range"`
	Values [][]any `json:"values"`
}

// ApplyFormatsOp applies the official Workiva format fields to one or more
// ranges. Omitted format fields are ignored by Workiva.
type ApplyFormatsOp struct {
	CellFormat             map[string]any `json:"cellFormat,omitempty"`
	ClearValueFormatStyles bool           `json:"clearValueFormatStyles,omitempty"`
	Ranges                 []Range        `json:"ranges"`
	TextFormat             map[string]any `json:"textFormat,omitempty"`
	ValueFormat            map[string]any `json:"valueFormat,omitempty"`
}

// Insertion describes an official Workiva row or column insertion.
type Insertion struct {
	Index int `json:"index"`
	Count int `json:"count"`
}

// insertRowsUpdate is the official nested insertRows request body.
type insertRowsUpdate struct {
	InheritFrom string      `json:"inheritFrom"`
	Insertions  []Insertion `json:"insertions"`
}

type applyFormatsUpdate struct {
	Formats []ApplyFormatsOp `json:"formats"`
}

// SheetUpdate is the body of a SheetUpdate request. The Workiva API
// accepts exactly one top-level update field per request, so the fields
// are unexported and can only be set through the constructor functions.
// To apply several changes of the same kind, batch them through the
// cells array nested under the corresponding update field instead of
// sending several requests.
type SheetUpdate struct {
	editCells    *editCellsUpdate
	editRange    *EditRangeOp
	applyFormats *applyFormatsUpdate
	insertRows   *insertRowsUpdate
}

// NewEditCellsUpdate builds a SheetUpdate that edits individual cells,
// batching the given edits into one request.
func NewEditCellsUpdate(cells []CellEdit) SheetUpdate {
	return SheetUpdate{editCells: &editCellsUpdate{Cells: cells}}
}

// NewEditRangeUpdate builds a SheetUpdate that writes a contiguous
// block of values.
func NewEditRangeUpdate(op EditRangeOp) SheetUpdate {
	return SheetUpdate{editRange: &op}
}

// NewApplyFormatsUpdate builds a SheetUpdate that applies cell formats,
// batching the given operations into one request.
func NewApplyFormatsUpdate(ops []ApplyFormatsOp) SheetUpdate {
	return SheetUpdate{applyFormats: &applyFormatsUpdate{Formats: ops}}
}

// NewInsertRowsUpdate builds a SheetUpdate that inserts rows, batching
// the given operations into one request. inheritFrom must be NONE, BEFORE,
// or AFTER.
func NewInsertRowsUpdate(inheritFrom string, insertions []Insertion) SheetUpdate {
	return SheetUpdate{insertRows: &insertRowsUpdate{InheritFrom: inheritFrom, Insertions: insertions}}
}

// MarshalJSON encodes exactly the one update field set by a
// constructor. A SheetUpdate with zero or multiple fields set is an
// error.
func (u SheetUpdate) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, 1)
	if u.editCells != nil {
		if len(u.editCells.Cells) == 0 {
			return nil, fmt.Errorf("editCells.cells must not be empty")
		}
		for i, cell := range u.editCells.Cells {
			if cell.Column < 0 || cell.Row < 0 {
				return nil, fmt.Errorf("editCells.cells[%d] must use non-negative column and row", i)
			}
		}
		m["editCells"] = u.editCells
	}
	if u.editRange != nil {
		if len(u.editRange.Values) == 0 {
			return nil, fmt.Errorf("editRange.values must not be empty")
		}
		for i, row := range u.editRange.Values {
			if len(row) == 0 {
				return nil, fmt.Errorf("editRange.values[%d] must not be empty", i)
			}
		}
		m["editRange"] = u.editRange
	}
	if u.applyFormats != nil {
		if len(u.applyFormats.Formats) == 0 {
			return nil, fmt.Errorf("applyFormats.formats must not be empty")
		}
		for i, format := range u.applyFormats.Formats {
			if len(format.Ranges) == 0 {
				return nil, fmt.Errorf("applyFormats.formats[%d].ranges must not be empty", i)
			}
			if len(format.CellFormat) == 0 && len(format.TextFormat) == 0 && len(format.ValueFormat) == 0 && !format.ClearValueFormatStyles {
				return nil, fmt.Errorf("applyFormats.formats[%d] has no format fields", i)
			}
		}
		m["applyFormats"] = u.applyFormats
	}
	if u.insertRows != nil {
		if u.insertRows.InheritFrom != "NONE" && u.insertRows.InheritFrom != "BEFORE" && u.insertRows.InheritFrom != "AFTER" {
			return nil, fmt.Errorf("insertRows.inheritFrom must be NONE, BEFORE, or AFTER")
		}
		if len(u.insertRows.Insertions) == 0 {
			return nil, fmt.Errorf("insertRows.insertions must not be empty")
		}
		for i, insertion := range u.insertRows.Insertions {
			if insertion.Index < 0 || insertion.Count < 1 {
				return nil, fmt.Errorf("insertRows.insertions[%d] must have index >= 0 and count >= 1", i)
			}
		}
		m["insertRows"] = u.insertRows
	}
	if len(m) != 1 {
		return nil, fmt.Errorf("SheetUpdate must set exactly one update field, got %d", len(m))
	}
	return json.Marshal(m)
}

// UpdateSheet preserves the original operation URL API and honors the
// initial Retry-After delay before returning. Call
// UpdateSheetWithRetryAfter when the caller will poll and needs to carry the
// delay explicitly.
func (c *Client) UpdateSheet(ctx context.Context, spreadsheetID, sheetID string, upd SheetUpdate) (string, error) {
	operationURL, initialRetryAfter, err := c.UpdateSheetWithRetryAfter(ctx, spreadsheetID, sheetID, upd)
	if err != nil {
		return "", err
	}
	if initialRetryAfter > 0 {
		if err := c.sleep(ctx, initialRetryAfter); err != nil {
			return "", fmt.Errorf("update sheet initial retry-after: %w", err)
		}
	}
	return operationURL, nil
}

// UpdateSheetWithRetryAfter sends one SheetUpdate and returns its operation
// URL plus the initial Retry-After delay from the 202 response. The delay is
// returned with this operation instead of being stored on Client state.
func (c *Client) UpdateSheetWithRetryAfter(ctx context.Context, spreadsheetID, sheetID string, upd SheetUpdate) (operationURL string, initialRetryAfter time.Duration, err error) {
	payload, err := json.Marshal(upd)
	if err != nil {
		return "", 0, err
	}

	path := fmt.Sprintf("/spreadsheets/%s/sheets/%s/update",
		url.PathEscape(spreadsheetID), url.PathEscape(sheetID))
	resp, err := c.Do(ctx, http.MethodPost, path, bytes.NewReader(payload), ratelimit.CategoryWrites)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close update response: %w", closeErr))
		}
	}()

	if resp.StatusCode != http.StatusAccepted {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if readErr != nil {
			return "", 0, fmt.Errorf("read update error response body: %w", readErr)
		}
		return "", 0, &APIError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	initialRetryAfter = initialRetryAfterDelay(resp.Header.Get("Retry-After"))

	var res struct {
		OperationLocation string `json:"operationLocation"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil && !errors.Is(err, io.EOF) {
		return "", 0, fmt.Errorf("decode update response: %w", err)
	}
	if res.OperationLocation == "" {
		res.OperationLocation = resp.Header.Get("Location")
	}
	if res.OperationLocation == "" {
		return "", 0, fmt.Errorf("update sheet: 202 response has no operationLocation and no Location header")
	}
	return res.OperationLocation, initialRetryAfter, nil
}

// WriteCells batch-writes the given cell edits in a single SheetUpdate
// request and waits for the async operation to complete.
//
// Note: values are written in Ones scale regardless of the cell display
// format, so a cell shown in thousands still takes its plain numeric
// value. Also note the one-top-level-field rule of SheetUpdate: many
// edits of the same kind must be batched in the cells array nested under
// editCells, which is what this function does.
func (c *Client) WriteCells(ctx context.Context, spreadsheetID, sheetID string, edits []CellEdit) error {
	opURL, initialRetryAfter, err := c.UpdateSheetWithRetryAfter(ctx, spreadsheetID, sheetID, NewEditCellsUpdate(edits))
	if err != nil {
		return err
	}
	_, err = c.WaitOperationWithInitialRetryAfter(ctx, opURL, initialRetryAfter)
	return err
}
