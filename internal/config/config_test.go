package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// configEnvKeys are the environment variables Load consults. Tests clear
// them so the host environment cannot leak into assertions.
var configEnvKeys = []string{
	"NL_REGION",
	"NL_WORKIVA_CLIENT_ID",
	"NL_WORKIVA_CLIENT_SECRET",
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range configEnvKeys {
		if v, ok := os.LookupEnv(k); ok {
			old := v
			os.Unsetenv(k)
			t.Cleanup(func() { os.Setenv(k, old) })
		} else {
			t.Cleanup(func() { os.Unsetenv(k) })
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

func TestDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertDefaults(t, cfg)
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
}

func TestYAMLOverridesDefaults(t *testing.T) {
	clearEnv(t)
	path := writeYAML(t, `
db_path: /tmp/custom.db
listen_addr: :9090
read_cache_ttl: 45s
require_write_confirmation: false
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
