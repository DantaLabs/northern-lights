package backupjob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

const (
	testScope = "api://11111111-2222-4333-8444-555555555555/.default"
	testKey   = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testToken = "fixture-token-never-expose"
)

type fixtureCredential struct {
	calls  atomic.Int32
	scopes []string
	token  string
	err    error
}

func (c *fixtureCredential) GetToken(_ context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.calls.Add(1)
	c.scopes = append([]string(nil), options.Scopes...)
	if c.err != nil {
		return azcore.AccessToken{}, c.err
	}
	return azcore.AccessToken{Token: c.token, ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func fixtureConfig(endpoint string) Config {
	return Config{Endpoint: endpoint, Audience: testScope, CheckpointPublicKeyHex: testKey}
}

func fixtureClient(t *testing.T, endpoint string, credential *fixtureCredential, client *http.Client) *Client {
	t.Helper()
	if credential == nil {
		credential = &fixtureCredential{token: testToken}
	}
	job, err := NewClient(fixtureConfig(endpoint), credential, client)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return job
}

func validReceiptJSON(key string) string {
	return fmt.Sprintf(`{"envelope_id":"0123456789abcdef0123456789abcdef","database_sha256":"%s","manifest_sha256":"%s","database_etag":"\"db-etag\"","manifest_etag":"\"manifest-etag\"","audit_high_water":7,"checkpoint_public_key":"%s","checkpoint_id":"123e4567-e89b-42d3-a456-426614174000","minimum_created_at":"2026-10-08T03:04:05Z"}`, strings.Repeat("a", 64), strings.Repeat("b", 64), key)
}

func TestRunOncePostsOneAuthenticatedRequestAndReturnsSealedReceipt(t *testing.T) {
	var requests atomic.Int32
	credential := &fixtureCredential{token: testToken}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/maintenance/backup" || r.URL.RawQuery != "" {
			t.Errorf("request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("authorization header mismatch")
		}
		if r.ContentLength != 0 {
			t.Errorf("request content length = %d, want empty body", r.ContentLength)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, validReceiptJSON(testKey))
	}))
	defer server.Close()

	job := fixtureClient(t, server.URL+"/maintenance/backup", credential, server.Client())
	event, err := job.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("backup POST count = %d, want one", requests.Load())
	}
	if got := credential.calls.Load(); got != 1 || len(credential.scopes) != 1 || credential.scopes[0] != testScope {
		t.Fatalf("credential calls/scopes = %d/%v", got, credential.scopes)
	}
	if event.Type != "nl.backup.sealed" || event.Host == "" || event.ReceivedAt.IsZero() || event.Receipt.EnvelopeID != "0123456789abcdef0123456789abcdef" || event.Receipt.CheckpointPublicKey != testKey {
		t.Fatalf("sealed event has unexpected public metadata: %+v", event)
	}
	if strings.Contains(event.Receipt.DatabaseETag, testToken) || strings.Contains(event.Receipt.ManifestETag, testToken) {
		t.Fatal("token appeared in receipt")
	}
}

func TestNewClientRejectsMissingCredential(t *testing.T) {
	if _, err := NewClient(fixtureConfig("https://backup.example/maintenance/backup"), nil, nil); err == nil {
		t.Fatal("nil credential accepted")
	}
}

func TestRunOnceRejectsNilOrCanceledContextBeforeCredentialUse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unexpected request") }))
	defer server.Close()
	credential := &fixtureCredential{token: testToken}
	job := fixtureClient(t, server.URL+"/maintenance/backup", credential, server.Client())
	var nilContext context.Context
	if _, err := job.RunOnce(nilContext); err == nil {
		t.Fatal("nil context accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := job.RunOnce(ctx); err == nil {
		t.Fatal("canceled context accepted")
	}
	if credential.calls.Load() != 0 {
		t.Fatalf("credential calls = %d, want zero", credential.calls.Load())
	}
}

func TestRunOnceDoesNotRetryHTTPStatusFailuresOrExposeResponseBody(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusBadRequest, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "private-response-body-"+testToken)
			}))
			defer server.Close()
			job := fixtureClient(t, server.URL+"/maintenance/backup", nil, server.Client())
			_, err := job.RunOnce(context.Background())
			if err == nil || requests.Load() != 1 || strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), "private-response-body") {
				t.Fatalf("status failure err=%v requests=%d", err, requests.Load())
			}
		})
	}
}

func TestRunOnceDoesNotFollowRedirect(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetRequests.Add(1) }))
	defer target.Close()
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, target.URL+"/maintenance/backup", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	job := fixtureClient(t, server.URL+"/maintenance/backup", nil, server.Client())
	_, err := job.RunOnce(context.Background())
	if err == nil || requests.Load() != 1 || targetRequests.Load() != 0 {
		t.Fatalf("redirect result err=%v original=%d target=%d", err, requests.Load(), targetRequests.Load())
	}
}

