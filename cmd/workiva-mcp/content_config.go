package main

import (
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/google/uuid"
)

const (
	contentPlacementOptInEnv = "NL_CONTENT_PLACEMENT"
	contentStorageKeyFileEnv = "NL_CONTENT_STORAGE_KEY_FILE"
	contentStorageKeyIDEnv   = "NL_CONTENT_STORAGE_KEY_ID"
	contentAccessPolicyEnv   = "NL_CONTENT_ACCESS_POLICY_ID"
	contentStorageKeyBytes   = 32
	contentStorageKeyMaxFile = 128
)

type contentStorageKeyConfig struct {
	keyID string
	key   []byte
}

type contentProductionSettings struct {
	accessPolicyID string
	trustedKey     []byte
	storageKey     contentStorageKeyConfig
}

// contentPlacementOptIn keeps the feature dark unless an operator opts in
// explicitly. Values are intentionally exact; whitespace and alternate
// boolean spellings are configuration errors.
func contentPlacementOptIn(value string, present bool) (bool, error) {
	if !present {
		return false, nil
	}
	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, errors.New("NL_CONTENT_PLACEMENT must be exactly true or false")
	}
}

func contentPlacementEnabled() (bool, error) {
	value, present := os.LookupEnv(contentPlacementOptInEnv)
	return contentPlacementOptIn(value, present)
}

func loadContentStorageKeyConfig() (contentStorageKeyConfig, error) {
	path, hasPath := os.LookupEnv(contentStorageKeyFileEnv)
	keyID, hasID := os.LookupEnv(contentStorageKeyIDEnv)
	if !hasPath || !hasID || strings.TrimSpace(path) != path || path == "" || !validContentStorageKeyID(keyID) {
		return contentStorageKeyConfig{}, errors.New("content storage key file and valid key ID are required")
	}
	key, err := loadContentStorageKey(path)
	if err != nil {
		return contentStorageKeyConfig{}, err
	}
	return contentStorageKeyConfig{keyID: keyID, key: key}, nil
}

func loadContentProductionSettings(cfg *config.Config) (contentProductionSettings, error) {
	if cfg == nil || cfg.DemoMode || cfg.AuthMode != config.AuthModeEntra || !cfg.AssuranceEnabled || !canonicalContentTenant(cfg.EntraTenantID) {
		return contentProductionSettings{}, errors.New("content placement requires non-demo Entra assurance and a trusted tenant")
	}
	accessPolicyID := os.Getenv(contentAccessPolicyEnv)
	if !validContentPolicyID(accessPolicyID) {
		return contentProductionSettings{}, errors.New("NL_CONTENT_ACCESS_POLICY_ID is required and invalid")
	}
	trustedKey, err := assurance.ParsePublicKey(cfg.AssuranceBundlePublicKey)
	if err != nil {
		return contentProductionSettings{}, errors.New("content placement requires a valid trusted assurance bundle public key")
	}
	storageKey, err := loadContentStorageKeyConfig()
	if err != nil {
		return contentProductionSettings{}, errors.New("content placement requires a valid encrypted source storage key file and key ID")
	}
	return contentProductionSettings{accessPolicyID: accessPolicyID, trustedKey: trustedKey, storageKey: storageKey}, nil
}

// ensurePrivateContentDatabase rejects unsafe parent/SQLite paths before the
// shared opener runs. sqlitedb.Open remains responsible for creating a new
// 0600 database and configuring WAL; this function never changes an existing
// database or sidecar's permissions.
func ensurePrivateContentDatabase(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return errors.New("content placement requires an absolute plain SQLite file path")
	}
	parent := filepath.Dir(path)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil || resolvedParent != parent {
		return errors.New("content SQLite parent must exist and must not traverse symlinks")
	}
	parentInfo, err := os.Stat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode().Perm()&0o077 != 0 || parentInfo.Mode().Perm()&0o700 != 0o700 {
		return errors.New("content SQLite parent must be a private owner-writable directory (0700)")
	}
	owner, ok := parentInfo.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return errors.New("content SQLite parent must be owned by the running service user")
	}
	if err := validatePrivateContentSQLiteFile(path, true); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		// SQLite creates these sidecars only while they are needed. Their
		// absence is the normal state before the database is opened.
		if err := validatePrivateContentSQLiteFile(path+suffix, true); err != nil {
			return err
		}
	}
	return nil
}

func validatePrivateContentSQLiteFile(path string, allowMissing bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && allowMissing {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return errors.New("content SQLite database and existing WAL/SHM files must be regular owner-only 0600 files")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("content SQLite file changed or is unavailable")
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0o600 {
		return errors.New("content SQLite file changed during privacy validation")
	}
	return nil
}

func canonicalContentTenant(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}

func validContentStorageKeyID(value string) bool {
	if len(value) == 0 || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:-", r)
		if !valid {
			return false
		}
	}
	return true
}

func validContentPolicyID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for i, r := range value {
		alphaNum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		valid := alphaNum || i > 0 && strings.ContainsRune("._:-", r)
		if !valid {
			return false
		}
	}
	return true
}

func loadContentStorageKey(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("NL_CONTENT_STORAGE_KEY_FILE must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != filepath.Clean(path) {
		return nil, errors.New("NL_CONTENT_STORAGE_KEY_FILE path must not traverse symlinks")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || (info.Mode().Perm() != 0o600 && info.Mode().Perm() != 0o400) || info.Size() > contentStorageKeyMaxFile {
		return nil, errors.New("NL_CONTENT_STORAGE_KEY_FILE must be a bounded regular file with mode 0600 or 0400")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("NL_CONTENT_STORAGE_KEY_FILE is unavailable")
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm() != info.Mode().Perm() {
		return nil, errors.New("NL_CONTENT_STORAGE_KEY_FILE changed during validation")
	}
	data, err := io.ReadAll(io.LimitReader(file, contentStorageKeyMaxFile+1))
	if err != nil || len(data) > contentStorageKeyMaxFile {
		return nil, errors.New("NL_CONTENT_STORAGE_KEY_FILE is malformed or oversized")
	}
	encoded := strings.TrimSuffix(string(data), "\n")
	if strings.ContainsAny(encoded, "\r\n \t") || len(encoded) != contentStorageKeyBytes*2 {
		return nil, errors.New("NL_CONTENT_STORAGE_KEY_FILE must contain exactly 32 bytes encoded as hex")
	}
	key, err := hex.DecodeString(encoded)
	if err != nil || len(key) != contentStorageKeyBytes {
		return nil, errors.New("NL_CONTENT_STORAGE_KEY_FILE must contain exactly 32 bytes encoded as hex")
	}
	return key, nil
}

func clearContentStorageKey(key []byte) {
	for i := range key {
		key[i] = 0
	}
}
