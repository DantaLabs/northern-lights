package main

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/mcpserver/tools"
)

func TestContentPlacementOptIn(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		present bool
		want    bool
		wantErr bool
	}{
		{name: "absent defaults off"},
		{name: "exact true", value: "true", present: true, want: true},
		{name: "exact false", value: "false", present: true},
		{name: "empty", present: true, wantErr: true},
		{name: "uppercase", value: "TRUE", present: true, wantErr: true},
		{name: "whitespace", value: " true", present: true, wantErr: true},
		{name: "numeric", value: "1", present: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := contentPlacementOptIn(tt.value, tt.present)
			if (err != nil) != tt.wantErr {
				t.Fatalf("contentPlacementOptIn() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("contentPlacementOptIn() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadContentStorageKeyRequiresPrivateBoundedHex(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "key")
	if err := os.WriteFile(valid, []byte(strings.Repeat("ab", contentStorageKeyBytes)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := loadContentStorageKey(valid)
	if err != nil || hex.EncodeToString(key) != strings.Repeat("ab", contentStorageKeyBytes) {
		t.Fatalf("valid key len=%d err=%v", len(key), err)
	}
	clearContentStorageKey(key)
	for _, tc := range []struct {
		name string
		mode os.FileMode
		data string
	}{
		{name: "public permissions", mode: 0o644, data: strings.Repeat("ab", contentStorageKeyBytes)},
		{name: "short key", mode: 0o600, data: strings.Repeat("ab", contentStorageKeyBytes-1)},
		{name: "malformed hex", mode: 0o600, data: strings.Repeat("zz", contentStorageKeyBytes)},
		{name: "embedded whitespace", mode: 0o600, data: strings.Repeat("ab", 16) + " " + strings.Repeat("ab", 16)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-"))
			if err := os.WriteFile(path, []byte(tc.data), tc.mode); err != nil {
				t.Fatal(err)
			}
			if got, err := loadContentStorageKey(path); err == nil {
				clearContentStorageKey(got)
				t.Fatal("invalid content storage key accepted")
			}
		})
	}
	link := filepath.Join(dir, "key-link")
	if err := os.Symlink(valid, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadContentStorageKey(link); err == nil {
		t.Fatal("symlinked content storage key accepted")
	}
	if _, err := loadContentStorageKey("relative-key"); err == nil {
		t.Fatal("relative content storage key path accepted")
	}
}

func TestContentStorageKeyConfigRequiresKeyIDAndFile(t *testing.T) {
	for _, name := range []string{contentStorageKeyFileEnv, contentStorageKeyIDEnv} {
		t.Setenv(name, "")
	}
	if _, err := loadContentStorageKeyConfig(); err == nil {
		t.Fatal("missing content storage key config accepted")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte(strings.Repeat("cd", contentStorageKeyBytes)), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv(contentStorageKeyFileEnv, path)
	t.Setenv(contentStorageKeyIDEnv, "key-v1")
	config, err := loadContentStorageKeyConfig()
	if err != nil || config.keyID != "key-v1" || len(config.key) != contentStorageKeyBytes {
		t.Fatalf("content key config = (id=%q, len=%d), err=%v", config.keyID, len(config.key), err)
	}
	clearContentStorageKey(config.key)
	for _, badID := range []string{"", " key", "a/b", strings.Repeat("a", 129)} {
		t.Setenv(contentStorageKeyIDEnv, badID)
		if _, err := loadContentStorageKeyConfig(); err == nil {
			t.Fatalf("invalid key ID %q accepted", badID)
		}
	}
}

func TestContentProductionSettingsRequireSignedPolicyAndEncryptedStorageConfig(t *testing.T) {
	for _, name := range []string{contentAccessPolicyEnv, contentStorageKeyFileEnv, contentStorageKeyIDEnv} {
		t.Setenv(name, "")
	}
	cfg := &config.Config{AuthMode: config.AuthModeEntra, AssuranceEnabled: true, EntraTenantID: "56c9ddd6-2cbf-4436-bd08-39813ef20f7c", AssuranceBundlePublicKey: strings.Repeat("01", 32)}
	if _, err := loadContentProductionSettings(cfg); err == nil {
		t.Fatal("missing signed policy selector and encrypted-storage key accepted")
	}
	for _, invalid := range []*config.Config{
		{AuthMode: config.AuthModeAPIKey, AssuranceEnabled: true, EntraTenantID: "56c9ddd6-2cbf-4436-bd08-39813ef20f7c", AssuranceBundlePublicKey: strings.Repeat("01", 32)},
		{AuthMode: config.AuthModeEntra, AssuranceEnabled: false, EntraTenantID: "56c9ddd6-2cbf-4436-bd08-39813ef20f7c", AssuranceBundlePublicKey: strings.Repeat("01", 32)},
		{AuthMode: config.AuthModeEntra, AssuranceEnabled: true, DemoMode: true, EntraTenantID: "56c9ddd6-2cbf-4436-bd08-39813ef20f7c", AssuranceBundlePublicKey: strings.Repeat("01", 32)},
		{AuthMode: config.AuthModeEntra, AssuranceEnabled: true, EntraTenantID: "tenant-a", AssuranceBundlePublicKey: strings.Repeat("01", 32)},
	} {
		if _, err := loadContentProductionSettings(invalid); err == nil {
			t.Fatal("unsafe content production identity config accepted")
		}
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "content-key")
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("ab", contentStorageKeyBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(contentAccessPolicyEnv, "content-access")
	t.Setenv(contentStorageKeyIDEnv, "content-key-v1")
	t.Setenv(contentStorageKeyFileEnv, keyPath)
	settings, err := loadContentProductionSettings(cfg)
	if err != nil {
		t.Fatalf("valid content production settings rejected: %v", err)
	}
	if settings.accessPolicyID != "content-access" || len(settings.trustedKey) != 32 || settings.storageKey.keyID != "content-key-v1" || len(settings.storageKey.key) != contentStorageKeyBytes {
		t.Fatal("content settings omitted policy binding, trusted key, or storage key")
	}
	clearContentStorageKey(settings.storageKey.key)
}

func TestContentPolicySelectorMatchesSignedBundleIDSyntax(t *testing.T) {
	for _, valid := range []string{"content-access", "Policy_1:v2", "a"} {
		if !validContentPolicyID(valid) {
			t.Errorf("valid policy selector %q rejected", valid)
		}
	}
	for _, invalid := range []string{"", ".policy", "-policy", "_policy", "a/b", "bad space", strings.Repeat("a", 129)} {
		if validContentPolicyID(invalid) {
			t.Errorf("invalid policy selector %q accepted", invalid)
		}
	}
}

func TestEnsurePrivateContentDatabaseProtectsDBAndWALPaths(t *testing.T) {
	root := t.TempDir()
	privateDir := filepath.Join(root, "private")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(privateDir, "state.db")
	if err := ensurePrivateContentDatabase(dbPath); err != nil {
		t.Fatalf("missing DB in private parent should be admitted without creating it: %v", err)
	}
	if _, err := os.Lstat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("privacy preflight created the database: stat err=%v", err)
	}
	if err := os.WriteFile(dbPath, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateContentDatabase(dbPath); err != nil {
		t.Fatalf("private existing DB rejected: %v", err)
	}

	publicParent := filepath.Join(root, "public")
	if err := os.Mkdir(publicParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(publicParent, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateContentDatabase(filepath.Join(publicParent, "state.db")); err == nil {
		t.Fatal("DB under group-accessible parent admitted")
	}

	publicDB := filepath.Join(privateDir, "public.db")
	if err := os.WriteFile(publicDB, []byte("existing"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateContentDatabase(publicDB); err == nil {
		t.Fatal("group-readable existing DB admitted")
	}
	if info, err := os.Stat(publicDB); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("rejected existing DB permissions were modified: info=%v err=%v", info, err)
	}

	walPath := dbPath + "-wal"
	if err := os.WriteFile(walPath, []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(walPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateContentDatabase(dbPath); err == nil {
		t.Fatal("public WAL sidecar admitted")
	}
}

func TestEnsurePrivateContentDatabaseRejectsSymlinksAndNonFilePaths(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target.db")
	if err := os.WriteFile(target, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, ":memory:", "relative.db", "file:state.db"} {
		if err := ensurePrivateContentDatabase(path); err == nil {
			t.Errorf("unsafe content SQLite path %q admitted", path)
		}
	}
	parentLink := filepath.Join(root, "linked-parent")
	if err := os.Symlink(dir, parentLink); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateContentDatabase(filepath.Join(parentLink, "new.db")); err == nil {
		t.Fatal("symlinked SQLite parent admitted")
	}
	if err := os.Mkdir(filepath.Join(dir, "not-a-db"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateContentDatabase(filepath.Join(dir, "not-a-db")); err == nil {
		t.Fatal("directory accepted as SQLite DB")
	}
	shm := target + "-shm"
	if err := os.Symlink(target, shm); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateContentDatabase(target); err == nil {
		t.Fatal("symlinked SQLite SHM sidecar admitted")
	}
}

func TestContentPlacementEnabledReadsExplicitOptIn(t *testing.T) {
	t.Setenv(contentPlacementOptInEnv, "")
	if enabled, err := contentPlacementEnabled(); err == nil || enabled {
		t.Fatalf("empty explicit opt-in = enabled %v, err %v; want configuration error", enabled, err)
	}
	t.Setenv(contentPlacementOptInEnv, "false")
	if enabled, err := contentPlacementEnabled(); err != nil || enabled {
		t.Fatalf("false opt-in = enabled %v, err %v", enabled, err)
	}
	t.Setenv(contentPlacementOptInEnv, "true")
	if enabled, err := contentPlacementEnabled(); err != nil || !enabled {
		t.Fatalf("true opt-in = enabled %v, err %v", enabled, err)
	}
}

func TestContentToolRegistrationIsOptInAndAddsOneTool(t *testing.T) {
	for _, tc := range []struct {
		name        string
		enabled     bool
		wantCount   int
		wantContent bool
	}{
		{name: "disabled preserves default catalog", wantCount: 13},
		{name: "enabled adds one placement tool", enabled: true, wantCount: 14, wantContent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := mcpserver.NewRegistry()
			registerProductionTools(registry, tc.enabled)
			names := registry.Names()
			if len(tools.All()) != 13 {
				t.Fatalf("base tool count = %d, want 13", len(tools.All()))
			}
			found := false
			for _, name := range names {
				if name == "workiva_content_placement" {
					found = true
				}
			}
			if len(names) != tc.wantCount || found != tc.wantContent {
				t.Fatalf("registered %d tools; content=%v, want count=%d content=%v", len(names), found, tc.wantCount, tc.wantContent)
			}
		})
	}
}
