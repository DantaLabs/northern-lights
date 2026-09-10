package workiva

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

//go:embed testdata/demo_sheetdata.json
var demoSheetdataJSON []byte

// DemoTransport is an http.RoundTripper that intercepts known Workiva API
// paths and returns synthetic fixture responses. It lets anyone run the
// full MCP flow without Workiva credentials.
type DemoTransport struct {
	mu sync.Mutex
	// WrittenCells records the most recent editCells write per
	// "spreadsheet/sheet" key. Tests and demo seeds can inspect it.
	WrittenCells map[string]string

	fixture *SheetData
}

// NewDemoTransport returns a transport preloaded with the VSME fixture.
func NewDemoTransport() *DemoTransport {
	var response sheetDataResponse
	if err := json.Unmarshal(demoSheetdataJSON, &response); err != nil {
		panic(fmt.Sprintf("demo fixture: %v", err))
	}
	return &DemoTransport{
		WrittenCells: make(map[string]string),
		fixture:      &response.Data,
	}
}

// RoundTrip implements http.RoundTripper. It recognises the token, sheetdata,
// values, update, and operations endpoints used by the Workiva client.
func (t *DemoTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	method := req.Method

	switch {
	case path == "/iam/v1/oauth2/token" && method == http.MethodPost:
		return demoTokenResponse(), nil
	case path == "/spreadsheets" && method == http.MethodGet:
		return demoSpreadsheetsResponse(), nil
	case isSheetsPath(path) && method == http.MethodGet:
		return demoSheetsResponse(path), nil
	case isSheetDataPath(path) && method == http.MethodGet:
		return t.sheetdataResponse(req.URL), nil
	case isValuesPath(path) && method == http.MethodGet:
		return t.valuesResponse(req.URL), nil
	case isUpdatePath(path) && method == http.MethodPost:
		return t.demoUpdateResponse(req), nil
	case isOperationsPath(path) && method == http.MethodGet:
		return demoJSONResponse(demoOperationCompletedJSON()), nil
	default:
		return demoNotFound(path), nil
	}
}

func isSheetsPath(path string) bool {
	parts := strings.Split(path, "/")
	return len(parts) == 4 && parts[1] == "spreadsheets" && parts[3] == "sheets"
}

func demoSpreadsheetsResponse() *http.Response {
	return demoJSONResponse([]byte(`{"data":[{"id":"demo-sp-energy-2026","name":"VSME Energy Report 2026","template":false},{"id":"demo-sp-energy-2025","name":"VSME Energy Report 2025","template":false}]}`))
}

func demoSheetsResponse(path string) *http.Response {
	parts := strings.Split(path, "/")
	if len(parts) != 4 {
		return demoBadRequest("sheets path too short")
	}
	switch parts[2] {
	case "demo-sp-energy-2026":
		return demoJSONResponse([]byte(`{"data":[{"id":"demo-sh-b3-energy","name":"B3 Energy","index":0},{"id":"demo-sh-reference","name":"Reference data","index":1}]}`))
	case "demo-sp-energy-2025":
		return demoJSONResponse([]byte(`{"data":[{"id":"demo-sh-b3-energy-2025","name":"B3 Energy","index":0}]}`))
	default:
		return demoNotFound(path)
	}
}

// isSheetDataPath reports whether path is a sheetdata GET endpoint.
func isSheetDataPath(path string) bool {
	parts := strings.Split(path, "/")
	return len(parts) >= 6 &&
		parts[1] == "spreadsheets" &&
		parts[3] == "sheets" &&
		parts[5] == "sheetdata"
}

// isValuesPath reports whether path is a values GET endpoint.
func isValuesPath(path string) bool {
	parts := strings.Split(path, "/")
	return len(parts) >= 7 &&
		parts[1] == "spreadsheets" &&
		parts[3] == "sheets" &&
		parts[5] == "values"
}

// isUpdatePath reports whether path is a sheet POST update endpoint.
func isUpdatePath(path string) bool {
	parts := strings.Split(path, "/")
	return len(parts) == 6 &&
		parts[1] == "spreadsheets" &&
		parts[3] == "sheets" &&
		parts[5] == "update"
}

// isOperationsPath reports whether path is an operations GET endpoint.
func isOperationsPath(path string) bool {
	parts := strings.Split(path, "/")
	return len(parts) == 3 && parts[1] == "operations"
}

// demoJSONResponse builds a 200 OK response with the given JSON body.
func demoJSONResponse(body []byte) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    &http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/"}},
	}
}

// demoTokenResponse returns a synthetic OAuth token.
func demoTokenResponse() *http.Response {
	body := []byte(`{"access_token":"demo-token","expires_in":3600}`)
	return demoJSONResponse(body)
}

// sheetdataResponse returns the fixture restricted to the requested
// $cellrange when present, otherwise the full fixture.
func (t *DemoTransport) sheetdataResponse(u *url.URL) *http.Response {
	cellRange := u.Query().Get("$cellrange")
	if cellRange == "" {
		return demoJSONResponse(demoSheetdataJSON)
	}
	rng, err := A1ToRange(cellRange)
	if err != nil {
		return demoBadRequest(fmt.Sprintf("invalid $cellrange: %s", cellRange))
	}
	out := sliceSheetData(t.fixture, rng)
	b, err := json.Marshal(sheetDataResponse{Data: *out})
	if err != nil {
		return demoServerError("marshal sheetdata")
	}
	return demoJSONResponse(b)
}

