package workiva

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const (
	esgDataEnv     = "NL_DEMO_ESG_DATA"
	esgDefaultDir  = "configs/demo/data"
	esgColumnCount = 4 // key | label | unit | value
	esgValueColumn = 3
)

// esgSheets is the demo layout: spreadsheet ID to its sheets in index order.
var esgSheets = map[string][]esgSheet{
	"demo-esg-source": {{id: "demo-esg-source-2025", name: "ESG data 2025", year: "2025", values: true}},
	"demo-esg-report": {
		{id: "demo-esg-report-cy", name: "CY Data Output", year: "2025", values: false},
		{id: "demo-esg-report-py", name: "PY Data Output", year: "2024", values: true},
	},
}

// esgSheet describes one demo sheet: which reporting period's CSV feeds it and
// whether the CSV value column is served (the current-year target stays empty).
type esgSheet struct {
	id, name, year string
	values         bool
}

// esgDecimal matches the plain decimals allowed in the CSV value column.
var esgDecimal = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// esgOverlays holds in-memory field edits per transport, because DemoTransport
// belongs to another lane. Keyed by transport, then "spreadsheet/sheet", then "row,column".
var (
	esgOverlayMu sync.Mutex
	esgOverlays  = map[*DemoTransport]map[string]map[[2]int]any{}
)

// demoESGResponse serves demo-esg spreadsheet requests in demo mode: the ESG
// field data read from the CSV files in NL_DEMO_ESG_DATA, plus in-memory writes.
func (t *DemoTransport) demoESGResponse(req *http.Request) *http.Response {
	path := req.URL.Path
	parts := strings.Split(path, "/") // "", "spreadsheets", id, "sheets", sheetID, kind, a1
	sheets, ok := esgSheets[parts[2]]
	if !ok || len(parts) < 4 || parts[3] != "sheets" {
		return demoESGNotFound("unknown ESG demo path: " + path)
	}
	if len(parts) == 4 && req.Method == http.MethodGet {
		return esgSheetsResponse(sheets)
	}
	if len(parts) < 6 {
		return demoESGNotFound("unknown ESG demo path: " + path)
	}
	var sheet *esgSheet
	for i := range sheets {
		if sheets[i].id == parts[4] {
			sheet = &sheets[i]
		}
	}
	if sheet == nil {
		return demoESGNotFound("unknown ESG demo sheet: " + path)
	}
	key := parts[2] + "/" + sheet.id
	switch {
	case len(parts) == 6 && parts[5] == "sheetdata" && req.Method == http.MethodGet:
		return t.esgReadResponse(key, sheet, req.URL.Query().Get("$cellrange"), false)
	case len(parts) == 7 && parts[5] == "values" && req.Method == http.MethodGet:
		a1, err := url.PathUnescape(parts[6])
		if err != nil {
			return esgError(http.StatusBadRequest, "values range encoding")
		}
		return t.esgReadResponse(key, sheet, a1, true)
	case len(parts) == 6 && parts[5] == "update" && req.Method == http.MethodPost:
		return t.esgUpdateResponse(req, key, sheet)
	}
	return demoESGNotFound("unknown ESG demo path: " + path)
}

func esgSheetsResponse(sheets []esgSheet) *http.Response {
	type entry struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Index int    `json:"index"`
	}
	data := make([]entry, 0, len(sheets))
	for i, s := range sheets {
		data = append(data, entry{ID: s.id, Name: s.name, Index: i})
	}
	return demoJSONResponse(mustJSON(map[string]any{"data": data}))
}

// esgGrid builds the sheet grid (header row, then one row per CSV data row) with overlay edits applied.
func (t *DemoTransport) esgGrid(key string, sheet *esgSheet) (*SheetData, *http.Response) {
	dir := os.Getenv(esgDataEnv)
	if dir == "" {
		dir = esgDefaultDir
	}
	name := "esg-" + sheet.year + ".csv"
	records, err := readESGCSV(filepath.Join(dir, name))
	if err != nil {
		return nil, esgError(http.StatusInternalServerError, fmt.Sprintf("%s: cannot serve %s from %s: %v", esgDataEnv, name, dir, err))
	}
	cells := make([][]Cell, 0, len(records))
	for i, rec := range records {
		row := []Cell{{Value: rec[0]}, {Value: rec[1]}, {Value: rec[2]}, {}}
		switch {
		case i == 0:
			row[esgValueColumn].Value = rec[3]
		case sheet.values && rec[3] != "":
			row[esgValueColumn].Value = esgValue(rec[3])
		}
		cells = append(cells, row)
	}
	esgOverlayMu.Lock()
	defer esgOverlayMu.Unlock()
	for pos, v := range esgOverlays[t][key] {
		cells[pos[0]][pos[1]] = Cell{Value: v}
	}
	return &SheetData{Cells: cells}, nil
}

