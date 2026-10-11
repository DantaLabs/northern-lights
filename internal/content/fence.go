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
	"sync"
	"time"
)

const ContentMutationKind = "content_placement"

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

var ErrContentFenceObjectNotFound = errors.New("content: exact fence object not found")

// ContentClaim contains only digests and opaque placement identity. It never
// carries source bytes, source text, confirmation tokens, or provider values.
type ContentClaim struct {
	SchemaVersion        int       `json:"schema_version"`
	MutationKind         string    `json:"mutation_kind"`
	EnvironmentDigest    string    `json:"environment_digest"`
	TenantDigest         string    `json:"tenant_digest"`
	PlacementIntentID    string    `json:"placement_intent_id"`
	CandidateKind        string    `json:"candidate_kind"`
	ActorDigest          string    `json:"actor_digest"`
	IdempotencyDigest    string    `json:"idempotency_digest"`
	RequestDigest        string    `json:"request_digest"`
	PreviewHash          string    `json:"preview_hash"`
	SourceArtifactHash   string    `json:"source_artifact_hash"`
	DraftArtifactHash    string    `json:"draft_artifact_hash,omitempty"`
	CandidateLineageHash string    `json:"candidate_lineage_hash,omitempty"`
	CandidateHash        string    `json:"candidate_hash"`
	ProfileHash          string    `json:"profile_hash"`
	ResourcePolicyHash   string    `json:"resource_policy_hash"`
	ConversionPolicyHash string    `json:"conversion_policy_hash"`
	AccessPolicyHash     string    `json:"access_policy_hash"`
	ProviderPolicyHash   string    `json:"provider_policy_hash"`
	RetentionPolicyHash  string    `json:"retention_policy_hash"`
	ActiveBundleHash     string    `json:"active_bundle_hash"`
	ActiveBundleVersion  int       `json:"active_bundle_version"`
	TargetHash           string    `json:"target_hash"`
	IntendedHash         string    `json:"intended_hash"`
	ClaimedAt            time.Time `json:"claimed_at"`
	ClaimRowVersion      int64     `json:"claim_row_version"`
}

type ContentOutcome string

const (
	ContentOutcomeAccepted              ContentOutcome = "accepted"
	ContentOutcomeRejectedPreacceptance ContentOutcome = "rejected_preacceptance"
	ContentOutcomeFailed                ContentOutcome = "failed"
	ContentOutcomeUnknown               ContentOutcome = "unknown"
)

// ContentTerminal binds a known outcome to the exact content claim and hashes
// of the provider operation and uncached readback. It contains no raw values.
type ContentTerminal struct {
	SchemaVersion          int            `json:"schema_version"`
	MutationKind           string         `json:"mutation_kind"`
	EnvironmentDigest      string         `json:"environment_digest"`
	TenantDigest           string         `json:"tenant_digest"`
	PlacementIntentID      string         `json:"placement_intent_id"`
	ClaimDigest            string         `json:"claim_digest"`
	Outcome                ContentOutcome `json:"outcome"`
	OperationReferenceHash string         `json:"operation_reference_hash"`
	ReadbackHash           string         `json:"readback_hash"`
	ReadbackCacheBypassed  bool           `json:"readback_cache_bypassed"`
	TerminalAt             time.Time      `json:"terminal_at"`
	TerminalRowVersion     int64          `json:"terminal_row_version"`
}

type ContentFence interface {
	Identity(tenantID string) (environmentDigest, tenantDigest string, err error)
	Durable() bool
	CreateClaim(context.Context, ContentClaim) (string, error)
	VerifyClaim(context.Context, ContentClaim, string) error
	CreateTerminal(context.Context, ContentClaim, ContentTerminal) (string, error)
	VerifyTerminal(context.Context, ContentClaim, ContentTerminal, string) error
	ReadVerifiedTerminal(context.Context, ContentClaim, string) (ContentTerminal, string, error)
	CaptureHighWater(context.Context, string) (ContentFenceHighWater, error)
	Scan(context.Context, ContentFenceScanRequest) ([]ContentFenceObject, error)
}

type ContentFenceHighWater struct {
	EnvironmentDigest string
	TenantDigest      string
	CapturedAt        string
	Authority         string
}

type ContentFenceScanRequest struct {
	TenantID  string
	HighWater ContentFenceHighWater
}

type ContentFenceObject struct {
	Name, PlacementIntentID, Kind, Digest string
	EnvironmentDigest, TenantDigest       string
	LastModified                          time.Time
	Body                                  []byte
}

