package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/dantalabs/northern-lights/internal/assurance"
)

// AzureFenceConfig identifies the private Blob namespace for one configured
// environment and tenant. ServiceURL is the account Blob endpoint, not a
// container URL.
type AzureFenceConfig struct {
	ServiceURL        string
	ContainerName     string
	TenantID          string
	EnvironmentDigest string
	Timeout           time.Duration
	MaxScanPages      int
	MaxScanObjects    int
	MaxScanBytes      int64
	ClientOptions     *azblob.ClientOptions
}

// AzureBlobFence is a create-only external Fence backed by Azure block blobs.
// It intentionally has no delete or overwrite operation.
type AzureBlobFence struct {
	client            *azblob.Client
	containerName     string
	tenantDigest      string
	environmentDigest string
	timeout           time.Duration
	maxScanPages      int
	maxScanObjects    int
	maxScanBytes      int64
	configured        bool
}

var _ Fence = (*AzureBlobFence)(nil)

// NewAzureBlobFence creates the configured production adapter using the
// process's DefaultAzureCredential. Constructing the client does not perform
// a cloud request; fence operations do, and are bounded by Timeout.
func NewAzureBlobFence(ctx context.Context, cfg AzureFenceConfig) (*AzureBlobFence, error) {
	if ctx == nil {
		return nil, errors.New("transfer: Azure fence context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateAzureFenceConfig(cfg, true); err != nil {
		return nil, err
	}
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("transfer: Azure fence credential: %w", err)
	}
	options := cfg.ClientOptions
	if options == nil {
		options = &azblob.ClientOptions{
			ClientOptions: azcore.ClientOptions{
				Retry: policy.RetryOptions{MaxRetries: -1},
			},
		}
	}
	client, err := azblob.NewClient(cfg.ServiceURL, credential, options)
	if err != nil {
		return nil, fmt.Errorf("transfer: Azure fence client: %w", err)
	}
	return newAzureBlobFence(client, cfg)
}

