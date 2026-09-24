package identity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
)

const (
	testTenantID = "11111111-1111-1111-1111-111111111111"
	testAudience = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	testClientID = "33333333-3333-3333-3333-333333333333"
	testIssuer   = "https://login.microsoftonline.com/11111111-1111-1111-1111-111111111111/v2.0"
)

var testNow = time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)

type exactKeySet struct {
	kid string
	key *rsa.PublicKey
}

func (s exactKeySet) VerifySignature(_ context.Context, raw string) ([]byte, error) {
	jws, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return nil, err
	}
	if len(jws.Signatures) != 1 || jws.Signatures[0].Header.KeyID != s.kid {
		return nil, errors.New("unknown key id")
	}
	return jws.Verify(s.key)
}

func TestValidDelegatedTokenProducesTrustedPrincipal(t *testing.T) {
	validator, key := newTestValidator(t, false)
	claims := validClaims()
	claims["scp"] = "nl.read nl.preview"
	claims["roles"] = []string{"nl.app.admin"} // ignored for delegated tokens
	claims["name"] = "Mutable Display Name"
	token := signClaims(t, key, "kid-1", jose.RS256, claims)

	p, err := validator.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.TenantID != testTenantID || p.ObjectID != "22222222-2222-2222-2222-222222222222" || p.Subject != "subject-1" {
		t.Fatalf("immutable identity = %#v", p)
	}
	if p.Issuer != testIssuer || p.AuthorizedClientID != testClientID || p.TokenType != TokenTypeDelegated {
		t.Fatalf("token metadata = %#v", p)
	}
	if p.DisplayLabel != "Mutable Display Name" || p.AuditActor() != testTenantID+"/22222222-2222-2222-2222-222222222222" {
		t.Fatalf("display/actor = %#v actor=%q", p, p.AuditActor())
	}
	if !p.HasPermission(PermissionWorkivaRead) || !p.HasPermission(PermissionWorkivaWritePreview) || p.HasPermission(PermissionTenantAdmin) {
		t.Fatalf("delegated permissions = %#v", p.Permissions)
	}
}

func TestValidApplicationTokenRequiresRolesAndClientPolicy(t *testing.T) {
	validator, key := newTestValidator(t, false)
	claims := validClaims()
	delete(claims, "scp")
	claims["idtyp"] = "app"
	claims["roles"] = []string{"nl.app.reader", "nl.app.admin"}
	token := signClaims(t, key, "kid-1", jose.RS256, claims)

	p, err := validator.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.TokenType != TokenTypeApplication {
		t.Fatalf("TokenType = %q, want application", p.TokenType)
	}
	if !p.HasPermission(PermissionWorkivaRead) || p.HasPermission(PermissionTenantAdmin) {
		t.Fatalf("application permissions = %#v, want client-policy intersection", p.Permissions)
	}

	claims["azp"] = "44444444-4444-4444-4444-444444444444"
	if _, err := validator.Verify(context.Background(), signClaims(t, key, "kid-1", jose.RS256, claims)); err == nil {
		t.Fatal("application token from an unconfigured client was accepted")
	}
}

func TestApplicationTokenRequiresExactAppIdentityType(t *testing.T) {
	validator, key := newTestValidator(t, false)
	tests := []struct {
		name         string
		identityType any
		accepted     bool
	}{
		{name: "missing"},
		{name: "user", identityType: "user"},
		{name: "mixed case", identityType: "App"},
		{name: "upper case", identityType: "APP"},
		{name: "unknown", identityType: "service"},
		{name: "exact app", identityType: "app", accepted: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			claims := validClaims()
			delete(claims, "scp")
			if tc.identityType != nil {
				claims["idtyp"] = tc.identityType
			}
			claims["roles"] = []string{"nl.app.reader"}

			principal, err := validator.Verify(context.Background(), signClaims(t, key, "kid-1", jose.RS256, claims))
			if tc.accepted {
				if err != nil {
					t.Fatalf("Verify: %v", err)
				}
				if principal.TokenType != TokenTypeApplication || !principal.HasPermission(PermissionWorkivaRead) {
					t.Fatalf("principal = %#v, want authorized application", principal)
				}
				return
			}

			if got, want := errorString(err), "identity: application token identity type must be exactly app"; got != want {
				t.Fatalf("Verify error = %q, want %q", got, want)
			}
			if principal.TokenType == TokenTypeApplication || principal.HasPermission(PermissionWorkivaRead) || len(principal.Permissions) != 0 {
				t.Fatalf("rejected token received application authorization: %#v", principal)
			}
		})
	}
}

