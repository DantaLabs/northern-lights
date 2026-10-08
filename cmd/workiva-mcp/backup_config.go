package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/transfer"
)

const backupMaxKeyFileBytes = 256

type backupDependencyFactory func(context.Context, assurance.AzureBackupConfig, transfer.AzureFenceConfig) (backupFence, backupBlob, error)

func configureProductionBackupWithFactory(ctx context.Context, cfg *config.Config, authOptions mcpserver.Options, maintenance *maintenanceCoordinator, auditLog *audit.Log, factory backupDependencyFactory) (http.Handler, error) {
	return configureProductionBackupWithKeyPaths(ctx, cfg, authOptions, maintenance, auditLog, factory, "", "")
}

func configureProductionBackupWithKeyPaths(ctx context.Context, cfg *config.Config, authOptions mcpserver.Options, maintenance *maintenanceCoordinator, auditLog *audit.Log, factory backupDependencyFactory, provisionedKeyPath, provisionedTempParent string) (http.Handler, error) {
	value, present := os.LookupEnv("NL_BACKUP_MAINTENANCE")
	if !present {
		return nil, nil
	}
	enabled, err := backupMaintenanceOptIn(value)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, nil
	}
	if ctx == nil || cfg == nil || cfg.AuthMode != config.AuthModeEntra || authOptions.AuthMode != config.AuthModeEntra || !cfg.AssuranceEnabled || cfg.DemoMode || strings.TrimSpace(cfg.EntraTenantID) == "" || strings.TrimSpace(cfg.EntraTenantID) != cfg.EntraTenantID || authOptions.TokenVerifier == nil {
		return nil, errors.New("backup maintenance requires non-demo Entra assurance, trusted tenant, and token verifier")
	}
	_, hasSecretID := os.LookupEnv(backupSigningKeySecretIDEnv)
	_, hasLegacyKey := os.LookupEnv("NL_BACKUP_SIGNING_KEY_FILE")
	_, hasLegacyParent := os.LookupEnv("NL_BACKUP_TEMP_PARENT")
	if hasSecretID && (hasLegacyKey || hasLegacyParent) {
		return nil, errors.New("backup signing key secret ID cannot be combined with legacy key file or temporary parent")
	}
	if hasSecretID && (provisionedKeyPath == "" || provisionedTempParent == "") {
		return nil, errors.New("backup signing key secret must be provisioned before backup configuration")
	}
	if maintenance == nil || maintenance.db == nil || maintenance.gate == nil || auditLog == nil || !auditLog.SharesDB(maintenance.db) {
		return nil, errors.New("backup maintenance requires shared database, drain coordinator, and audit log")
	}
	if factory == nil {
		return nil, errors.New("backup maintenance dependency factory unavailable")
	}
	serviceURL, err := requiredBackupEnv("NL_BACKUP_BLOB_SERVICE_URL")
	if err != nil {
		return nil, err
	}
	if err := validateBackupAccountURL(serviceURL); err != nil {
		return nil, err
	}
	container, err := requiredBackupEnv("NL_BACKUP_BLOB_CONTAINER")
	if err != nil {
		return nil, err
	}
	if !validBackupContainer(container) {
		return nil, errors.New("NL_BACKUP_BLOB_CONTAINER is invalid")
	}
	environment, err := requiredBackupEnv("NL_BACKUP_ENVIRONMENT_DIGEST")
	if err != nil {
		return nil, err
	}
	if !validBackupIdentifier(environment) {
		return nil, errors.New("NL_BACKUP_ENVIRONMENT_DIGEST is invalid")
	}
	keyPath := provisionedKeyPath
	if keyPath == "" {
		keyPath, err = requiredBackupEnv("NL_BACKUP_SIGNING_KEY_FILE")
		if err != nil {
			return nil, err
		}
	}
	key, err := loadBackupSigningKey(keyPath)
	if err != nil {
		return nil, err
	}
	tempParent := provisionedTempParent
	if tempParent == "" {
		tempParent, err = requiredBackupEnv("NL_BACKUP_TEMP_PARENT")
		if err != nil {
			return nil, err
		}
	}
	if err := validatePrivateBackupTempParent(tempParent); err != nil {
		return nil, err
	}
	blobConfig := assurance.AzureBackupConfig{ServiceURL: serviceURL, ContainerName: container, TenantID: cfg.EntraTenantID, EnvironmentDigest: environment, Timeout: 90 * time.Second, MaxDatabaseBytes: 2 << 30}
	fenceConfig := transfer.AzureFenceConfig{ServiceURL: serviceURL, ContainerName: container, TenantID: cfg.EntraTenantID, EnvironmentDigest: environment, Timeout: 90 * time.Second, MaxScanPages: 256, MaxScanObjects: 10000, MaxScanBytes: 64 << 20}
	fence, blob, err := factory(ctx, blobConfig, fenceConfig)
	if err != nil {
		return nil, fmt.Errorf("initialize backup storage adapters: %w", err)
	}
	if fence == nil || blob == nil {
		return nil, errors.New("initialize backup storage adapters: incomplete adapters")
	}
	maintenance.operation = &productionBackupOperation{db: maintenance.db, config: BackupOperationConfig{
		TenantID: cfg.EntraTenantID, EnvironmentDigest: environment, TempParent: tempParent,
		SigningKey: assurance.SigningKey{Ed25519Private: key}, CheckpointKey: key,
		Audit: auditLog, Fence: fence, Blob: blob,
	}}
	return newBackupMaintenanceEndpoint(authOptions.TokenVerifier, cfg.EntraTenantID, maintenance.CreateProductionBackup), nil
}