func CanonicalContentClaim(claim ContentClaim) ([]byte, error) {
	if err := validateContentClaim(claim); err != nil {
		return nil, err
	}
	return json.Marshal(claim)
}

func ValidateContentClaimJSON(body []byte) (ContentClaim, error) {
	var claim ContentClaim
	if err := decodeCanonical(body, &claim); err != nil {
		return ContentClaim{}, err
	}
	if err := validateContentClaim(claim); err != nil {
		return ContentClaim{}, err
	}
	canonical, _ := json.Marshal(claim)
	if !bytes.Equal(body, canonical) {
		return ContentClaim{}, errors.New("content: noncanonical fence claim")
	}
	return claim, nil
}

func CanonicalContentTerminal(claim ContentClaim, terminal ContentTerminal) ([]byte, error) {
	claimBytes, err := CanonicalContentClaim(claim)
	if err != nil {
		return nil, err
	}
	if err := validateContentTerminal(claim, terminal, hexDigest(claimBytes)); err != nil {
		return nil, err
	}
	return json.Marshal(terminal)
}

func ValidateContentTerminalJSON(body []byte, claim ContentClaim) (ContentTerminal, error) {
	var terminal ContentTerminal
	if err := decodeCanonical(body, &terminal); err != nil {
		return ContentTerminal{}, err
	}
	claimBytes, err := CanonicalContentClaim(claim)
	if err != nil {
		return ContentTerminal{}, err
	}
	if err := validateContentTerminal(claim, terminal, hexDigest(claimBytes)); err != nil {
		return ContentTerminal{}, err
	}
	canonical, _ := json.Marshal(terminal)
	if !bytes.Equal(body, canonical) {
		return ContentTerminal{}, errors.New("content: noncanonical fence terminal")
	}
	return terminal, nil
}

func validateContentClaim(c ContentClaim) error {
	if c.SchemaVersion != 1 || c.MutationKind != ContentMutationKind || c.EnvironmentDigest == "" || !validContentFenceSegment(c.EnvironmentDigest) ||
		!digestPattern.MatchString(c.TenantDigest) || !validContentFenceSegment(c.PlacementIntentID) ||
		(c.CandidateKind != "extracted_item" && c.CandidateKind != "draft_artifact") ||
		!digestPattern.MatchString(c.ActorDigest) || !digestPattern.MatchString(c.IdempotencyDigest) || !digestPattern.MatchString(c.RequestDigest) ||
		!digestPattern.MatchString(c.PreviewHash) || !digestPattern.MatchString(c.SourceArtifactHash) ||
		!digestPattern.MatchString(c.CandidateHash) ||
		!digestPattern.MatchString(c.ProfileHash) || !digestPattern.MatchString(c.ResourcePolicyHash) ||
		!digestPattern.MatchString(c.ConversionPolicyHash) || !digestPattern.MatchString(c.AccessPolicyHash) ||
		!digestPattern.MatchString(c.ProviderPolicyHash) || !digestPattern.MatchString(c.RetentionPolicyHash) ||
		!digestPattern.MatchString(c.ActiveBundleHash) || c.ActiveBundleVersion < 1 || !digestPattern.MatchString(c.TargetHash) ||
		!digestPattern.MatchString(c.IntendedHash) || c.ClaimedAt.IsZero() || c.ClaimedAt.Location() != time.UTC || c.ClaimRowVersion < 1 {
		return errors.New("content: invalid content fence claim")
	}
	if c.CandidateKind == "extracted_item" {
		if c.DraftArtifactHash != "" || c.CandidateLineageHash != "" && !digestPattern.MatchString(c.CandidateLineageHash) {
			return errors.New("content: extracted-item claim has invalid optional candidate lineage")
		}
	} else if !digestPattern.MatchString(c.DraftArtifactHash) || c.DraftArtifactHash != c.CandidateHash || !digestPattern.MatchString(c.CandidateLineageHash) {
		return errors.New("content: draft claim requires matching draft and candidate hashes plus lineage")
	}
	return nil
}

func validateContentTerminal(claim ContentClaim, terminal ContentTerminal, claimDigest string) error {
	if terminal.SchemaVersion != 1 || terminal.MutationKind != ContentMutationKind ||
		terminal.EnvironmentDigest != claim.EnvironmentDigest || terminal.TenantDigest != claim.TenantDigest ||
		terminal.PlacementIntentID != claim.PlacementIntentID || terminal.ClaimDigest != claimDigest ||
		!knownContentOutcome(terminal.Outcome) ||
		terminal.TerminalAt.IsZero() || terminal.TerminalAt.Location() != time.UTC || terminal.TerminalRowVersion < claim.ClaimRowVersion {
		return errors.New("content: invalid content fence terminal binding")
	}
	if err := validateContentTerminalEvidence(terminal); err != nil {
		return err
	}
	return nil
}