// NewAzureBlobFenceWithClient validates the same fence configuration around a
// caller-owned Blob client. The caller is responsible for the client's
// authentication and transport policy; production startup should use
// NewAzureBlobFence to retain DefaultAzureCredential.
func NewAzureBlobFenceWithClient(ctx context.Context, cfg AzureFenceConfig, client *azblob.Client) (*AzureBlobFence, error) {
	if ctx == nil {
		return nil, errors.New("transfer: Azure fence context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateAzureFenceConfig(cfg, true); err != nil {
		return nil, err
	}
	return newAzureBlobFence(client, cfg)
}

// newAzureBlobFence is the dependency-injection seam used by the local HTTP
// fake-server tests. Production callers should use NewAzureBlobFence.
func newAzureBlobFence(client *azblob.Client, cfg AzureFenceConfig) (*AzureBlobFence, error) {
	if client == nil {
		return nil, errors.New("transfer: Azure fence Blob client is required")
	}
	if err := validateAzureFenceConfig(cfg, false); err != nil {
		return nil, err
	}
	maxScanPages, maxScanObjects, maxScanBytes := azureFenceScanLimits(cfg)
	return &AzureBlobFence{
		client:            client,
		containerName:     cfg.ContainerName,
		tenantDigest:      sha256Hex([]byte(cfg.TenantID)),
		environmentDigest: cfg.EnvironmentDigest,
		timeout:           cfg.Timeout,
		maxScanPages:      maxScanPages,
		maxScanObjects:    maxScanObjects,
		maxScanBytes:      maxScanBytes,
		configured:        true,
	}, nil
}

const (
	defaultAzureFenceScanMaxPages   = 128
	defaultAzureFenceScanMaxObjects = 10000
	defaultAzureFenceScanMaxBytes   = 64 << 20
	maxAzureFenceETagLength         = 1024
)

func azureFenceScanLimits(cfg AzureFenceConfig) (int, int, int64) {
	pages, objects, bytes := cfg.MaxScanPages, cfg.MaxScanObjects, cfg.MaxScanBytes
	if pages == 0 {
		pages = defaultAzureFenceScanMaxPages
	}
	if objects == 0 {
		objects = defaultAzureFenceScanMaxObjects
	}
	if bytes == 0 {
		bytes = defaultAzureFenceScanMaxBytes
	}
	return pages, objects, bytes
}

func validateAzureFenceConfig(cfg AzureFenceConfig, requireServiceURL bool) error {
	if requireServiceURL && strings.TrimSpace(cfg.ServiceURL) == "" {
		return errors.New("transfer: Azure fence service URL is required")
	}
	if strings.TrimSpace(cfg.ContainerName) == "" || !validAzureFenceSegment(cfg.ContainerName) {
		return errors.New("transfer: Azure fence container name is invalid")
	}
	if strings.TrimSpace(cfg.TenantID) == "" {
		return errors.New("transfer: Azure fence tenant is required")
	}
	if strings.TrimSpace(cfg.EnvironmentDigest) == "" || !validAzureFenceSegment(cfg.EnvironmentDigest) {
		return errors.New("transfer: Azure fence environment digest is invalid")
	}
	if cfg.Timeout <= 0 {
		return errors.New("transfer: Azure fence timeout must be positive")
	}
	return nil
}

func validAzureFenceSegment(value string) bool {
	return value != "." && value != ".." && !strings.ContainsAny(value, `/\\`)
}

func (f *AzureBlobFence) Durable() bool {
	return f != nil && f.configured
}

func (f *AzureBlobFence) Identity(tenant string) (string, string, error) {
	if f == nil || !f.configured || tenant == "" || sha256Hex([]byte(tenant)) != f.tenantDigest {
		return "", "", errors.New("transfer: Azure fence tenant mismatch")
	}
	return f.environmentDigest, f.tenantDigest, nil
}

func (f *AzureBlobFence) CreateClaim(ctx context.Context, id string, body []byte) (string, error) {
	return f.create(ctx, id, body, "claim")
}

func (f *AzureBlobFence) CreateTerminal(ctx context.Context, id string, body []byte) (string, error) {
	return f.create(ctx, id, body, "terminal")
}

func (f *AzureBlobFence) VerifyClaim(ctx context.Context, id, digest string, expected ...[]byte) error {
	return f.verify(ctx, id, digest, "claim", expected...)
}

func (f *AzureBlobFence) VerifyTerminal(ctx context.Context, id, digest string, expected ...[]byte) error {
	return f.verify(ctx, id, digest, "terminal", expected...)
}

// CaptureHighWater obtains the storage service's Date on a bounded namespace
// listing. Call it while write admission is drained and sign it into the
// backup manifest; never synthesize the fence mark from the local clock.
func (f *AzureBlobFence) CaptureHighWater(ctx context.Context, tenant string) (assurance.BackupFenceHighWater, error) {
	if f == nil || !f.configured || f.client == nil || ctx == nil || tenant == "" || sha256Hex([]byte(tenant)) != f.tenantDigest {
		return assurance.BackupFenceHighWater{}, errors.New("transfer: high-water capture binding invalid")
	}
	work, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	prefix := "env/" + f.environmentDigest + "/tenant/" + f.tenantDigest + "/fences/"
	one := int32(1)
	pager := f.client.NewListBlobsFlatPager(f.containerName, &azblob.ListBlobsFlatOptions{Prefix: &prefix, MaxResults: &one})
	page, err := pager.NextPage(work)
	if err != nil {
		return assurance.BackupFenceHighWater{}, fmt.Errorf("transfer: capture storage high-water: %w", err)
	}
	if page.Date == nil || page.Date.IsZero() {
		return assurance.BackupFenceHighWater{}, errors.New("transfer: storage service Date missing")
	}
	return assurance.BackupFenceHighWater{TenantDigest: f.tenantDigest, EnvironmentDigest: f.environmentDigest, CapturedAt: page.Date.UTC().Format(time.RFC3339Nano), Authority: "azure_blob_last_modified"}, nil
}

// Scan inventories the complete configured fence namespace at a signed Blob
// Last-Modified high-water. Every listed object is re-read before it is
// returned, so the inventory is never based on listing metadata alone.
func (f *AzureBlobFence) Scan(ctx context.Context, req FenceScanRequest) ([]FenceObject, error) {
	if f == nil || !f.configured || f.client == nil {
		return nil, errors.New("transfer: Azure fence is not configured")
	}
	if ctx == nil {
		return nil, errors.New("transfer: Azure fence context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.TenantID == "" || req.TenantDigest != f.tenantDigest || sha256Hex([]byte(req.TenantID)) != f.tenantDigest || req.EnvironmentDigest != f.environmentDigest {
		return nil, errors.New("transfer: Azure fence scan binding mismatch")
	}
	if req.HighWater.TenantDigest != f.tenantDigest || req.HighWater.EnvironmentDigest != f.environmentDigest || req.HighWater.Authority != "azure_blob_last_modified" || strings.TrimSpace(req.HighWater.CapturedAt) == "" {
		return nil, errors.New("transfer: Azure fence scan high-water binding mismatch")
	}
	highWater, err := time.Parse(time.RFC3339Nano, req.HighWater.CapturedAt)
	if err != nil {
		return nil, fmt.Errorf("transfer: Azure fence scan high-water time: %w", err)
	}
	if f.maxScanPages <= 0 || f.maxScanObjects <= 0 || f.maxScanBytes <= 0 {
		return nil, errors.New("transfer: Azure fence scan limits are invalid")
	}

	workCtx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	prefix := "env/" + f.environmentDigest + "/tenant/" + f.tenantDigest + "/fences/"
	pageSize := int32(5000)
	if f.maxScanObjects < int(pageSize) {
		pageSize = int32(f.maxScanObjects)
	}
	options := &azblob.ListBlobsFlatOptions{
		Prefix:     &prefix,
		MaxResults: &pageSize,
		Include:    azblob.ListBlobsInclude{Metadata: true},
	}
	pager := f.client.NewListBlobsFlatPager(f.containerName, options)
	objects := make([]FenceObject, 0)
	seen := make(map[string]struct{})
	var totalBytes int64
	pages := 0
	for pager.More() {
		pages++
		if pages > f.maxScanPages {
			return nil, fmt.Errorf("transfer: Azure fence scan page limit exceeded (%d)", f.maxScanPages)
		}
		page, err := pager.NextPage(workCtx)
		if err != nil {
			return nil, fmt.Errorf("transfer: Azure fence scan list: %w", err)
		}
		if page.Segment == nil {
			continue
		}
		for _, item := range page.Segment.BlobItems {
			if len(objects) >= f.maxScanObjects {
				return nil, fmt.Errorf("transfer: Azure fence scan object limit exceeded (%d)", f.maxScanObjects)
			}
			if item == nil || item.Name == nil || item.Properties == nil {
				return nil, errors.New("transfer: Azure fence scan returned an incomplete Blob item")
			}
			name := *item.Name
			id, kind, err := f.scanObjectIdentity(name, prefix)
			if err != nil {
				return nil, err
			}
			if _, duplicate := seen[name]; duplicate {
				return nil, fmt.Errorf("transfer: Azure fence scan duplicate object %q", name)
			}
			seen[name] = struct{}{}
			properties := item.Properties
			if properties.ETag == nil || strings.TrimSpace(string(*properties.ETag)) == "" || properties.LastModified == nil || properties.ContentLength == nil || *properties.ContentLength < 0 {
				return nil, fmt.Errorf("transfer: Azure fence scan metadata incomplete for %q", name)
			}
			if properties.LastModified.After(highWater) {
				return nil, fmt.Errorf("transfer: Azure fence object %q is newer than restore high-water", name)
			}
			if *properties.ContentLength > f.maxScanBytes-totalBytes {
				return nil, fmt.Errorf("transfer: Azure fence scan byte limit exceeded (%d)", f.maxScanBytes)
			}

			body, etag, metadata, lastModified, contentLength, err := f.readScanObject(workCtx, name, f.maxScanBytes-totalBytes)
			if err != nil {
				return nil, fmt.Errorf("transfer: Azure fence scan read %q: %w", name, err)
			}
			if etag == nil || properties.ETag == nil || !sameStrongAzureFenceETag(string(*etag), string(*properties.ETag)) {
				return nil, fmt.Errorf("transfer: Azure fence scan ETag mismatch for %q", name)
			}
			if lastModified == nil || lastModified.After(highWater) || !lastModified.Equal(*properties.LastModified) {
				return nil, fmt.Errorf("transfer: Azure fence scan Last-Modified mismatch for %q", name)
			}
			if contentLength == nil || *contentLength != *properties.ContentLength || int64(len(body)) != *properties.ContentLength {
				return nil, fmt.Errorf("transfer: Azure fence scan content length mismatch for %q", name)
			}
			if !equalAzureFenceMetadata(item.Metadata, metadata) {
				return nil, fmt.Errorf("transfer: Azure fence scan metadata mismatch for %q", name)
			}
			if err := canonicalFenceBody(body, kind, id, f.environmentDigest, f.tenantDigest); err != nil {
				return nil, fmt.Errorf("transfer: Azure fence scan canonical body %q: %w", name, err)
			}
			totalBytes += int64(len(body))
			digest := sha256Hex(body)
			objects = append(objects, FenceObject{
				Name: name, ObjectName: name, TenantDigest: f.tenantDigest, EnvironmentDigest: f.environmentDigest,
				TransferID: id, Kind: kind, FenceKind: kind, Digest: digest, FenceDigest: digest,
				LastModified: *lastModified, Body: append([]byte(nil), body...),
			})
		}
	}
	return objects, nil
}

func sameStrongAzureFenceETag(left, right string) bool {
	leftOpaque, leftOK := normalizeStrongAzureFenceETag(left)
	rightOpaque, rightOK := normalizeStrongAzureFenceETag(right)
	return leftOK && rightOK && leftOpaque == rightOpaque
}

func normalizeStrongAzureFenceETag(raw string) (string, bool) {
	if raw == "" || len(raw) > maxAzureFenceETagLength || strings.TrimSpace(raw) != raw || strings.HasPrefix(raw, "W/") || strings.HasPrefix(raw, "w/") {
		return "", false
	}
	startsQuoted := strings.HasPrefix(raw, `"`)
	endsQuoted := strings.HasSuffix(raw, `"`)
	if startsQuoted != endsQuoted {
		return "", false
	}
	opaque := raw
	if startsQuoted {
		if len(raw) < 3 {
			return "", false
		}
		opaque = raw[1 : len(raw)-1]
	}
	if opaque == "" {
		return "", false
	}
	for _, character := range opaque {
		if character < 0x21 || character > 0x7e || character == '"' {
			return "", false
		}
	}
	return opaque, true
}

func (f *AzureBlobFence) scanObjectIdentity(name, prefix string) (string, string, error) {
	if !strings.HasPrefix(name, prefix) {
		return "", "", fmt.Errorf("transfer: Azure fence scan unknown object path %q", name)
	}
	parts := strings.Split(strings.TrimPrefix(name, prefix), "/")
	if len(parts) != 2 || parts[0] == "" || !validAzureFenceSegment(parts[0]) {
		return "", "", fmt.Errorf("transfer: Azure fence scan unknown object path %q", name)
	}
	id := parts[0]
	var kind string
	switch parts[1] {
	case "claim.json":
		kind = "claim"
	case "terminal.json":
		kind = "terminal"
	default:
		return "", "", fmt.Errorf("transfer: Azure fence scan unknown object name %q", name)
	}
	if f.objectName(id, kind) != name {
		return "", "", fmt.Errorf("transfer: Azure fence scan path mismatch for %q", name)
	}
	return id, kind, nil
}

func (f *AzureBlobFence) readScanObject(ctx context.Context, name string, maxBytes int64) ([]byte, *azcore.ETag, map[string]*string, *time.Time, *int64, error) {
	response, err := f.client.DownloadStream(ctx, f.containerName, name, nil)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if response.Body == nil {
		return nil, response.ETag, response.Metadata, response.LastModified, response.ContentLength, errors.New("empty response body")
	}
	limit := maxBytes
	if limit < int64(^uint64(0)>>1) {
		limit++
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, limit))
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, response.ETag, response.Metadata, response.LastModified, response.ContentLength, readErr
	}
	if closeErr != nil {
		return nil, response.ETag, response.Metadata, response.LastModified, response.ContentLength, closeErr
	}
	if int64(len(body)) > maxBytes {
		return nil, response.ETag, response.Metadata, response.LastModified, response.ContentLength, errors.New("response body exceeds scan byte limit")
	}
	return body, response.ETag, response.Metadata, response.LastModified, response.ContentLength, nil
}

