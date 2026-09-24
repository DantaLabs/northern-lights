package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
)

// PersonalMicrosoftAccountTenantID is the well-known consumer Microsoft
// account tenant, which Northern Lights never accepts for production access.
const PersonalMicrosoftAccountTenantID = "9188040d-6c67-4c5b-b112-36a304b66dad"

// DiscoveryHTTPTimeout bounds OIDC discovery and JWKS refresh requests.
const DiscoveryHTTPTimeout = 10 * time.Second

// TokenVerifier authenticates a bearer access token and returns its trusted
// principal. Implementations must never return token contents in errors.
type TokenVerifier interface {
	Verify(context.Context, string) (Principal, error)
}

// EntraVerifierConfig contains the identity and authorization policy enforced
// for one Microsoft Entra tenant and API audience.
type EntraVerifierConfig struct {
	Authority            string
	Audience             string
	TenantID             string
	AllowSubjectFallback bool
	Policy               PermissionPolicy
}

type oidcVerifier interface {
	Verify(context.Context, string) (*oidc.IDToken, error)
}

// EntraVerifier validates signed Microsoft Entra access tokens and derives a
// trusted principal from immutable claims.
type EntraVerifier struct {
	config   EntraVerifierConfig
	verifier oidcVerifier
}

// NewEntraVerifier constructs a verifier with an injected OIDC key source and
// clock. Tests use a static source; production uses discovery and RemoteKeySet.
func NewEntraVerifier(config EntraVerifierConfig, keySet oidc.KeySet, now func() time.Time) (*EntraVerifier, error) {
	if keySet == nil {
		return nil, errors.New("identity: OIDC key set is required")
	}
	if err := validateVerifierConfig(config); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	verifier := oidc.NewVerifier(config.Authority, keySet, &oidc.Config{
		ClientID:             config.Audience,
		SupportedSigningAlgs: []string{oidc.RS256},
		Now:                  now,
	})
	return newEntraVerifier(config, verifier), nil
}

// NewDiscoveredEntraVerifier performs bounded OIDC discovery and returns a
// long-lived verifier whose RemoteKeySet caches keys and refreshes on an
// unfamiliar key ID.
func NewDiscoveredEntraVerifier(ctx context.Context, config EntraVerifierConfig, client *http.Client) (*EntraVerifier, error) {
	if err := validateVerifierConfig(config); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: DiscoveryHTTPTimeout}
	} else if client.Timeout <= 0 {
		clone := *client
		clone.Timeout = DiscoveryHTTPTimeout
		client = &clone
	}
	oidcCtx := oidc.ClientContext(ctx, client)
	provider, err := oidc.NewProvider(oidcCtx, config.Authority)
	if err != nil {
		return nil, fmt.Errorf("identity: Entra OIDC discovery failed: %w", err)
	}
	verifier := provider.VerifierContext(oidcCtx, &oidc.Config{
		ClientID:             config.Audience,
		SupportedSigningAlgs: []string{oidc.RS256},
		Now:                  time.Now,
	})
	return newEntraVerifier(config, verifier), nil
}

func newEntraVerifier(config EntraVerifierConfig, verifier oidcVerifier) *EntraVerifier {
	return &EntraVerifier{config: config, verifier: verifier}
}

func validateVerifierConfig(config EntraVerifierConfig) error {
	if strings.TrimSpace(config.Authority) == "" || strings.TrimSpace(config.Audience) == "" || strings.TrimSpace(config.TenantID) == "" {
		return errors.New("identity: authority, audience, and tenant ID are required")
	}
	if config.TenantID == PersonalMicrosoftAccountTenantID {
		return errors.New("identity: personal Microsoft account tenant is not allowed")
	}
	return nil
}

type entraClaims struct {
	TenantID           string   `json:"tid"`
	ObjectID           string   `json:"oid"`
	Subject            string   `json:"sub"`
	AuthorizedClientID string   `json:"azp"`
	LegacyClientID     string   `json:"appid"`
	Scopes             string   `json:"scp"`
	Roles              []string `json:"roles"`
	IdentityType       string   `json:"idtyp"`
	Name               string   `json:"name"`
	PreferredUsername  string   `json:"preferred_username"`
	NotBefore          *int64   `json:"nbf"`
}

