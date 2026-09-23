package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
)

const documentedEntraAudience = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

// configEnvKeys are the environment variables Load consults. Tests clear
// them so the host environment cannot leak into assertions.
var configEnvKeys = []string{
	"NL_REGION",
	"NL_DB_PATH",
	"NL_LISTEN_ADDR",
	"NL_READ_CACHE_TTL",
	"NL_REQUIRE_WRITE_CONFIRMATION",
	"NL_DISABLE_LOCALHOST_PROTECTION",
	"NL_ALLOWED_RESOURCES",
	"NL_WORKIVA_CLIENT_ID",
	"NL_WORKIVA_CLIENT_SECRET",
	"NL_DEMO_MODE",
	"NL_AUTH_MODE",
	"NL_ENTRA_TENANT_ID",
	"NL_ENTRA_AUTHORITY",
	"NL_ENTRA_AUDIENCE",
	"NL_ENTRA_ALLOW_SUBJECT_FALLBACK",
	"NL_ENTRA_SCOPE_PERMISSIONS",
	"NL_ENTRA_ROLE_PERMISSIONS",
	"NL_ENTRA_APP_CLIENT_PERMISSIONS",
}

func TestAuthModeDefaultsToAPIKey(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthMode != AuthModeAPIKey {
		t.Fatalf("AuthMode = %q, want %q", cfg.AuthMode, AuthModeAPIKey)
	}
}

func TestEntraConfigurationLoadsFromEnvironment(t *testing.T) {
	clearEnv(t)
	setValidEntraEnv(t)
	t.Setenv("NL_ENTRA_ALLOW_SUBJECT_FALLBACK", "true")
	cfg, err := Load(writeYAML(t, "auth_mode: api_key\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AuthMode != AuthModeEntra {
		t.Fatalf("AuthMode = %q, want entra env override", cfg.AuthMode)
	}
	if !cfg.EntraAllowSubjectFallback {
		t.Fatal("subject fallback env flag was not loaded")
	}
	if cfg.EntraAudience != documentedEntraAudience {
		t.Fatalf("EntraAudience = %q, want exact documented API client ID %q", cfg.EntraAudience, documentedEntraAudience)
	}
	wantScopes := map[string][]identity.Permission{"nl.read": {identity.PermissionWorkivaRead}}
	if !reflect.DeepEqual(cfg.EntraScopePermissions, wantScopes) {
		t.Fatalf("scope permissions = %#v, want %#v", cfg.EntraScopePermissions, wantScopes)
	}
	verifierConfig := cfg.EntraVerifierConfig()
	if verifierConfig.TenantID != cfg.EntraTenantID || verifierConfig.Audience != cfg.EntraAudience || verifierConfig.Authority != cfg.EntraAuthority {
		t.Fatalf("verifier config = %#v", verifierConfig)
	}
}

func TestApplicationClientPermissionKeysAreCanonicalized(t *testing.T) {
	clearEnv(t)
	setValidEntraEnv(t)
	t.Setenv("NL_ENTRA_ROLE_PERMISSIONS", `{"nl.reader":["workiva.read"]}`)
	t.Setenv("NL_ENTRA_APP_CLIENT_PERMISSIONS", `{"{AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE}":["workiva.read"]}`)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const canonicalClientID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	want := map[string][]identity.Permission{canonicalClientID: {identity.PermissionWorkivaRead}}
	if !reflect.DeepEqual(cfg.EntraAppClientPermissions, want) {
		t.Fatalf("application client permissions = %#v, want %#v", cfg.EntraAppClientPermissions, want)
	}

	permissions, err := cfg.EntraVerifierConfig().Policy.PermissionsFor(identity.Principal{
		TokenType:          identity.TokenTypeApplication,
		AuthorizedClientID: canonicalClientID,
		Roles:              []string{"nl.reader"},
	})
	if err != nil {
		t.Fatalf("canonical client policy lookup: %v", err)
	}
	if !reflect.DeepEqual(permissions, []identity.Permission{identity.PermissionWorkivaRead}) {
		t.Fatalf("permissions = %#v, want workiva.read", permissions)
	}
}

func TestEntraGUIDConfigurationIsCanonicalized(t *testing.T) {
	tests := []struct {
		name          string
		tenantID      string
		audience      string
		wantTenantID  string
		wantAudience  string
		wantAuthority string
	}{
		{
			name:          "uppercase",
			tenantID:      "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE",
			audience:      "FFFFFFFF-AAAA-BBBB-CCCC-DDDDDDDDDDDD",
			wantTenantID:  "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			wantAudience:  "ffffffff-aaaa-bbbb-cccc-dddddddddddd",
			wantAuthority: "https://login.microsoftonline.com/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/v2.0",
		},
		{
			name:          "braced",
			tenantID:      "{AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE}",
			audience:      "{FFFFFFFF-AAAA-BBBB-CCCC-DDDDDDDDDDDD}",
			wantTenantID:  "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			wantAudience:  "ffffffff-aaaa-bbbb-cccc-dddddddddddd",
			wantAuthority: "https://login.microsoftonline.com/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/v2.0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			setValidEntraEnv(t)
			t.Setenv("NL_ENTRA_TENANT_ID", tc.tenantID)
			t.Setenv("NL_ENTRA_AUTHORITY", tc.wantAuthority)
			t.Setenv("NL_ENTRA_AUDIENCE", tc.audience)

			cfg, err := Load("")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.EntraTenantID != tc.wantTenantID {
				t.Errorf("EntraTenantID = %q, want %q", cfg.EntraTenantID, tc.wantTenantID)
			}
			if cfg.EntraAudience != tc.wantAudience {
				t.Errorf("EntraAudience = %q, want %q", cfg.EntraAudience, tc.wantAudience)
			}

			verifierConfig := cfg.EntraVerifierConfig()
			if verifierConfig.TenantID != tc.wantTenantID {
				t.Errorf("verifier TenantID = %q, want %q", verifierConfig.TenantID, tc.wantTenantID)
			}
			if verifierConfig.Audience != tc.wantAudience {
				t.Errorf("verifier Audience = %q, want %q", verifierConfig.Audience, tc.wantAudience)
			}
		})
	}
}

