package workiva

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
)

// demoDocumentsResponse serves report document and content requests in demo mode. L3 replaces this body with an in-memory report document.
func (t *DemoTransport) demoDocumentsResponse(req *http.Request) *http.Response {
	return demoLaneHookNotFound("demo report documents not served yet: " + req.URL.Path)
}

// demoLaneHookNotFound returns a 404 JSON response for a lane hook that has not been filled yet.
func demoLaneHookNotFound(message string) *http.Response {
	body, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		return demoServerError("marshal lane hook error")
	}
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    &http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/"}},
	}
}