// Verify validates the token before exposing any claims to authorization.
func (v *EntraVerifier) Verify(ctx context.Context, raw string) (Principal, error) {
	if v == nil || v.verifier == nil {
		return Principal{}, errors.New("identity: token verifier is unavailable")
	}
	if strings.TrimSpace(raw) == "" {
		return Principal{}, errors.New("identity: bearer token is empty")
	}
	token, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		return Principal{}, errors.New("identity: access token validation failed")
	}
	var claims entraClaims
	if err := token.Claims(&claims); err != nil {
		return Principal{}, errors.New("identity: access token claims are malformed")
	}
	if claims.NotBefore == nil {
		return Principal{}, errors.New("identity: access token not-before claim is required")
	}
	if claims.TenantID == PersonalMicrosoftAccountTenantID {
		return Principal{}, errors.New("identity: personal Microsoft account tenant is not allowed")
	}
	if claims.TenantID == "" || claims.TenantID != v.config.TenantID {
		return Principal{}, errors.New("identity: access token tenant is not allowed")
	}
	if _, err := uuid.Parse(claims.TenantID); err != nil {
		return Principal{}, errors.New("identity: access token tenant ID is malformed")
	}
	if strings.TrimSpace(claims.Subject) == "" {
		return Principal{}, errors.New("identity: access token subject is required")
	}

	usedSubjectFallback := false
	if claims.ObjectID == "" {
		if !v.config.AllowSubjectFallback {
			return Principal{}, errors.New("identity: access token object ID is required")
		}
		usedSubjectFallback = true
	} else if _, err := uuid.Parse(claims.ObjectID); err != nil {
		return Principal{}, errors.New("identity: access token object ID is malformed")
	}

	clientID, err := authorizedClientID(claims.AuthorizedClientID, claims.LegacyClientID)
	if err != nil {
		return Principal{}, err
	}
	principal := Principal{
		TenantID:            claims.TenantID,
		ObjectID:            claims.ObjectID,
		Subject:             claims.Subject,
		Issuer:              token.Issuer,
		AuthorizedClientID:  clientID,
		Roles:               append([]string(nil), claims.Roles...),
		DisplayLabel:        displayLabel(claims.Name, claims.PreferredUsername),
		UsedSubjectFallback: usedSubjectFallback,
	}

	if claims.Scopes != "" {
		if claims.IdentityType == "app" {
			return Principal{}, errors.New("identity: application token cannot contain delegated scopes")
		}
		principal.TokenType = TokenTypeDelegated
		principal.Scopes = strings.Fields(claims.Scopes)
	} else {
		if claims.IdentityType != "app" {
			return Principal{}, errors.New("identity: application token identity type must be exactly app")
		}
		if len(claims.Roles) == 0 {
			return Principal{}, errors.New("identity: token contains neither delegated scopes nor application roles")
		}
		if principal.AuthorizedClientID == "" {
			return Principal{}, errors.New("identity: application token client ID is required")
		}
		principal.TokenType = TokenTypeApplication
	}

	permissions, err := v.config.Policy.PermissionsFor(principal)
	if err != nil {
		return Principal{}, fmt.Errorf("identity: application authorization failed: %w", err)
	}
	if principal.TokenType == TokenTypeApplication && len(permissions) == 0 {
		return Principal{}, errors.New("identity: application roles and client policy grant no permissions")
	}
	principal.Permissions = permissions
	return principal, nil
}

func authorizedClientID(azp, appID string) (string, error) {
	canonicalAZP, err := canonicalClientID(azp)
	if err != nil {
		return "", err
	}
	canonicalAppID, err := canonicalClientID(appID)
	if err != nil {
		return "", err
	}
	if canonicalAZP != "" && canonicalAppID != "" && canonicalAZP != canonicalAppID {
		return "", errors.New("identity: conflicting authorized client IDs")
	}
	if canonicalAZP != "" {
		return canonicalAZP, nil
	}
	return canonicalAppID, nil
}

func canonicalClientID(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	clientID, err := uuid.Parse(raw)
	if err != nil {
		return "", errors.New("identity: access token authorized client ID is malformed")
	}
	return clientID.String(), nil
}

func displayLabel(name, preferredUsername string) string {
	label := strings.TrimSpace(name)
	if label == "" {
		label = strings.TrimSpace(preferredUsername)
	}
	if len(label) > 256 {
		return ""
	}
	for _, r := range label {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return label
}