// esgValue keeps decimal text exact as json.Number; anything else stays a string.
func esgValue(text string) any {
	if esgDecimal.MatchString(text) {
		return json.Number(text)
	}
	return text
}

// readESGCSV parses one ESG CSV file and checks the header key,label,unit,value.
func readESGCSV(path string) ([][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	r := csv.NewReader(f)
	r.FieldsPerRecord = esgColumnCount
	records, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(records) == 0 || strings.Join(records[0], ",") != "key,label,unit,value" {
		return nil, fmt.Errorf("header must be key,label,unit,value")
	}
	return records, nil
}

func (t *DemoTransport) esgReadResponse(key string, sheet *esgSheet, a1 string, valuesOnly bool) *http.Response {
	grid, errResp := t.esgGrid(key, sheet)
	if errResp != nil {
		return errResp
	}
	if a1 == "" && valuesOnly {
		return esgError(http.StatusBadRequest, "values range is required")
	}
	rng := Range{StartRow: 0, StartCol: 0, StopRow: len(grid.Cells) - 1, StopCol: esgColumnCount - 1}
	if a1 != "" {
		parsed, err := A1ToRange(a1)
		if err != nil {
			return esgError(http.StatusBadRequest, "invalid range: "+a1)
		}
		rng = parsed
	}
	if valuesOnly {
		return demoJSONResponse(mustJSON(ValuesResponse{Data: []RangeValues{{Range: a1, Values: ValuesOnlyGrid(grid, rng)}}}))
	}
	return demoJSONResponse(mustJSON(sheetDataResponse{Data: *sliceSheetData(grid, rng)}))
}

// esgUpdateResponse validates an editCells payload against the data grid and stores it in the overlay.
func (t *DemoTransport) esgUpdateResponse(req *http.Request, key string, sheet *esgSheet) *http.Response {
	var payload struct {
		EditCells struct {
			Cells []CellEdit `json:"cells"`
		} `json:"editCells"`
	}
	decoder := json.NewDecoder(req.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil || len(payload.EditCells.Cells) == 0 {
		return esgError(http.StatusBadRequest, "update body must contain editCells.cells")
	}
	grid, errResp := t.esgGrid(key, sheet)
	if errResp != nil {
		return errResp
	}
	for i, edit := range payload.EditCells.Cells {
		if edit.Row < 0 || edit.Column < 0 || edit.Row >= len(grid.Cells) || edit.Column >= esgColumnCount {
			return esgError(http.StatusBadRequest, fmt.Sprintf("editCells.cells[%d] is outside the demo sheet", i))
		}
		switch edit.Value.(type) {
		case nil, bool, string, json.Number:
		default:
			return esgError(http.StatusBadRequest, fmt.Sprintf("editCells.cells[%d].value must be a scalar", i))
		}
	}
	esgOverlayMu.Lock()
	if esgOverlays[t] == nil {
		esgOverlays[t] = map[string]map[[2]int]any{}
	}
	if esgOverlays[t][key] == nil {
		esgOverlays[t][key] = map[[2]int]any{}
	}
	for _, edit := range payload.EditCells.Cells {
		esgOverlays[t][key][[2]int{edit.Row, edit.Column}] = edit.Value
	}
	esgOverlayMu.Unlock()
	resp := esgJSON(http.StatusAccepted, []byte(`{"operationLocation":"/operations/demo-op-1","id":"demo-op-1"}`))
	resp.Request.Method = http.MethodPost
	return resp
}

// demoESGNotFound returns a 404 JSON response for an unknown ESG demo path.
func demoESGNotFound(message string) *http.Response {
	return esgError(http.StatusNotFound, message)
}

func esgError(status int, message string) *http.Response {
	return esgJSON(status, mustJSON(map[string]string{"error": message}))
}

func esgJSON(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    &http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/"}},
	}
}
