package workiva

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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
			if _, err := fmt.Fprint(w, `{"access_token":"tok-1","expires_in":3600}`); err != nil {
				t.Errorf("write token response: %v", err)
			}
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
		if _, err := fmt.Fprint(w, `{"ok":true}`); err != nil {
			t.Errorf("write API response: %v", err)
		}
	}))

	resp, err := c.Do(context.Background(), http.MethodGet, "/spreadsheets/s1/sheets/sh1/sheetdata", nil, ratelimit.CategoryReads)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	})

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
		if _, err := fmt.Fprint(w, `{}`); err != nil {
			t.Errorf("write API response: %v", err)
		}
	}))

	_, err := c.Do(context.Background(), http.MethodPatch, "/x", newStubReader("{}"), ratelimit.CategoryWrites)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}
}

func TestDoRetries401WithFreshToken(t *testing.T) {
	var tokenCalls atomic.Int32
	var apiCalls atomic.Int32
	var authHeaders []string
	var authMu sync.Mutex

	c, _, _ := setupTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/iam/v1/oauth2/token" {
			n := tokenCalls.Add(1)
			if _, err := fmt.Fprintf(w, `{"access_token":"tok-%d","expires_in":3600}`, n); err != nil {
				t.Errorf("write token response: %v", err)
			}
			return
		}

		n := apiCalls.Add(1)
		authMu.Lock()
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		authMu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		if _, err := fmt.Fprint(w, `{"ok":true}`); err != nil {
			t.Errorf("write API response: %v", err)
		}
	}))

	resp, err := c.Do(context.Background(), http.MethodGet, "/x", nil, ratelimit.CategoryReads)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}

	if got := tokenCalls.Load(); got != 2 {
		t.Errorf("token requests = %d, want 2", got)
	}
	if got := apiCalls.Load(); got != 2 {
		t.Errorf("API requests = %d, want 2", got)
	}
	authMu.Lock()
	defer authMu.Unlock()
	if got := authHeaders[0]; got != "Bearer tok-1" {
		t.Errorf("first Authorization = %q, want %q", got, "Bearer tok-1")
	}
	if got := authHeaders[1]; got != "Bearer tok-2" {
		t.Errorf("second Authorization = %q, want %q", got, "Bearer tok-2")
	}
}

func TestDoReturns401APIErrorAfterOne401Retry(t *testing.T) {
	var tokenCalls atomic.Int32
	var apiCalls atomic.Int32

	c, _, _ := setupTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/iam/v1/oauth2/token" {
			tokenCalls.Add(1)
			if _, err := fmt.Fprint(w, `{"access_token":"tok-1","expires_in":3600}`); err != nil {
				t.Errorf("write token response: %v", err)
			}
			return
		}
		apiCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))

	_, err := c.Do(context.Background(), http.MethodGet, "/x", nil, ratelimit.CategoryReads)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", apiErr.StatusCode)
	}
	if got := apiCalls.Load(); got != 2 {
		t.Errorf("API requests = %d, want 2", got)
	}
	if got := tokenCalls.Load(); got != 2 {
		t.Errorf("token requests = %d, want 2", got)
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
		if _, err := fmt.Fprint(w, `{"ok":true}`); err != nil {
			t.Errorf("write API response: %v", err)
		}
	}))

	resp, err := c.Do(context.Background(), http.MethodGet, "/x", nil, ratelimit.CategoryReads)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}

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
		if _, err := fmt.Fprint(w, `{"error":"bad range"}`); err != nil {
			t.Errorf("write error response: %v", err)
		}
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

func TestDoRetries500ForGetNotPatch(t *testing.T) {
	var apiCalls atomic.Int32
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		n := apiCalls.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		if _, err := fmt.Fprint(w, `{"ok":true}`); err != nil {
			t.Errorf("write API response: %v", err)
		}
	}))

	resp, err := c.Do(context.Background(), http.MethodGet, "/x", nil, ratelimit.CategoryReads)
	if err != nil {
		t.Fatalf("GET Do: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}
	if got := apiCalls.Load(); got != 2 {
		t.Errorf("GET API calls = %d, want 2", got)
	}

	apiCalls.Store(0)
	headerCalls := make(map[string]int)
	c2, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		headerCalls[key]++
		if headerCalls[key] == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		if _, err := fmt.Fprint(w, `{"ok":true}`); err != nil {
			t.Errorf("write API response: %v", err)
		}
	}))
	_, err = c2.Do(context.Background(), http.MethodPatch, "/x", strings.NewReader(`{}`), ratelimit.CategoryWrites)
	if err == nil {
		t.Fatal("PATCH Do: expected error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("PATCH Do error = %v, want 500 APIError", err)
	}
	if got := headerCalls["PATCH /x"]; got != 1 {
		t.Errorf("PATCH API calls = %d, want 1 (no retry on 500 for non-idempotent method)", got)
	}
}

