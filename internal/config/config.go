// Package config loads northern-lights configuration from a YAML file
// with NL_ prefixed environment variable overrides.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/dantalabs/northern-lights/internal/identity"
)

// AuthMode selects the authentication boundary protecting /mcp.
type AuthMode string

const (
	AuthModeAPIKey AuthMode = "api_key"
	AuthModeEntra  AuthMode = "entra"
)

// baseURLs maps a Workiva region to its API base URL.
var baseURLs = map[string]string{
	"eu":   "https://api.eu.wdesk.com",
	"us":   "https://api.app.wdesk.com",
	"apac": "https://api.apac.wdesk.com",
}

// Config holds all runtime configuration for the server.
type Config struct {
	AuthMode                 AuthMode
	Region                   string
	WorkivaClientID          string
	WorkivaClientSecret      string
	DBPath                   string
	ListenAddr               string
	ReadCacheTTL             time.Duration
	RequireWriteConfirmation bool
	// DisableLocalhostProtection permits non-localhost Host headers at the
	// MCP transport. Keep false unless a trusted HTTPS ingress requires it.
	DisableLocalhostProtection bool
	// AllowedResources is nil when access is unrestricted. When configured,
	// keys are spreadsheet IDs and values are allowed sheet IDs; "*" allows
	// every sheet in that spreadsheet.
	AllowedResources map[string][]string
	// DemoMode swaps the Workiva client for a synthetic fixture client so
	// the server can be tried without real Workiva credentials.
	DemoMode bool

	// Entra identity settings are environment-only. They are kept out of YAML
	// so deployment identity cannot be silently inherited from a repository.
	EntraTenantID             string
	EntraAuthority            string
	EntraAudience             string
	EntraAllowSubjectFallback bool
	EntraScopePermissions     map[string][]identity.Permission
	EntraRolePermissions      map[string][]identity.Permission
	EntraAppClientPermissions map[string][]identity.Permission
}

// rawConfig mirrors the YAML file. Pointer fields distinguish an absent
// key from an explicitly zero value.
type rawConfig struct {
	AuthMode                   *string             `yaml:"auth_mode"`
	Region                     *string             `yaml:"region"`
	DBPath                     *string             `yaml:"db_path"`
	ListenAddr                 *string             `yaml:"listen_addr"`
	ReadCacheTTL               *string             `yaml:"read_cache_ttl"`
	RequireWriteConfirmation   *bool               `yaml:"require_write_confirmation"`
	DisableLocalhostProtection *bool               `yaml:"disable_localhost_protection"`
	AllowedResources           map[string][]string `yaml:"allowed_resources"`
	WorkivaClientID            *string             `yaml:"workiva_client_id"`
	WorkivaClientSecret        *string             `yaml:"workiva_client_secret"`
}

