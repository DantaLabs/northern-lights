package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
	"github.com/dantalabs/northern-lights/internal/config"
)

const (
	backupSigningKeySecretIDEnv = "NL_BACKUP_SIGNING_KEY_SECRET_ID"
	backupKeyReadTimeout        = 15 * time.Second
)

type backupKeySecretReader func(context.Context, string) ([]byte, error)

var productionBackupKeySecretReader backupKeySecretReader = defaultBackupKeySecretReader

func provisionRuntimeBackupSigningKey(ctx context.Context, cfg *config.Config, secretID string, reader backupKeySecretReader, tempParent string) (*provisionedBackupKey, error) {
	if cfg == nil || cfg.AuthMode != config.AuthModeEntra || cfg.DemoMode || !cfg.AssuranceEnabled || strings.TrimSpace(cfg.EntraTenantID) == "" || strings.TrimSpace(cfg.EntraTenantID) != cfg.EntraTenantID {
		return nil, errors.New("backup signing key provisioning requires non-demo Entra assurance and trusted tenant")
	}
	return provisionBackupSigningKey(ctx, secretID, reader, tempParent)
}

type provisionedBackupKey struct {
	root    string
	keyPath string
	stage   string
	key     ed25519.PrivateKey
}

func (p *provisionedBackupKey) cleanup() {
	if p == nil {
		return
	}
	clear(p.key)
	p.key = nil
	if p.keyPath != "" {
		_ = os.Remove(p.keyPath)
	}
	if p.stage != "" {
		_ = os.Remove(p.stage)
	}
	if p.root != "" {
		_ = os.Remove(p.root)
	}
}

type backupKeySecretReference struct {
	vaultURL string
	name     string
	version  string
}

func parseBackupKeySecretID(raw string) (backupKeySecretReference, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Port() != "" || u.Host == "" || u.Host != strings.ToLower(u.Host) || u.String() != raw {
		return backupKeySecretReference{}, errors.New("NL_BACKUP_SIGNING_KEY_SECRET_ID must be a canonical versioned Key Vault secret URL")
	}
	host := u.Hostname()
	validHost := false
	for _, suffix := range []string{".vault.azure.net", ".vault.usgovcloudapi.net", ".vault.azure.cn"} {
		if strings.HasSuffix(host, suffix) {
			name := strings.TrimSuffix(host, suffix)
			validHost = len(name) >= 3 && len(name) <= 24
			for _, r := range name {
				if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
					validHost = false
				}
			}
			break
		}
	}
	parts := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if !validHost || len(parts) != 3 || parts[0] != "secrets" || parts[1] == "" || len(parts[1]) > 127 || len(parts[2]) != 32 {
		return backupKeySecretReference{}, errors.New("NL_BACKUP_SIGNING_KEY_SECRET_ID must identify a named, pinned Key Vault secret version")
	}
	for _, r := range parts[1] {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return backupKeySecretReference{}, errors.New("NL_BACKUP_SIGNING_KEY_SECRET_ID contains an invalid secret name")
		}
	}
	for _, r := range parts[2] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return backupKeySecretReference{}, errors.New("NL_BACKUP_SIGNING_KEY_SECRET_ID version must be 32 lowercase hexadecimal characters")
		}
	}
	return backupKeySecretReference{vaultURL: u.Scheme + "://" + u.Host, name: parts[1], version: parts[2]}, nil
}

func defaultBackupKeySecretReader(ctx context.Context, secretID string) ([]byte, error) {
	ref, err := parseBackupKeySecretID(secretID)
	if err != nil {
		return nil, errors.New("backup signing key secret reference is invalid")
	}
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, errors.New("backup signing key credential unavailable")
	}
	client, err := azsecrets.NewClient(ref.vaultURL, credential, &azsecrets.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		return nil, errors.New("backup signing key Key Vault client unavailable")
	}
	response, err := client.GetSecret(ctx, ref.name, ref.version, nil)
	if err != nil || response.Value == nil {
		return nil, errors.New("backup signing key retrieval failed")
	}
	return []byte(*response.Value), nil
}

func provisionBackupSigningKey(ctx context.Context, secretID string, reader backupKeySecretReader, tempParent string) (*provisionedBackupKey, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, errors.New("backup signing key provisioning canceled")
	}
	if _, err := parseBackupKeySecretID(secretID); err != nil {
		return nil, err
	}
	if reader == nil || tempParent == "" || !filepath.IsAbs(tempParent) {
		return nil, errors.New("backup signing key provisioning configuration invalid")
	}
	work, cancel := context.WithTimeout(ctx, backupKeyReadTimeout)
	defer cancel()
	secret, err := reader(work, secretID)
	if err != nil {
		clear(secret)
		return nil, errors.New("backup signing key retrieval failed")
	}
	defer clear(secret)
	if work.Err() != nil {
		return nil, errors.New("backup signing key provisioning canceled")
	}
	if len(secret) == ed25519.PrivateKeySize*2+1 && secret[len(secret)-1] == '\n' {
		secret = secret[:len(secret)-1]
	}
	if len(secret) != ed25519.PrivateKeySize*2 {
		return nil, errors.New("backup signing key secret is empty or oversized")
	}
	raw := make([]byte, ed25519.PrivateKeySize)
	decoded, decodeErr := hex.Decode(raw, secret)
	if decodeErr != nil || decoded != ed25519.PrivateKeySize {
		clear(raw)
		return nil, errors.New("backup signing key secret is malformed")
	}
	key := ed25519.PrivateKey(raw)
	derived := ed25519.NewKeyFromSeed(key[:ed25519.SeedSize])
	if !equalBytes(key, derived) {
		clear(key)
		clear(derived)
		return nil, errors.New("backup signing key secret is inconsistent")
	}
	clear(derived)

	if work.Err() != nil {
		clear(key)
		return nil, errors.New("backup signing key provisioning canceled")
	}
	root, err := os.MkdirTemp(tempParent, "northern-lights-key-")
	if err != nil {
		clear(key)
		return nil, errors.New("backup signing key private directory creation failed")
	}
	owned := &provisionedBackupKey{root: root, key: key}
	failed := true
	defer func() {
		if failed {
			owned.cleanup()
		}
	}()
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, errors.New("backup signing key private directory setup failed")
	}
	owned.stage = filepath.Join(root, "staging")
	if err := os.Mkdir(owned.stage, 0o700); err != nil {
		return nil, errors.New("backup signing key staging directory setup failed")
	}
	owned.keyPath = filepath.Join(root, "signing-key.hex")
	file, err := os.OpenFile(owned.keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, errors.New("backup signing key file creation failed")
	}
	writeErr := writeBoundedKey(file, secret)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return nil, errors.New("backup signing key file write failed")
	}
	if work.Err() != nil {
		return nil, errors.New("backup signing key provisioning canceled")
	}
	readback, err := loadBackupSigningKey(owned.keyPath)
	if err != nil || !equalBytes(readback, key) {
		clear(readback)
		return nil, errors.New("backup signing key read-back validation failed")
	}
	clear(readback)
	if work.Err() != nil {
		return nil, errors.New("backup signing key provisioning canceled")
	}
	failed = false
	return owned, nil
}

func writeBoundedKey(file *os.File, data []byte) error {
	if len(data) == 0 || len(data) > backupMaxKeyFileBytes {
		return errors.New("invalid key data size")
	}
	for len(data) > 0 {
		n, err := file.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("short key write")
		}
		data = data[n:]
	}
	return nil
}