func equalAzureFenceMetadata(left, right map[string]*string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, leftValue := range left {
		rightValue, ok := right[key]
		if !ok {
			for otherKey, value := range right {
				if strings.EqualFold(key, otherKey) {
					rightValue, ok = value, true
					break
				}
			}
		}
		if !ok || (leftValue == nil) != (rightValue == nil) {
			return false
		}
		if leftValue != nil && *leftValue != *rightValue {
			return false
		}
	}
	return true
}

func (f *AzureBlobFence) create(ctx context.Context, id string, body []byte, kind string) (string, error) {
	if err := f.validateOperation(ctx, id, kind); err != nil {
		return "", err
	}
	if err := canonicalFenceBody(body, kind, id, f.environmentDigest, f.tenantDigest); err != nil {
		return "", err
	}
	workCtx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	name := f.objectName(id, kind)
	anyETag := azcore.ETagAny
	options := &azblob.UploadStreamOptions{
		Concurrency: 1,
		AccessConditions: &azblob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: &anyETag},
		},
	}
	upload, err := f.client.UploadStream(workCtx, f.containerName, name, bytes.NewReader(body), options)
	if err != nil {
		if status, ok := azureFenceStatus(err); ok && (status == 409 || status == 412) {
			return "", fmt.Errorf("transfer: Azure %s fence already exists; create-only claim rejected: %w", kind, err)
		}
		return "", fmt.Errorf("transfer: Azure %s fence create: %w", kind, err)
	}
	if upload.ETag == nil || strings.TrimSpace(string(*upload.ETag)) == "" {
		return "", fmt.Errorf("transfer: Azure %s fence create returned no ETag", kind)
	}

	readBody, readETag, err := f.read(workCtx, name)
	if err != nil {
		return "", fmt.Errorf("transfer: Azure %s fence read-back: %w", kind, err)
	}
	if !bytes.Equal(readBody, body) {
		return "", fmt.Errorf("transfer: Azure %s fence read-back bytes differ", kind)
	}
	if readETag == nil || strings.TrimSpace(string(*readETag)) == "" {
		return "", fmt.Errorf("transfer: Azure %s fence read-back returned no ETag", kind)
	}
	if *readETag != *upload.ETag {
		return "", fmt.Errorf("transfer: Azure %s fence read-back ETag differs", kind)
	}
	return sha256Hex(body), nil
}