// Load reads the YAML file at path (a missing file is fine, defaults
// apply), then applies NL_ environment overrides, then validates the region
// and authentication configuration. Workiva credentials and Entra identity
// policy come only from environment variables, never from YAML.
func Load(path string) (*Config, error) {
	cfg := &Config{
		AuthMode:                 AuthModeAPIKey,
		Region:                   "eu",
		DBPath:                   "./northern-lights.db",
		ListenAddr:               ":8080",
		ReadCacheTTL:             30 * time.Second,
		RequireWriteConfirmation: true,
	}

	if path != "" {
		if err := loadYAML(path, cfg); err != nil {
			return nil, err
		}
	}

	if err := applyEnv(cfg); err != nil {
		return nil, err
	}

	if _, ok := baseURLs[cfg.Region]; !ok {
		return nil, fmt.Errorf("invalid region %q: must be one of eu, us, apac", cfg.Region)
	}
	if err := validateIdentityConfig(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func loadYAML(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read config file: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	var document yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&document); err != nil {
		return fmt.Errorf("parse config file %s: %w", path, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("parse config file %s: additional YAML documents are not allowed", path)
		}
		return fmt.Errorf("parse config file %s: %w", path, err)
	}
	if err := rejectYAMLReferences(&document); err != nil {
		return fmt.Errorf("parse config file %s: %w", path, err)
	}
	var raw rawConfig
	strict := yaml.NewDecoder(bytes.NewReader(data))
	strict.KnownFields(true)
	if err := strict.Decode(&raw); err != nil {
		return fmt.Errorf("parse config file %s: %w", path, err)
	}
	if raw.Region != nil {
		cfg.Region = *raw.Region
	}
	if raw.AuthMode != nil {
		cfg.AuthMode = AuthMode(strings.TrimSpace(*raw.AuthMode))
	}
	if raw.DBPath != nil {
		cfg.DBPath = *raw.DBPath
	}
	if raw.ListenAddr != nil {
		cfg.ListenAddr = *raw.ListenAddr
	}
	if raw.ReadCacheTTL != nil {
		d, err := time.ParseDuration(*raw.ReadCacheTTL)
		if err != nil {
			return fmt.Errorf("invalid read_cache_ttl %q: %w", *raw.ReadCacheTTL, err)
		}
		cfg.ReadCacheTTL = d
	}
	if raw.RequireWriteConfirmation != nil {
		cfg.RequireWriteConfirmation = *raw.RequireWriteConfirmation
	}
	if raw.DisableLocalhostProtection != nil {
		cfg.DisableLocalhostProtection = *raw.DisableLocalhostProtection
	}
	if len(document.Content) > 0 {
		root := document.Content[0]
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == "allowed_resources" {
				if err := validatePolicyNode(root.Content[i+1]); err != nil {
					return err
				}
			}
		}
	}
	if raw.AllowedResources != nil {
		if err := validateAllowedResources(raw.AllowedResources); err != nil {
			return err
		}
		cfg.AllowedResources = raw.AllowedResources
	}
	return nil
}

func rejectYAMLReferences(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode || node.Anchor != "" {
		return errors.New("YAML aliases and anchors are not allowed")
	}
	if node.Value == "<<" && node.Tag == "!!merge" {
		return errors.New("YAML merge keys are not allowed")
	}
	for _, child := range node.Content {
		if err := rejectYAMLReferences(child); err != nil {
			return err
		}
	}
	return nil
}

