package workiva

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type metadataBody struct {
	io.Reader
	readErr, closeErr error
}

func (b metadataBody) Read(p []byte) (int, error) {
	if b.readErr != nil {
		return 0, b.readErr
	}
	return b.Reader.Read(p)
}
func (b metadataBody) Close() error { return b.closeErr }

func TestUpdatePreservesMetadataOnErrors(t *testing.T) {
	boom := errors.New("body failure")
	for _, tc := range []struct {
		name, body        string
		status            int
		readErr, closeErr error
		sleep             bool
	}{
		{name: "malformed accepted", status: 202, body: "{"},
		{name: "unreadable accepted", status: 202, readErr: boom},
		{name: "unreadable server error", status: 503, readErr: boom},
		{name: "accepted close header", status: 202, body: "{}", closeErr: boom},
		{name: "accepted close body", status: 202, body: `{"operationLocation":"/operations/body"}`, closeErr: boom},
		{name: "server close", status: 503, body: "failure", closeErr: boom},
		{name: "unauthorized unreadable", status: 401, readErr: boom},
		{name: "rate limited close", status: 429, closeErr: boom},
		{name: "rate limited sleep", status: 429, body: "{}", sleep: true},
		{name: "initial sleep", status: 202, body: "{}", sleep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := setupFastClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {}))
			original := c.httpClient.Transport
			posts := 0
			c.httpClient.Transport = roundTripFn(func(r *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(r.URL.Path, "/update") {
					return original.RoundTrip(r)
				}
				posts++
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Location": {"/operations/header"}, "Retry-After": {"1"}}, Body: metadataBody{Reader: strings.NewReader(tc.body), readErr: tc.readErr, closeErr: tc.closeErr}}, nil
			})
			if tc.sleep {
				c.sleep = func(context.Context, time.Duration) error { return context.Canceled }
			}
			op, err := c.UpdateSheet(context.Background(), "sp", "sh", NewEditCellsUpdate([]CellEdit{{Value: 1}}))
			want := "/operations/header"
			if strings.Contains(tc.body, "/operations/body") {
				want = "/operations/body"
			}
			if err == nil || op != want {
				t.Fatalf("op=%q err=%v, want %q and error", op, err, want)
			}
			if tc.status >= 300 {
				var api *APIError
				if !errors.As(err, &api) || api.StatusCode != tc.status || api.OperationURL != want {
					t.Fatalf("missing API metadata: %v", err)
				}
			}
			if posts != 1 {
				t.Fatalf("POSTs=%d", posts)
			}
		})
	}
}

func TestPOSTRetriesOnlyExplicitNonAcceptance(t *testing.T) {
	for _, status := range []int{401, 429, 400, 500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			posts := 0
			c, _ := setupFastClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("method=%s", r.Method)
				}
				posts++
				if posts == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(status)
					return
				}
				w.Header().Set("Location", "/operations/retried")
				w.WriteHeader(202)
			}))
			op, err := c.UpdateSheet(context.Background(), "sp", "sh", NewEditCellsUpdate([]CellEdit{{Value: 1}}))
			if status == 401 || status == 429 {
				if posts != 2 || err != nil || op != "/operations/retried" {
					t.Fatalf("posts=%d op=%q err=%v", posts, op, err)
				}
			} else if posts != 1 || err == nil {
				t.Fatalf("posts=%d err=%v", posts, err)
			}
		})
	}
}