func (f *AzureBlobFence) verify(ctx context.Context, id, expectedDigest, kind string, expected ...[]byte) error {
	if err := f.validateOperation(ctx, id, kind); err != nil {
		return err
	}
	if strings.TrimSpace(expectedDigest) == "" {
		return errors.New("transfer: Azure fence digest is required")
	}
	workCtx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	body, etag, err := f.read(workCtx, f.objectName(id, kind))
	if err != nil {
		return fmt.Errorf("transfer: Azure %s fence verify read-back: %w", kind, err)
	}
	if etag == nil || strings.TrimSpace(string(*etag)) == "" {
		return fmt.Errorf("transfer: Azure %s fence verify returned no ETag", kind)
	}
	if err := canonicalFenceBody(body, kind, id, f.environmentDigest, f.tenantDigest); err != nil {
		return err
	}
	actualDigest := sha256Hex(body)
	if actualDigest != expectedDigest {
		return fmt.Errorf("transfer: Azure %s fence digest mismatch", kind)
	}
	if len(expected) > 0 && (len(expected) != 1 || !bytes.Equal(body, expected[0]) || sha256Hex(expected[0]) != expectedDigest) {
		return fmt.Errorf("transfer: Azure %s fence exact bytes mismatch", kind)
	}
	return nil
}