func applyEnv(cfg *Config) error {
	if v := os.Getenv("NL_AUTH_MODE"); v != "" {
		cfg.AuthMode = AuthMode(strings.TrimSpace(v))
	}
	if v := os.Getenv("NL_REGION"); v != "" {
		cfg.Region = v
	}
	if v := os.Getenv("NL_DB_PATH"); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv("NL_LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("NL_READ_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid NL_READ_CACHE_TTL %q: %w", v, err)
		}
		cfg.ReadCacheTTL = d
	}
	if v := os.Getenv("NL_REQUIRE_WRITE_CONFIRMATION"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid NL_REQUIRE_WRITE_CONFIRMATION %q: %w", v, err)
		}
		cfg.RequireWriteConfirmation = enabled
	}
	if v := os.Getenv("NL_DISABLE_LOCALHOST_PROTECTION"); v != "" {
		disabled, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid NL_DISABLE_LOCALHOST_PROTECTION %q: %w", v, err)
		}
		cfg.DisableLocalhostProtection = disabled
	}
	if v, configured := os.LookupEnv("NL_ALLOWED_RESOURCES"); configured {
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(v), &node); err != nil {
			return fmt.Errorf("invalid NL_ALLOWED_RESOURCES: %w", err)
		}
		if len(node.Content) == 0 {
			return errors.New("NL_ALLOWED_RESOURCES must not be empty")
		}
		if err := validatePolicyNode(node.Content[0]); err != nil {
			return err
		}
		var policy map[string][]string
		if err := json.Unmarshal([]byte(v), &policy); err != nil {
			return fmt.Errorf("invalid NL_ALLOWED_RESOURCES JSON: %w", err)
		}
		if err := validateAllowedResources(policy); err != nil {
			return fmt.Errorf("invalid NL_ALLOWED_RESOURCES: %w", err)
		}
		cfg.AllowedResources = policy
	}
	cfg.WorkivaClientID = os.Getenv("NL_WORKIVA_CLIENT_ID")
	cfg.WorkivaClientSecret = os.Getenv("NL_WORKIVA_CLIENT_SECRET")
	cfg.DemoMode = os.Getenv("NL_DEMO_MODE") == "true"
	cfg.EntraTenantID = strings.TrimSpace(os.Getenv("NL_ENTRA_TENANT_ID"))
	cfg.EntraAuthority = strings.TrimSpace(os.Getenv("NL_ENTRA_AUTHORITY"))
	cfg.EntraAudience = strings.TrimSpace(os.Getenv("NL_ENTRA_AUDIENCE"))
	if v := os.Getenv("NL_ENTRA_ALLOW_SUBJECT_FALLBACK"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid NL_ENTRA_ALLOW_SUBJECT_FALLBACK %q: %w", v, err)
		}
		cfg.EntraAllowSubjectFallback = enabled
	}
	var err error
	if cfg.EntraScopePermissions, err = parsePermissionMapping("NL_ENTRA_SCOPE_PERMISSIONS", os.Getenv("NL_ENTRA_SCOPE_PERMISSIONS")); err != nil {
		return err
	}
	if cfg.EntraRolePermissions, err = parsePermissionMapping("NL_ENTRA_ROLE_PERMISSIONS", os.Getenv("NL_ENTRA_ROLE_PERMISSIONS")); err != nil {
		return err
	}
	if cfg.EntraAppClientPermissions, err = parsePermissionMapping("NL_ENTRA_APP_CLIENT_PERMISSIONS", os.Getenv("NL_ENTRA_APP_CLIENT_PERMISSIONS")); err != nil {
		return err
	}
	if cfg.EntraAppClientPermissions, err = canonicalizeApplicationClientPermissions(cfg.EntraAppClientPermissions); err != nil {
		return err
	}
	return nil
}

func canonicalizeApplicationClientPermissions(raw map[string][]identity.Permission) (map[string][]identity.Permission, error) {
	if raw == nil {
		return nil, nil
	}
	canonical := make(map[string][]identity.Permission, len(raw))
	for key, permissions := range raw {
		clientID, err := uuid.Parse(key)
		if err != nil {
			return nil, fmt.Errorf("invalid application client ID %q: %w", key, err)
		}
		canonicalKey := clientID.String()
		if _, exists := canonical[canonicalKey]; exists {
			return nil, fmt.Errorf("duplicate application client ID %q after UUID canonicalization", key)
		}
		canonical[canonicalKey] = permissions
	}
	return canonical, nil
}

func parsePermissionMapping(name, value string) (map[string][]identity.Permission, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(value), &node); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", name, err)
	}
	if len(node.Content) == 0 {
		return nil, fmt.Errorf("invalid %s: mapping must not be empty", name)
	}
	if err := validateStringListMapNode(node.Content[0], name); err != nil {
		return nil, err
	}
	var raw map[string][]string
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return nil, fmt.Errorf("invalid %s JSON: %w", name, err)
	}
	permissions := make(map[string][]identity.Permission, len(raw))
	for claim, values := range raw {
		seen := make(map[identity.Permission]bool, len(values))
		for _, value := range values {
			permission := identity.Permission(value)
			if !isKnownPermission(permission) {
				return nil, fmt.Errorf("invalid %s: unknown permission %q", name, value)
			}
			if seen[permission] {
				return nil, fmt.Errorf("invalid %s: duplicate permission %q for %q", name, permission, claim)
			}
			seen[permission] = true
			permissions[claim] = append(permissions[claim], permission)
		}
	}
	return permissions, nil
}

