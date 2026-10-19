package workiva

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDemoDocumentsResponseNotFound(t *testing.T) {
	transport := NewDemoTransport()
	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/documents/x"},
		{http.MethodPost, "/documents/x"},
		{http.MethodGet, "/content/x"},
	}
	for _, tc := range cases {
		req, err := http.NewRequestWithContext(context.Background(), tc.method, "https://api.eu.wdesk.com"+tc.path, nil)
		if err != nil {
			t.Fatalf("build request %s %s: %v", tc.method, tc.path, err)
		}
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip %s %s: %v", tc.method, tc.path, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("read body %s %s: %v", tc.method, tc.path, err)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404", tc.method, tc.path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s %s Content-Type = %q, want application/json", tc.method, tc.path, ct)
		}
		if !json.Valid(body) {
			t.Errorf("%s %s body is not valid JSON: %s", tc.method, tc.path, body)
		}
		if !strings.Contains(string(body), "demo report documents not served yet") {
			t.Errorf("%s %s body = %s, want lane hook message", tc.method, tc.path, body)
		}
	}
}
