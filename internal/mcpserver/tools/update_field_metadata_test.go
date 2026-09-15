package tools

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/workiva"
)

type failedUpdateBody struct {
	io.Reader
	readErr, closeErr error
}

func (b failedUpdateBody) Read(p []byte) (int, error) {
	if b.readErr != nil {
		return 0, b.readErr
	}
	return b.Reader.Read(p)
}
func (b failedUpdateBody) Close() error { return b.closeErr }

func TestUpdateFieldMetadataReconciliation(t *testing.T) {
	boom := errors.New("response body failure")
	for _, tc := range []struct {
		name, body        string
		status            int
		readErr, closeErr error
	}{
		{"malformed accepted", "{", 202, nil, nil},
		{"unreadable accepted", "", 202, boom, nil},
		{"unreadable non2xx", "", 503, boom, nil},
		{"accepted close", "{}", 202, nil, boom},
		{"non2xx close", "failure", 503, nil, boom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, tokenHandler(t, http.NotFound))
			seedField(t, env)
			env.deps.Cfg.RequireWriteConfirmation = false
			posts := 0
			hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				response := &http.Response{StatusCode: 200, Header: make(http.Header)}
				switch {
				case strings.Contains(r.URL.Path, "/oauth2/token"):
					response.Body = io.NopCloser(strings.NewReader(`{"access_token":"token","expires_in":3600}`))
				case strings.HasSuffix(r.URL.Path, "/sheetdata"):
					response.Body = io.NopCloser(strings.NewReader(sheetdataBody))
				case strings.HasSuffix(r.URL.Path, "/update"):
					posts++
					response.StatusCode = tc.status
					response.Header.Set("Location", "/operations/reconcile")
					response.Body = failedUpdateBody{Reader: strings.NewReader(tc.body), readErr: tc.readErr, closeErr: tc.closeErr}
				default:
					t.Errorf("unexpected request: %s", r.URL)
					return nil, errors.New("unexpected request")
				}
				return response, nil
			})}
			base, _ := url.Parse("https://workiva.test")
			env.deps.Client = workiva.NewClient(base, workiva.NewTokenProvider(base, "id", "secret", "file:write", hc), nil, hc)
			result := callTool(t, env.deps, UpdateField(), map[string]any{"name": "scope2_energy_kwh", "value": "new"})
			if result.IsError {
				t.Fatal(result.Content)
			}
			got := structuredContent(t, result)
			if got["status"] != "write_outcome_unknown" || got["workiva_op_url"] != "/operations/reconcile" || got["before"] != "1234" || got["after_preview"] != "new" {
				t.Fatalf("reconciliation=%v", got)
			}
			if !strings.Contains(got["message"].(string), "Do not retry") || posts != 1 {
				t.Fatalf("posts=%d output=%v", posts, got)
			}
		})
	}
}