func validateStringListMapNode(node *yaml.Node, name string) error {
	if node.Kind != yaml.MappingNode || len(node.Content) == 0 {
		return fmt.Errorf("invalid %s: must be a nonempty JSON object", name)
	}
	seen := make(map[string]bool)
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Tag != "!!str" || strings.TrimSpace(key.Value) == "" || seen[key.Value] {
			return fmt.Errorf("invalid %s: requires unique nonempty string keys", name)
		}
		seen[key.Value] = true
		if value.Kind != yaml.SequenceNode || len(value.Content) == 0 {
			return fmt.Errorf("invalid %s: %q must contain permissions", name, key.Value)
		}
		for _, permission := range value.Content {
			if permission.Tag != "!!str" {
				return fmt.Errorf("invalid %s: permissions must be strings", name)
			}
		}
	}
	return nil
}

func isKnownPermission(permission identity.Permission) bool {
	for _, known := range identity.AllPermissions() {
		if permission == known {
			return true
		}
	}
	return false
}

func validateIdentityConfig(cfg *Config) error {
	switch cfg.AuthMode {
	case AuthModeAPIKey:
		for _, name := range []string{
			"NL_ENTRA_TENANT_ID", "NL_ENTRA_AUTHORITY", "NL_ENTRA_AUDIENCE", "NL_ENTRA_ALLOW_SUBJECT_FALLBACK",
			"NL_ENTRA_SCOPE_PERMISSIONS", "NL_ENTRA_ROLE_PERMISSIONS", "NL_ENTRA_APP_CLIENT_PERMISSIONS",
		} {
			if _, configured := os.LookupEnv(name); configured {
				return fmt.Errorf("%s requires NL_AUTH_MODE=entra", name)
			}
		}
		return nil
	case AuthModeEntra:
		return validateEntraConfig(cfg)
	default:
		return fmt.Errorf("invalid auth mode %q: must be api_key or entra", cfg.AuthMode)
	}
}

func validateEntraConfig(cfg *Config) error {
	if cfg.EntraTenantID == "" || cfg.EntraAuthority == "" || cfg.EntraAudience == "" {
		return errors.New("entra auth requires NL_ENTRA_TENANT_ID, NL_ENTRA_AUTHORITY, and NL_ENTRA_AUDIENCE")
	}
	tenantID, err := uuid.Parse(cfg.EntraTenantID)
	if err != nil {
		return fmt.Errorf("invalid NL_ENTRA_TENANT_ID: %w", err)
	}
	cfg.EntraTenantID = tenantID.String()
	if cfg.EntraTenantID == identity.PersonalMicrosoftAccountTenantID {
		return errors.New("entra auth does not allow the personal Microsoft account tenant")
	}
	audience, err := uuid.Parse(cfg.EntraAudience)
	if err != nil {
		return fmt.Errorf("invalid NL_ENTRA_AUDIENCE: %w", err)
	}
	cfg.EntraAudience = audience.String()
	authority, err := url.Parse(cfg.EntraAuthority)
	if err != nil || authority.Scheme != "https" || authority.User != nil || authority.RawQuery != "" || authority.Fragment != "" {
		return errors.New("NL_ENTRA_AUTHORITY must be an HTTPS Microsoft authority URL without user info, query, or fragment")
	}
	allowedHosts := map[string]bool{
		"login.microsoftonline.com":        true,
		"login.microsoftonline.us":         true,
		"login.chinacloudapi.cn":           true,
		"login.partner.microsoftonline.cn": true,
	}
	if !allowedHosts[strings.ToLower(authority.Hostname())] || authority.Port() != "" {
		return errors.New("NL_ENTRA_AUTHORITY host is not a supported Microsoft identity authority")
	}
	if authority.Path != "/"+cfg.EntraTenantID+"/v2.0" {
		return errors.New("NL_ENTRA_AUTHORITY must name the configured tenant's v2.0 issuer exactly")
	}
	if len(cfg.EntraScopePermissions) == 0 && len(cfg.EntraRolePermissions) == 0 {
		return errors.New("entra auth requires delegated scope or application role permission mappings")
	}
	if (len(cfg.EntraRolePermissions) == 0) != (len(cfg.EntraAppClientPermissions) == 0) {
		return errors.New("application role mappings and application client policies must be configured together")
	}
	rolePermissions := make(map[identity.Permission]bool)
	for _, permissions := range cfg.EntraRolePermissions {
		for _, permission := range permissions {
			rolePermissions[permission] = true
		}
	}
	for clientID, permissions := range cfg.EntraAppClientPermissions {
		if _, err := uuid.Parse(clientID); err != nil {
			return fmt.Errorf("invalid application client ID %q: %w", clientID, err)
		}
		overlaps := false
		for _, permission := range permissions {
			if rolePermissions[permission] {
				overlaps = true
				break
			}
		}
		if !overlaps {
			return fmt.Errorf("application client %q has no permission allowed by any configured role", clientID)
		}
	}
	return nil
}

