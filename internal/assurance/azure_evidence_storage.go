package assurance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/google/uuid"
)

const defaultAzureEvidenceMaxBytes int64 = 64 << 20

// AzureEvidenceConfig binds independently sealed evidence artifacts to one
// private container, tenant and environment. EnvironmentDigest is a
// deployment-controlled identifier, not a credential.
type AzureEvidenceConfig struct {
	ServiceURL, ContainerName, TenantID, EnvironmentDigest string
	Timeout                                                time.Duration
	MaxArtifactBytes                                       int64
	ClientOptions                                          *azblob.ClientOptions
}

// AzureEvidenceStorage stores sealed evidence in a separate, opaque namespace.
// It deliberately does not share backup or transfer-fence object names.
type AzureEvidenceStorage struct {
	client                                     *azblob.Client
	container, tenantDigest, environmentDigest string
	timeout                                    time.Duration
	maxArtifactBytes                           int64
}

var _ EvidenceStorage = (*AzureEvidenceStorage)(nil)

// NewAzureEvidenceStorage creates the production adapter using
// DefaultAzureCredential. It never creates containers or changes access policy.
func NewAzureEvidenceStorage(ctx context.Context, cfg AzureEvidenceConfig) (*AzureEvidenceStorage, error) {
	if ctx == nil {
		return nil, errors.New("assurance: Azure evidence context required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateAzureEvidenceConfig(cfg, true); err != nil {
		return nil, err
	}
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("assurance: Azure evidence credential: %w", err)
	}
	options := cfg.ClientOptions
	if options == nil {
		options = &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}}
	}
	client, err := azblob.NewClient(cfg.ServiceURL, credential, options)
	if err != nil {
		return nil, fmt.Errorf("assurance: Azure evidence client: %w", err)
	}
	return newAzureEvidenceStorage(client, cfg)
}

// NewAzureEvidenceStorageWithClient is a test/integration seam for caller-owned
// clients. Production startup should use NewAzureEvidenceStorage.
func NewAzureEvidenceStorageWithClient(ctx context.Context, cfg AzureEvidenceConfig, client *azblob.Client) (*AzureEvidenceStorage, error) {
	if ctx == nil {
		return nil, errors.New("assurance: Azure evidence context required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateAzureEvidenceConfig(cfg, false); err != nil {
		return nil, err
	}
	return newAzureEvidenceStorage(client, cfg)
}

func newAzureEvidenceStorage(client *azblob.Client, cfg AzureEvidenceConfig) (*AzureEvidenceStorage, error) {
	if client == nil {
		return nil, errors.New("assurance: Azure evidence Blob client required")
	}
	if err := validateAzureEvidenceConfig(cfg, false); err != nil {
		return nil, err
	}
	if cfg.MaxArtifactBytes == 0 {
		cfg.MaxArtifactBytes = defaultAzureEvidenceMaxBytes
	}
	tenantHash := sha256.Sum256([]byte(cfg.TenantID))
	environmentHash := sha256.Sum256([]byte(cfg.EnvironmentDigest))
	return &AzureEvidenceStorage{client: client, container: cfg.ContainerName, tenantDigest: hex.EncodeToString(tenantHash[:]), environmentDigest: hex.EncodeToString(environmentHash[:]), timeout: cfg.Timeout, maxArtifactBytes: cfg.MaxArtifactBytes}, nil
}

func validateAzureEvidenceConfig(cfg AzureEvidenceConfig, requireServiceURL bool) error {
	if (requireServiceURL && !validEvidenceAccountURL(cfg.ServiceURL)) || !validAzureBackupContainer(cfg.ContainerName) || !validEvidenceIdentifier(cfg.EnvironmentDigest) || strings.TrimSpace(cfg.TenantID) == "" || cfg.TenantID != strings.TrimSpace(cfg.TenantID) || cfg.Timeout <= 0 || cfg.MaxArtifactBytes < 0 || cfg.MaxArtifactBytes > defaultAzureEvidenceMaxBytes {
		return errors.New("assurance: invalid Azure evidence binding or limits")
	}
	return nil
}

func validEvidenceAccountURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "" || u.RawPath != "" || u.Host == "" || u.Port() != "" || u.Host != strings.ToLower(u.Host) || u.String() != raw {
		return false
	}
	host := u.Hostname()
	for _, suffix := range []string{".blob.core.windows.net", ".blob.core.usgovcloudapi.net", ".blob.core.chinacloudapi.cn"} {
		if strings.HasSuffix(host, suffix) {
			account := strings.TrimSuffix(host, suffix)
			if len(account) < 3 || len(account) > 24 {
				return false
			}
			for _, r := range account {
				if !validAzureEvidenceAccountRune(r) {
					return false
				}
			}
			return true
		}
	}
	return false
}