func defaultBackupDependencyFactory(ctx context.Context, blobConfig assurance.AzureBackupConfig, fenceConfig transfer.AzureFenceConfig) (backupFence, backupBlob, error) {
	blobClient, err := assurance.NewAzureBackupTransport(ctx, blobConfig)
	if err != nil {
		return nil, nil, err
	}
	fence, err := transfer.NewAzureBlobFence(ctx, fenceConfig)
	if err != nil {
		return nil, nil, err
	}
	return fence, blobClient, nil
}

func backupMaintenanceOptIn(value string) (bool, error) {
	switch value {
	case "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errors.New("NL_BACKUP_MAINTENANCE must be exactly true or false")
	}
}

func requiredBackupEnv(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" || strings.TrimSpace(value) != value {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func validateBackupAccountURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "" || u.RawPath != "" || u.Host == "" || u.Port() != "" || u.Host != strings.ToLower(u.Host) || u.String() != raw {
		return errors.New("NL_BACKUP_BLOB_SERVICE_URL must be a canonical HTTPS Azure account endpoint without path, credentials, query, or fragment")
	}
	host := u.Hostname()
	suffix := ""
	for _, candidate := range []string{".blob.core.windows.net", ".blob.core.usgovcloudapi.net", ".blob.core.chinacloudapi.cn"} {
		if strings.HasSuffix(host, candidate) {
			suffix = candidate
			break
		}
	}
	account := strings.TrimSuffix(host, suffix)
	if suffix == "" || len(account) < 3 || len(account) > 24 {
		return errors.New("NL_BACKUP_BLOB_SERVICE_URL must identify an Azure Blob account endpoint")
	}
	for _, r := range account {
		if !isBackupAccountRune(r) {
			return errors.New("NL_BACKUP_BLOB_SERVICE_URL must identify a canonical Azure storage account")
		}
	}
	return nil
}

func validBackupContainer(value string) bool {
	if len(value) < 3 || len(value) > 63 || value != strings.ToLower(value) || value[0] == '-' || value[len(value)-1] == '-' || strings.Contains(value, "--") {
		return false
	}
	for _, r := range value {
		if !isBackupContainerRune(r) {
			return false
		}
	}
	return true
}

func validBackupIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 128 || strings.TrimSpace(value) != value || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if !isBackupIdentifierRune(r) {
			return false
		}
	}
	return true
}

func isBackupAccountRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
}

func isBackupContainerRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-'
}

func isBackupIdentifierRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-'
}

func loadBackupSigningKey(path string) (ed25519.PrivateKey, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("NL_BACKUP_SIGNING_KEY_FILE must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != filepath.Clean(path) {
		return nil, errors.New("NL_BACKUP_SIGNING_KEY_FILE path must not traverse symlinks")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("NL_BACKUP_SIGNING_KEY_FILE is unavailable")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || (info.Mode().Perm() != 0o600 && info.Mode().Perm() != 0o400) || info.Size() > backupMaxKeyFileBytes {
		return nil, errors.New("NL_BACKUP_SIGNING_KEY_FILE must be a bounded regular file with mode 0600 or 0400")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("NL_BACKUP_SIGNING_KEY_FILE is unavailable")
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm() != info.Mode().Perm() {
		return nil, errors.New("NL_BACKUP_SIGNING_KEY_FILE changed during validation")
	}
	data, err := io.ReadAll(io.LimitReader(file, backupMaxKeyFileBytes+1))
	if err != nil || len(data) > backupMaxKeyFileBytes {
		return nil, errors.New("NL_BACKUP_SIGNING_KEY_FILE is malformed or oversized")
	}
	encoded := strings.TrimSuffix(string(data), "\n")
	if strings.ContainsAny(encoded, "\r\n \t") || len(encoded) != ed25519.PrivateKeySize*2 {
		return nil, errors.New("NL_BACKUP_SIGNING_KEY_FILE must contain one 64-byte Ed25519 private key in hex")
	}
	raw, err := hex.DecodeString(encoded)
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, errors.New("NL_BACKUP_SIGNING_KEY_FILE must contain one 64-byte Ed25519 private key in hex")
	}
	private := ed25519.PrivateKey(raw)
	derived := ed25519.NewKeyFromSeed(private[:ed25519.SeedSize])
	if !equalBytes(private, derived) {
		return nil, errors.New("NL_BACKUP_SIGNING_KEY_FILE contains an inconsistent Ed25519 private key")
	}
	return private, nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var difference byte
	for i := range a {
		difference |= a[i] ^ b[i]
	}
	return difference == 0
}

func validatePrivateBackupTempParent(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("NL_BACKUP_TEMP_PARENT must be an absolute canonical directory path")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o700 != 0o700 {
		return errors.New("NL_BACKUP_TEMP_PARENT must be an existing private directory without symlinks")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return errors.New("NL_BACKUP_TEMP_PARENT path must not traverse symlinks")
	}
	return nil
}