func TestEntraAudienceMustBeGUID(t *testing.T) {
	for _, audience := range []string{"not-a-guid", "api://aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"} {
		t.Run(audience, func(t *testing.T) {
			clearEnv(t)
			setValidEntraEnv(t)
			t.Setenv("NL_ENTRA_AUDIENCE", audience)

			_, err := Load("")
			if err == nil {
				t.Fatal("Load succeeded; want fail-closed audience error")
			}
			if !strings.HasPrefix(err.Error(), "invalid NL_ENTRA_AUDIENCE: ") {
				t.Fatalf("Load error = %q, want invalid NL_ENTRA_AUDIENCE prefix", err)
			}
		})
	}
}

func TestEntraAuthorityRejectsNoncanonicalTenantPath(t *testing.T) {
	clearEnv(t)
	setValidEntraEnv(t)
	t.Setenv("NL_ENTRA_TENANT_ID", "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE")
	t.Setenv("NL_ENTRA_AUTHORITY", "https://login.microsoftonline.com/AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE/v2.0")

	_, err := Load("")
	if err == nil {
		t.Fatal("Load succeeded; want fail-closed canonical-authority error")
	}
	const want = "NL_ENTRA_AUTHORITY must name the configured tenant's v2.0 issuer exactly"
	if err.Error() != want {
		t.Fatalf("Load error = %q, want %q", err, want)
	}
}

func TestEntraConfigurationFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T)
	}{
		{name: "missing tenant", mutate: func(t *testing.T) { t.Setenv("NL_ENTRA_TENANT_ID", "") }},
		{name: "missing authority", mutate: func(t *testing.T) { t.Setenv("NL_ENTRA_AUTHORITY", "") }},
		{name: "missing audience", mutate: func(t *testing.T) { t.Setenv("NL_ENTRA_AUDIENCE", "") }},
		{name: "missing authorization mapping", mutate: func(t *testing.T) { t.Setenv("NL_ENTRA_SCOPE_PERMISSIONS", "") }},
		{name: "personal account tenant", mutate: func(t *testing.T) {
			t.Setenv("NL_ENTRA_TENANT_ID", identity.PersonalMicrosoftAccountTenantID)
			t.Setenv("NL_ENTRA_AUTHORITY", "https://login.microsoftonline.com/"+identity.PersonalMicrosoftAccountTenantID+"/v2.0")
		}},
		{name: "non Microsoft authority", mutate: func(t *testing.T) { t.Setenv("NL_ENTRA_AUTHORITY", "https://issuer.example/tenant/v2.0") }},
		{name: "http authority", mutate: func(t *testing.T) {
			t.Setenv("NL_ENTRA_AUTHORITY", "http://login.microsoftonline.com/11111111-1111-1111-1111-111111111111/v2.0")
		}},
		{name: "authority tenant mismatch", mutate: func(t *testing.T) {
			t.Setenv("NL_ENTRA_AUTHORITY", "https://login.microsoftonline.com/99999999-9999-9999-9999-999999999999/v2.0")
		}},
		{name: "unknown permission", mutate: func(t *testing.T) { t.Setenv("NL_ENTRA_SCOPE_PERMISSIONS", `{"nl.read":["workiva.superuser"]}`) }},
		{name: "role mapping without client policy", mutate: func(t *testing.T) {
			t.Setenv("NL_ENTRA_SCOPE_PERMISSIONS", "")
			t.Setenv("NL_ENTRA_ROLE_PERMISSIONS", `{"nl.reader":["workiva.read"]}`)
		}},
		{name: "client policy without role mapping", mutate: func(t *testing.T) {
			t.Setenv("NL_ENTRA_APP_CLIENT_PERMISSIONS", `{"33333333-3333-3333-3333-333333333333":["workiva.read"]}`)
		}},
		{name: "client policy without role overlap", mutate: func(t *testing.T) {
			t.Setenv("NL_ENTRA_ROLE_PERMISSIONS", `{"nl.reader":["workiva.read"]}`)
			t.Setenv("NL_ENTRA_APP_CLIENT_PERMISSIONS", `{"33333333-3333-3333-3333-333333333333":["audit.read"]}`)
		}},
		{name: "malformed subject fallback", mutate: func(t *testing.T) { t.Setenv("NL_ENTRA_ALLOW_SUBJECT_FALLBACK", "sometimes") }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			setValidEntraEnv(t)
			tc.mutate(t)
			if _, err := Load(""); err == nil {
				t.Fatal("Load succeeded; want fail-closed error")
			}
		})
	}
}

func TestUnknownAuthModeAndPartialEntraSettingsFail(t *testing.T) {
	t.Run("unknown auth mode", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("NL_AUTH_MODE", "oauth")
		if _, err := Load(""); err == nil {
			t.Fatal("unknown auth mode accepted")
		}
	})
	t.Run("partial settings in api key mode", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("NL_ENTRA_AUDIENCE", documentedEntraAudience)
		if _, err := Load(""); err == nil {
			t.Fatal("partial Entra settings accepted in api_key mode")
		}
	})
}

func setValidEntraEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NL_AUTH_MODE", "entra")
	t.Setenv("NL_ENTRA_TENANT_ID", "11111111-1111-1111-1111-111111111111")
	t.Setenv("NL_ENTRA_AUTHORITY", "https://login.microsoftonline.com/11111111-1111-1111-1111-111111111111/v2.0")
	t.Setenv("NL_ENTRA_AUDIENCE", documentedEntraAudience)
	t.Setenv("NL_ENTRA_SCOPE_PERMISSIONS", `{"nl.read":["workiva.read"]}`)
}