func (f *AzureBlobFence) read(ctx context.Context, name string) ([]byte, *azcore.ETag, error) {
	response, err := f.client.DownloadStream(ctx, f.containerName, name, nil)
	if err != nil {
		return nil, nil, err
	}
	if response.Body == nil {
		return nil, response.ETag, errors.New("empty response body")
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, response.ETag, readErr
	}
	if closeErr != nil {
		return nil, response.ETag, closeErr
	}
	return body, response.ETag, nil
}

func (f *AzureBlobFence) validateOperation(ctx context.Context, id, kind string) error {
	if f == nil || !f.configured || f.client == nil {
		return errors.New("transfer: Azure fence is not configured")
	}
	if ctx == nil {
		return errors.New("transfer: Azure fence context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if (kind != "claim" && kind != "terminal") || id == "" || !validAzureFenceSegment(id) {
		return errors.New("transfer: Azure fence ID is invalid")
	}
	return nil
}

func (f *AzureBlobFence) objectName(id, kind string) string {
	return "env/" + f.environmentDigest + "/tenant/" + f.tenantDigest + "/fences/" + id + "/" + kind + ".json"
}

func sha256Hex(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func azureFenceStatus(err error) (int, bool) {
	var responseErr *azcore.ResponseError
	if errors.As(err, &responseErr) && responseErr != nil {
		return responseErr.StatusCode, true
	}
	return 0, false
}
