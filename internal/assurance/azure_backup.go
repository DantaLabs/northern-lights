package assurance

// This transport is deliberately not wired into server startup or backup
// scheduling. A local drain is not a production-wide coordinated drain.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
)

const maxBlobManifestBytes = 8 << 20

// AzureBackupConfig binds a private, pre-existing container to one tenant and
// environment. The caller supplies an independent minimum audit high-water
// mark at restore time; Blob metadata is evidence, not a rollback authority.
type AzureBackupConfig struct {
	ServiceURL, ContainerName, TenantID, EnvironmentDigest string
	Timeout                                                time.Duration
	MaxDatabaseBytes                                       int64
	ClientOptions                                          *azblob.ClientOptions
}

type AzureBackupTransport struct {
	client                               *azblob.Client
	container, tenantDigest, environment string
	timeout                              time.Duration
	maxDatabaseBytes                     int64
}

type BlobBackupSeal struct {
	EnvelopeID, ManifestSHA256, DatabaseSHA256 string
	ManifestETag, DatabaseETag                 string
	AuditHighWater                             int64
}

// NewAzureBackupTransport obtains DefaultAzureCredential without contacting Azure.
// No container is created, and public access is never requested.
func NewAzureBackupTransport(ctx context.Context, cfg AzureBackupConfig) (*AzureBackupTransport, error) {
	if ctx == nil {
		return nil, errors.New("assurance: Azure backup context required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateAzureBackupConfig(cfg, true); err != nil {
		return nil, err
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}
	options := cfg.ClientOptions
	if options == nil {
		options = &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}}
	}
	client, err := azblob.NewClient(cfg.ServiceURL, cred, options)
	if err != nil {
		return nil, err
	}
	return newAzureBackupTransport(client, cfg)
}

// NewAzureBackupTransportWithClient constructs the same validated Blob
// transport around a caller-owned client. The caller is responsible for the
// client's authentication and transport policy. Production startup should use
// NewAzureBackupTransport to retain DefaultAzureCredential.
func NewAzureBackupTransportWithClient(ctx context.Context, cfg AzureBackupConfig, client *azblob.Client) (*AzureBackupTransport, error) {
	if ctx == nil {
		return nil, errors.New("assurance: Azure backup context required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateAzureBackupConfig(cfg, true); err != nil {
		return nil, err
	}
	return newAzureBackupTransport(client, cfg)
}

// newAzureBackupTransport is the isolated fake-HTTP testing seam.
func newAzureBackupTransport(client *azblob.Client, cfg AzureBackupConfig) (*AzureBackupTransport, error) {
	if client == nil {
		return nil, errors.New("assurance: Azure Blob client required")
	}
	if err := validateAzureBackupConfig(cfg, false); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(cfg.TenantID))
	return &AzureBackupTransport{client: client, container: cfg.ContainerName, tenantDigest: hex.EncodeToString(sum[:]), environment: cfg.EnvironmentDigest, timeout: cfg.Timeout, maxDatabaseBytes: cfg.MaxDatabaseBytes}, nil
}
func validateAzureBackupConfig(cfg AzureBackupConfig, service bool) error {
	if service && cfg.ServiceURL == "" {
		return errors.New("assurance: Azure account URL required")
	}
	if !backupSegment(cfg.ContainerName) || !backupSegment(cfg.EnvironmentDigest) || strings.TrimSpace(cfg.TenantID) == "" || cfg.Timeout <= 0 || cfg.MaxDatabaseBytes <= 0 {
		return errors.New("assurance: invalid Azure backup binding or limits")
	}
	return nil
}