func TestAllowedResourcesYAMLAndJSONEnv(t *testing.T) {
	clearEnv(t)
	path := writeYAML(t, "allowed_resources:\n  yaml-spreadsheet: [sheet-a]\n")
	t.Setenv("NL_ALLOWED_RESOURCES", `{"env-spreadsheet":["sheet-b"]}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ResourceAllowed("yaml-spreadsheet", "sheet-a") || !cfg.ResourceAllowed("env-spreadsheet", "sheet-b") || cfg.ResourceAllowed("env-spreadsheet", "sheet-x") {
		t.Fatalf("environment policy did not replace YAML policy: %#v", cfg.AllowedResources)
	}
}

func TestMalformedAllowedResourcesFailClosedAtLoad(t *testing.T) {
	clearEnv(t)
	for _, value := range []string{`nope`, `{"sp":[]}`, `{"": ["sheet"]}`, `{"sp":[""]}`} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("NL_ALLOWED_RESOURCES", value)
			if _, err := Load(""); err == nil {
				t.Fatalf("Load(%q) succeeded", value)
			}
		})
	}
}

func TestAbsentAllowedResourcesIsUnrestricted(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ResourceAllowed("any-spreadsheet", "any-sheet") {
		t.Fatal("absent policy must be unrestricted")
	}
}

func TestEmptyYAMLIsSupportedAndUnrestricted(t *testing.T) {
	clearEnv(t)
	cfg, err := Load(writeYAML(t, ""))
	if err != nil {
		t.Fatalf("Load empty YAML: %v", err)
	}
	if !cfg.ResourceAllowed("any", "resource") {
		t.Fatal("empty YAML must preserve unrestricted access")
	}
}

func TestYAMLRejectsAmbiguousOrUnknownConfiguration(t *testing.T) {
	clearEnv(t)
	for _, tc := range []struct{ name, yaml string }{
		{"misspelled allowlist", "allowed_resource:\n  sp: [sh]\n"},
		{"additional document", "region: eu\n---\nallowed_resources:\n  sp: [sh]\n"},
		{"merge key", "<<: {allowed_resources: null}\nregion: eu\n"},
		{"alias", "allowed_resources: &policy\n  sp: [sh]\nregion: *policy\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeYAML(t, tc.yaml)); err == nil {
				t.Fatal("Load succeeded; want strict YAML rejection")
			}
		})
	}
}

func TestYAMLAcceptsSupportedConfiguration(t *testing.T) {
	clearEnv(t)
	cfg, err := Load(writeYAML(t, "region: eu\ndb_path: /tmp/nl.db\nallowed_resources:\n  sp: [sh, '*']\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.ResourceAllowed("sp", "sh") || cfg.ResourceAllowed("other", "sh") {
		t.Fatalf("unexpected policy: %#v", cfg.AllowedResources)
	}
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range configEnvKeys {
		if v, ok := os.LookupEnv(k); ok {
			old := v
			if err := os.Unsetenv(k); err != nil {
				t.Fatalf("unset %s: %v", k, err)
			}
			t.Cleanup(func() {
				if err := os.Setenv(k, old); err != nil {
					t.Errorf("restore %s: %v", k, err)
				}
			})
		} else {
			t.Cleanup(func() {
				if err := os.Unsetenv(k); err != nil {
					t.Errorf("clear %s: %v", k, err)
				}
			})
		}
	}
}

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	return path
}

func TestRegionBaseURLs(t *testing.T) {
	clearEnv(t)
	cases := []struct {
		region  string
		wantURL string
	}{
		{"eu", "https://api.eu.wdesk.com"},
		{"us", "https://api.app.wdesk.com"},
		{"apac", "https://api.apac.wdesk.com"},
	}
	for _, tc := range cases {
		t.Run(tc.region, func(t *testing.T) {
			path := writeYAML(t, "region: "+tc.region+"\n")
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.BaseURL(); got != tc.wantURL {
				t.Errorf("BaseURL() = %q, want %q", got, tc.wantURL)
			}
		})
	}
}

func TestInvalidRegionErrors(t *testing.T) {
	clearEnv(t)
	path := writeYAML(t, "region: asia\n")
	if _, err := Load(path); err == nil {
		t.Fatal("Load with region asia: expected error, got nil")
	}
}

func TestEnvRegionOverridesYAML(t *testing.T) {
	clearEnv(t)
	t.Setenv("NL_REGION", "us")
	path := writeYAML(t, "region: eu\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.BaseURL(); got != "https://api.app.wdesk.com" {
		t.Errorf("BaseURL() = %q, want US URL (env override)", got)
	}
}

func TestMissingYAMLFileUsesDefaults(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	assertDefaults(t, cfg)
}

func TestMalformedYAMLErrors(t *testing.T) {
	clearEnv(t)
	path := writeYAML(t, "region: [unclosed\n")
	if _, err := Load(path); err == nil {
		t.Fatal("Load with malformed YAML: expected error, got nil")
	}
}

func TestEnvDBPathOverridesYAML(t *testing.T) {
	clearEnv(t)
	t.Setenv("NL_DB_PATH", "/data/northern-lights.db")
	path := writeYAML(t, "db_path: /tmp/from-yaml.db\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DBPath != "/data/northern-lights.db" {
		t.Errorf("DBPath = %q, want env override /data/northern-lights.db", cfg.DBPath)
	}
}

func TestRuntimeEnvOverridesYAML(t *testing.T) {
	clearEnv(t)
	t.Setenv("NL_LISTEN_ADDR", ":9091")
	t.Setenv("NL_READ_CACHE_TTL", "2m15s")
	t.Setenv("NL_REQUIRE_WRITE_CONFIRMATION", "false")
	t.Setenv("NL_DISABLE_LOCALHOST_PROTECTION", "true")
	path := writeYAML(t, `
listen_addr: :8080
read_cache_ttl: 30s
require_write_confirmation: true
disable_localhost_protection: false
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != ":9091" {
		t.Errorf("ListenAddr = %q, want :9091", cfg.ListenAddr)
	}
	if cfg.ReadCacheTTL != 2*time.Minute+15*time.Second {
		t.Errorf("ReadCacheTTL = %v, want 2m15s", cfg.ReadCacheTTL)
	}
	if cfg.RequireWriteConfirmation {
		t.Error("RequireWriteConfirmation = true, want false")
	}
	if !cfg.DisableLocalhostProtection {
		t.Error("DisableLocalhostProtection = false, want true")
	}
}

func TestInvalidRuntimeEnvErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		value string
	}{
		{name: "cache duration", key: "NL_READ_CACHE_TTL", value: "forever"},
		{name: "write confirmation", key: "NL_REQUIRE_WRITE_CONFIRMATION", value: "sometimes"},
		{name: "localhost protection", key: "NL_DISABLE_LOCALHOST_PROTECTION", value: "yes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load(""); err == nil {
				t.Fatalf("Load with %s=%q returned nil error", tc.key, tc.value)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertDefaults(t, cfg)
}

func TestDemoModeFromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("NL_DEMO_MODE", "true")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.DemoMode {
		t.Error("DemoMode = false, want true")
	}
}