func validateContentTerminalEvidence(terminal ContentTerminal) error {
	if terminal.OperationReferenceHash != "" && !digestPattern.MatchString(terminal.OperationReferenceHash) {
		return errors.New("content: invalid terminal operation reference digest")
	}
	if terminal.ReadbackHash != "" && (!digestPattern.MatchString(terminal.ReadbackHash) || !terminal.ReadbackCacheBypassed) || terminal.ReadbackHash == "" && terminal.ReadbackCacheBypassed {
		return errors.New("content: invalid terminal readback evidence")
	}
	if terminal.Outcome == ContentOutcomeAccepted && (!digestPattern.MatchString(terminal.OperationReferenceHash) || !digestPattern.MatchString(terminal.ReadbackHash) || !terminal.ReadbackCacheBypassed) {
		return errors.New("content: accepted terminal requires operation and uncached readback evidence")
	}
	return nil
}

func knownContentOutcome(outcome ContentOutcome) bool {
	switch outcome {
	case ContentOutcomeAccepted, ContentOutcomeRejectedPreacceptance, ContentOutcomeFailed, ContentOutcomeUnknown:
		return true
	default:
		return false
	}
}

func decodeCanonical(body []byte, target any) error {
	if len(body) == 0 || len(body) > 8192 {
		return errors.New("content: fence payload size invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("content: trailing fence payload data")
	}
	return nil
}

func hexDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func validContentFenceSegment(value string) bool {
	return segmentPattern.MatchString(value) && value != "." && value != ".."
}

func contentFenceObjectKey(id, kind string) string { return id + "\x00" + kind }

// LocalTestFence is a process-local test double. It has no cloud persistence
// and must never be treated as a durable production fence.
type LocalTestFence struct {
	mu                        sync.Mutex
	env, tenant, tenantDigest string
	claims                    map[string][]byte
	terminals                 map[string][]byte
}

func NewLocalTestFence(tenantID, environmentDigest string) (*LocalTestFence, error) {
	if tenantID == "" || !validContentFenceSegment(environmentDigest) {
		return nil, errors.New("content: local test fence identity is invalid")
	}
	return &LocalTestFence{env: environmentDigest, tenant: tenantID, tenantDigest: hexDigest([]byte(tenantID)), claims: map[string][]byte{}, terminals: map[string][]byte{}}, nil
}

func (f *LocalTestFence) Identity(tenantID string) (string, string, error) {
	if f == nil || tenantID == "" || tenantID != f.tenant {
		return "", "", errors.New("content: fence tenant mismatch")
	}
	return f.env, f.tenantDigest, nil
}
func (f *LocalTestFence) Durable() bool { return false }
func (f *LocalTestFence) CreateClaim(ctx context.Context, claim ContentClaim) (string, error) {
	if err := checkLocalFenceContext(ctx); err != nil {
		return "", err
	}
	body, err := CanonicalContentClaim(claim)
	if err != nil {
		return "", err
	}
	if claim.EnvironmentDigest != f.env || claim.TenantDigest != f.tenantDigest {
		return "", errors.New("content: fence claim identity mismatch")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := contentFenceObjectKey(claim.PlacementIntentID, "claim")
	if _, exists := f.claims[key]; exists {
		return "", errors.New("content: claim fence already exists")
	}
	f.claims[key] = append([]byte(nil), body...)
	return hexDigest(body), nil
}
func (f *LocalTestFence) VerifyClaim(ctx context.Context, claim ContentClaim, expectedDigest string) error {
	if err := checkLocalFenceContext(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	body := append([]byte(nil), f.claims[contentFenceObjectKey(claim.PlacementIntentID, "claim")]...)
	f.mu.Unlock()
	canonical, err := CanonicalContentClaim(claim)
	if err != nil || len(body) == 0 || !bytes.Equal(body, canonical) || hexDigest(body) != expectedDigest {
		return errors.New("content: claim fence verification failed")
	}
	return nil
}
func (f *LocalTestFence) CreateTerminal(ctx context.Context, claim ContentClaim, terminal ContentTerminal) (string, error) {
	if err := checkLocalFenceContext(ctx); err != nil {
		return "", err
	}
	if err := f.VerifyClaim(ctx, claim, terminal.ClaimDigest); err != nil {
		return "", err
	}
	body, err := CanonicalContentTerminal(claim, terminal)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := contentFenceObjectKey(claim.PlacementIntentID, "terminal")
	if _, exists := f.terminals[key]; exists {
		return "", errors.New("content: terminal fence already exists")
	}
	f.terminals[key] = append([]byte(nil), body...)
	return hexDigest(body), nil
}
func (f *LocalTestFence) VerifyTerminal(ctx context.Context, claim ContentClaim, terminal ContentTerminal, expectedDigest string) error {
	if err := checkLocalFenceContext(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	body := append([]byte(nil), f.terminals[contentFenceObjectKey(claim.PlacementIntentID, "terminal")]...)
	f.mu.Unlock()
	want, err := CanonicalContentTerminal(claim, terminal)
	if err != nil || len(body) == 0 || !bytes.Equal(body, want) || hexDigest(body) != expectedDigest {
		return errors.New("content: terminal fence verification failed")
	}
	return nil
}
func (f *LocalTestFence) ReadVerifiedTerminal(ctx context.Context, claim ContentClaim, expectedDigest string) (ContentTerminal, string, error) {
	if err := checkLocalFenceContext(ctx); err != nil {
		return ContentTerminal{}, "", err
	}
	if _, _, err := f.Identity(f.tenant); err != nil || claim.EnvironmentDigest != f.env || claim.TenantDigest != f.tenantDigest {
		return ContentTerminal{}, "", errors.New("content: local terminal fence identity mismatch")
	}
	claimBody, err := CanonicalContentClaim(claim)
	if err != nil || f.VerifyClaim(ctx, claim, hexDigest(claimBody)) != nil {
		return ContentTerminal{}, "", errors.New("content: local terminal fence claim verification failed")
	}
	if expectedDigest != "" && !digestPattern.MatchString(expectedDigest) {
		return ContentTerminal{}, "", errors.New("content: expected terminal digest is invalid")
	}
	f.mu.Lock()
	body, exists := f.terminals[contentFenceObjectKey(claim.PlacementIntentID, "terminal")]
	body = append([]byte(nil), body...)
	f.mu.Unlock()
	if !exists {
		return ContentTerminal{}, "", ErrContentFenceObjectNotFound
	}
	digest := hexDigest(body)
	if expectedDigest != "" && digest != expectedDigest {
		return ContentTerminal{}, "", errors.New("content: terminal fence digest mismatch")
	}
	terminal, err := ValidateContentTerminalJSON(body, claim)
	return terminal, digest, err
}
func (f *LocalTestFence) CaptureHighWater(ctx context.Context, tenantID string) (ContentFenceHighWater, error) {
	if err := checkLocalFenceContext(ctx); err != nil {
		return ContentFenceHighWater{}, err
	}
	if _, _, err := f.Identity(tenantID); err != nil {
		return ContentFenceHighWater{}, err
	}
	return ContentFenceHighWater{EnvironmentDigest: f.env, TenantDigest: f.tenantDigest, CapturedAt: time.Now().UTC().Format(time.RFC3339Nano), Authority: "local_test"}, nil
}
func (f *LocalTestFence) Scan(ctx context.Context, req ContentFenceScanRequest) ([]ContentFenceObject, error) {
	if err := checkLocalFenceContext(ctx); err != nil {
		return nil, err
	}
	if req.HighWater.Authority != "local_test" || req.HighWater.EnvironmentDigest != f.env || req.HighWater.TenantDigest != f.tenantDigest || req.TenantID != f.tenant {
		return nil, errors.New("content: local test fence high-water binding invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, req.HighWater.CapturedAt); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	objects := make([]ContentFenceObject, 0, len(f.claims)+len(f.terminals))
	for _, set := range []struct {
		kind   string
		values map[string][]byte
	}{{"claim", f.claims}, {"terminal", f.terminals}} {
		for key, body := range set.values {
			parts := strings.Split(key, "\x00")
			objects = append(objects, ContentFenceObject{Name: contentFencePath(f.env, f.tenantDigest, parts[0], set.kind), PlacementIntentID: parts[0], Kind: set.kind, Digest: hexDigest(body), EnvironmentDigest: f.env, TenantDigest: f.tenantDigest, LastModified: time.Now().UTC(), Body: append([]byte(nil), body...)})
		}
	}
	return objects, nil
}

var _ ContentFence = (*LocalTestFence)(nil)

func checkLocalFenceContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("content: fence context is required")
	}
	return ctx.Err()
}

func contentFencePath(env, tenantDigest, id, kind string) string {
	return fmt.Sprintf("env/%s/tenant/%s/content_placement_fences/%s/%s.json", env, tenantDigest, id, kind)
}
