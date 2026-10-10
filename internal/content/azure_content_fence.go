package content

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
)

const (
	defaultContentFenceScanPages         = 128
	defaultContentFenceScanObjects       = 10000
	defaultContentFenceScanBytes   int64 = 64 << 20
)

var privateAzureContainerPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{1,61}[a-z0-9])$`)

type AzureContentFenceConfig struct {
	ServiceURL string
	// ContainerName must identify an administratively private container with
	// public access disabled. The adapter never creates or changes containers.
	ContainerName     string
	TenantID          string
	EnvironmentDigest string
	Timeout           time.Duration
	MaxScanPages      int
	MaxScanObjects    int
	MaxScanBytes      int64
	ClientOptions     *azblob.ClientOptions
}

type AzureContentFence struct {
	client                                       *azblob.Client
	container, tenant, tenantDigest, environment string
	timeout                                      time.Duration
	maxPages, maxObjects                         int
	maxBytes                                     int64
	configured                                   bool
}

var _ ContentFence = (*AzureContentFence)(nil)

// NewAzureContentFence creates the production adapter with DefaultAzureCredential.
// Construction validates configuration and creates clients without cloud calls.
func NewAzureContentFence(ctx context.Context, cfg AzureContentFenceConfig) (*AzureContentFence, error) {
	if ctx == nil {
		return nil, errors.New("content: Azure fence context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateAzureContentFenceConfig(cfg, true); err != nil {
		return nil, err
	}
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("content: Azure fence credential: %w", err)
	}
	options := cfg.ClientOptions
	if options == nil {
		options = &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}}
	}
	client, err := azblob.NewClient(cfg.ServiceURL, credential, options)
	if err != nil {
		return nil, fmt.Errorf("content: Azure fence client: %w", err)
	}
	return newAzureContentFence(client, cfg)
}

// NewAzureContentFenceWithClient is the local-fixture seam. Production wiring
// should use NewAzureContentFence to retain DefaultAzureCredential.
func NewAzureContentFenceWithClient(ctx context.Context, cfg AzureContentFenceConfig, client *azblob.Client) (*AzureContentFence, error) {
	if ctx == nil {
		return nil, errors.New("content: Azure fence context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateAzureContentFenceConfig(cfg, true); err != nil {
		return nil, err
	}
	return newAzureContentFence(client, cfg)
}

func newAzureContentFence(client *azblob.Client, cfg AzureContentFenceConfig) (*AzureContentFence, error) {
	if client == nil {
		return nil, errors.New("content: Azure fence Blob client is required")
	}
	if err := validateAzureContentFenceConfig(cfg, false); err != nil {
		return nil, err
	}
	pages, objects, bytes := cfg.MaxScanPages, cfg.MaxScanObjects, cfg.MaxScanBytes
	if pages == 0 {
		pages = defaultContentFenceScanPages
	}
	if objects == 0 {
		objects = defaultContentFenceScanObjects
	}
	if bytes == 0 {
		bytes = defaultContentFenceScanBytes
	}
	if pages > defaultContentFenceScanPages || objects > defaultContentFenceScanObjects || bytes > defaultContentFenceScanBytes {
		return nil, errors.New("content: Azure fence scan limits exceed configured safety bounds")
	}
	return &AzureContentFence{client: client, container: cfg.ContainerName, tenant: cfg.TenantID,
		tenantDigest: contentDigest([]byte(cfg.TenantID)), environment: cfg.EnvironmentDigest,
		timeout: cfg.Timeout, maxPages: pages, maxObjects: objects, maxBytes: bytes, configured: true}, nil
}

func validateAzureContentFenceConfig(cfg AzureContentFenceConfig, requireURL bool) error {
	if requireURL && strings.TrimSpace(cfg.ServiceURL) == "" {
		return errors.New("content: Azure fence service URL is required")
	}
	if !privateAzureContainerPattern.MatchString(cfg.ContainerName) {
		return errors.New("content: Azure fence container is required")
	}
	if strings.TrimSpace(cfg.TenantID) == "" {
		return errors.New("content: Azure fence trusted tenant is required")
	}
	if !validContentFenceSegment(cfg.EnvironmentDigest) {
		return errors.New("content: Azure fence environment digest is invalid")
	}
	if cfg.Timeout <= 0 {
		return errors.New("content: Azure fence timeout must be positive")
	}
	if cfg.MaxScanPages < 0 || cfg.MaxScanObjects < 0 || cfg.MaxScanBytes < 0 {
		return errors.New("content: Azure fence scan bounds cannot be negative")
	}
	return nil
}

func (f *AzureContentFence) Durable() bool { return f != nil && f.configured && f.client != nil }

func (f *AzureContentFence) Identity(tenantID string) (string, string, error) {
	if !f.Durable() || tenantID == "" || tenantID != f.tenant {
		return "", "", errors.New("content: Azure fence tenant mismatch")
	}
	return f.environment, f.tenantDigest, nil
}

func (f *AzureContentFence) CreateClaim(ctx context.Context, claim ContentClaim) (string, error) {
	body, err := CanonicalContentClaim(claim)
	if err != nil {
		return "", err
	}
	if err := f.validateIdentity(ctx, claim.EnvironmentDigest, claim.TenantDigest, claim.PlacementIntentID); err != nil {
		return "", err
	}
	return f.create(ctx, claim.PlacementIntentID, "claim", body)
}

func (f *AzureContentFence) VerifyClaim(ctx context.Context, claim ContentClaim, expectedDigest string) error {
	body, err := CanonicalContentClaim(claim)
	if err != nil {
		return err
	}
	if expectedDigest != contentDigest(body) {
		return errors.New("content: claim fence expected digest mismatch")
	}
	if err := f.validateIdentity(ctx, claim.EnvironmentDigest, claim.TenantDigest, claim.PlacementIntentID); err != nil {
		return err
	}
	return f.verify(ctx, claim.PlacementIntentID, "claim", expectedDigest, body)
}

func (f *AzureContentFence) CreateTerminal(ctx context.Context, claim ContentClaim, terminal ContentTerminal) (string, error) {
	claimBody, err := CanonicalContentClaim(claim)
	if err != nil {
		return "", err
	}
	if err := f.validateIdentity(ctx, claim.EnvironmentDigest, claim.TenantDigest, claim.PlacementIntentID); err != nil {
		return "", err
	}
	claimDigest := contentDigest(claimBody)
	if err := f.verify(ctx, claim.PlacementIntentID, "claim", claimDigest, claimBody); err != nil {
		return "", fmt.Errorf("content: verify claim before terminal fence: %w", err)
	}
	body, err := CanonicalContentTerminal(claim, terminal)
	if err != nil {
		return "", err
	}
	return f.create(ctx, claim.PlacementIntentID, "terminal", body)
}

func (f *AzureContentFence) VerifyTerminal(ctx context.Context, claim ContentClaim, terminal ContentTerminal, expectedDigest string) error {
	claimBody, err := CanonicalContentClaim(claim)
	if err != nil {
		return err
	}
	if err := f.validateIdentity(ctx, claim.EnvironmentDigest, claim.TenantDigest, claim.PlacementIntentID); err != nil {
		return err
	}
	claimDigest := contentDigest(claimBody)
	if err := f.verify(ctx, claim.PlacementIntentID, "claim", claimDigest, claimBody); err != nil {
		return err
	}
	body, err := CanonicalContentTerminal(claim, terminal)
	if err != nil {
		return err
	}
	if expectedDigest != contentDigest(body) {
		return errors.New("content: terminal fence expected digest mismatch")
	}
	return f.verify(ctx, claim.PlacementIntentID, "terminal", expectedDigest, body)
}

// ReadVerifiedTerminal reads only the exact terminal object under the owned
// content-fence namespace. Not-found is explicit; transport and authorization
// failures remain ambiguous errors and must never trigger a create attempt.
func (f *AzureContentFence) ReadVerifiedTerminal(ctx context.Context, claim ContentClaim, expectedDigest string) (ContentTerminal, string, error) {
	claimBody, err := CanonicalContentClaim(claim)
	if err != nil {
		return ContentTerminal{}, "", err
	}
	if expectedDigest != "" && !digestPattern.MatchString(expectedDigest) {
		return ContentTerminal{}, "", errors.New("content: expected terminal digest is invalid")
	}
	if err := f.validateIdentity(ctx, claim.EnvironmentDigest, claim.TenantDigest, claim.PlacementIntentID); err != nil {
		return ContentTerminal{}, "", err
	}
	claimDigest := contentDigest(claimBody)
	if err := f.verify(ctx, claim.PlacementIntentID, "claim", claimDigest, claimBody); err != nil {
		return ContentTerminal{}, "", fmt.Errorf("content: verify claim before terminal read: %w", err)
	}
	work, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	body, etag, err := f.read(work, contentFencePath(f.environment, f.tenantDigest, claim.PlacementIntentID, "terminal"), 8192)
	if err != nil {
		var responseErr *azcore.ResponseError
		if errors.As(err, &responseErr) && responseErr.StatusCode == 404 {
			return ContentTerminal{}, "", ErrContentFenceObjectNotFound
		}
		return ContentTerminal{}, "", fmt.Errorf("content: read exact terminal fence: %w", err)
	}
	digest := contentDigest(body)
	if etag == nil || expectedDigest != "" && digest != expectedDigest {
		return ContentTerminal{}, "", errors.New("content: exact terminal fence digest or etag is invalid")
	}
	terminal, err := ValidateContentTerminalJSON(body, claim)
	if err != nil {
		return ContentTerminal{}, "", err
	}
	return terminal, digest, nil
}

func (f *AzureContentFence) CaptureHighWater(ctx context.Context, tenantID string) (ContentFenceHighWater, error) {
	if err := f.validateTenant(ctx, tenantID); err != nil {
		return ContentFenceHighWater{}, err
	}
	work, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	prefix := f.namespacePrefix()
	one := int32(1)
	pager := f.client.NewListBlobsFlatPager(f.container, &azblob.ListBlobsFlatOptions{Prefix: &prefix, MaxResults: &one})
	page, err := pager.NextPage(work)
	if err != nil {
		return ContentFenceHighWater{}, fmt.Errorf("content: capture Azure fence high-water: %w", err)
	}
	if page.Date == nil || page.Date.IsZero() {
		return ContentFenceHighWater{}, errors.New("content: Azure fence storage Date missing")
	}
	return ContentFenceHighWater{EnvironmentDigest: f.environment, TenantDigest: f.tenantDigest,
		CapturedAt: page.Date.UTC().Format(time.RFC3339Nano), Authority: "azure_blob_last_modified"}, nil
}

// Scan returns a complete, verified inventory or an error. Partial listings
// are never returned as ready evidence; callers must keep readiness closed.
func (f *AzureContentFence) Scan(ctx context.Context, req ContentFenceScanRequest) ([]ContentFenceObject, error) {
	if err := f.validateTenant(ctx, req.TenantID); err != nil {
		return nil, err
	}
	hw := req.HighWater
	if hw.EnvironmentDigest != f.environment || hw.TenantDigest != f.tenantDigest || hw.Authority != "azure_blob_last_modified" || hw.CapturedAt == "" {
		return nil, errors.New("content: Azure fence scan high-water binding missing or invalid")
	}
	highWater, err := time.Parse(time.RFC3339Nano, hw.CapturedAt)
	if err != nil {
		return nil, fmt.Errorf("content: Azure fence high-water time: %w", err)
	}
	work, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	prefix := f.namespacePrefix()
	pageSize := int32(5000)
	if f.maxObjects < int(pageSize) {
		pageSize = int32(f.maxObjects)
	}
	pager := f.client.NewListBlobsFlatPager(f.container, &azblob.ListBlobsFlatOptions{Prefix: &prefix, MaxResults: &pageSize, Include: azblob.ListBlobsInclude{Metadata: true}})
	objects := make([]ContentFenceObject, 0)
	seen := make(map[string]struct{})
	var totalBytes int64
	pages := 0
	claims := make(map[string]ContentClaim)
	for pager.More() {
		pages++
		if pages > f.maxPages {
			return nil, errors.New("content: Azure fence scan page limit exceeded")
		}
		page, err := pager.NextPage(work)
		if err != nil {
			return nil, fmt.Errorf("content: Azure fence scan list: %w", err)
		}
		if page.Segment == nil {
			return nil, errors.New("content: Azure fence scan returned incomplete page")
		}
		for _, item := range page.Segment.BlobItems {
			if len(objects) >= f.maxObjects {
				return nil, errors.New("content: Azure fence scan object limit exceeded")
			}
			if item == nil || item.Name == nil || item.Properties == nil {
				return nil, errors.New("content: Azure fence scan returned incomplete Blob item")
			}
			name := *item.Name
			id, kind, err := f.scanObjectIdentity(name)
			if err != nil {
				return nil, err
			}
			if _, exists := seen[name]; exists {
				return nil, errors.New("content: duplicate Azure content fence object")
			}
			seen[name] = struct{}{}
			props := item.Properties
			if props.ETag == nil || props.LastModified == nil || props.ContentLength == nil || *props.ContentLength < 0 {
				return nil, errors.New("content: Azure content fence metadata incomplete")
			}
			if !props.LastModified.Before(highWater) {
				return nil, errors.New("content: Azure content fence is at or newer than restore high-water")
			}
			if *props.ContentLength > f.maxBytes-totalBytes {
				return nil, errors.New("content: Azure content fence scan byte limit exceeded")
			}
			body, etag, metadata, modified, length, err := f.readScanObject(work, name, f.maxBytes-totalBytes)
			if err != nil {
				return nil, fmt.Errorf("content: read Azure content fence: %w", err)
			}
			if etag == nil || !sameContentETag(string(*etag), string(*props.ETag)) || modified == nil || !modified.Equal(*props.LastModified) || !modified.Before(highWater) || length == nil || *length != *props.ContentLength || int64(len(body)) != *props.ContentLength || !sameContentMetadata(item.Metadata, metadata) {
				return nil, errors.New("content: Azure content fence listing/readback metadata mismatch")
			}
			if kind == "claim" {
				claim, err := ValidateContentClaimJSON(body)
				if err != nil || claim.PlacementIntentID != id || claim.EnvironmentDigest != f.environment || claim.TenantDigest != f.tenantDigest {
					return nil, errors.New("content: invalid scanned content claim")
				}
				claims[id] = claim
			} else if _, err := decodeScannedTerminal(body, id, f.environment, f.tenantDigest); err != nil {
				return nil, err
			}
			totalBytes += int64(len(body))
			objects = append(objects, ContentFenceObject{Name: name, PlacementIntentID: id, Kind: kind, Digest: contentDigest(body), EnvironmentDigest: f.environment, TenantDigest: f.tenantDigest, LastModified: *modified, Body: append([]byte(nil), body...)})
		}
	}
	for _, object := range objects {
		if object.Kind != "terminal" {
			continue
		}
		claim, ok := claims[object.PlacementIntentID]
		if !ok {
			return nil, errors.New("content: terminal fence has no claim in complete inventory")
		}
		if _, err := ValidateContentTerminalJSON(object.Body, claim); err != nil {
			return nil, err
		}
	}
	return objects, nil
}

func decodeScannedTerminal(body []byte, id, env, tenant string) (ContentTerminal, error) {
	var terminal ContentTerminal
	if err := decodeCanonical(body, &terminal); err != nil {
		return terminal, err
	}
	if terminal.SchemaVersion != 1 || terminal.MutationKind != ContentMutationKind || terminal.PlacementIntentID != id || terminal.EnvironmentDigest != env || terminal.TenantDigest != tenant || !knownContentOutcome(terminal.Outcome) || !digestPattern.MatchString(terminal.ClaimDigest) || terminal.TerminalAt.IsZero() || terminal.TerminalAt.Location() != time.UTC || terminal.TerminalRowVersion < 1 {
		return terminal, errors.New("content: invalid scanned content terminal")
	}
	if err := validateContentTerminalEvidence(terminal); err != nil {
		return terminal, err
	}
	canonical, _ := json.Marshal(terminal)
	if !bytes.Equal(body, canonical) {
		return terminal, errors.New("content: noncanonical scanned content terminal")
	}
	return terminal, nil
}

func (f *AzureContentFence) create(ctx context.Context, id, kind string, body []byte) (string, error) {
	if err := f.validateOperation(ctx, id, kind); err != nil {
		return "", err
	}
	work, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	name := contentFencePath(f.environment, f.tenantDigest, id, kind)
	anyETag := azcore.ETagAny
	resp, err := f.client.UploadStream(work, f.container, name, bytes.NewReader(body), &azblob.UploadStreamOptions{Concurrency: 1,
		AccessConditions: &azblob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: &anyETag}}})
	if err != nil {
		return "", fmt.Errorf("content: Azure %s fence create-only write: %w", kind, err)
	}
	if resp.ETag == nil || strings.TrimSpace(string(*resp.ETag)) == "" {
		return "", errors.New("content: Azure fence create returned no ETag")
	}
	readBody, etag, err := f.read(work, name, 8192)
	if err != nil {
		return "", fmt.Errorf("content: Azure fence create readback: %w", err)
	}
	if !bytes.Equal(readBody, body) || etag == nil || *etag != *resp.ETag {
		return "", errors.New("content: Azure fence create readback differs")
	}
	return contentDigest(body), nil
}

func (f *AzureContentFence) verify(ctx context.Context, id, kind, expectedDigest string, expected []byte) error {
	if err := f.validateOperation(ctx, id, kind); err != nil {
		return err
	}
	if !digestPattern.MatchString(expectedDigest) || contentDigest(expected) != expectedDigest {
		return errors.New("content: Azure fence expected digest invalid")
	}
	work, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	body, etag, err := f.read(work, contentFencePath(f.environment, f.tenantDigest, id, kind), 8192)
	if err != nil {
		return fmt.Errorf("content: Azure fence verify readback: %w", err)
	}
	if etag == nil || !bytes.Equal(body, expected) || contentDigest(body) != expectedDigest {
		return errors.New("content: Azure fence bytes or digest mismatch")
	}
	return nil
}

func (f *AzureContentFence) read(ctx context.Context, name string, max int64) ([]byte, *azcore.ETag, error) {
	response, err := f.client.DownloadStream(ctx, f.container, name, nil)
	if err != nil {
		return nil, nil, err
	}
	if response.Body == nil {
		return nil, response.ETag, errors.New("content: empty Azure fence response body")
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, max+1))
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, response.ETag, readErr
	}
	if closeErr != nil {
		return nil, response.ETag, closeErr
	}
	if int64(len(body)) > max {
		return nil, response.ETag, errors.New("content: Azure fence response exceeds byte bound")
	}
	return body, response.ETag, nil
}

func (f *AzureContentFence) readScanObject(ctx context.Context, name string, max int64) ([]byte, *azcore.ETag, map[string]*string, *time.Time, *int64, error) {
	response, err := f.client.DownloadStream(ctx, f.container, name, nil)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if response.Body == nil {
		return nil, response.ETag, response.Metadata, response.LastModified, response.ContentLength, errors.New("content: empty scan response")
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, max+1))
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, response.ETag, response.Metadata, response.LastModified, response.ContentLength, readErr
	}
	if closeErr != nil {
		return nil, response.ETag, response.Metadata, response.LastModified, response.ContentLength, closeErr
	}
	if int64(len(body)) > max {
		return nil, response.ETag, response.Metadata, response.LastModified, response.ContentLength, errors.New("content: fence exceeds scan byte bound")
	}
	return body, response.ETag, response.Metadata, response.LastModified, response.ContentLength, nil
}

func (f *AzureContentFence) validateTenant(ctx context.Context, tenant string) error {
	if !f.Durable() || ctx == nil {
		return errors.New("content: Azure fence is not configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenant == "" || tenant != f.tenant {
		return errors.New("content: Azure fence tenant mismatch")
	}
	return nil
}
func (f *AzureContentFence) validateIdentity(ctx context.Context, env, tenantDigest, id string) error {
	if err := f.validateTenant(ctx, f.tenant); err != nil {
		return err
	}
	if env != f.environment || tenantDigest != f.tenantDigest || !validContentFenceSegment(id) {
		return errors.New("content: Azure fence identity or placement binding mismatch")
	}
	return nil
}
func (f *AzureContentFence) validateOperation(ctx context.Context, id, kind string) error {
	if err := f.validateTenant(ctx, f.tenant); err != nil {
		return err
	}
	if !validContentFenceSegment(id) || kind != "claim" && kind != "terminal" {
		return errors.New("content: Azure fence operation invalid")
	}
	return nil
}
func (f *AzureContentFence) namespacePrefix() string {
	return "env/" + f.environment + "/tenant/" + f.tenantDigest + "/content_placement_fences/"
}
func (f *AzureContentFence) scanObjectIdentity(name string) (string, string, error) {
	prefix := f.namespacePrefix()
	if !strings.HasPrefix(name, prefix) {
		return "", "", errors.New("content: Azure scan object escaped namespace")
	}
	parts := strings.Split(strings.TrimPrefix(name, prefix), "/")
	if len(parts) != 2 || !validContentFenceSegment(parts[0]) {
		return "", "", errors.New("content: Azure scan object path invalid")
	}
	kind := strings.TrimSuffix(parts[1], ".json")
	if (kind != "claim" && kind != "terminal") || parts[1] != kind+".json" || contentFencePath(f.environment, f.tenantDigest, parts[0], kind) != name {
		return "", "", errors.New("content: Azure scan object kind or path invalid")
	}
	return parts[0], kind, nil
}

func sameContentETag(left, right string) bool {
	return left != "" && right != "" && left == right && !strings.HasPrefix(strings.ToUpper(left), "W/")
}
func sameContentMetadata(left, right map[string]*string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		other, ok := right[key]
		if !ok || (value == nil) != (other == nil) {
			return false
		}
		if value != nil && *value != *other {
			return false
		}
	}
	return true
}
func contentDigest(body []byte) string { sum := sha256.Sum256(body); return hex.EncodeToString(sum[:]) }
