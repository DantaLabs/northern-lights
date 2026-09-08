package workiva

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/dantalabs/northern-lights/internal/ratelimit"
)

// DefaultAPIVersion is pinned to the Workiva 2026-01-01 API version and
// sent on every request as the X-Version header.
const DefaultAPIVersion = "2026-01-01"

// maxAttempts is the total number of tries (initial + retries) for a
// single Do call.
const maxAttempts = 5

// maxRetryAfterDelay is the longest duration the client will wait in
// response to a Retry-After header, protecting against unreasonably long
// server-requested delays.
const maxRetryAfterDelay = 30 * time.Second

// APIError describes a terminal non-2xx response from the Workiva API
// after all retries are exhausted.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("workiva api error: status %d: %s", e.StatusCode, e.Body)
}

// Client is the rate-limited, retrying HTTP client for the Workiva API.
// It injects auth and version headers and honors Retry-After on 429s.
// waitLimiter is the subset of *ratelimit.Limiter used by Client so tests
// can inject a fake without exposing production internals.
type waitLimiter interface {
	Wait(ctx context.Context, category ratelimit.Category) error
}

type Client struct {
	httpClient *http.Client
	tokens     *TokenProvider
	limiter    waitLimiter
	baseURL    *url.URL
	apiVersion string

	// sleep waits for a retry delay; replaceable in tests so retry
	// scenarios run instantly. Production uses sleepContext.
	sleep func(ctx context.Context, d time.Duration) error
}

// NewClient builds a Client targeting baseURL. A nil limiter disables
// rate limiting (useful in tests); a nil httpClient uses
// http.DefaultClient.
func NewClient(baseURL *url.URL, tokens *TokenProvider, limiter *ratelimit.Limiter, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	c := &Client{
		httpClient: httpClient,
		tokens:     tokens,
		baseURL:    baseURL,
		apiVersion: DefaultAPIVersion,
		sleep:      sleepContext,
	}
	// Keep the limiter field as a nil interface when no limiter is
	// supplied so the nil check in Do behaves intuitively.
	if limiter != nil {
		c.limiter = limiter
	}
	return c
}

// Do executes a single API request with rate limiting, auth headers, and
// retry handling, returning the final response. The caller owns
// resp.Body and must close it.
func (c *Client) Do(ctx context.Context, method, path string, body io.Reader, category ratelimit.Category) (*http.Response, error) {
	token, err := c.tokens.ClientCredentialsToken(ctx)
	if err != nil {
		return nil, err
	}

	rel, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("parse path %q: %w", path, err)
	}
	endpoint := c.baseURL.ResolveReference(rel)

	// Buffer the body so retries can resend it. SheetUpdate payloads are
	// small, so materializing them once per call is cheap.
	var bodyBytes []byte
	if body != nil {
		bodyBytes, err = io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
	}

	var lastErr error
	retried401 := false
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if token == "" {
			token, err = c.tokens.ClientCredentialsToken(ctx)
			if err != nil {
				return nil, err
			}
		}
		if c.limiter != nil {
			if err := c.limiter.Wait(ctx, category); err != nil {
				return nil, fmt.Errorf("rate limit wait (%s): %w", category, err)
			}
		}

		var reqBody io.Reader
		if body != nil {
			reqBody = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reqBody)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Version", c.apiVersion)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			// Transport errors are retried with the same backoff as 5xx,
			// but only for idempotent methods where the request may not
			// have reached the server.
			if attempt < maxAttempts && isIdempotent(method) {
				if serr := c.sleep(ctx, backoff(attempt)); serr != nil {
					return nil, serr
				}
				continue
			}
			return nil, fmt.Errorf("request %s %s: %w", method, path, err)
		}

		if resp.StatusCode == http.StatusUnauthorized && !retried401 {
			retried401 = true
			drainAndClose(resp)
			c.tokens.Invalidate()
			token = ""
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxAttempts {
			delay := retryAfterDelay(resp.Header.Get("Retry-After"))
			drainAndClose(resp)
			if err := c.sleep(ctx, delay); err != nil {
				return nil, err
			}
			continue
		}
		if isRetryableServerError(resp.StatusCode) && attempt < maxAttempts && isIdempotent(method) {
			drainAndClose(resp)
			if err := c.sleep(ctx, backoff(attempt)); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return errorResponse(resp)
		}
		return resp, nil
	}

	// Unreachable in practice: the loop either returns or retries, and
	// the final attempt never retries. Guard anyway.
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("workiva api: exhausted %d attempts for %s %s", maxAttempts, method, path)
}

// errorResponse converts a terminal non-2xx response into an *APIError,
// consuming the body.
func errorResponse(resp *http.Response) (*http.Response, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read error response body (status %d): %w", resp.StatusCode, err)
	}
	return nil, &APIError{StatusCode: resp.StatusCode, Body: string(body)}
}

// drainAndClose discards and closes a response body before a retry so the
// connection can be reused.
func drainAndClose(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}

// retryAfterDelay parses the Retry-After header (seconds), falls back to
// one second when absent or unparsable, and caps the returned delay at
// maxRetryAfterDelay.
func retryAfterDelay(header string) time.Duration {
	if header == "" {
		return time.Second
	}
	if secs, err := strconv.Atoi(header); err == nil {
		if secs < 0 {
			return time.Second
		}
		d := time.Duration(secs) * time.Second
		if d > maxRetryAfterDelay {
			return maxRetryAfterDelay
		}
		return d
	}
	return time.Second
}

// isIdempotent reports whether method is safe to retry after the request
// may have reached the server. Only GET and HEAD are idempotent here;
// PATCH/POST/PUT are retried only on 429 (the request was rejected).
func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead:
		return true
	default:
		return false
	}
}

// isRetryableServerError reports 5xx statuses worth retrying.
func isRetryableServerError(code int) bool {
	switch code {
	case http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// backoff returns the exponential delay before retry attempt (1-based):
// 1s, 2s, 4s, 8s for attempts 1..4.
func backoff(attempt int) time.Duration {
	d := time.Second
	for i := 1; i < attempt; i++ {
		d *= 2
	}
	return d
}

// sleepContext waits for d or returns early with ctx.Err().
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
