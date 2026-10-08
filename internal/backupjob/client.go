// Package backupjob contains the bounded HTTP caller used by the dedicated
// backup job. It receives only the sealed receipt returned by the API; it
// never reads backup objects or selects a restore target.
package backupjob

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/google/uuid"
)

const (
	maxResponseBytes = 16 * 1024
	requestTimeout   = 150 * time.Second
	sealedEventType  = "nl.backup.sealed"
)

type Config struct {
	Endpoint               string
	Audience               string
	CheckpointPublicKeyHex string
}

// Receipt is the exact nine-field public receipt from POST /maintenance/backup.
type Receipt struct {
	EnvelopeID          string `json:"envelope_id"`
	DatabaseSHA256      string `json:"database_sha256"`
	ManifestSHA256      string `json:"manifest_sha256"`
	DatabaseETag        string `json:"database_etag"`
	ManifestETag        string `json:"manifest_etag"`
	AuditHighWater      int64  `json:"audit_high_water"`
	CheckpointPublicKey string `json:"checkpoint_public_key"`
	CheckpointID        string `json:"checkpoint_id"`
	MinimumCreatedAt    string `json:"minimum_created_at"`
}

// SealedEvent is the only successful output. Its fields contain public receipt
// metadata and the API host, never credentials or key material.
type SealedEvent struct {
	Type       string    `json:"type"`
	Receipt    Receipt   `json:"receipt"`
	ReceivedAt time.Time `json:"received_at"`
	Host       string    `json:"host"`
}

type Client struct {
	endpoint   *url.URL
	audience   string
	publicKey  []byte
	credential azcore.TokenCredential
	httpClient *http.Client
}

// ValidateConfig rejects endpoints and trust values that could change the
// authority, path, token audience, or checkpoint key used by the caller.
func ValidateConfig(cfg Config) error {
	parsed, err := url.Parse(cfg.Endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Opaque != "" || parsed.User != nil || parsed.Host == "" || parsed.Hostname() == "" || parsed.Path != "/maintenance/backup" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" || parsed.Host != strings.ToLower(parsed.Host) || strings.Contains(parsed.Host, "*") || parsed.String() != cfg.Endpoint {
		return errors.New("backup endpoint must be a canonical HTTPS /maintenance/backup URL")
	}
	if !validAudience(cfg.Audience) {
		return errors.New("backup audience must be api://<canonical-uuid>/.default")
	}
	if !isLowerHex(cfg.CheckpointPublicKeyHex, 64) {
		return errors.New("checkpoint public key must be 32 bytes of lowercase hex")
	}
	return nil
}

func validAudience(value string) bool {
	const prefix, suffix = "api://", "/.default"
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, suffix) {
		return false
	}
	text := strings.TrimSuffix(strings.TrimPrefix(value, prefix), suffix)
	if text == "" {
		return false
	}
	id, err := uuid.Parse(text)
	return err == nil && id.String() == text
}

func isLowerHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' && r < 'a' || r > 'f' {
			return false
		}
	}
	return true
}

func NewClient(cfg Config, credential azcore.TokenCredential, httpClient *http.Client) (*Client, error) {
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	if credential == nil {
		return nil, errors.New("managed identity credential is required")
	}
	endpoint, _ := url.Parse(cfg.Endpoint)
	publicKey, _ := hex.DecodeString(cfg.CheckpointPublicKeyHex)
	client := http.DefaultClient
	if httpClient != nil {
		client = httpClient
	}
	copyClient := *client
	copyClient.Timeout = requestTimeout
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{endpoint: endpoint, audience: cfg.Audience, publicKey: publicKey, credential: credential, httpClient: &copyClient}, nil
}

