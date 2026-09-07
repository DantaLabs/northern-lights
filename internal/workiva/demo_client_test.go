package workiva

import (
	"context"
	"encoding/json"
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
	rng, err := A1ToRange("B2")
	if err != nil {
		t.Fatalf("A1ToRange: %v", err)
	}
	opURL, err := client.UpdateSheet(context.Background(), "demo-sp-1", "demo-sh-b3-energy", NewEditCellsUpdate([]CellEdit{{Range: rng, Value: "99999"}}))
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
	grid, ok := values.([]any)
	if !ok || len(grid) != 6 {
		t.Fatalf("expected 6-row array, got %T / %d rows", values, len(grid))
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
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
