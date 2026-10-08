package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testBackupSecretID = "https://example.vault.azure.net/secrets/backup-signing-key/0123456789abcdef0123456789abcdef"

func TestProvisionBackupKeyCreatesPrivateOwnedMaterializationAndCleansIt(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	called := false
	gotID := ""
	provisioned, err := provisionBackupSigningKey(context.Background(), testBackupSecretID, func(_ context.Context, id string) ([]byte, error) {
		called, gotID = true, id
		return []byte(strings.ToUpper(hexEncode(private))), nil
	}, root)
	if err != nil {
		t.Fatal(err)
	}
	if !called || gotID != testBackupSecretID {
		t.Fatalf("secret request called=%v id=%q", called, gotID)
	}
	dirInfo, err := os.Lstat(provisioned.root)
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("private root info=%v err=%v", dirInfo, err)
	}
	fileInfo, err := os.Lstat(provisioned.keyPath)
	if err != nil || !fileInfo.Mode().IsRegular() || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("key file info=%v err=%v", fileInfo, err)
	}
	key, err := loadBackupSigningKey(provisioned.keyPath)
	if err != nil || !equalBytes(key, private) {
		t.Fatalf("loader read-back key match=%v err=%v", equalBytes(key, private), err)
	}
	provisioned.cleanup()
	if _, err := os.Lstat(provisioned.keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned key remains: %v", err)
	}
	if _, err := os.Lstat(provisioned.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned root remains: %v", err)
	}
}

func TestProvisionBackupKeyAcceptsSingleTerminalLineFeed(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	provisioned, err := provisionBackupSigningKey(context.Background(), testBackupSecretID, func(context.Context, string) ([]byte, error) {
		return []byte(hexEncode(private) + "\n"), nil
	}, root)
	if err != nil {
		t.Fatal(err)
	}
	defer provisioned.cleanup()
	loaded, err := loadBackupSigningKey(provisioned.keyPath)
	if err != nil || !equalBytes(loaded, private) {
		t.Fatalf("provisioned key validation failed: equal=%v err=%v", equalBytes(loaded, private), err)
	}
}

func TestProvisionBackupKeyRejectsCRMultipleLineFeedsAndWhitespace(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyHex := hexEncode(private)
	for name, value := range map[string]string{
		"carriage-return":     keyHex + "\r",
		"crlf":                keyHex + "\r\n",
		"multiple-line-feeds": keyHex + "\n\n",
		"leading-space":       " " + keyHex,
		"trailing-space":      keyHex + " ",
		"trailing-tab":        keyHex + "\t",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			secret := []byte(value)
			provisioned, provisionErr := provisionBackupSigningKey(context.Background(), testBackupSecretID, func(context.Context, string) ([]byte, error) {
				return append([]byte(nil), secret...), nil
			}, root)
			if provisioned != nil || provisionErr == nil || strings.Contains(provisionErr.Error(), keyHex) {
				t.Fatalf("malformed key returned materialization=%v err=%v", provisioned != nil, provisionErr)
			}
			entries, readErr := os.ReadDir(root)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("malformed key left artifacts: entries=%v err=%v", entries, readErr)
			}
		})
	}
}

func TestBackupSecretReferenceRequiresCanonicalPinnedVersion(t *testing.T) {
	for _, value := range []string{
		"https://example.vault.azure.net/secrets/key",
		"https://example.vault.azure.net/secrets/key/latest",
		"https://example.vault.azure.net:443/secrets/key/0123456789abcdef0123456789abcdef",
		"https://user@example.vault.azure.net/secrets/key/0123456789abcdef0123456789abcdef",
		"https://example.vault.azure.net/secrets/key/0123456789abcdef0123456789abcdef?x=1",
		"https://example.vault.azure.net/other/key/0123456789abcdef0123456789abcdef",
		"https://example.vault.azure.net/secrets/key/0123456789ABCDEF0123456789ABCDEF",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseBackupKeySecretID(value); err == nil {
				t.Fatalf("accepted invalid ID %q", value)
			}
		})
	}
	for _, endpoint := range []string{"https://example.vault.azure.net", "https://example.vault.usgovcloudapi.net", "https://example.vault.azure.cn"} {
		id := strings.TrimSuffix(endpoint, "/") + "/secrets/backup-signing-key/0123456789abcdef0123456789abcdef"
		if _, err := parseBackupKeySecretID(id); err != nil {
			t.Errorf("canonical pinned ID %q rejected: %v", id, err)
		}
	}
}

func TestProvisionBackupKeyDoesNotExposeFetchErrorsOrWriteInvalidKey(t *testing.T) {
	root := t.TempDir()
	secret := "do-not-disclose-secret-bytes"
	path, err := provisionBackupSigningKey(context.Background(), testBackupSecretID, func(context.Context, string) ([]byte, error) {
		return []byte(secret), errors.New(secret)
	}, root)
	if err == nil || strings.Contains(err.Error(), secret) || path != nil {
		t.Fatalf("fetch failure leaked or returned materialization path=%v err=%v", path, err)
	}
	path, err = provisionBackupSigningKey(context.Background(), testBackupSecretID, func(context.Context, string) ([]byte, error) {
		return []byte(strings.Repeat("0", ed25519.PrivateKeySize*2)), nil
	}, root)
	if err == nil || path != nil || strings.Contains(err.Error(), strings.Repeat("0", 20)) {
		t.Fatalf("inconsistent key accepted or leaked: path=%v err=%v", path, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed provisioning left artifacts: entries=%v err=%v", entries, err)
	}
}

func TestProvisionBackupKeyDiscardsResultWhenReaderCancelsContext(t *testing.T) {
	root := t.TempDir()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	got, err := provisionBackupSigningKey(ctx, testBackupSecretID, func(context.Context, string) ([]byte, error) {
		cancel()
		return []byte(hexEncode(private)), nil
	}, root)
	if got != nil || err == nil || strings.Contains(err.Error(), hexEncode(private)) {
		t.Fatalf("canceled fetch returned materialization=%v err=%v", got != nil, err)
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("canceled fetch left owned files: entries=%v err=%v", entries, readErr)
	}
}

func TestProvisionBackupKeyHonorsCancellationAndPreservesExistingCandidate(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	if got, err := provisionBackupSigningKey(ctx, testBackupSecretID, func(context.Context, string) ([]byte, error) {
		calls++
		return nil, nil
	}, root); err == nil || got != nil || calls != 0 {
		t.Fatalf("canceled provision path=%v calls=%d err=%v", got, calls, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("canceled provisioning left artifacts: %v %v", entries, err)
	}

	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	reader := func(context.Context, string) ([]byte, error) { return []byte(hexEncode(private)), nil }
	provisioned, err := provisionBackupSigningKey(context.Background(), testBackupSecretID, reader, root)
	if err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(provisioned.root, "orphan-backup-staging")
	if err := os.WriteFile(orphan, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	provisioned.cleanup()
	if data, err := os.ReadFile(orphan); err != nil || string(data) != "preserve" {
		t.Fatalf("orphan staging was removed or changed: %q %v", data, err)
	}
	if _, err := os.Stat(provisioned.root); err != nil {
		t.Fatalf("nonempty owned root should be retained: %v", err)
	}
}