// ValidatePrivateContainer reads Azure's authoritative access policy. Azure
// omits x-ms-blob-public-access for a private container; any reported value or
// an unreadable policy fails closed.
func (a *AzureBackupTransport) ValidatePrivateContainer(ctx context.Context) error {
	if a == nil || a.client == nil || ctx == nil {
		return errors.New("assurance: Azure backup transport/context required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	properties, err := a.client.ServiceClient().NewContainerClient(a.container).GetProperties(requestCtx, nil)
	if err != nil {
		return fmt.Errorf("assurance: unable to verify Azure backup container access policy: %w", err)
	}
	if properties.BlobPublicAccess != nil {
		return fmt.Errorf("assurance: Azure backup container is not private (public access %q)", string(*properties.BlobPublicAccess))
	}
	return nil
}

func backupSegment(s string) bool {
	if s == "" || s == "." || s == ".." || strings.TrimSpace(s) != s || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-", r) {
			return false
		}
	}
	return true
}
func (a *AzureBackupTransport) objectName(id, member string) string {
	return "env/" + a.environment + "/tenant/" + a.tenantDigest + "/backups/" + id + "/" + member
}
func (a *AzureBackupTransport) check(ctx context.Context, tenant, id string) error {
	if a == nil || a.client == nil || ctx == nil {
		return errors.New("assurance: Azure backup transport/context required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(tenant))
	if tenant == "" || hex.EncodeToString(sum[:]) != a.tenantDigest {
		return ErrBackupWrongTenant
	}
	if len(id) != 32 {
		return fmt.Errorf("%w: invalid envelope ID", ErrBackupTampered)
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return fmt.Errorf("%w: invalid envelope ID", ErrBackupTampered)
		}
	}
	return nil
}
func (a *AzureBackupTransport) metadata(m BackupManifest, manifestHash string) map[string]*string {
	values := map[string]string{"nltenant": a.tenantDigest, "nlenvironment": a.environment, "nlenvelope": m.EnvelopeID, "nlmanifestsha256": manifestHash, "nldatabasesha256": m.DatabaseSHA256, "nlaudithighwater": fmt.Sprint(m.Audit.LastSequence)}
	result := make(map[string]*string, len(values))
	for k, v := range values {
		value := v
		result[k] = &value
	}
	return result
}
func equalMetadata(got, want map[string]*string) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range got {
		other, ok := want[strings.ToLower(k)]
		if !ok || other == nil || v == nil || *other != *v {
			return false
		}
	}
	return true
}
func (a *AzureBackupTransport) read(ctx context.Context, name string, max int64, expected map[string]*string, etag *azcore.ETag, sink io.Writer) (string, error) {
	response, err := a.client.DownloadStream(ctx, a.container, name, nil)
	if err != nil {
		return "", err
	}
	if response.Body == nil {
		return "", ErrBackupIncomplete
	}
	defer func() { _ = response.Body.Close() }()
	if response.ETag == nil || strings.TrimSpace(string(*response.ETag)) == "" || (etag != nil && *etag != *response.ETag) || !equalMetadata(response.Metadata, expected) {
		return "", fmt.Errorf("%w: Blob ETag or binding metadata mismatch", ErrBackupTampered)
	}
	h := sha256.New()
	count, err := io.Copy(io.MultiWriter(sink, h), io.LimitReader(response.Body, max+1))
	if err != nil {
		return "", err
	}
	if count > max {
		return "", ErrBackupTampered
	}
	if response.ContentLength == nil || *response.ContentLength != count {
		return "", ErrBackupTampered
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func (a *AzureBackupTransport) create(ctx context.Context, name string, file *os.File, size int64, digest string, meta map[string]*string) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	any := azcore.ETagAny
	response, err := a.client.UploadStream(ctx, a.container, name, io.LimitReader(file, size), &azblob.UploadStreamOptions{Concurrency: 1, Metadata: meta, AccessConditions: &azblob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: &any}}})
	if err != nil {
		return "", fmt.Errorf("assurance: create-only Blob upload %s: %w", name, err)
	}
	if response.ETag == nil || strings.TrimSpace(string(*response.ETag)) == "" {
		return "", ErrBackupTampered
	}
	got, err := a.read(ctx, name, size, meta, response.ETag, io.Discard)
	if err != nil {
		return "", fmt.Errorf("assurance: verify upload %s: %w", name, err)
	}
	if got != digest {
		return "", fmt.Errorf("%w: upload digest mismatch for %s", ErrBackupTampered, name)
	}
	return string(*response.ETag), nil
}

// UploadEnvelope never adopts an existing object, even if its bytes match.
// A failure after database creation intentionally leaves an orphan: no delete,
// overwrite, retry-on-conflict or implicit promotion is safe.
func (a *AzureBackupTransport) UploadEnvelope(ctx context.Context, envelopeDirectory string, opts RestoreOptions) (BlobBackupSeal, error) {
	if a == nil || ctx == nil {
		return BlobBackupSeal{}, errors.New("assurance: transport/context required")
	}
	dbPath, manifestPath, err := envelopePaths(envelopeDirectory)
	if err != nil {
		return BlobBackupSeal{}, err
	}
	signed, err := readSignedManifest(manifestPath)
	if err != nil {
		return BlobBackupSeal{}, err
	}
	m := signed.Manifest
	if err := a.check(ctx, opts.ExpectedTenantID, m.EnvelopeID); err != nil {
		return BlobBackupSeal{}, err
	}
	if err := verifySignedManifest(signed, opts.SigningKey); err != nil {
		return BlobBackupSeal{}, err
	}
	if !m.Complete || m.FormatVersion != backupFormatVersion || m.DatabaseFile != backupDatabaseName {
		return BlobBackupSeal{}, ErrBackupIncomplete
	}
	if m.TenantID != opts.ExpectedTenantID {
		return BlobBackupSeal{}, ErrBackupWrongTenant
	}
	if m.DatabaseBytes <= 0 || m.DatabaseBytes > a.maxDatabaseBytes {
		return BlobBackupSeal{}, ErrBackupTampered
	}
	if err := verifyEnvelopeFiles(envelopeDirectory, dbPath, m); err != nil {
		return BlobBackupSeal{}, err
	}
	if err := verifyBackupDatabase(ctx, dbPath, m, opts); err != nil {
		return BlobBackupSeal{}, err
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return BlobBackupSeal{}, err
	}
	if len(raw) > maxBlobManifestBytes {
		return BlobBackupSeal{}, ErrBackupTampered
	}
	manifestSum := sha256Hex(raw)
	meta := a.metadata(m, manifestSum)
	work, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	db, err := os.Open(dbPath)
	if err != nil {
		return BlobBackupSeal{}, err
	}
	defer func() { _ = db.Close() }()
	databaseETag, err := a.create(work, a.objectName(m.EnvelopeID, backupDatabaseName), db, m.DatabaseBytes, m.DatabaseSHA256, meta)
	if err != nil {
		return BlobBackupSeal{}, err
	}
	manifest, err := os.Open(manifestPath)
	if err != nil {
		return BlobBackupSeal{}, err
	}
	defer func() { _ = manifest.Close() }()
	manifestETag, err := a.create(work, a.objectName(m.EnvelopeID, backupManifestName), manifest, int64(len(raw)), manifestSum, meta)
	if err != nil {
		return BlobBackupSeal{}, err
	}
	return BlobBackupSeal{EnvelopeID: m.EnvelopeID, ManifestSHA256: manifestSum, DatabaseSHA256: m.DatabaseSHA256, ManifestETag: manifestETag, DatabaseETag: databaseETag, AuditHighWater: m.Audit.LastSequence}, nil
}

