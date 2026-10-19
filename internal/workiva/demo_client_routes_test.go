package workiva

import (
	"context"
	"net/http"
	"testing"
)

func TestDemoTransportRoutesLaneHooks(t *testing.T) {
	cases := []struct {
		name string
		path string
		want func(string) bool
		yes  bool
	}{
		{"documents root", "/documents", isDemoDocumentsPath, true},
		{"documents child", "/documents/x", isDemoDocumentsPath, true},
		{"content child", "/content/x", isDemoDocumentsPath, true},
		{"documents prefix only", "/documentsx", isDemoDocumentsPath, false},
		{"spreadsheet sheets is not documents", "/spreadsheets/demo-sp-energy-2026/sheets", isDemoDocumentsPath, false},
		{"esg sheets", "/spreadsheets/demo-esg-x/sheets", isDemoESGPath, true},
		{"fixture sheets is not esg", "/spreadsheets/demo-sp-energy-2026/sheets", isDemoESGPath, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.want(tc.path); got != tc.yes {
				t.Errorf("predicate(%q) = %v, want %v", tc.path, got, tc.yes)
			}
		})
	}

	transport := NewDemoTransport()
	for _, path := range []string{"/spreadsheets", "/spreadsheets/demo-sp-energy-2026/sheets"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.eu.wdesk.com"+path, nil)
		if err != nil {
			t.Fatalf("build request %s: %v", path, err)
		}
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, resp.StatusCode)
		}
	}
}