func validAzureBackupContainer(value string) bool {
	if len(value) < 3 || len(value) > 63 || value != strings.ToLower(value) || strings.HasPrefix(value, "-") || strings.HasSuffix(value, "-") || strings.Contains(value, "--") {
		return false
	}
	for _, r := range value {
		if !validAzureEvidenceContainerRune(r) {
			return false
		}
	}
	return true
}

func validEvidenceIdentifier(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if !validAzureEvidenceIdentifierRune(r) {
			return false
		}
	}
	return true
}

func validAzureEvidenceAccountRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
}

func validAzureEvidenceContainerRune(r rune) bool {
	return validAzureEvidenceAccountRune(r) || r == '-'
}

func validAzureEvidenceIdentifierRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-'
}

// ValidatePrivateContainer is a startup admission check. Azure's authoritative
// properties must be observable and must show no public-access policy.
func (s *AzureEvidenceStorage) ValidatePrivateContainer(ctx context.Context) error {
	if s == nil || s.client == nil || ctx == nil {
		return errors.New("assurance: Azure evidence storage/context required")
	}
	requestCtx, cancel, err := s.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	properties, err := s.client.ServiceClient().NewContainerClient(s.container).GetProperties(requestCtx, nil)
	if err != nil {
		return fmt.Errorf("assurance: unable to verify Azure evidence container access policy: %w", err)
	}
	if properties.BlobPublicAccess != nil {
		return errors.New("assurance: Azure evidence container must be private")
	}
	return nil
}

type azureEvidenceReference struct {
	ObjectID string
	SHA256   string
	ETagHash string
}

func (s *AzureEvidenceStorage) Put(ctx context.Context, name, mediaType string, data []byte) (string, error) {
	if s == nil || s.client == nil {
		return "", errors.New("assurance: Azure evidence storage unavailable")
	}
	if err := s.authorize(ctx, identity.PermissionEvidenceExport); err != nil {
		return "", err
	}
	if !validEvidenceArtifactName(name) || !validEvidenceMediaType(mediaType) || len(data) == 0 || int64(len(data)) > s.maxArtifactBytes {
		return "", errors.New("assurance: invalid or oversized evidence artifact")
	}
	objectID := uuid.NewString()
	objectName := s.objectName(objectID)
	digest := sha256.Sum256(data)
	knownObjectRef, err := encodeAzureEvidenceReference(s, azureEvidenceReference{ObjectID: objectID, SHA256: hex.EncodeToString(digest[:])})
	if err != nil {
		return "", err
	}
	requestCtx, cancel, err := s.operationContext(ctx)
	if err != nil {
		return "", err
	}
	defer cancel()
	if err := s.ValidatePrivateContainer(requestCtx); err != nil {
		return "", err
	}
	any := azcore.ETagAny
	created, err := s.client.UploadStream(requestCtx, s.container, objectName, bytes.NewReader(data), &azblob.UploadStreamOptions{Concurrency: 1, AccessConditions: &azblob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: &any}}})
	if err != nil {
		// The request may have reached Blob even when its response was lost.
		// Preserve its opaque object/hash reference for tenant-admin cleanup;
		// exporter cleanup must not adopt an unknown version.
		return knownObjectRef, fmt.Errorf("assurance: create-only Azure evidence upload outcome is unknown: %w", err)
	}
	if created.ETag == nil || strings.TrimSpace(string(*created.ETag)) == "" {
		return knownObjectRef, errors.New("assurance: Azure evidence upload returned no ETag")
	}
	ref := azureEvidenceReference{ObjectID: objectID, SHA256: hex.EncodeToString(digest[:]), ETagHash: hashETag(string(*created.ETag))}
	encoded, err := encodeAzureEvidenceReference(s, ref)
	if err != nil {
		return "", err
	}
	readback, err := s.readObject(requestCtx, ref)
	if err != nil || !bytes.Equal(readback, data) {
		if err == nil {
			err = errors.New("assurance: azure evidence read-back bytes mismatch")
		}
		// Preserve the exact object reference so the evidence lifecycle can
		// record a cleanup tombstone/reconciliation outcome. Do not perform a
		// hidden delete of an artifact whose readback was not verified.
		return encoded, fmt.Errorf("assurance: verify Azure evidence upload: %w", err)
	}
	return encoded, nil
}