// RestoreEnvelope selects an immutable envelope by ID, using caller-provided
// minimums (not Blob metadata) as rollback policy. No SQL touches Azure storage.
// The destination must be empty; download occurs in an empty private local temp
// directory and RestoreBackup re-verifies signature, hashes, schema, audit,
// checkpoint and row counts before returning Ready.
func (a *AzureBackupTransport) RestoreEnvelope(ctx context.Context, id, destination string, opts RestoreOptions) (RestoreResult, error) {
	if a == nil || ctx == nil {
		return RestoreResult{}, errors.New("assurance: transport/context required")
	}
	if err := a.check(ctx, opts.ExpectedTenantID, id); err != nil {
		return RestoreResult{}, err
	}
	if err := requireEmptyRestoreDestination(destination); err != nil {
		return RestoreResult{}, err
	}
	if opts.MinimumCreatedAt.IsZero() && opts.MinimumAuditSequence == 0 && opts.ExpectedDatabaseSHA256 == "" {
		return RestoreResult{}, fmt.Errorf("%w: independent rollback minimum required", ErrBackupRollback)
	}
	work, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	dir, err := os.MkdirTemp(filepath.Dir(destination), ".blob-envelope-*")
	if err != nil {
		return RestoreResult{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.Chmod(dir, 0o700); err != nil {
		return RestoreResult{}, err
	}
	manifestPath := filepath.Join(dir, backupManifestName)
	mf, err := os.OpenFile(manifestPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return RestoreResult{}, err
	}
	// First read metadata and bytes. Its self-declared hashes are not trusted
	// until the signature is verified against the configured key.
	response, err := a.client.DownloadStream(work, a.container, a.objectName(id, backupManifestName), nil)
	if err != nil {
		_ = mf.Close()
		return RestoreResult{}, err
	}
	if response.Body == nil {
		_ = mf.Close()
		return RestoreResult{}, ErrBackupIncomplete
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, maxBlobManifestBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || len(raw) > maxBlobManifestBytes || response.ContentLength == nil || *response.ContentLength != int64(len(raw)) || response.ETag == nil || strings.TrimSpace(string(*response.ETag)) == "" {
		_ = mf.Close()
		return RestoreResult{}, ErrBackupTampered
	}
	if _, err = mf.Write(raw); err != nil {
		_ = mf.Close()
		return RestoreResult{}, err
	}
	if err = mf.Sync(); err != nil {
		_ = mf.Close()
		return RestoreResult{}, err
	}
	if err = mf.Close(); err != nil {
		return RestoreResult{}, err
	}
	signed, err := readSignedManifest(manifestPath)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := verifySignedManifest(signed, opts.SigningKey); err != nil {
		return RestoreResult{}, err
	}
	m := signed.Manifest
	if m.EnvelopeID != id || m.TenantID != opts.ExpectedTenantID || m.DatabaseBytes <= 0 || m.DatabaseBytes > a.maxDatabaseBytes || !equalMetadata(response.Metadata, a.metadata(m, sha256Hex(raw))) {
		return RestoreResult{}, ErrBackupTampered
	}
	dbPath := filepath.Join(dir, backupDatabaseName)
	db, err := os.OpenFile(dbPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return RestoreResult{}, err
	}
	digest, err := a.read(work, a.objectName(id, backupDatabaseName), m.DatabaseBytes, a.metadata(m, sha256Hex(raw)), nil, db)
	if err != nil {
		_ = db.Close()
		return RestoreResult{}, err
	}
	if err = db.Sync(); err != nil {
		_ = db.Close()
		return RestoreResult{}, err
	}
	if err = db.Close(); err != nil {
		return RestoreResult{}, err
	}
	if digest != m.DatabaseSHA256 {
		return RestoreResult{}, ErrBackupTampered
	}
	return RestoreBackup(work, dir, destination, opts)
}
