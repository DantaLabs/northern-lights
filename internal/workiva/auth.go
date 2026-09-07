package workiva

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// expiryBuffer is subtracted from the token lifetime so a cached token is
// refreshed before Workiva would actually reject it.
const expiryBuffer = 30 * time.Second

// now is the package clock, swappable in tests.
var now = time.Now

// TokenError describes a failed token endpoint response. StatusCode and
// Body capture the raw upstream response for debugging.
type TokenError struct {
	StatusCode int
	Body       string
}

func (e *TokenError) Error() string {
	return fmt.Sprintf("workiva token request failed: status %d: %s", e.StatusCode, e.Body)
}

// TokenProvider issues and caches OAuth2 client-credentials tokens for a
// single client ID. Safe for concurrent use.
type TokenProvider struct {
	baseURL      *url.URL
	clientID     string
	clientSecret string
	scope        string
	httpClient   *http.Client

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewTokenProvider builds a provider that fetches tokens from baseURL
// using client credentials. scope is passed through to the token
// endpoint verbatim (for example "file:read" or "file:write"). If
// httpClient is nil, http.DefaultClient is used; pass srv.Client() in
// tests to hit an httptest server.
func NewTokenProvider(baseURL *url.URL, clientID, clientSecret, scope string, httpClient *http.Client) *TokenProvider {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &TokenProvider{
		baseURL:      baseURL,
		clientID:     clientID,
		clientSecret: clientSecret,
		scope:        scope,
		httpClient:   httpClient,
	}
}

// ClientCredentialsToken returns a valid access token, fetching a new one
// only when the cached token is missing or within expiryBuffer of
// expiring.
func (p *TokenProvider) ClientCredentialsToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.token != "" && now().Before(p.expiresAt) {
		return p.token, nil
	}

	if err := p.fetch(ctx); err != nil {
		return "", err
	}
	return p.token, nil
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// fetch requests a new token. Callers must hold p.mu.
func (p *TokenProvider) fetch(ctx context.Context) error {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {p.clientID},
		"client_secret": {p.clientSecret},
		"scope":         {p.scope},
	}

	endpoint := p.baseURL.ResolveReference(&url.URL{Path: "/iam/v1/oauth2/token"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return &TokenError{StatusCode: resp.StatusCode, Body: string(body)}
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return fmt.Errorf("parse token response: %w", err)
	}
	if tr.AccessToken == "" {
		return fmt.Errorf("token response missing access_token")
	}
	if tr.ExpiresIn <= 0 {
		return fmt.Errorf("token response missing expires_in")
	}

	lifetime := time.Duration(tr.ExpiresIn) * time.Second
	if lifetime > expiryBuffer {
		lifetime -= expiryBuffer
	} else {
		lifetime = 0
	}
	p.token = tr.AccessToken
	p.expiresAt = now().Add(lifetime)
	return nil
}