func (s *AzureEvidenceStorage) Read(ctx context.Context, ref string) ([]byte, error) {
	if err := s.authorize(ctx, identity.PermissionEvidenceExport); err != nil {
		return nil, err
	}
	parsed, err := decodeAzureEvidenceReference(s, ref)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel, err := s.operationContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	if err := s.ValidatePrivateContainer(requestCtx); err != nil {
		return nil, err
	}
	return s.readObject(requestCtx, parsed)
}

func (s *AzureEvidenceStorage) Delete(ctx context.Context, ref string) error {
	if err := s.authorizeAny(ctx, identity.PermissionEvidenceExport, identity.PermissionTenantAdmin); err != nil {
		return err
	}
	parsed, err := decodeAzureEvidenceReference(s, ref)
	if err != nil {
		return err
	}
	if parsed.ETagHash == "" {
		principal, _ := identity.PrincipalFromContext(ctx)
		if !principal.HasPermission(identity.PermissionTenantAdmin) {
			return errors.New("assurance: resolving an unknown Azure evidence version requires tenant.admin")
		}
	}
	requestCtx, cancel, err := s.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	if err := s.ValidatePrivateContainer(requestCtx); err != nil {
		return err
	}
	// Normal exporter deletion is pinned to the create ETag recorded in the
	// reference. Tenant admins may resolve an unknown cleanup version only
	// after verifying its bounded body/hash and then condition the delete on
	// the exact ETag observed by that GET.
	var etag azcore.ETag
	if parsed.ETagHash == "" {
		etag, err = s.observeUnverifiedObject(requestCtx, parsed)
	} else {
		_, etag, err = s.readObjectWithETag(requestCtx, parsed)
	}
	if err != nil {
		return err
	}
	// The verified read returned the exact current ETag used by this conditional
	// delete; the opaque reference never authorizes an unconditional Blob delete.
	return s.deleteObject(requestCtx, s.objectName(parsed.ObjectID), etag)
}

func (s *AzureEvidenceStorage) observeUnverifiedObject(ctx context.Context, ref azureEvidenceReference) (azcore.ETag, error) {
	response, err := s.client.DownloadStream(ctx, s.container, s.objectName(ref.ObjectID), nil)
	if err != nil {
		return "", evidenceStorageError(err)
	}
	if response.Body == nil {
		return "", errors.New("assurance: Azure evidence cleanup response body unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.ETag == nil || strings.TrimSpace(string(*response.ETag)) == "" || response.ContentLength == nil || *response.ContentLength < 0 || *response.ContentLength > s.maxArtifactBytes {
		return "", errors.New("assurance: Azure evidence cleanup object ETag or size is unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, s.maxArtifactBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) != *response.ContentLength || int64(len(data)) > s.maxArtifactBytes {
		return "", errors.New("assurance: Azure evidence cleanup object length mismatch")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != ref.SHA256 {
		return "", errors.New("assurance: Azure evidence cleanup object SHA-256 mismatch")
	}
	return *response.ETag, nil
}

func (s *AzureEvidenceStorage) deleteObject(ctx context.Context, objectName string, etag azcore.ETag) error {
	_, err := s.client.ServiceClient().NewContainerClient(s.container).NewBlobClient(objectName).Delete(ctx, &blob.DeleteOptions{AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: &etag}}})
	return evidenceStorageError(err)
}

func (s *AzureEvidenceStorage) readObject(ctx context.Context, ref azureEvidenceReference) ([]byte, error) {
	data, _, err := s.readObjectWithETag(ctx, ref)
	return data, err
}

func (s *AzureEvidenceStorage) readObjectWithETag(ctx context.Context, ref azureEvidenceReference) ([]byte, azcore.ETag, error) {
	// A missing ETag binding remains unreadable to ordinary callers. The
	// separate admin cleanup resolver never returns content, only an observed
	// ETag for a digest-verified conditional deletion.
	if ref.ETagHash == "" {
		return nil, "", errors.New("assurance: Azure evidence version is unverified")
	}
	response, err := s.client.DownloadStream(ctx, s.container, s.objectName(ref.ObjectID), nil)
	if err != nil {
		return nil, "", evidenceStorageError(err)
	}
	if response.Body == nil {
		return nil, "", errors.New("assurance: Azure evidence response body unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.ETag == nil || strings.TrimSpace(string(*response.ETag)) == "" || (ref.ETagHash != "" && hashETag(string(*response.ETag)) != ref.ETagHash) || response.ContentLength == nil || *response.ContentLength < 0 || *response.ContentLength > s.maxArtifactBytes {
		return nil, "", errors.New("assurance: Azure evidence ETag or size binding mismatch")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, s.maxArtifactBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) != *response.ContentLength || int64(len(data)) > s.maxArtifactBytes {
		return nil, "", errors.New("assurance: Azure evidence length mismatch")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != ref.SHA256 {
		return nil, "", errors.New("assurance: Azure evidence SHA-256 mismatch")
	}
	return data, *response.ETag, nil
}

func (s *AzureEvidenceStorage) authorize(ctx context.Context, permission identity.Permission) error {
	if s == nil || ctx == nil {
		return errors.New("assurance: trusted evidence context required")
	}
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok || principal.TenantID == "" || principal.ObjectID == "" || sha256HexBytes([]byte(principal.TenantID)) != s.tenantDigest || !principal.HasPermission(permission) {
		return errors.New("assurance: trusted tenant authorization required for evidence storage")
	}
	return ctx.Err()
}

func (s *AzureEvidenceStorage) authorizeAny(ctx context.Context, permissions ...identity.Permission) error {
	if s == nil || ctx == nil {
		return errors.New("assurance: trusted evidence context required")
	}
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok || principal.TenantID == "" || principal.ObjectID == "" || sha256HexBytes([]byte(principal.TenantID)) != s.tenantDigest {
		return errors.New("assurance: trusted tenant authorization required for evidence storage")
	}
	for _, permission := range permissions {
		if principal.HasPermission(permission) {
			return ctx.Err()
		}
	}
	return errors.New("assurance: trusted tenant authorization required for evidence storage")
}

func (s *AzureEvidenceStorage) operationContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if ctx == nil {
		return nil, nil, errors.New("assurance: Azure evidence context required")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, s.timeout)
	return requestCtx, cancel, nil
}

func validEvidenceArtifactName(name string) bool {
	if name == "" || len(name) > 64 || strings.ContainsAny(name, "/\\\x00") || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if !validAzureEvidenceArtifactRune(r) {
			return false
		}
	}
	return true
}

func validAzureEvidenceArtifactRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_'
}