func assertDefaults(t *testing.T, cfg *Config) {
	t.Helper()
	if cfg.Region != "eu" {
		t.Errorf("Region = %q, want eu", cfg.Region)
	}
	if cfg.DBPath != "./northern-lights.db" {
		t.Errorf("DBPath = %q, want ./northern-lights.db", cfg.DBPath)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", cfg.ListenAddr)
	}
	if cfg.ReadCacheTTL != 30*time.Second {
		t.Errorf("ReadCacheTTL = %v, want 30s", cfg.ReadCacheTTL)
	}
	if !cfg.RequireWriteConfirmation {
		t.Error("RequireWriteConfirmation = false, want true")
	}
	if cfg.DisableLocalhostProtection {
		t.Error("DisableLocalhostProtection = true, want false")
	}
}

func TestYAMLOverridesDefaults(t *testing.T) {
	clearEnv(t)
	path := writeYAML(t, `
db_path: /tmp/custom.db
listen_addr: :9090
read_cache_ttl: 45s
require_write_confirmation: false
disable_localhost_protection: true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DBPath != "/tmp/custom.db" {
		t.Errorf("DBPath = %q, want /tmp/custom.db", cfg.DBPath)
	}
	if cfg.ListenAddr != ":9090" {
		t.Errorf("ListenAddr = %q, want :9090", cfg.ListenAddr)
	}
	if cfg.ReadCacheTTL != 45*time.Second {
		t.Errorf("ReadCacheTTL = %v, want 45s", cfg.ReadCacheTTL)
	}
	if cfg.RequireWriteConfirmation {
		t.Error("RequireWriteConfirmation = true, want false")
	}
	if !cfg.DisableLocalhostProtection {
		t.Error("DisableLocalhostProtection = false, want true")
	}
}

func TestCredentialsFromEnvOnly(t *testing.T) {
	clearEnv(t)
	t.Setenv("NL_WORKIVA_CLIENT_ID", "id-from-env")
	t.Setenv("NL_WORKIVA_CLIENT_SECRET", "secret-from-env")
	// YAML attempts to set credentials; they must be ignored.
	path := writeYAML(t, `
workiva_client_id: id-from-yaml
workiva_client_secret: secret-from-yaml
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkivaClientID != "id-from-env" {
		t.Errorf("WorkivaClientID = %q, want env value", cfg.WorkivaClientID)
	}
	if cfg.WorkivaClientSecret != "secret-from-env" {
		t.Errorf("WorkivaClientSecret = %q, want env value", cfg.WorkivaClientSecret)
	}
}

func TestEmptyEnvCredentials(t *testing.T) {
	clearEnv(t)
	path := writeYAML(t, "workiva_client_id: id-from-yaml\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkivaClientID != "" {
		t.Errorf("WorkivaClientID = %q, want empty (YAML never consulted)", cfg.WorkivaClientID)
	}
}

func TestExplicitMalformedPolicyNeverMeansUnrestricted(t *testing.T) {
	for _, value := range []string{"allowed_resources: null\n", "allowed_resources:\n", "allowed_resources:\n  sp: [123]\n", "allowed_resources:\n  ' ': [sheet]\n"} {
		t.Run(value, func(t *testing.T) {
			clearEnv(t)
			if _, err := Load(writeYAML(t, value)); err == nil {
				t.Fatal("malformed policy accepted")
			}
		})
	}
	t.Run("empty environment", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("NL_ALLOWED_RESOURCES", "")
		if _, err := Load(""); err == nil {
			t.Fatal("empty policy override accepted")
		}
	})
	t.Run("duplicate JSON", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("NL_ALLOWED_RESOURCES", `{"sp":["sheet"],"sp":["*"]}`)
		if _, err := Load(""); err == nil {
			t.Fatal("ambiguous duplicate policy accepted")
		}
	})
}

func TestWildcardPolicyAllowsOnlyNamedSpreadsheet(t *testing.T) {
	clearEnv(t)
	cfg, err := Load(writeYAML(t, "allowed_resources:\n  sp: ['*']\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SpreadsheetAllowed("sp") || !cfg.ResourceAllowed("sp", "any-sheet") || cfg.SpreadsheetAllowed("other") || cfg.ResourceAllowed("other", "any-sheet") {
		t.Fatalf("wildcard escaped spreadsheet: %+v", cfg.AllowedResources)
	}
}