// EntraVerifierConfig returns an isolated identity verifier configuration.
func (c *Config) EntraVerifierConfig() identity.EntraVerifierConfig {
	return identity.EntraVerifierConfig{
		Authority:            c.EntraAuthority,
		Audience:             c.EntraAudience,
		TenantID:             c.EntraTenantID,
		AllowSubjectFallback: c.EntraAllowSubjectFallback,
		Policy: identity.PermissionPolicy{
			DelegatedScopes:    clonePermissionMap(c.EntraScopePermissions),
			ApplicationRoles:   clonePermissionMap(c.EntraRolePermissions),
			ApplicationClients: clonePermissionMap(c.EntraAppClientPermissions),
		},
	}
}

func clonePermissionMap(in map[string][]identity.Permission) map[string][]identity.Permission {
	if in == nil {
		return nil
	}
	out := make(map[string][]identity.Permission, len(in))
	for key, permissions := range in {
		out[key] = append([]identity.Permission(nil), permissions...)
	}
	return out
}

func validateAllowedResources(policy map[string][]string) error {
	if len(policy) == 0 {
		return errors.New("allowed_resources must contain at least one spreadsheet")
	}
	for spreadsheetID, sheets := range policy {
		if strings.TrimSpace(spreadsheetID) == "" {
			return errors.New("spreadsheet ID must not be empty")
		}
		if len(sheets) == 0 {
			return fmt.Errorf("spreadsheet %q must allow at least one sheet ID or *", spreadsheetID)
		}
		for _, sheetID := range sheets {
			if strings.TrimSpace(sheetID) == "" {
				return fmt.Errorf("spreadsheet %q contains an empty sheet ID", spreadsheetID)
			}
		}
	}
	return nil
}

// SpreadsheetAllowed and ResourceAllowed centralize resource governance.
func (c *Config) SpreadsheetAllowed(spreadsheetID string) bool {
	if c == nil || c.AllowedResources == nil {
		return true
	}
	_, ok := c.AllowedResources[spreadsheetID]
	return ok
}

func (c *Config) ResourceAllowed(spreadsheetID, sheetID string) bool {
	if c == nil || c.AllowedResources == nil {
		return true
	}
	sheets, ok := c.AllowedResources[spreadsheetID]
	if !ok {
		return false
	}
	for _, allowed := range sheets {
		if allowed == "*" || allowed == sheetID {
			return true
		}
	}
	return false
}

// BaseURL returns the Workiva API base URL for the configured region.
func (c *Config) BaseURL() string {
	return baseURLs[c.Region]
}

// Validate scalar types and duplicate keys before decoding can coerce or replace them.
func validatePolicyNode(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode || len(node.Content) == 0 {
		return errors.New("allowed_resources must be a nonempty map")
	}
	seen := make(map[string]bool)
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Tag != "!!str" || seen[key.Value] {
			return errors.New("allowed_resources requires unique string spreadsheet IDs")
		}
		seen[key.Value] = true
		if value.Kind != yaml.SequenceNode {
			return errors.New("allowed_resources requires sheet ID arrays")
		}
		for _, sheet := range value.Content {
			if sheet.Tag != "!!str" {
				return errors.New("allowed_resources requires string sheet IDs")
			}
		}
	}
	return nil
}