func validEvidenceMediaType(mediaType string) bool {
	return mediaType == "application/json" || mediaType == "text/csv"
}

func (s *AzureEvidenceStorage) objectName(objectID string) string {
	return "evidence/v1/" + s.tenantDigest + "/" + s.environmentDigest + "/" + objectID
}

func encodeAzureEvidenceReference(s *AzureEvidenceStorage, ref azureEvidenceReference) (string, error) {
	if _, err := uuid.Parse(ref.ObjectID); err != nil {
		return "", err
	}
	payload := strings.Join([]string{s.tenantDigest, s.environmentDigest, ref.ObjectID, ref.SHA256, ref.ETagHash}, ".")
	return "azev1." + base64.RawURLEncoding.EncodeToString([]byte(payload)), nil
}

func decodeAzureEvidenceReference(s *AzureEvidenceStorage, raw string) (azureEvidenceReference, error) {
	if len(raw) > 512 || !strings.HasPrefix(raw, "azev1.") {
		return azureEvidenceReference{}, errors.New("assurance: invalid Azure evidence reference")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, "azev1."))
	parts := strings.Split(string(payload), ".")
	if err != nil || len(parts) != 5 || parts[0] != s.tenantDigest || parts[1] != s.environmentDigest || len(parts[3]) != 64 || (parts[4] != "" && len(parts[4]) != 64) {
		return azureEvidenceReference{}, errors.New("assurance: Azure evidence reference binding mismatch")
	}
	id, err := uuid.Parse(parts[2])
	if err != nil || id.String() != parts[2] {
		return azureEvidenceReference{}, errors.New("assurance: invalid Azure evidence reference")
	}
	for _, digest := range parts[3:] {
		if digest == "" && parts[4] == "" {
			continue
		}
		if len(digest) != 64 {
			return azureEvidenceReference{}, errors.New("assurance: invalid Azure evidence digest reference")
		}
		if _, err := hex.DecodeString(digest); err != nil || strings.ToLower(digest) != digest {
			return azureEvidenceReference{}, errors.New("assurance: invalid Azure evidence digest reference")
		}
	}
	return azureEvidenceReference{ObjectID: parts[2], SHA256: parts[3], ETagHash: parts[4]}, nil
}

func hashETag(etag string) string {
	sum := sha256.Sum256([]byte(etag))
	return hex.EncodeToString(sum[:])
}

func sha256HexBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func evidenceStorageError(err error) error {
	if err == nil {
		return nil
	}
	var responseErr *azcore.ResponseError
	if errors.As(err, &responseErr) && responseErr.StatusCode == 404 {
		return ErrEvidenceNotFound
	}
	return err
}