func TestDoRetriesTransportErrorForGetNotPatch(t *testing.T) {
	var attempts atomic.Int32
	errDo := errors.New("boom")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// token endpoint returns token
		if _, err := fmt.Fprint(w, `{"access_token":"tok-1","expires_in":3600}`); err != nil {
			t.Errorf("write token response: %v", err)
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	tokens := NewTokenProvider(u, "c", "s", "file:read", srv.Client())
	c := NewClient(u, tokens, nil, &http.Client{
		Transport: roundTripFn(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/iam/v1/oauth2/token" {
				return srv.Client().Transport.RoundTrip(r)
			}
			attempts.Add(1)
			return nil, errDo
		}),
	})
	c.sleep = func(ctx context.Context, d time.Duration) error { return nil }

	_, err := c.Do(context.Background(), http.MethodGet, "/x", nil, ratelimit.CategoryReads)
	if err == nil {
		t.Fatal("GET Do: expected error")
	}
	if got := attempts.Load(); got != 5 {
		t.Errorf("GET attempts = %d, want 5", got)
	}

	attempts.Store(0)
	_, err = c.Do(context.Background(), http.MethodPatch, "/x", strings.NewReader(`{}`), ratelimit.CategoryWrites)
	if err == nil {
		t.Fatal("PATCH Do: expected error")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("PATCH attempts = %d, want 1 (no retry on transport error for non-idempotent method)", got)
	}
}

type roundTripFn func(*http.Request) (*http.Response, error)

func (f roundTripFn) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDoRetriesServerErrorWithBackoff(t *testing.T) {
	var apiCalls atomic.Int32
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		n := apiCalls.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		if _, err := fmt.Fprint(w, `{"ok":true}`); err != nil {
			t.Errorf("write API response: %v", err)
		}
	}))

	resp, err := c.Do(context.Background(), http.MethodGet, "/x", nil, ratelimit.CategoryReads)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}
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

// fakeWaitLimiter records how many times Wait was called and optionally
// returns an error after a configured number of calls.
type fakeWaitLimiter struct {
	mu       sync.Mutex
	calls    int
	errAfter int
}

func (f *fakeWaitLimiter) Wait(ctx context.Context, category ratelimit.Category) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.errAfter > 0 && f.calls >= f.errAfter {
		return errors.New("rate limiter exhausted")
	}
	return nil
}

func (f *fakeWaitLimiter) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// setupTestClientWithLimiter wires a Client with an injectable limiter.
func setupTestClientWithLimiter(t *testing.T, handler http.Handler, limiter waitLimiter) (*Client, *atomic.Int32) {
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
	c := NewClient(u, tokens, nil, srv.Client())
	c.limiter = limiter
	c.sleep = func(ctx context.Context, d time.Duration) error { return nil }
	return c, &requests
}

func TestDoCallsLimiterOncePerAttempt(t *testing.T) {
	var apiCalls atomic.Int32
	lim := &fakeWaitLimiter{}
	c, requests := setupTestClientWithLimiter(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		n := apiCalls.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		if _, err := fmt.Fprint(w, `{"ok":true}`); err != nil {
			t.Errorf("write API response: %v", err)
		}
	}), lim)

	resp, err := c.Do(context.Background(), http.MethodGet, "/x", nil, ratelimit.CategoryReads)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}

	if got := apiCalls.Load(); got != 2 {
		t.Errorf("API calls = %d, want 2", got)
	}
	// token fetch + 2 API attempts, each gated by the limiter.
	if got := requests.Load(); got != 3 {
		t.Errorf("total requests = %d, want 3", got)
	}
	if got := lim.Calls(); got != 2 {
		t.Errorf("limiter Wait calls = %d, want 2 (one per attempt)", got)
	}
}

func TestDoHonorsRetryAfterBeyondThirtySeconds(t *testing.T) {
	c, _, _ := setupTestClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))

	var slept atomic.Value
	c.sleep = func(ctx context.Context, d time.Duration) error {
		slept.Store(d)
		return nil // continue retry loop; eventually maxAttempts exhausted.
	}

	_, err := c.Do(context.Background(), http.MethodGet, "/x", nil, ratelimit.CategoryReads)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	d, ok := slept.Load().(time.Duration)
	if !ok {
		t.Fatal("sleep was not recorded")
	}
	if d != 120*time.Second {
		t.Errorf("retry sleep = %v, want 120s from server", d)
	}
}

func TestRetryAfterDelayRejectsOverflowWithoutDurationOverflow(t *testing.T) {
	if got := retryAfterDelay("999999999999999999999999999999"); got != time.Second {
		t.Errorf("overflow Retry-After delay = %v, want 1s fallback", got)
	}
}
