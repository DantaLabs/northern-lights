package workiva

import (
	"context"
	"testing"
)

func TestDemoClientDiscoversESGSpreadsheets(t *testing.T) {
	client := NewDemoClient(demoBaseURL(t))
	spreadsheets, err := client.ListSpreadsheets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"demo-sp-energy-2026": "VSME Energy Report 2026",
		"demo-sp-energy-2025": "VSME Energy Report 2025",
		"demo-esg-source":     "ESG Source",
		"demo-esg-report":     "ESG Report",
	}
	if len(spreadsheets) != len(want) {
		t.Fatalf("spreadsheets = %+v, want four demo spreadsheets", spreadsheets)
	}
	for _, spreadsheet := range spreadsheets {
		name, ok := want[spreadsheet.ID]
		if !ok || spreadsheet.Name != name || spreadsheet.Template {
			t.Fatalf("unexpected spreadsheet: %+v", spreadsheet)
		}
		delete(want, spreadsheet.ID)
		sheets, err := client.ListSheets(context.Background(), spreadsheet.ID)
		if err != nil || len(sheets) == 0 {
			t.Fatalf("ListSheets(%q) = %+v, %v", spreadsheet.ID, sheets, err)
		}
	}
}