// RunOnce makes exactly one authenticated POST. Any ambiguous result is
// returned as an error; this client never retries the operation.
func (c *Client) RunOnce(ctx context.Context) (SealedEvent, error) {
	if c == nil || c.credential == nil || c.httpClient == nil || c.endpoint == nil {
		return SealedEvent{}, errors.New("backup caller is unavailable")
	}
	if ctx == nil {
		return SealedEvent{}, errors.New("backup caller requires a context")
	}
	if err := ctx.Err(); err != nil {
		return SealedEvent{}, errors.New("backup operation canceled before request")
	}
	bounded, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	accessToken, err := c.credential.GetToken(bounded, policy.TokenRequestOptions{Scopes: []string{c.audience}})
	if err != nil || accessToken.Token == "" || len(accessToken.Token) > maxResponseBytes || strings.ContainsAny(accessToken.Token, "\r\n") {
		return SealedEvent{}, errors.New("managed identity token unavailable")
	}
	request, err := http.NewRequestWithContext(bounded, http.MethodPost, c.endpoint.String(), nil)
	if err != nil {
		return SealedEvent{}, errors.New("backup request could not be created")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken.Token)
	request.Header.Set("Accept", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return SealedEvent{}, errors.New("backup request failed; outcome is unknown and must not be retried")
	}
	if response == nil || response.Body == nil {
		return SealedEvent{}, errors.New("backup endpoint returned no response body; outcome is unknown and must not be retried")
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return SealedEvent{}, errors.New("backup response could not be completely read and closed; outcome is unknown and must not be retried")
	}
	if len(body) > maxResponseBytes {
		return SealedEvent{}, errors.New("backup response exceeded the permitted size; outcome is unknown and must not be retried")
	}
	if response.StatusCode != http.StatusOK {
		return SealedEvent{}, fmt.Errorf("backup endpoint returned HTTP %d; do not retry automatically", response.StatusCode)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		return SealedEvent{}, errors.New("backup endpoint returned an invalid receipt media type; outcome is unknown and must not be retried")
	}
	receipt, err := decodeReceipt(body, c.publicKey)
	if err != nil {
		return SealedEvent{}, errors.New("backup endpoint returned an invalid sealed receipt; outcome is unknown and must not be retried")
	}
	return SealedEvent{Type: sealedEventType, Receipt: receipt, ReceivedAt: time.Now().UTC(), Host: c.endpoint.Host}, nil
}

func decodeReceipt(body, expectedPublicKey []byte) (Receipt, error) {
	var receipt Receipt
	if len(body) == 0 || len(body) > maxResponseBytes {
		return Receipt{}, errors.New("invalid receipt size")
	}
	if err := requireClosedUniqueReceiptObject(body); err != nil {
		return Receipt{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return Receipt{}, errors.New("invalid receipt JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Receipt{}, errors.New("receipt has trailing content")
	}
	if !isLowerHex(receipt.EnvelopeID, 32) || !isLowerHex(receipt.DatabaseSHA256, 64) || !isLowerHex(receipt.ManifestSHA256, 64) || !validQuotedETag(receipt.DatabaseETag) || !validQuotedETag(receipt.ManifestETag) || receipt.AuditHighWater <= 0 || !isLowerHex(receipt.CheckpointPublicKey, 64) || !bytes.Equal(mustDecodeHex(receipt.CheckpointPublicKey), expectedPublicKey) || !isCanonicalUUID(receipt.CheckpointID) {
		return Receipt{}, errors.New("receipt fields failed validation")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, receipt.MinimumCreatedAt)
	if err != nil || createdAt.IsZero() {
		return Receipt{}, errors.New("receipt creation time is invalid")
	}
	return receipt, nil
}

func requireClosedUniqueReceiptObject(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return errors.New("receipt must be a JSON object")
	}
	allowed := map[string]struct{}{
		"envelope_id": {}, "database_sha256": {}, "manifest_sha256": {},
		"database_etag": {}, "manifest_etag": {}, "audit_high_water": {},
		"checkpoint_public_key": {}, "checkpoint_id": {}, "minimum_created_at": {},
	}
	seen := make(map[string]struct{}, len(allowed))
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return errors.New("receipt object key is invalid")
		}
		if _, ok := allowed[name]; !ok {
			return errors.New("receipt object has an unknown field")
		}
		if _, duplicate := seen[name]; duplicate {
			return errors.New("receipt object has a duplicate field")
		}
		seen[name] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return errors.New("receipt object value is invalid")
		}
	}
	last, err := decoder.Token()
	if err != nil || last != json.Delim('}') || len(seen) != len(allowed) {
		return errors.New("receipt object is incomplete")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("receipt has trailing content")
	}
	return nil
}

func mustDecodeHex(value string) []byte {
	decoded, _ := hex.DecodeString(value)
	return decoded
}

func validQuotedETag(value string) bool {
	if len(value) < 3 || len(value) > 1024 || value[0] != '"' || value[len(value)-1] != '"' {
		return false
	}
	for _, b := range []byte(value[1 : len(value)-1]) {
		if b == '"' || b < 0x21 || b == 0x7f {
			return false
		}
	}
	return true
}

func isCanonicalUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value
}