func TestApplicationIdentityTypeCannotContainDelegatedScopes(t *testing.T) {
	validator, key := newTestValidator(t, false)
	claims := validClaims()
	claims["idtyp"] = "app"
	claims["roles"] = []string{"nl.app.reader"}

	principal, err := validator.Verify(context.Background(), signClaims(t, key, "kid-1", jose.RS256, claims))
	if got, want := errorString(err), "identity: application token cannot contain delegated scopes"; got != want {
		t.Fatalf("Verify error = %q, want %q", got, want)
	}
	if principal.TokenType != "" || len(principal.Permissions) != 0 {
		t.Fatalf("rejected token received authorization: %#v", principal)
	}
}

func TestEntraTokenValidationRejectsUntrustedTokens(t *testing.T) {
	validator, key := newTestValidator(t, false)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		token func(t *testing.T) string
	}{
		{name: "bad signature", token: func(t *testing.T) string { return signClaims(t, otherKey, "kid-1", jose.RS256, validClaims()) }},
		{name: "alg none", token: func(t *testing.T) string { return unsignedToken(t, validClaims()) }},
		{name: "symmetric algorithm", token: func(t *testing.T) string {
			return signClaims(t, []byte(strings.Repeat("x", 32)), "kid-1", jose.HS256, validClaims())
		}},
		{name: "wrong issuer", token: func(t *testing.T) string {
			claims := validClaims()
			claims["iss"] = "https://issuer.invalid/v2.0"
			return signClaims(t, key, "kid-1", jose.RS256, claims)
		}},
		{name: "wrong audience", token: func(t *testing.T) string {
			claims := validClaims()
			claims["aud"] = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
			return signClaims(t, key, "kid-1", jose.RS256, claims)
		}},
		{name: "wrong tenant", token: func(t *testing.T) string {
			claims := validClaims()
			claims["tid"] = "55555555-5555-5555-5555-555555555555"
			return signClaims(t, key, "kid-1", jose.RS256, claims)
		}},
		{name: "unknown kid", token: func(t *testing.T) string { return signClaims(t, key, "kid-2", jose.RS256, validClaims()) }},
		{name: "expired", token: func(t *testing.T) string {
			claims := validClaims()
			claims["exp"] = testNow.Add(-time.Second).Unix()
			return signClaims(t, key, "kid-1", jose.RS256, claims)
		}},
		{name: "missing tid", token: func(t *testing.T) string {
			claims := validClaims()
			delete(claims, "tid")
			return signClaims(t, key, "kid-1", jose.RS256, claims)
		}},
		{name: "missing oid", token: func(t *testing.T) string {
			claims := validClaims()
			delete(claims, "oid")
			return signClaims(t, key, "kid-1", jose.RS256, claims)
		}},
		{name: "missing subject", token: func(t *testing.T) string {
			claims := validClaims()
			delete(claims, "sub")
			return signClaims(t, key, "kid-1", jose.RS256, claims)
		}},
		{name: "personal account tenant", token: func(t *testing.T) string {
			claims := validClaims()
			claims["tid"] = PersonalMicrosoftAccountTenantID
			return signClaims(t, key, "kid-1", jose.RS256, claims)
		}},
		{name: "malformed", token: func(*testing.T) string { return "not-a-jwt" }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validator.Verify(context.Background(), tc.token(t)); err == nil {
				t.Fatal("Verify succeeded; want rejection")
			}
		})
	}
}

