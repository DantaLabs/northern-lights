package workiva

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
)

// demoESGResponse serves demo-esg spreadsheet requests in demo mode. L4 replaces this body with the ESG fixture spreadsheets.
func (t *DemoTransport) demoESGResponse(req *http.Request) *http.Response {
	return demoESGNotFound("demo ESG fixture not served yet: " + req.URL.Path)
}

// demoESGNotFound returns a 404 JSON response for an ESG fixture that has not been filled yet.
func demoESGNotFound(message string) *http.Response {
	body, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		return demoServerError("marshal ESG fixture error")
	}
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    &http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/"}},
	}
}
