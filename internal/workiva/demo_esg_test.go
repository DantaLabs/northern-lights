package workiva

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	esgTestCSV2025 = "key,label,unit,value\n" +
		"energy_total,\"Total energy, all sources\",MWh,1240000.5\n" +
		"water_total,Water withdrawal,m3,\n" +
		"employees_total,Total employees,FTE,812\n"
	esgTestCSV2024 = "key,label,unit,value\n" +
		"energy_total,\"Total energy, all sources\",MWh,1100000\n" +
		"water_total,Water withdrawal,m3,77.25\n" +
		"employees_total,Total employees,FTE,790\n"
)

// useESGData writes the two ESG CSV files into a temp dir and points NL_DEMO_ESG_DATA at it.
func useESGData(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeESGFiles(t, dir)
	t.Setenv("NL_DEMO_ESG_DATA", dir)
	return dir
}

func writeESGFiles(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for name, content := range map[string]string{"esg-2025.csv": esgTestCSV2025, "esg-2024.csv": esgTestCSV2024} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// esgClient builds a client over the given transport so tests control transport identity.
func esgClient(t *testing.T, transport *DemoTransport) *Client {
	t.Helper()
	base := demoBaseURL(t)
	httpClient := &http.Client{Transport: transport}
	tokens := NewTokenProvider(base, "demo", "demo", "file:read file:write", httpClient)
	return NewClient(base, tokens, nil, httpClient)
}

func esgRoundTrip(t *testing.T, transport *DemoTransport, method, path, body string) (int, string, http.Header) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, "https://api.eu.wdesk.com"+path, reader)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip %s %s: %v", method, path, err)
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body %s %s: %v", method, path, err)
	}
	return resp.StatusCode, string(data), resp.Header
}

// esgCellText returns the cell's value text, or "" for an empty cell.
func esgCellText(t *testing.T, c Cell) string {
	t.Helper()
	switch v := c.Value.(type) {
	case nil:
		return ""
	case json.Number:
		return v.String()
	case string:
		return v
	default:
		t.Fatalf("unexpected cell value type %T", c.Value)
		return ""
	}
}

func esgRead(t *testing.T, client *Client, spreadsheet, sheet, a1 string) string {
	t.Helper()
	data, err := client.GetSheetDataTyped(context.Background(), spreadsheet, sheet, a1, nil)
	if err != nil {
		t.Fatalf("GetSheetDataTyped %s/%s %s: %v", spreadsheet, sheet, a1, err)
	}
	if len(data.Cells) != 1 || len(data.Cells[0]) != 1 {
		t.Fatalf("%s %s: want one cell, got %v", sheet, a1, data.Cells)
	}
	return esgCellText(t, data.Cells[0][0])
}