func TestNotBeforeClaimUsesOIDCClockSkewTolerance(t *testing.T) {
	validator, key := newTestValidator(t, false)

	t.Run("missing claim is rejected", func(t *testing.T) {
		claims := validClaims()
		delete(claims, "nbf")
		_, err := validator.Verify(context.Background(), signClaims(t, key, "kid-1", jose.RS256, claims))
		if got, want := errorString(err), "identity: access token not-before claim is required"; got != want {
			t.Fatalf("Verify error = %q, want %q", got, want)
		}
	})

	t.Run("slightly future claim within five minutes is accepted", func(t *testing.T) {
		claims := validClaims()
		claims["nbf"] = testNow.Add(5*time.Minute - time.Second).Unix()
		if _, err := validator.Verify(context.Background(), signClaims(t, key, "kid-1", jose.RS256, claims)); err != nil {
			t.Fatalf("Verify at injected clock %v: %v", testNow, err)
		}
	})

	t.Run("first second beyond five minutes is rejected", func(t *testing.T) {
		claims := validClaims()
		claims["nbf"] = testNow.Add(5*time.Minute + time.Second).Unix()
		_, err := validator.Verify(context.Background(), signClaims(t, key, "kid-1", jose.RS256, claims))
		if got, want := errorString(err), "identity: access token validation failed"; got != want {
			t.Fatalf("Verify error = %q, want %q", got, want)
		}
	})
}

func TestAuthorizedClientIDsAreCanonicalUUIDs(t *testing.T) {
	validator, key := newTestValidator(t, false)

	t.Run("equivalent azp and appid forms match policy", func(t *testing.T) {
		claims := validClaims()
		delete(claims, "scp")
		claims["idtyp"] = "app"
		claims["roles"] = []string{"nl.app.reader"}
		claims["azp"] = "{33333333-3333-3333-3333-333333333333}"
		claims["appid"] = "33333333-3333-3333-3333-333333333333"

		principal, err := validator.Verify(context.Background(), signClaims(t, key, "kid-1", jose.RS256, claims))
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if got := principal.AuthorizedClientID; got != testClientID {
			t.Fatalf("AuthorizedClientID = %q, want %q", got, testClientID)
		}
		if !principal.HasPermission(PermissionWorkivaRead) {
			t.Fatalf("permissions = %#v, want workiva.read", principal.Permissions)
		}
	})

	for _, claimName := range []string{"azp", "appid"} {
		t.Run("malformed "+claimName+" is rejected", func(t *testing.T) {
			claims := validClaims()
			delete(claims, "azp")
			claims[claimName] = "not-a-client-uuid"
			_, err := validator.Verify(context.Background(), signClaims(t, key, "kid-1", jose.RS256, claims))
			if got, want := errorString(err), "identity: access token authorized client ID is malformed"; got != want {
				t.Fatalf("Verify error = %q, want %q", got, want)
			}
		})
	}

	t.Run("different canonical azp and appid conflict", func(t *testing.T) {
		claims := validClaims()
		claims["azp"] = "{33333333-3333-3333-3333-333333333333}"
		claims["appid"] = "44444444-4444-4444-4444-444444444444"
		_, err := validator.Verify(context.Background(), signClaims(t, key, "kid-1", jose.RS256, claims))
		if got, want := errorString(err), "identity: conflicting authorized client IDs"; got != want {
			t.Fatalf("Verify error = %q, want %q", got, want)
		}
	})
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestSubjectFallbackMustBeExplicitlyEnabled(t *testing.T) {
	validator, key := newTestValidator(t, true)
	claims := validClaims()
	delete(claims, "oid")
	p, err := validator.Verify(context.Background(), signClaims(t, key, "kid-1", jose.RS256, claims))
	if err != nil {
		t.Fatalf("Verify with explicit fallback: %v", err)
	}
	if !p.UsedSubjectFallback || p.AuditActor() != testTenantID+"/sub:subject-1" {
		t.Fatalf("fallback principal = %#v actor=%q", p, p.AuditActor())
	}
}

func TestDiscoveredVerifierCachesAndRefreshesJWKS(t *testing.T) {
	key1, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	key2, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	currentKey := &key1.PublicKey
	currentKid := "kid-1"
	var jwksRequests atomic.Int32
	const authority = "https://issuer.example"
	client := &http.Client{Timeout: time.Second, Transport: identityRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body []byte
		var marshalErr error
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			body, marshalErr = json.Marshal(map[string]any{
				"issuer":                                authority,
				"jwks_uri":                              authority + "/keys",
				"id_token_signing_alg_values_supported": []string{"RS256"},
			})
		case "/keys":
			jwksRequests.Add(1)
			body, marshalErr = json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
				Key: currentKey, KeyID: currentKid, Algorithm: "RS256", Use: "sig",
			}}})
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header), Request: r}, nil
		}
		if marshalErr != nil {
			return nil, marshalErr
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{"Content-Type": []string{"application/json"}}, Request: r}, nil
	})}

	config := EntraVerifierConfig{
		Authority: authority,
		Audience:  testAudience,
		TenantID:  testTenantID,
		Policy: PermissionPolicy{DelegatedScopes: map[string][]Permission{
			"nl.read": {PermissionWorkivaRead},
		}},
	}
	validator, err := NewDiscoveredEntraVerifier(context.Background(), config, client)
	if err != nil {
		t.Fatalf("NewDiscoveredEntraVerifier: %v", err)
	}
	claims := validClaims()
	claims["iss"] = authority
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	claims["nbf"] = time.Now().Add(-time.Minute).Unix()

	if _, err := validator.Verify(context.Background(), signClaims(t, key1, "kid-1", jose.RS256, claims)); err != nil {
		t.Fatalf("verify first key: %v", err)
	}
	if _, err := validator.Verify(context.Background(), signClaims(t, key1, "kid-1", jose.RS256, claims)); err != nil {
		t.Fatalf("verify cached key: %v", err)
	}
	if got := jwksRequests.Load(); got != 1 {
		t.Fatalf("JWKS requests after cached verify = %d, want 1", got)
	}

	currentKey = &key2.PublicKey
	currentKid = "kid-2"
	if _, err := validator.Verify(context.Background(), signClaims(t, key2, "kid-2", jose.RS256, claims)); err != nil {
		t.Fatalf("verify rotated key: %v", err)
	}
	if got := jwksRequests.Load(); got != 2 {
		t.Fatalf("JWKS requests after rotation = %d, want 2", got)
	}
}

