package assurance

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

// RestoreOptions defines the trust boundary for a restore. ExpectedTenantID
// is mandatory; ExpectedSchemaMarkers and Minimum* fields prevent an older or
// incompatible envelope from silently becoming the new active database.
type RestoreOptions struct {
	ExpectedTenantID       string
	SigningKey             SigningKey
	ExpectedSchemaMarkers  map[string]int
	CheckpointPublicKey    ed25519.PublicKey
	CheckpointID           string
	MinimumCreatedAt       time.Time
	MinimumAuditSequence   int64
	ExpectedDatabaseSHA256 string
	RequireFenceHighWater  bool
}

type RestoreResult struct {
	Manifest       BackupManifest
	DatabasePath   string
	DatabaseSHA256 string
	Ready          bool
}

// Restore verifies an envelope completely before installing it at the
// destination. The destination database and its SQLite sidecars must not
// exist. A failed verification never creates the destination.
func RestoreBackup(ctx context.Context, envelopeDirectory, destination string, opts RestoreOptions) (RestoreResult, error) {
	if opts.ExpectedTenantID == "" {
		return RestoreResult{}, fmt.Errorf("%w: expected tenant is required", ErrBackupWrongTenant)
	}
	if _, _, err := signingScheme(opts.SigningKey, true); err != nil {
		return RestoreResult{}, fmt.Errorf("%w: %v", ErrBackupWrongKey, err)
	}
	if err := requireEmptyRestoreDestination(destination); err != nil {
		return RestoreResult{}, err
	}
	databasePath, manifestPath, err := envelopePaths(envelopeDirectory)
	if err != nil {
		return RestoreResult{}, err
	}
	signed, err := readSignedManifest(manifestPath)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := verifySignedManifest(signed, opts.SigningKey); err != nil {
		return RestoreResult{}, err
	}
	manifest := signed.Manifest
	if !manifest.Complete || manifest.FormatVersion != backupFormatVersion || manifest.DatabaseFile != backupDatabaseName {
		return RestoreResult{}, fmt.Errorf("%w: manifest completion or format marker is invalid", ErrBackupIncomplete)
	}
	if manifest.TenantID != opts.ExpectedTenantID {
		return RestoreResult{}, fmt.Errorf("%w: manifest=%q expected=%q", ErrBackupWrongTenant, manifest.TenantID, opts.ExpectedTenantID)
	}
	if opts.RequireFenceHighWater {
		if err := validateFenceHighWater(manifest.FenceHighWater, manifest.TenantID); err != nil {
			return RestoreResult{}, err
		}
	}
	if !opts.MinimumCreatedAt.IsZero() {
		created, parseErr := time.Parse(backupTimeFormat, manifest.CreatedAt)
		if parseErr != nil || created.Before(opts.MinimumCreatedAt.UTC()) {
			return RestoreResult{}, fmt.Errorf("%w: backup created at %q", ErrBackupRollback, manifest.CreatedAt)
		}
	}
	if opts.MinimumAuditSequence > 0 && manifest.Audit.LastSequence < opts.MinimumAuditSequence {
		return RestoreResult{}, fmt.Errorf("%w: audit sequence %d is below minimum %d", ErrBackupRollback, manifest.Audit.LastSequence, opts.MinimumAuditSequence)
	}
	if opts.ExpectedDatabaseSHA256 != "" && !strings.EqualFold(opts.ExpectedDatabaseSHA256, manifest.DatabaseSHA256) {
		return RestoreResult{}, fmt.Errorf("%w: database digest is not the expected rollback target", ErrBackupRollback)
	}
	if err := verifyEnvelopeFiles(envelopeDirectory, databasePath, manifest); err != nil {
		return RestoreResult{}, err
	}
	if err := verifyBackupDatabase(ctx, databasePath, manifest, opts); err != nil {
		return RestoreResult{}, err
	}

	if err := requireEmptyRestoreDestination(destination); err != nil {
		return RestoreResult{}, err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return RestoreResult{}, fmt.Errorf("assurance: create restore parent: %w", err)
	}
	tmp, err := os.CreateTemp(parent, ".sqlite-restore-*")
	if err != nil {
		return RestoreResult{}, fmt.Errorf("assurance: create restore temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := copyFileSync(tmp, databasePath); err != nil {
		_ = tmp.Close()
		return RestoreResult{}, err
	}
	if err := tmp.Close(); err != nil {
		return RestoreResult{}, fmt.Errorf("assurance: close restored database: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return RestoreResult{}, fmt.Errorf("assurance: protect restored database: %w", err)
	}
	if err := requireEmptyRestoreDestination(destination); err != nil {
		return RestoreResult{}, err
	}
	if err := os.Rename(tmpPath, destination); err != nil {
		return RestoreResult{}, fmt.Errorf("assurance: install restored database: %w", err)
	}
	if err := reopenAndCheck(ctx, destination); err != nil {
		_ = os.Remove(destination)
		return RestoreResult{}, fmt.Errorf("assurance: restored database is not ready: %w", err)
	}
	return RestoreResult{Manifest: manifest, DatabasePath: destination, DatabaseSHA256: manifest.DatabaseSHA256, Ready: true}, nil
}

func (m *BackupManager) Restore(ctx context.Context, envelopeDirectory, destination string, opts RestoreOptions) (RestoreResult, error) {
	return RestoreBackup(ctx, envelopeDirectory, destination, opts)
}

func envelopePaths(directory string) (string, string, error) {
	if directory == "" {
		return "", "", fmt.Errorf("%w: envelope directory is required", ErrBackupIncomplete)
	}
	info, err := os.Stat(directory)
	if err != nil {
		return "", "", fmt.Errorf("%w: envelope directory: %v", ErrBackupIncomplete, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("%w: envelope is not a directory", ErrBackupIncomplete)
	}
	databasePath := filepath.Join(directory, backupDatabaseName)
	manifestPath := filepath.Join(directory, backupManifestName)
	for _, path := range []string{databasePath, manifestPath} {
		entry, err := os.Lstat(path)
		if err != nil {
			return "", "", fmt.Errorf("%w: missing %s: %v", ErrBackupIncomplete, filepath.Base(path), err)
		}
		if entry.Mode()&os.ModeSymlink != 0 || !entry.Mode().IsRegular() {
			return "", "", fmt.Errorf("%w: non-regular envelope member %s", ErrBackupTampered, filepath.Base(path))
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", "", err
	}
	for _, entry := range entries {
		if entry.Name() != backupDatabaseName && entry.Name() != backupManifestName {
			return "", "", fmt.Errorf("%w: unexpected envelope member %s", ErrBackupTampered, entry.Name())
		}
	}
	return databasePath, manifestPath, nil
}

func readSignedManifest(path string) (signedBackupManifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return signedBackupManifest{}, fmt.Errorf("%w: open manifest: %v", ErrBackupIncomplete, err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 8<<20+1))
	if err != nil {
		return signedBackupManifest{}, fmt.Errorf("%w: read manifest: %v", ErrBackupIncomplete, err)
	}
	if len(raw) > 8<<20 {
		return signedBackupManifest{}, fmt.Errorf("%w: manifest is too large", ErrBackupTampered)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var signed signedBackupManifest
	if err := decoder.Decode(&signed); err != nil {
		return signedBackupManifest{}, fmt.Errorf("%w: decode manifest: %v", ErrBackupTampered, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return signedBackupManifest{}, fmt.Errorf("%w: manifest has trailing content", ErrBackupTampered)
	}
	return signed, nil
}

func verifySignedManifest(signed signedBackupManifest, key SigningKey) error {
	raw, err := json.Marshal(signed.Manifest)
	if err != nil {
		return fmt.Errorf("%w: canonical manifest: %v", ErrBackupTampered, err)
	}
	if !validSHA256(signed.ManifestHash) || !hmac.Equal([]byte(strings.ToLower(signed.ManifestHash)), []byte(sha256Hex(raw))) {
		return fmt.Errorf("%w: manifest hash mismatch", ErrBackupTampered)
	}
	scheme, publicKey, err := signingScheme(key, true)
	if err != nil || scheme != signed.SignatureScheme {
		return fmt.Errorf("%w: signature scheme mismatch", ErrBackupWrongKey)
	}
	signature, err := hex.DecodeString(signed.SignatureHex)
	if err != nil {
		return fmt.Errorf("%w: malformed signature", ErrBackupTampered)
	}
	message := append([]byte(backupDomain), raw...)
	valid := false
	switch scheme {
	case "hmac-sha256":
		mac := hmac.New(sha256.New, key.HMAC)
		_, _ = mac.Write(message)
		valid = hmac.Equal(mac.Sum(nil), signature)
	case "ed25519":
		valid = ed25519.Verify(publicKey, message, signature)
	}
	if !valid {
		return fmt.Errorf("%w: signature does not verify", ErrBackupWrongKey)
	}
	return nil
}

func verifyEnvelopeFiles(directory, databasePath string, manifest BackupManifest) error {
	stat, err := os.Stat(databasePath)
	if err != nil {
		return fmt.Errorf("%w: database file: %v", ErrBackupIncomplete, err)
	}
	if stat.Size() != manifest.DatabaseBytes {
		return fmt.Errorf("%w: database size %d, want %d", ErrBackupTampered, stat.Size(), manifest.DatabaseBytes)
	}
	hash, err := fileSHA256(databasePath)
	if err != nil {
		return err
	}
	if !strings.EqualFold(hash, manifest.DatabaseSHA256) {
		return fmt.Errorf("%w: database digest mismatch", ErrBackupTampered)
	}
	return nil
}

func verifyBackupDatabase(ctx context.Context, path string, manifest BackupManifest, opts RestoreOptions) error {
	db, err := sql.Open("sqlite", readOnlySQLiteDSN(path))
	if err != nil {
		return fmt.Errorf("%w: open database: %v", ErrBackupTampered, err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || !strings.EqualFold(integrity, "ok") {
		return fmt.Errorf("%w: integrity_check=%q err=%v", ErrBackupTampered, integrity, err)
	}
	foreign, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("%w: foreign_key_check: %v", ErrBackupTampered, err)
	}
	if foreign.Next() {
		_ = foreign.Close()
		return fmt.Errorf("%w: foreign key violation", ErrBackupTampered)
	}
	if err := foreign.Err(); err != nil {
		_ = foreign.Close()
		return fmt.Errorf("%w: foreign_key_check: %v", ErrBackupTampered, err)
	}
	_ = foreign.Close()
	actualOpts := BackupOptions{TenantID: manifest.TenantID, ExpectedSchemaMarkers: opts.ExpectedSchemaMarkers, CheckpointPublicKey: opts.CheckpointPublicKey, CheckpointID: opts.CheckpointID, FenceHighWater: manifest.FenceHighWater}
	if actualOpts.ExpectedSchemaMarkers == nil {
		actualOpts.ExpectedSchemaMarkers = manifest.SchemaMarkers
	}
	actual, err := collectBackupManifest(ctx, db, path, actualOpts)
	if err != nil {
		return err
	}
	if err := compareManifestState(actual, manifest); err != nil {
		return err
	}
	return nil
}

func readOnlySQLiteDSN(path string) string {
	return "file:" + url.PathEscape(path) + "?mode=ro&_pragma=busy_timeout(5000)"
}

func compareManifestState(actual, expected BackupManifest) error {
	if actual.TenantID != expected.TenantID || actual.DatabaseSHA256 != expected.DatabaseSHA256 || actual.DatabaseBytes != expected.DatabaseBytes {
		return fmt.Errorf("%w: database identity changed", ErrBackupTampered)
	}
	if !reflect.DeepEqual(actual.SchemaMarkers, expected.SchemaMarkers) || !reflect.DeepEqual(actual.TenantRowCounts, expected.TenantRowCounts) || !reflect.DeepEqual(actual.TableRowCounts, expected.TableRowCounts) || !reflect.DeepEqual(actual.EvidenceManifestHashes, expected.EvidenceManifestHashes) {
		return fmt.Errorf("%w: schema or row-count evidence changed", ErrBackupTampered)
	}
	if !reflect.DeepEqual(actual.Audit, expected.Audit) {
		return fmt.Errorf("%w: audit chain or checkpoint evidence changed", ErrBackupTampered)
	}
	return nil
}

func requireEmptyRestoreDestination(destination string) error {
	if destination == "" {
		return errors.New("assurance: restore destination is required")
	}
	for _, path := range []string{destination, destination + "-wal", destination + "-shm", destination + "-journal"} {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("%w: %s", ErrBackupDestinationNotEmpty, path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: inspect %s: %v", ErrBackupDestinationNotEmpty, path, err)
		}
	}
	return nil
}

func copyFileSync(destination *os.File, sourcePath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("assurance: open source for restore: %w", err)
	}
	defer func() { _ = source.Close() }()
	if _, err := io.Copy(destination, source); err != nil {
		return fmt.Errorf("assurance: copy database for restore: %w", err)
	}
	if err := destination.Sync(); err != nil {
		return fmt.Errorf("assurance: sync restored database: %w", err)
	}
	return nil
}

func reopenAndCheck(ctx context.Context, path string) error {
	db, err := sqlitedb.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return err
	}
	if !strings.EqualFold(integrity, "ok") {
		return fmt.Errorf("integrity_check=%q", integrity)
	}
	return nil
}
