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

	"github.com/dantalabs/northern-lights/internal/ratelimit"
)

// CellEdit is one entry of an editCells update: it writes Value to
// every cell identified by Range.
type CellEdit struct {
	Range Range `json:"range"`
	Value any   `json:"value"`
}

// EditRangeOp writes a contiguous block of values starting at Range.
// Values is a row-major array whose shape must fit inside Range.
type EditRangeOp struct {
	Range  Range   `json:"range"`
	Values [][]any `json:"values"`
}

// ApplyFormatsOp applies a cell format to the cells identified by
// Range. Format follows the 2026-01-01 CellFormat schema.
type ApplyFormatsOp struct {
	Range  Range          `json:"range"`
	Format map[string]any `json:"format"`
}

// InsertRowsOp inserts Count empty rows starting at StartRow
// (zero-based).
type InsertRowsOp struct {
	StartRow int `json:"startRow"`
	Count    int `json:"count"`
}

// SheetUpdate is the body of a SheetUpdate request. The Workiva API
// accepts exactly one top-level update field per request, so the fields
// are unexported and can only be set through the constructor functions.
// To apply several changes of the same kind, batch them through the
// array value of a single field instead of sending several requests.
type SheetUpdate struct {
	editCells    *[]CellEdit
	editRange    *EditRangeOp
	applyFormats *[]ApplyFormatsOp
	insertRows   *[]InsertRowsOp
}

// NewEditCellsUpdate builds a SheetUpdate that edits individual cells,
// batching the given edits into one request.
func NewEditCellsUpdate(cells []CellEdit) SheetUpdate {
	return SheetUpdate{editCells: &cells}
}

// NewEditRangeUpdate builds a SheetUpdate that writes a contiguous
// block of values.
func NewEditRangeUpdate(op EditRangeOp) SheetUpdate {
	return SheetUpdate{editRange: &op}
}

// NewApplyFormatsUpdate builds a SheetUpdate that applies cell formats,
// batching the given operations into one request.
func NewApplyFormatsUpdate(ops []ApplyFormatsOp) SheetUpdate {
	return SheetUpdate{applyFormats: &ops}
}

// NewInsertRowsUpdate builds a SheetUpdate that inserts rows, batching
// the given operations into one request.
func NewInsertRowsUpdate(ops []InsertRowsOp) SheetUpdate {
	return SheetUpdate{insertRows: &ops}
}

// MarshalJSON encodes exactly the one update field set by a
// constructor. A SheetUpdate with zero or multiple fields set is an
// error.
func (u SheetUpdate) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, 1)
	if u.editCells != nil {
		m["editCells"] = u.editCells
	}
	if u.editRange != nil {
		m["editRange"] = u.editRange
	}
	if u.applyFormats != nil {
		m["applyFormats"] = u.applyFormats
	}
	if u.insertRows != nil {
		m["insertRows"] = u.insertRows
	}
	if len(m) != 1 {
		return nil, fmt.Errorf("SheetUpdate must set exactly one update field, got %d", len(m))
	}
	return json.Marshal(m)
}

// UpdateSheet sends one SheetUpdate to the sheet data endpoint and
// returns the operationLocation of the accepted async operation. The
// location is read from the 202 response body, falling back to the
// Location header.
func (c *Client) UpdateSheet(ctx context.Context, spreadsheetID, sheetID string, upd SheetUpdate) (operationURL string, err error) {
	payload, err := json.Marshal(upd)
	if err != nil {
		return "", err
	}

	path := fmt.Sprintf("/spreadsheets/%s/sheets/%s/data",
		url.PathEscape(spreadsheetID), url.PathEscape(sheetID))
	resp, err := c.Do(ctx, http.MethodPatch, path, bytes.NewReader(payload), ratelimit.CategoryWrites)
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close update response: %w", closeErr))
		}
	}()

	if resp.StatusCode != http.StatusAccepted {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if readErr != nil {
			return "", fmt.Errorf("read update error response body: %w", readErr)
		}
		return "", &APIError{StatusCode: resp.StatusCode, Body: string(body)}
	}

	var res struct {
		OperationLocation string `json:"operationLocation"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("decode update response: %w", err)
	}
	if res.OperationLocation == "" {
		res.OperationLocation = resp.Header.Get("Location")
	}
	if res.OperationLocation == "" {
		return "", fmt.Errorf("update sheet: 202 response has no operationLocation and no Location header")
	}
	return res.OperationLocation, nil
}

// WriteCells batch-writes the given cell edits in a single SheetUpdate
// request and waits for the async operation to complete.
//
// Note: values are written in Ones scale regardless of the cell display
// format, so a cell shown in thousands still takes its plain numeric
// value. Also note the one-top-level-field rule of SheetUpdate: many
// edits of the same kind must be batched in the editCells array of one
// request, which is what this function does.
func (c *Client) WriteCells(ctx context.Context, spreadsheetID, sheetID string, edits []CellEdit) error {
	opURL, err := c.UpdateSheet(ctx, spreadsheetID, sheetID, NewEditCellsUpdate(edits))
	if err != nil {
		return err
	}
	_, err = c.WaitOperation(ctx, opURL)
	return err
}
