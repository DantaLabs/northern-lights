package workiva

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/ratelimit"
)

// setupTestClient wires a Client against a single httptest server that
// serves both the token endpoint and the API paths under test. The sleep
// function is replaced with a no-op recorder so retry tests run fast.
func setupTestClient(t *testing.T, handler http.Handler) (*Client, *atomic.Int32, *url.URL) {
	t.Helper()

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}

	tokens := NewTokenProvider(u, "test-client", "test-secret", "file:read", srv.Client())
	c := NewClient(u, tokens, ratelimit.NewLimiter(), srv.Client())

	c.sleep = func(ctx context.Context, d time.Duration) error {
		return nil
	}
	return c, &requests, u
}

// tokenResponder serves token requests; anything else 404s.
func tokenResponder(t *testing.T, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/iam/v1/oauth2/token":
			fmt.Fprint(w, `{"access_token":"tok-1","expires_in":3600}`)
		default:
			next(w, r)
		}
	}
}

func TestDoInjectsHeaders(t *testing.T) {
	type headers struct {
		auth      string
		xVersion  string
		accept    string
		authCount int
	}
	var got headers

	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		got.auth = r.Header.Get("Authorization")
		got.xVersion = r.Header.Get("X-Version")
		got.accept = r.Header.Get("Accept")
		got.authCount++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))

	resp, err := c.Do(context.Background(), http.MethodGet, "/spreadsheets/s1/sheets/sh1/sheetdata", nil, ratelimit.CategoryReads)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if got.auth != "Bearer tok-1" {
		t.Errorf("Authorization = %q, want %q", got.auth, "Bearer tok-1")
	}
	if got.xVersion != "2026-01-01" {
		t.Errorf("X-Version = %q, want %q", got.xVersion, "2026-01-01")
	}
	if got.accept != "application/json" {
		t.Errorf("Accept = %q, want %q", got.accept, "application/json")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %s", body)
	}
}

func TestDoSetsContentTypeWithBody(t *testing.T) {
	var contentType string
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		fmt.Fprint(w, `{}`)
	}))

	_, err := c.Do(context.Background(), http.MethodPatch, "/x", newStubReader("{}"), ratelimit.CategoryWrites)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}
}

func TestDoRetries429HonoringRetryAfter(t *testing.T) {
	var apiCalls atomic.Int32
	c, requests, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		n := apiCalls.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ok":true}`)
	}))

	resp, err := c.Do(context.Background(), http.MethodGet, "/x", nil, ratelimit.CategoryReads)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if got := requests.Load(); got != 3 {
		t.Errorf("total requests = %d, want 3 (1 token + 2 API: initial 429 and retry)", got)
	}
	if got := apiCalls.Load(); got != 2 {
		t.Errorf("API calls = %d, want 2", got)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestDo429ForeverReturnsAPIErrorAfterFiveAttempts(t *testing.T) {
	var apiCalls atomic.Int32
	c, requests, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))

	_, err := c.Do(context.Background(), http.MethodGet, "/x", nil, ratelimit.CategoryReads)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want 429", apiErr.StatusCode)
	}
	if got := apiCalls.Load(); got != 5 {
		t.Errorf("API attempts = %d, want 5", got)
	}
	// 5 API attempts + 1 token fetch.
	if got := requests.Load(); got != 6 {
		t.Errorf("total requests = %d, want 6", got)
	}
}

func TestDo400ReturnsAPIErrorWithoutRetry(t *testing.T) {
	var apiCalls atomic.Int32
	c, requests, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"bad range"}`)
	}))

	_, err := c.Do(context.Background(), http.MethodGet, "/x", nil, ratelimit.CategoryReads)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want 400", apiErr.StatusCode)
	}
	if apiErr.Body != `{"error":"bad range"}` {
		t.Errorf("Body = %q", apiErr.Body)
	}
	if got := apiCalls.Load(); got != 1 {
		t.Errorf("API attempts = %d, want 1 (no retry on 400)", got)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("total requests = %d, want 2 (token + 1 API)", got)
	}
}

func TestDoRetriesServerErrorWithBackoff(t *testing.T) {
	var apiCalls atomic.Int32
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		n := apiCalls.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ok":true}`)
	}))

	resp, err := c.Do(context.Background(), http.MethodGet, "/x", nil, ratelimit.CategoryReads)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if got := apiCalls.Load(); got != 3 {
		t.Errorf("API attempts = %d, want 3", got)
	}
}

func TestDoRespectsContextCancellationDuringRetrySleep(t *testing.T) {
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	c.sleep = func(ctx context.Context, d time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return nil
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.Do(ctx, http.MethodGet, "/x", nil, ratelimit.CategoryReads)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

// stubReader is the minimal io.Reader for test bodies.
type stubReader struct {
	s string
	i int
}

func newStubReader(s string) *stubReader { return &stubReader{s: s} }

func (r *stubReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}
