package workiva

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// tokenHandler serves the recorded fixture token response and records
// every request so tests can assert on method, path, and form values.
type tokenHandler struct {
	hits    atomic.Int32
	expires int // expires_in seconds served in the response
}

func (h *tokenHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.hits.Add(1)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fmt.Fprintf(w, `{"access_token":"test-token-abc123","token_type":"Bearer","expires_in":%d}`, h.expires)
}

// fakeTime replaces the package clock and returns a restore function.
func fakeTime(t time.Time) func() {
	old := now
	now = func() time.Time { return t }
	return func() { now = old }
}

func newTokenServer(t *testing.T, h http.Handler) (*httptest.Server, *url.URL) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	return srv, u
}

func TestClientCredentialsTokenRequestsToken(t *testing.T) {
	fixture, err := os.ReadFile("testdata/token_response.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var gotMethod, gotPath string
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gotForm = r.Form
		w.Header().Set("Content-Type", "application/json")
		w.Write(fixture)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}

	p := NewTokenProvider(u, "client-1", "secret-1", "file:read", srv.Client())
	tok, err := p.ClientCredentialsToken(context.Background())
	if err != nil {
		t.Fatalf("ClientCredentialsToken: %v", err)
	}
	if tok != "test-token-abc123" {
		t.Errorf("token = %q, want %q", tok, "test-token-abc123")
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/iam/v1/oauth2/token" {
		t.Errorf("path = %q, want /iam/v1/oauth2/token", gotPath)
	}
	wantForm := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {"client-1"},
		"client_secret": {"secret-1"},
		"scope":         {"file:read"},
	}
	for k, want := range wantForm {
		if got := gotForm.Get(k); got != want[0] {
			t.Errorf("form %s = %q, want %q", k, got, want[0])
		}
	}
}

func TestClientCredentialsTokenCachesWithinTTL(t *testing.T) {
	h := &tokenHandler{expires: 3600}
	_, u := newTokenServer(t, h)

	p := NewTokenProvider(u, "client-1", "secret-1", "file:read", http.DefaultClient)
	ctx := context.Background()

	if _, err := p.ClientCredentialsToken(ctx); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := p.ClientCredentialsToken(ctx); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := h.hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1 (second call must be served from cache)", got)
	}
}

func TestClientCredentialsTokenRefetchesAfterExpiry(t *testing.T) {
	h := &tokenHandler{expires: 3600}
	_, u := newTokenServer(t, h)

	p := NewTokenProvider(u, "client-1", "secret-1", "file:read", http.DefaultClient)
	ctx := context.Background()

	if _, err := p.ClientCredentialsToken(ctx); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Advance the clock past the token lifetime minus the 30s buffer.
	restore := fakeTime(time.Now().Add(time.Hour))
	defer restore()

	if _, err := p.ClientCredentialsToken(ctx); err != nil {
		t.Fatalf("call after expiry: %v", err)
	}
	if got := h.hits.Load(); got != 2 {
		t.Errorf("server hits = %d, want 2", got)
	}
}

func TestClientCredentialsTokenScopePassedThrough(t *testing.T) {
	cases := []struct {
		scope string
	}{
		{"file:read"},
		{"file:write"},
		{"file:read file:write"},
	}
	for _, tc := range cases {
		t.Run(tc.scope, func(t *testing.T) {
			var gotScope string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				gotScope = r.Form.Get("scope")
				fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
			}))
			defer srv.Close()

			u, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatalf("parse server url: %v", err)
			}
			p := NewTokenProvider(u, "c", "s", tc.scope, srv.Client())
			if _, err := p.ClientCredentialsToken(context.Background()); err != nil {
				t.Fatalf("ClientCredentialsToken: %v", err)
			}
			if gotScope != tc.scope {
				t.Errorf("scope sent = %q, want %q", gotScope, tc.scope)
			}
		})
	}
}

func TestClientCredentialsTokenNon200ReturnsTypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid_client","error_description":"bad credentials"}`)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	p := NewTokenProvider(u, "c", "s", "file:read", srv.Client())
	_, err = p.ClientCredentialsToken(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	te, ok := err.(*TokenError)
	if !ok {
		t.Fatalf("error type = %T, want *TokenError", err)
	}
	if te.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want %d", te.StatusCode, http.StatusUnauthorized)
	}
	if te.Body == "" {
		t.Error("Body is empty, want the response body captured")
	}
}
