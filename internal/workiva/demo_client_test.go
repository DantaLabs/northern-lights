package workiva

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func demoBaseURL(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse("https://api.eu.wdesk.com")
	if err != nil {
		t.Fatalf("parse base URL: %v", err)
	}
	return u
}

func TestDemoClientSheetData(t *testing.T) {
	client := NewDemoClient(demoBaseURL(t))
	data, err := client.GetSheetData(context.Background(), "demo-sp-1", "demo-sh-b3-energy", "A1:D6", []string{"value", "calculatedValue"})
	if err != nil {
		t.Fatalf("GetSheetData: %v", err)
	}
	if len(data.Cells) != 6 {
		t.Fatalf("expected 6 rows, got %d", len(data.Cells))
	}
	if len(data.Cells[0]) != 4 {
		t.Fatalf("expected 4 columns, got %d", len(data.Cells[0]))
	}
	if value(data.Cells[1][1]) != "125000" {
		t.Errorf("scope 1 value = %q, want 125000", value(data.Cells[1][1]))
	}
	if data.Cells[3][1].CalculatedValue == nil {
		t.Error("total energy cell missing calculated value")
	}
}

func TestDemoClientUpdateSheet(t *testing.T) {
	client := NewDemoClient(demoBaseURL(t))
	opURL, err := client.UpdateSheet(context.Background(), "demo-sp-1", "demo-sh-b3-energy", NewEditCellsUpdate([]CellEdit{{Column: 1, Row: 1, Value: "99999"}}))
	if err != nil {
		t.Fatalf("UpdateSheet: %v", err)
	}
	if !strings.Contains(opURL, "/operations/demo-op-1") {
		t.Errorf("operation URL = %q, want /operations/demo-op-1", opURL)
	}
	resource, err := client.WaitOperation(context.Background(), opURL)
	if err != nil {
		t.Fatalf("WaitOperation: %v", err)
	}
	if resource == "" {
		t.Error("expected non-empty resource URL")
	}
}

func TestDemoClientRangeValues(t *testing.T) {
	client := NewDemoClient(demoBaseURL(t))
	values, err := client.GetRangeValues(context.Background(), "demo-sp-1", "demo-sh-b3-energy", "A1:D6")
	if err != nil {
		t.Fatalf("GetRangeValues: %v", err)
	}
	if len(values.Data) != 1 || len(values.Data[0].Values) != 6 {
		t.Fatalf("expected one typed 6-row response, got %+v", values.Data)
	}
}

func TestDemoTransportTokenEndpoint(t *testing.T) {
	transport := NewDemoTransport()
	req, err := http.NewRequest(http.MethodPost, "https://api.eu.wdesk.com/iam/v1/oauth2/token", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	t.Cleanup(func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close token response: %v", err)
		}
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read token response: %v", err)
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if tr.AccessToken == "" {
		t.Error("access token empty")
	}
}

func value(c Cell) string {
	if c.Value == nil {
		return ""
	}
	return *c.Value
}

func TestDemoClientValuesInvalidRangeReturnsBadRequest(t *testing.T) {
	client := NewDemoClient(demoBaseURL(t))
	_, err := client.GetRangeValues(context.Background(), "demo-sp-1", "demo-sh-b3-energy", "not-a-range")
	if err == nil {
		t.Fatal("expected error for invalid range, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", apiErr.StatusCode)
	}
}

func TestDemoTransportAcceptsOnlyOfficialUpdateContract(t *testing.T) {
	transport := NewDemoTransport()
	req, err := http.NewRequest(http.MethodPost, "https://api.eu.wdesk.com/spreadsheets/demo-sp-1/sheets/demo-sh-b3-energy/update", strings.NewReader(`{"editCells":{"cells":[{"column":1,"row":1,"value":"99999"}]}}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if !strings.Contains(transport.WrittenCells[req.URL.Path], `"column":1`) {
		t.Errorf("recorded update = %q, want official cell payload", transport.WrittenCells[req.URL.Path])
	}

	legacy, err := http.NewRequest(http.MethodPatch, "https://api.eu.wdesk.com/spreadsheets/demo-sp-1/sheets/demo-sh-b3-energy/data", strings.NewReader(`{"editCells":[]}`))
	if err != nil {
		t.Fatalf("legacy NewRequest: %v", err)
	}
	legacyResp, err := transport.RoundTrip(legacy)
	if err != nil {
		t.Fatalf("legacy RoundTrip: %v", err)
	}
	_ = legacyResp.Body.Close()
	if legacyResp.StatusCode != http.StatusNotFound {
		t.Errorf("legacy status = %d, want 404", legacyResp.StatusCode)
	}
}