func TestDemoESGSheetsList(t *testing.T) {
	useESGData(t)
	transport := NewDemoTransport()
	cases := []struct {
		spreadsheet string
		want        map[string]string
	}{
		{"demo-esg-source", map[string]string{"demo-esg-source-2025": "ESG data 2025"}},
		{"demo-esg-report", map[string]string{"demo-esg-report-cy": "CY Data Output", "demo-esg-report-py": "PY Data Output"}},
	}
	for _, tc := range cases {
		status, body, _ := esgRoundTrip(t, transport, http.MethodGet, "/spreadsheets/"+tc.spreadsheet+"/sheets", "")
		if status != http.StatusOK {
			t.Fatalf("%s status = %d, body %s", tc.spreadsheet, status, body)
		}
		var parsed struct {
			Data []struct {
				ID    string `json:"id"`
				Name  string `json:"name"`
				Index int    `json:"index"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			t.Fatalf("%s decode: %v", tc.spreadsheet, err)
		}
		if len(parsed.Data) != len(tc.want) {
			t.Fatalf("%s sheets = %d, want %d", tc.spreadsheet, len(parsed.Data), len(tc.want))
		}
		for i, sheet := range parsed.Data {
			if tc.want[sheet.ID] != sheet.Name || sheet.Index != i {
				t.Errorf("%s sheet %d = %+v, want name %q index %d", tc.spreadsheet, i, sheet, tc.want[sheet.ID], i)
			}
		}
	}
}

func TestDemoESGSourceReadsCurrentYear(t *testing.T) {
	useESGData(t)
	client := esgClient(t, NewDemoTransport())
	checks := map[string]string{
		"D2": "1240000.5",
		"A2": "energy_total",
		"B2": "Total energy, all sources",
		"C2": "MWh",
		"D3": "",
		"A3": "water_total",
		"D4": "812",
		"A1": "key",
		"D1": "value",
	}
	for a1, want := range checks {
		if got := esgRead(t, client, "demo-esg-source", "demo-esg-source-2025", a1); got != want {
			t.Errorf("source %s = %q, want %q", a1, got, want)
		}
	}
}

func TestDemoESGPriorPeriodAndTarget(t *testing.T) {
	useESGData(t)
	client := esgClient(t, NewDemoTransport())
	if got := esgRead(t, client, "demo-esg-report", "demo-esg-report-py", "D3"); got != "77.25" {
		t.Errorf("PY D3 = %q, want 77.25", got)
	}
	if got := esgRead(t, client, "demo-esg-report", "demo-esg-report-py", "D2"); got != "1100000" {
		t.Errorf("PY D2 = %q, want 1100000", got)
	}
	for a1, want := range map[string]string{"A4": "employees_total", "B4": "Total employees", "C4": "FTE", "D2": "", "D4": ""} {
		if got := esgRead(t, client, "demo-esg-report", "demo-esg-report-cy", a1); got != want {
			t.Errorf("CY %s = %q, want %q", a1, got, want)
		}
	}
}

func TestDemoESGWriteThenReadBack(t *testing.T) {
	useESGData(t)
	ctx := context.Background()
	transport := NewDemoTransport()
	client := esgClient(t, transport)
	const sp, sh = "demo-esg-report", "demo-esg-report-cy"
	if err := client.WriteCells(ctx, sp, sh, []CellEdit{{Row: 1, Column: 3, Value: "123.4"}}); err != nil {
		t.Fatalf("WriteCells: %v", err)
	}
	if got := esgRead(t, client, sp, sh, "D2"); got != "123.4" {
		t.Errorf("after write D2 = %q, want 123.4", got)
	}
	other := esgClient(t, NewDemoTransport())
	if got := esgRead(t, other, sp, sh, "D2"); got != "" {
		t.Errorf("write leaked to another transport: D2 = %q", got)
	}
	if err := client.WriteCells(ctx, sp, sh, []CellEdit{{Row: 1, Column: 3, Value: nil}}); err != nil {
		t.Fatalf("WriteCells nil: %v", err)
	}
	if got := esgRead(t, client, sp, sh, "D2"); got != "" {
		t.Errorf("after clear D2 = %q, want empty", got)
	}
	literal := NewLiteralEditCellsUpdate([]CellEdit{{Row: 2, Column: 3, Value: "5"}})
	if _, err := client.UpdateSheet(ctx, sp, sh, literal); err != nil {
		t.Fatalf("literal update: %v", err)
	}
	if got := esgRead(t, client, sp, sh, "D3"); got != "5" {
		t.Errorf("after literal write D3 = %q, want 5", got)
	}
}

func TestDemoESGValuesEndpoint(t *testing.T) {
	useESGData(t)
	client := esgClient(t, NewDemoTransport())
	resp, err := client.GetRangeValues(context.Background(), "demo-esg-source", "demo-esg-source-2025", "D2:D3")
	if err != nil {
		t.Fatalf("GetRangeValues: %v", err)
	}
	if len(resp.Data) != 1 || len(resp.Data[0].Values) != 2 {
		t.Fatalf("values = %+v, want one range of two rows", resp.Data)
	}
	if v, ok := resp.Data[0].Values[0][0].(float64); !ok || v != 1240000.5 {
		t.Errorf("D2 = %#v, want 1240000.5", resp.Data[0].Values[0][0])
	}
	if v := resp.Data[0].Values[1][0]; v != "" {
		t.Errorf("D3 = %#v, want empty string", v)
	}
}

func TestDemoESGUnknownIDs(t *testing.T) {
	useESGData(t)
	transport := NewDemoTransport()
	notFound := []string{
		"/spreadsheets/demo-esg-nope/sheets",
		"/spreadsheets/demo-esg-source/sheets/nope/sheetdata",
		"/spreadsheets/demo-esg-source/sheets/nope/values/A1",
		"/spreadsheets/demo-esg-source/other",
	}
	for _, path := range notFound {
		status, body, header := esgRoundTrip(t, transport, http.MethodGet, path, "")
		if status != http.StatusNotFound || !json.Valid([]byte(body)) || header.Get("Content-Type") != "application/json" {
			t.Errorf("GET %s = %d %q %q, want JSON 404", path, status, header.Get("Content-Type"), body)
		}
	}
	update := "/spreadsheets/demo-esg-report/sheets/demo-esg-report-cy/update"
	bad := map[string]string{
		"row past data":    `{"editCells":{"cells":[{"row":4,"column":3,"value":"1"}]}}`,
		"column past data": `{"editCells":{"cells":[{"row":1,"column":4,"value":"1"}]}}`,
		"negative row":     `{"editCells":{"cells":[{"row":-1,"column":3,"value":"1"}]}}`,
		"object value":     `{"editCells":{"cells":[{"row":1,"column":3,"value":{"a":1}}]}}`,
		"no cells":         `{"editCells":{"cells":[]}}`,
	}
	for name, body := range bad {
		status, resp, _ := esgRoundTrip(t, transport, http.MethodPost, update, body)
		if status != http.StatusBadRequest || !json.Valid([]byte(resp)) {
			t.Errorf("%s: status = %d body %s, want JSON 400", name, status, resp)
		}
	}
	ok := `{"editCells":{"cells":[{"row":1,"column":3,"value":7}],"options":{"applyEnteredInScaling":false}}}`
	status, resp, _ := esgRoundTrip(t, transport, http.MethodPost, update, ok)
	if status != http.StatusAccepted || !strings.Contains(resp, "/operations/demo-op-1") {
		t.Errorf("valid update = %d %s, want 202 with operation", status, resp)
	}
	status, _, _ = esgRoundTrip(t, transport, http.MethodPost, "/spreadsheets/demo-esg-report/sheets/nope/update", ok)
	if status != http.StatusNotFound {
		t.Errorf("update on unknown sheet = %d, want 404", status)
	}
}

func TestDemoESGMissingDataDir(t *testing.T) {
	t.Setenv("NL_DEMO_ESG_DATA", filepath.Join(t.TempDir(), "missing"))
	status, body, _ := esgRoundTrip(t, NewDemoTransport(), http.MethodGet,
		"/spreadsheets/demo-esg-source/sheets/demo-esg-source-2025/sheetdata", "")
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", status)
	}
	if !json.Valid([]byte(body)) || !strings.Contains(body, "NL_DEMO_ESG_DATA") {
		t.Errorf("body = %s, want JSON naming NL_DEMO_ESG_DATA", body)
	}
}

func TestDemoESGInvalidCSV(t *testing.T) {
	dir := useESGData(t)
	if err := os.WriteFile(filepath.Join(dir, "esg-2025.csv"), []byte("wrong,header\n1,2\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	status, body, _ := esgRoundTrip(t, NewDemoTransport(), http.MethodGet,
		"/spreadsheets/demo-esg-source/sheets/demo-esg-source-2025/sheetdata", "")
	if status != http.StatusInternalServerError || !strings.Contains(body, "esg-2025.csv") {
		t.Errorf("status = %d body %s, want 500 naming esg-2025.csv", status, body)
	}
}

func TestDemoESGDefaultDataDir(t *testing.T) {
	t.Setenv("NL_DEMO_ESG_DATA", "")
	if err := os.Unsetenv("NL_DEMO_ESG_DATA"); err != nil {
		t.Fatalf("unsetenv: %v", err)
	}
	root := t.TempDir()
	writeESGFiles(t, filepath.Join(root, "configs", "demo", "data"))
	t.Chdir(root)
	client := esgClient(t, NewDemoTransport())
	if got := esgRead(t, client, "demo-esg-source", "demo-esg-source-2025", "D2"); got != "1240000.5" {
		t.Errorf("default dir D2 = %q, want 1240000.5", got)
	}
}

func TestDemoESGKeepsVSMERoutes(t *testing.T) {
	useESGData(t)
	status, body, _ := esgRoundTrip(t, NewDemoTransport(), http.MethodGet, "/spreadsheets/demo-sp-energy-2026/sheets", "")
	if status != http.StatusOK || !strings.Contains(body, "demo-sh-b3-energy") {
		t.Errorf("VSME sheets = %d %s, want 200 with demo-sh-b3-energy", status, body)
	}
}