// valuesResponse returns the values grid restricted to the requested A1
// range encoded in the URL path.
func (t *DemoTransport) valuesResponse(u *url.URL) *http.Response {
	parts := strings.Split(u.Path, "/")
	if len(parts) < 7 {
		return demoBadRequest("values path too short")
	}
	cellRange, err := url.PathUnescape(parts[6])
	if err != nil {
		return demoBadRequest("values range encoding")
	}
	rng, err := A1ToRange(cellRange)
	if err != nil {
		return demoBadRequest(fmt.Sprintf("invalid range: %s", cellRange))
	}
	return demoJSONResponse(mustJSON(ValuesResponse{Data: []RangeValues{{
		Range:  cellRange,
		Values: ValuesOnlyGrid(t.fixture, rng),
	}}}))
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("[]")
	}
	return b
}

// ValuesOnlyGrid extracts the raw values for rng from full. It is exported
// so tests can assert on fixture slicing without using the transport.
func ValuesOnlyGrid(full *SheetData, rng Range) [][]any {
	rows := sliceCells(full.Cells, rng)
	out := make([][]any, 0, len(rows))
	for _, row := range rows {
		outRow := make([]any, 0, len(row))
		for _, cell := range row {
			outRow = append(outRow, cellDisplayValue(cell))
		}
		out = append(out, outRow)
	}
	return out
}

// sliceSheetData returns a copy of full restricted to rng with metadata
// matching the slice.
func sliceSheetData(full *SheetData, rng Range) *SheetData {
	out := &SheetData{
		Range:          &rng,
		Cells:          sliceCells(full.Cells, rng),
		Merges:         []json.RawMessage{},
		ColumnMetadata: []json.RawMessage{},
		RowMetadata:    []json.RawMessage{},
	}
	return out
}

// sliceCells extracts the cell grid for rng from rows. Unbounded dimensions
// are clamped to the available data.
func sliceCells(rows [][]Cell, rng Range) [][]Cell {
	startRow := rng.StartRow
	if startRow < 0 {
		startRow = 0
	}
	stopRow := rng.StopRow
	if stopRow >= len(rows) || stopRow < 0 {
		stopRow = len(rows) - 1
	}
	if startRow >= len(rows) {
		return nil
	}

	out := make([][]Cell, 0, stopRow-startRow+1)
	for r := startRow; r <= stopRow && r < len(rows); r++ {
		row := rows[r]
		startCol := rng.StartCol
		if startCol < 0 {
			startCol = 0
		}
		stopCol := rng.StopCol
		if stopCol >= len(row) || stopCol < 0 {
			stopCol = len(row) - 1
		}
		if startCol >= len(row) {
			continue
		}
		outRow := make([]Cell, 0, stopCol-startCol+1)
		for c := startCol; c <= stopCol && c < len(row); c++ {
			outRow = append(outRow, row[c])
		}
		out = append(out, outRow)
	}
	return out
}

// cellDisplayValue returns the evaluated value for display: formulas resolve
// to their calculated value, literal values pass through.
func cellDisplayValue(c Cell) any {
	if value, ok := c.Value.(string); ok && strings.HasPrefix(value, "=") {
		if c.CalculatedValue != nil {
			return c.CalculatedValue
		}
		return value
	}
	if c.Value != nil {
		return c.Value
	}
	return ""
}

// demoUpdateResponse validates and records the official nested editCells
// payload, then returns 202 Accepted.
func (t *DemoTransport) demoUpdateResponse(req *http.Request) *http.Response {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return demoBadRequest("read update body")
	}
	var payload struct {
		EditCells struct {
			Cells []CellEdit `json:"cells"`
		} `json:"editCells"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.EditCells.Cells) == 0 {
		return demoBadRequest("update body must contain editCells.cells")
	}
	responseBody := []byte(`{"operationLocation":"/operations/demo-op-1","id":"demo-op-1"}`)
	t.mu.Lock()
	t.WrittenCells[req.URL.Path] = string(body)
	t.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusAccepted,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(responseBody)),
		Request:    &http.Request{Method: http.MethodPost, URL: &url.URL{Path: req.URL.Path}},
	}
}

// demoOperationCompletedJSON returns a completed async operation.
func demoOperationCompletedJSON() []byte {
	return []byte(`{"id":"demo-op-1","status":"completed","resourceUrl":"/spreadsheets/demo-sp-1/sheets/demo-sh-b3-energy/update"}`)
}

// demoNotFound returns a 404 for unrecognised demo paths.
func demoNotFound(path string) *http.Response {
	body := []byte(fmt.Sprintf(`{"error":"demo path not handled: %s"}`, path))
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    &http.Request{Method: http.MethodGet, URL: &url.URL{Path: path}},
	}
}

// demoBadRequest returns a 400 with a synthetic error body.
func demoBadRequest(msg string) *http.Response {
	body := []byte(fmt.Sprintf(`{"error":"%s"}`, msg))
	return &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    &http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/"}},
	}
}

// demoServerError returns a 500 with a synthetic error body.
func demoServerError(msg string) *http.Response {
	body := []byte(fmt.Sprintf(`{"error":"%s"}`, msg))
	return &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    &http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/"}},
	}
}

// NewDemoClient builds a Workiva client backed entirely by synthetic
// fixtures. It is used when NL_DEMO_MODE is enabled.
func NewDemoClient(baseURL *url.URL) *Client {
	transport := NewDemoTransport()
	httpClient := &http.Client{Transport: transport}
	tokens := NewTokenProvider(baseURL, "demo", "demo", "file:read file:write", httpClient)
	return NewClient(baseURL, tokens, nil, httpClient)
}