func TestRunOnceSanitizesCredentialAndTransportFailuresWithoutRetry(t *testing.T) {
	t.Run("credential", func(t *testing.T) {
		credential := &fixtureCredential{err: errors.New("sdk leaked " + testToken)}
		job := fixtureClient(t, "https://backup.example/maintenance/backup", credential, nil)
		_, err := job.RunOnce(context.Background())
		if err == nil || strings.Contains(err.Error(), testToken) {
			t.Fatalf("credential error leaked details: %v", err)
		}
	})
	t.Run("transport", func(t *testing.T) {
		var requests atomic.Int32
		transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, errors.New("transport leaked " + testToken)
		})
		credential := &fixtureCredential{token: testToken}
		job := fixtureClient(t, "https://backup.example/maintenance/backup", credential, &http.Client{Transport: transport})
		_, err := job.RunOnce(context.Background())
		if err == nil || requests.Load() != 1 || strings.Contains(err.Error(), testToken) {
			t.Fatalf("transport error leaked details or retried: err=%v requests=%d", err, requests.Load())
		}
	})
}

func TestRunOnceHonorsCancellationWithoutRetry(t *testing.T) {
	var requests atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	job := fixtureClient(t, "https://backup.example/maintenance/backup", nil, &http.Client{Transport: transport})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	_, err := job.RunOnce(ctx)
	if err == nil || requests.Load() != 1 {
		t.Fatalf("cancellation err=%v requests=%d", err, requests.Load())
	}
}

func TestRunOnceHonorsCallerDeadlineWithoutRetry(t *testing.T) {
	var requests atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	job := fixtureClient(t, "https://backup.example/maintenance/backup", nil, &http.Client{Transport: transport})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := job.RunOnce(ctx)
	if err == nil || requests.Load() != 1 {
		t.Fatalf("deadline err=%v requests=%d", err, requests.Load())
	}
}

func TestRunOnceRejectsOversizedBodyAndClosesIt(t *testing.T) {
	body := &closeTrackingBody{Reader: strings.NewReader(strings.Repeat("x", maxResponseBytes+1))}
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body}, nil
	})
	job := fixtureClient(t, "https://backup.example/maintenance/backup", nil, &http.Client{Transport: transport})
	if _, err := job.RunOnce(context.Background()); err == nil || !body.closed {
		t.Fatalf("oversized response err=%v body_closed=%v", err, body.closed)
	}
}

func TestRunOnceRejectsMalformedOrIncompleteReceiptAndClosesBody(t *testing.T) {
	for name, response := range map[string]string{
		"malformed json": `{`,
		"missing field":  `{"envelope_id":"0123456789abcdef0123456789abcdef"}`,
		"uppercase hash": strings.Replace(validReceiptJSON(testKey), strings.Repeat("a", 64), strings.Repeat("A", 64), 1),
		"wrong key":      validReceiptJSON(strings.Repeat("c", 64)),
		"invalid etag":   strings.Replace(validReceiptJSON(testKey), `\"db-etag\"`, `db-etag`, 1),
		"unknown field":  strings.TrimSuffix(validReceiptJSON(testKey), "}") + `,"unexpected":"value"}`,
		"trailing json":  validReceiptJSON(testKey) + `{}`,
		"duplicate key":  strings.Replace(validReceiptJSON(testKey), `,"database_sha256":`, `,"envelope_id":"fedcba9876543210fedcba9876543210","database_sha256":`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			body := &closeTrackingBody{Reader: strings.NewReader(response)}
			transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body}, nil
			})
			job := fixtureClient(t, "https://backup.example/maintenance/backup", nil, &http.Client{Transport: transport})
			if _, err := job.RunOnce(context.Background()); err == nil || !body.closed || strings.Contains(err.Error(), testToken) {
				t.Fatalf("bad receipt err=%v body_closed=%v", err, body.closed)
			}
		})
	}
}

func TestValidateConfigRejectsMalformedTrustedEndpointAndIdentityValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{"http", func(c *Config) { c.Endpoint = "http://backup.example/maintenance/backup" }},
		{"wrong path", func(c *Config) { c.Endpoint = "https://backup.example/other" }},
		{"query", func(c *Config) { c.Endpoint += "?next=elsewhere" }},
		{"fragment", func(c *Config) { c.Endpoint += "#fragment" }},
		{"userinfo", func(c *Config) { c.Endpoint = "https://user@backup.example/maintenance/backup" }},
		{"wildcard host", func(c *Config) { c.Endpoint = "https://*.example/maintenance/backup" }},
		{"bad scope", func(c *Config) { c.Audience = "api://not-a-uuid/.default" }},
		{"uppercase scope uuid", func(c *Config) { c.Audience = "api://11111111-2222-4333-8444-55555555555A/.default" }},
		{"bad checkpoint key", func(c *Config) { c.CheckpointPublicKeyHex = "not-a-key" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fixtureConfig("https://backup.example/maintenance/backup")
			tc.edit(&cfg)
			if err := ValidateConfig(cfg); err == nil {
				t.Fatal("invalid trusted configuration accepted")
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type closeTrackingBody struct {
	io.Reader
	closed bool
}

func (b *closeTrackingBody) Close() error { b.closed = true; return nil }