type identityRoundTripFunc func(*http.Request) (*http.Response, error)

func (f identityRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func newTestValidator(t *testing.T, allowSubjectFallback bool) (*EntraVerifier, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := NewEntraVerifier(EntraVerifierConfig{
		Authority:            testIssuer,
		Audience:             testAudience,
		TenantID:             testTenantID,
		AllowSubjectFallback: allowSubjectFallback,
		Policy: PermissionPolicy{
			DelegatedScopes: map[string][]Permission{
				"nl.read":    {PermissionWorkivaRead},
				"nl.preview": {PermissionWorkivaWritePreview},
			},
			ApplicationRoles: map[string][]Permission{
				"nl.app.reader": {PermissionWorkivaRead},
				"nl.app.admin":  {PermissionTenantAdmin},
			},
			ApplicationClients: map[string][]Permission{
				testClientID: {PermissionWorkivaRead},
			},
		},
	}, exactKeySet{kid: "kid-1", key: &key.PublicKey}, func() time.Time { return testNow })
	if err != nil {
		t.Fatalf("NewEntraVerifier: %v", err)
	}
	return validator, key
}

func validClaims() map[string]any {
	return map[string]any{
		"iss": testIssuer,
		"aud": testAudience,
		"exp": testNow.Add(time.Hour).Unix(),
		"nbf": testNow.Add(-time.Minute).Unix(),
		"iat": testNow.Add(-time.Minute).Unix(),
		"tid": testTenantID,
		"oid": "22222222-2222-2222-2222-222222222222",
		"sub": "subject-1",
		"azp": testClientID,
		"scp": "nl.read",
	}
}

func signClaims(t *testing.T, key any, kid string, algorithm jose.SignatureAlgorithm, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: algorithm, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", kid).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	token, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func unsignedToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "none", "kid": "kid-1", "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

var _ oidc.KeySet = exactKeySet{}
