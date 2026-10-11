package content

// This file is the bounded public-ingress projection for the published v1
// content-placement ingest request. It deliberately performs no authorization,
// ownership lookup, parsing, extraction, or workflow action.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/identity"
)

const (
	contentIngressMaxBytes           = 1_048_576
	contentIngressMaxSourceBytes     = 786_432
	contentIngressMaxTextSourceBytes = 262_144
)

// ContentIngestProjection contains the private persistence input and the two
// separate digests required by the intake service. CanonicalRequest omits only
// idempotency_key; every other supplied public field, including explicit nulls,
// remains part of RequestDigest.
type ContentIngestProjection struct {
	Input             assurance.ContentIngest
	CanonicalRequest  []byte
	RequestDigest     assurance.Digest
	IdempotencyDigest assurance.Digest
}

type contentIngestDTO struct {
	Phase                        string              `json:"phase"`
	IdempotencyKey               string              `json:"idempotency_key"`
	Source                       *contentSourceDTO   `json:"source,omitempty"`
	ExistingSourceArtifactID     string              `json:"existing_source_artifact_id,omitempty"`
	ExpectedExistingSourceSHA256 string              `json:"expected_existing_source_sha256,omitempty"`
	Origin                       contentOriginDTO    `json:"origin"`
	WorkflowContext              *contentWorkflowDTO `json:"workflow_context,omitempty"`
	VerifiedEvidenceRefs         []string            `json:"verified_evidence_refs,omitempty"`
	ExtractedItems               []contentItemDTO    `json:"extracted_items"`
	DraftArtifacts               []contentDraftDTO   `json:"draft_artifacts"`
}

type contentSourceDTO struct {
	SourceText     *string `json:"source_text,omitempty"`
	SourceBytesB64 *string `json:"source_bytes_base64,omitempty"`
	MediaType      string  `json:"media_type"`
	Filename       *string `json:"filename,omitempty"`
}

type contentOriginDTO struct {
	Kind  string  `json:"kind"`
	Label *string `json:"label,omitempty"`
}

type contentWorkflowDTO struct {
	WorkflowID string `json:"workflow_id"`
	StepID     string `json:"step_id"`
	BindingID  string `json:"binding_id"`
}

type contentLocationDTO struct {
	Page      *int64  `json:"page,omitempty"`
	Table     *string `json:"table,omitempty"`
	Segment   *string `json:"segment,omitempty"`
	StartByte *int64  `json:"start_byte,omitempty"`
	EndByte   *int64  `json:"end_byte,omitempty"`
}

type contentProvenanceDTO struct {
	contentLocationDTO
	Availability string `json:"availability"`
}

type contentSegmentDTO struct {
	contentLocationDTO
	Label string `json:"label"`
}

// RawMessage fields preserve the difference between omitted interpretation
// hints and explicit JSON null through immutable metadata serialization.
type contentInterpretationDTO struct {
	Period       json.RawMessage `json:"period,omitempty"`
	Currency     json.RawMessage `json:"currency,omitempty"`
	Unit         json.RawMessage `json:"unit,omitempty"`
	Scale        json.RawMessage `json:"scale,omitempty"`
	PercentBasis json.RawMessage `json:"percent_basis,omitempty"`
	Precision    json.RawMessage `json:"precision,omitempty"`
}

type contentItemDTO struct {
	LocalID        string                    `json:"local_id"`
	Text           string                    `json:"text"`
	KindHint       string                    `json:"kind_hint"`
	Interpretation *contentInterpretationDTO `json:"interpretation,omitempty"`
	Provenance     *contentProvenanceDTO     `json:"provenance,omitempty"`
	Segments       []contentSegmentDTO       `json:"segments"`
}

type contentDraftDTO struct {
	LocalID              string                    `json:"local_id"`
	Text                 string                    `json:"text"`
	ItemLocalIDs         []string                  `json:"item_local_ids,omitempty"`
	VerifiedEvidenceRefs []string                  `json:"verified_evidence_refs,omitempty"`
	Interpretation       *contentInterpretationDTO `json:"interpretation,omitempty"`
	Origin               contentOriginDTO          `json:"origin"`
}

// DecodeContentIngest validates a bounded JSON ingest request and projects it
// onto the assurance persistence DTO. Source bytes are always supplied inline;
// existing_source_artifact_id is only an ownership-checked re-ingest hint and
// never causes this adapter to fetch or trust a caller-provided external URI.
func DecodeContentIngest(raw []byte) (ContentIngestProjection, error) {
	if len(raw) == 0 || len(raw) > contentIngressMaxBytes || !utf8.Valid(raw) {
		return ContentIngestProjection{}, errors.New("content ingress: request is empty, oversized, or not UTF-8")
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return ContentIngestProjection{}, err
	}
	if err := validateIngressNullSemantics(raw); err != nil {
		return ContentIngestProjection{}, err
	}
	if err := requireIngressMembers(raw); err != nil {
		return ContentIngestProjection{}, err
	}
	if err := validateOptionalIngressMembers(raw); err != nil {
		return ContentIngestProjection{}, err
	}
	var dto contentIngestDTO
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&dto); err != nil {
		return ContentIngestProjection{}, fmt.Errorf("content ingress: invalid closed ingest request: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return ContentIngestProjection{}, errors.New("content ingress: request must contain exactly one JSON object")
	}
	if err := validateContentIngestDTO(dto); err != nil {
		return ContentIngestProjection{}, err
	}

	input, err := projectContentIngest(dto)
	if err != nil {
		return ContentIngestProjection{}, err
	}
	canonical, err := canonicalPublicIngestWithoutIdempotencyKey(raw)
	if err != nil {
		return ContentIngestProjection{}, err
	}
	return ContentIngestProjection{
		Input: input, CanonicalRequest: canonical, RequestDigest: assurance.HashBytes(canonical),
		IdempotencyDigest: assurance.DigestIdempotencyKey(dto.IdempotencyKey),
	}, nil
}

func validateContentIngestDTO(d contentIngestDTO) error {
	if d.Phase != "ingest" || !validIngressKey(d.IdempotencyKey) || len(d.ExtractedItems) > 64 || len(d.DraftArtifacts) > 8 {
		return errors.New("content ingress: phase, idempotency key, or collection bounds are invalid")
	}
	if !validIngressOrigin(d.Origin) {
		return errors.New("content ingress: origin is invalid")
	}
	if d.Source == nil {
		if d.WorkflowContext == nil || len(d.VerifiedEvidenceRefs) == 0 || len(d.ExtractedItems) != 0 || len(d.DraftArtifacts) == 0 {
			return errors.New("content ingress: source-free ingest requires workflow, verified evidence, and evidence-only drafts")
		}
	} else {
		if err := validateIngressSource(*d.Source); err != nil {
			return err
		}
	}
	if d.ExistingSourceArtifactID != "" && (d.Source == nil || d.ExpectedExistingSourceSHA256 == "") || d.ExpectedExistingSourceSHA256 != "" && (d.Source == nil || d.ExistingSourceArtifactID == "") {
		return errors.New("content ingress: existing source re-ingest requires inline source bytes, artifact ID, and expected digest")
	}
	if d.ExpectedExistingSourceSHA256 != "" && !validIngressSHA256(d.ExpectedExistingSourceSHA256) {
		return errors.New("content ingress: expected existing source digest is invalid")
	}
	if d.ExistingSourceArtifactID != "" && !validIngressOpaqueID(d.ExistingSourceArtifactID) {
		return errors.New("content ingress: existing source artifact ID is invalid")
	}
	if d.WorkflowContext != nil && (!validIngressOpaqueID(d.WorkflowContext.WorkflowID) || !validIngressOpaqueID(d.WorkflowContext.StepID) || !validIngressOpaqueID(d.WorkflowContext.BindingID)) {
		return errors.New("content ingress: workflow context is invalid")
	}
	if err := validateIngressEvidenceRefs(d.VerifiedEvidenceRefs); err != nil {
		return err
	}
	if d.VerifiedEvidenceRefs != nil && len(d.VerifiedEvidenceRefs) == 0 {
		return errors.New("content ingress: top-level evidence references, when present, must be nonempty")
	}
	if len(d.VerifiedEvidenceRefs) > 0 && d.WorkflowContext == nil {
		return errors.New("content ingress: verified evidence references require workflow context")
	}
	itemIDs := make(map[string]struct{}, len(d.ExtractedItems))
	for _, item := range d.ExtractedItems {
		if !validIngressLocalID(item.LocalID) || !validIngressText(item.Text, 16384) || !oneOf(item.KindHint, "text", "number", "date", "unknown") || len(item.Segments) > 16 {
			return errors.New("content ingress: extracted item is invalid or exceeds bounds")
		}
		if _, exists := itemIDs[item.LocalID]; exists {
			return errors.New("content ingress: extracted item local IDs must be unique")
		}
		itemIDs[item.LocalID] = struct{}{}
		if item.Interpretation != nil {
			if err := validateIngressInterpretation(*item.Interpretation); err != nil {
				return err
			}
		}
		if item.Provenance != nil {
			if err := validateIngressProvenance(*item.Provenance); err != nil {
				return err
			}
		}
		for _, segment := range item.Segments {
			if !validIngressText(segment.Label, 128) {
				return errors.New("content ingress: segment label is invalid")
			}
			if err := validateIngressLocation(segment.contentLocationDTO, true); err != nil {
				return err
			}
		}
	}
	draftIDs := make(map[string]struct{}, len(d.DraftArtifacts))
	for _, draft := range d.DraftArtifacts {
		if !validIngressLocalID(draft.LocalID) || !validIngressText(draft.Text, 32768) || !validIngressOrigin(draft.Origin) || len(draft.ItemLocalIDs) > 64 || len(draft.VerifiedEvidenceRefs) > 16 || len(draft.ItemLocalIDs) == 0 && len(draft.VerifiedEvidenceRefs) == 0 {
			return errors.New("content ingress: draft is invalid or lacks candidate/evidence lineage")
		}
		if draft.ItemLocalIDs != nil && len(draft.ItemLocalIDs) == 0 {
			return errors.New("content ingress: draft item references, when present, must be nonempty")
		}
		if _, exists := draftIDs[draft.LocalID]; exists {
			return errors.New("content ingress: draft local IDs must be unique")
		}
		draftIDs[draft.LocalID] = struct{}{}
		seen := make(map[string]struct{}, len(draft.ItemLocalIDs))
		for _, ref := range draft.ItemLocalIDs {
			if _, ok := itemIDs[ref]; !ok {
				return errors.New("content ingress: draft references an unknown extracted item")
			}
			if _, ok := seen[ref]; ok {
				return errors.New("content ingress: draft item references must be unique")
			}
			seen[ref] = struct{}{}
		}
		if err := validateIngressEvidenceRefs(draft.VerifiedEvidenceRefs); err != nil {
			return err
		}
		if draft.VerifiedEvidenceRefs != nil && len(draft.VerifiedEvidenceRefs) == 0 {
			return errors.New("content ingress: draft evidence references, when present, must be nonempty")
		}
		for _, ref := range draft.VerifiedEvidenceRefs {
			if !containsString(d.VerifiedEvidenceRefs, ref) {
				return errors.New("content ingress: draft evidence reference is not in the authorized input set")
			}
		}
		if draft.Interpretation != nil {
			if err := validateIngressInterpretation(*draft.Interpretation); err != nil {
				return err
			}
		}
		if !validIngressOrigin(draft.Origin) {
			return errors.New("content ingress: draft origin is invalid")
		}
	}
	if d.Source == nil {
		for _, draft := range d.DraftArtifacts {
			if len(draft.VerifiedEvidenceRefs) == 0 || len(draft.ItemLocalIDs) != 0 {
				return errors.New("content ingress: source-free drafts must cite only verified evidence")
			}
		}
	}
	return nil
}

func validateIngressSource(s contentSourceDTO) error {
	if (s.SourceText == nil) == (s.SourceBytesB64 == nil) || !validIngressText(s.MediaType, 128) || s.Filename != nil && !validIngressTextAllowEmpty(*s.Filename, 255) {
		return errors.New("content ingress: source must contain exactly one inline source representation and bounded metadata")
	}
	var source []byte
	if s.SourceText != nil {
		if !validIngressText(*s.SourceText, contentIngressMaxTextSourceBytes) {
			return errors.New("content ingress: source_text must be nonempty valid UTF-8 within bounds")
		}
		source = []byte(*s.SourceText)
	} else {
		encoded := *s.SourceBytesB64
		if len(encoded) < 4 || len(encoded) > 1_048_576 {
			return errors.New("content ingress: base64 source exceeds encoded bounds")
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil || base64.StdEncoding.EncodeToString(decoded) != encoded {
			return errors.New("content ingress: base64 source must use canonical padded encoding")
		}
		source = decoded
	}
	if len(source) == 0 || len(source) > contentIngressMaxSourceBytes {
		return errors.New("content ingress: decoded source is empty or exceeds byte bounds")
	}
	return nil
}

func validateIngressInterpretation(i contentInterpretationDTO) error {
	for name, raw := range map[string]json.RawMessage{"period": i.Period, "currency": i.Currency, "unit": i.Unit, "scale": i.Scale, "percent_basis": i.PercentBasis, "precision": i.Precision} {
		if len(raw) == 0 {
			continue
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		switch name {
		case "period":
			if !validRawString(raw, 128) {
				return errors.New("content ingress: interpretation period is invalid")
			}
		case "currency":
			if !validRawString(raw, 16) {
				return errors.New("content ingress: interpretation currency is invalid")
			}
		case "unit":
			if !validRawString(raw, 64) {
				return errors.New("content ingress: interpretation unit is invalid")
			}
		case "scale":
			if !validRawEnum(raw, "ones", "thousands", "millions", "billions") {
				return errors.New("content ingress: interpretation scale is invalid")
			}
		case "percent_basis":
			if !validRawEnum(raw, "ratio", "percent") {
				return errors.New("content ingress: interpretation percent basis is invalid")
			}
		case "precision":
			var n json.Number
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			if err := dec.Decode(&n); err != nil {
				return errors.New("content ingress: interpretation precision is invalid")
			}
			value, err := n.Int64()
			if err != nil || value < 0 || value > 15 {
				return errors.New("content ingress: interpretation precision is invalid")
			}
		}
	}
	return nil
}

func validateIngressProvenance(p contentProvenanceDTO) error {
	if !oneOf(p.Availability, "available", "unavailable") {
		return errors.New("content ingress: provenance availability is invalid")
	}
	if err := validateIngressLocation(p.contentLocationDTO, p.Availability == "available"); err != nil {
		return err
	}
	if p.Availability == "available" && p.Page == nil && p.Table == nil && p.Segment == nil && p.StartByte == nil {
		return errors.New("content ingress: available provenance requires a location")
	}
	if p.Availability == "unavailable" && (p.Page != nil || p.Table != nil || p.Segment != nil || p.StartByte != nil || p.EndByte != nil) {
		return errors.New("content ingress: unavailable provenance cannot claim coordinates")
	}
	return nil
}

func validateIngressLocation(l contentLocationDTO, allowEmpty bool) error {
	if l.Page != nil && (*l.Page < 1 || *l.Page > 2147483647) || l.Table != nil && !validIngressText(*l.Table, 128) || l.Segment != nil && !validIngressText(*l.Segment, 128) {
		return errors.New("content ingress: source location is invalid")
	}
	if (l.StartByte == nil) != (l.EndByte == nil) || l.StartByte != nil && (*l.StartByte < 0 || *l.EndByte < 1 || *l.StartByte >= *l.EndByte || *l.EndByte > 786432) {
		return errors.New("content ingress: byte offsets must be paired, ordered, and bounded")
	}
	if !allowEmpty && l.Page == nil && l.Table == nil && l.Segment == nil && l.StartByte == nil {
		return errors.New("content ingress: segment requires a location")
	}
	return nil
}

func projectContentIngest(d contentIngestDTO) (assurance.ContentIngest, error) {
	var source []byte
	media, filename := "", ""
	if d.Source != nil {
		media = d.Source.MediaType
		if d.Source.Filename != nil {
			filename = *d.Source.Filename
		}
		if d.Source.SourceText != nil {
			source = []byte(*d.Source.SourceText)
		} else {
			decoded, err := base64.StdEncoding.Strict().DecodeString(*d.Source.SourceBytesB64)
			if err != nil {
				return assurance.ContentIngest{}, err
			}
			source = decoded
		}
	}
	metadataValues := map[string]any{"origin": d.Origin}
	metadataValues["projection_trust"] = "caller_unverified"
	if d.Source != nil {
		metadataValues["source_representation"] = "source_text"
		if d.Source.SourceBytesB64 != nil {
			metadataValues["source_representation"] = "source_bytes_base64"
		}
		metadataValues["source_filename_present"] = d.Source.Filename != nil
		if d.Source.Filename != nil {
			metadataValues["source_filename_value"] = *d.Source.Filename
		}
	}
	if d.WorkflowContext != nil {
		metadataValues["caller_workflow_context"] = d.WorkflowContext
	}
	if d.VerifiedEvidenceRefs != nil {
		metadataValues["claimed_verified_evidence_refs"] = d.VerifiedEvidenceRefs
	}
	metadata, err := metadataObject(metadataValues)
	if err != nil {
		return assurance.ContentIngest{}, err
	}
	input := assurance.ContentIngest{SourceBytes: source, MediaType: media, Filename: filename, MetadataBLOB: metadata, ExistingSourceArtifactID: d.ExistingSourceArtifactID, ExpectedExistingSourceSHA256: d.ExpectedExistingSourceSHA256, Items: make([]assurance.ContentItemInput, 0, len(d.ExtractedItems)), Drafts: make([]assurance.ContentDraftInput, 0, len(d.DraftArtifacts))}
	for _, item := range d.ExtractedItems {
		values := map[string]any{"kind_hint": item.KindHint, "segments": item.Segments, "projection_trust": "caller_unverified"}
		if item.Interpretation != nil {
			values["interpretation"] = item.Interpretation
		}
		if item.Provenance != nil {
			values["caller_provenance"] = item.Provenance
		}
		meta, err := metadataObject(values)
		if err != nil {
			return assurance.ContentIngest{}, err
		}
		input.Items = append(input.Items, assurance.ContentItemInput{LocalID: item.LocalID, Text: item.Text, MetadataBLOB: meta})
	}
	for _, draft := range d.DraftArtifacts {
		values := map[string]any{"origin": draft.Origin, "projection_trust": "caller_unverified"}
		if draft.VerifiedEvidenceRefs != nil {
			values["claimed_verified_evidence_refs"] = draft.VerifiedEvidenceRefs
		}
		if draft.Interpretation != nil {
			values["interpretation"] = draft.Interpretation
		}
		meta, err := metadataObject(values)
		if err != nil {
			return assurance.ContentIngest{}, err
		}
		input.Drafts = append(input.Drafts, assurance.ContentDraftInput{LocalID: draft.LocalID, Text: draft.Text, ItemLocalIDs: append([]string(nil), draft.ItemLocalIDs...), MetadataBLOB: meta})
	}
	return input, nil
}

func metadataObject(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return assurance.CanonicalJSONBytes(raw)
}
func validIngressKey(s string) bool {
	if len(s) < 16 || len(s) > 256 {
		return false
	}
	for _, r := range s {
		if r < '!' || r > '~' {
			return false
		}
	}
	return true
}
func validIngressSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
func validIngressOpaqueID(s string) bool {
	if len(s) < 1 || len(s) > 128 || !asciiAlphaNum(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !asciiAlphaNum(c) && !strings.ContainsRune("._:-", rune(c)) {
			return false
		}
	}
	return true
}
func validIngressLocalID(s string) bool {
	if len(s) < 1 || len(s) > 64 || !asciiAlphaNum(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !asciiAlphaNum(c) && !strings.ContainsRune("._:-", rune(c)) {
			return false
		}
	}
	return true
}
func asciiAlphaNum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
func validIngressText(s string, max int) bool {
	return validIngressTextAllowEmpty(s, max) && len(s) > 0
}
func validIngressTextAllowEmpty(s string, max int) bool {
	return utf8.ValidString(s) && len([]byte(s)) <= max
}
func validIngressOrigin(o contentOriginDTO) bool {
	return oneOf(o.Kind, "analyst", "agent_flow", "copilot", "other") && (o.Label == nil || validIngressTextAllowEmpty(*o.Label, 128))
}
func validRawString(raw json.RawMessage, max int) bool {
	var s string
	return json.Unmarshal(raw, &s) == nil && validIngressText(s, max)
}
func validRawEnum(raw json.RawMessage, values ...string) bool {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return false
	}
	return oneOf(s, values...)
}
func oneOf(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}
func validateIngressEvidenceRefs(refs []string) error {
	if len(refs) > 16 {
		return errors.New("content ingress: evidence reference count exceeds bounds")
	}
	seen := map[string]struct{}{}
	for _, ref := range refs {
		if !validIngressOpaqueID(ref) {
			return errors.New("content ingress: evidence reference is invalid")
		}
		if _, ok := seen[ref]; ok {
			return errors.New("content ingress: evidence references must be unique")
		}
		seen[ref] = struct{}{}
	}
	return nil
}
func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func canonicalPublicIngestWithoutIdempotencyKey(raw []byte) ([]byte, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("content ingress: request must be a JSON object")
	}
	delete(object, "idempotency_key")
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	return assurance.CanonicalJSONBytes(encoded)
}

// PublicContentIngestRequestDigest returns the stable public request digest;
// the raw idempotency key is excluded and hashed separately by the projection.
func PublicContentIngestRequestDigest(raw []byte) (assurance.Digest, error) {
	projection, err := DecodeContentIngest(raw)
	if err != nil {
		return assurance.Digest{}, err
	}
	return projection.RequestDigest, nil
}

// IngestPublic is the local raw-JSON service seam for the public projection.
// The request and idempotency digests are computed here; callers cannot supply
// or override either digest. Workflow/evidence references remain unverified
// until their separately authorized 4.3 capability is available.
func (s *IntakeService) IngestPublic(ctx context.Context, raw []byte, auditID string, now time.Time) (assurance.ContentIngestResult, error) {
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok || principal.TokenType != identity.TokenTypeDelegated || principal.UsedSubjectFallback || !principal.HasPermission(identity.PermissionContentIngest) || !isCanonicalUUID(principal.TenantID) || !isCanonicalUUID(principal.ObjectID) {
		return assurance.ContentIngestResult{}, &assurance.Error{Code: "forbidden", Message: "trusted delegated content.ingest capability required"}
	}
	if s == nil || s.store == nil {
		return assurance.ContentIngestResult{}, &assurance.Error{Code: "invalid_request", Message: "content intake store is required"}
	}
	projection, err := DecodeContentIngest(raw)
	if err != nil {
		return assurance.ContentIngestResult{}, &assurance.Error{Code: "invalid_request", Message: err.Error()}
	}
	now = now.UTC()
	request := assurance.ReservationRequest{ActorID: principal.AuditActor(), Tool: contentIngestTool, Action: contentIngestAction, IdempotencyDigest: projection.IdempotencyDigest, RequestDigest: projection.RequestDigest, RetentionClass: "signed_content_ingest"}
	prior, found, err := s.store.LookupReservation(ctx, request)
	if err != nil {
		return assurance.ContentIngestResult{}, err
	}
	if found {
		return contentIngestReplay(prior)
	}
	if s.resolver == nil {
		return assurance.ContentIngestResult{}, &assurance.Error{Code: "invalid_request", Message: "configured signed content policy resolver is required for a fresh ingest"}
	}
	if len(projection.Input.SourceBytes) == 0 || hasUnverifiedWorkflowOrEvidence(raw) {
		return assurance.ContentIngestResult{}, &assurance.Error{Code: "capability_unverified", Message: "refs-only, workflow, and evidence-bound ingest are unavailable until their server-owned capability is implemented"}
	}
	if err := rejectIntakeAuthorityMetadata(projection.Input); err != nil {
		return assurance.ContentIngestResult{}, err
	}
	if err := assurance.ValidateContentIngest(projection.Input); err != nil {
		return assurance.ContentIngestResult{}, err
	}
	if auditID == "" {
		return assurance.ContentIngestResult{}, &assurance.Error{Code: "invalid_request", Message: "content ingest audit ID is required"}
	}
	finalizer, ok := any(s.store).(publicDigestContentIngestFinalizer)
	if !ok {
		return assurance.ContentIngestResult{}, &assurance.Error{Code: "capability_unverified", Message: "assurance store does not yet support binding the canonical public request digest"}
	}
	policy, err := s.store.ResolveContentIntakePolicySnapshot(ctx, s.resolver, now)
	if err != nil {
		return assurance.ContentIngestResult{}, err
	}
	if policy.Retention.IdempotencyLifetimeSeconds < int64(minimumContentIdempotencyLifetime/time.Second) {
		return assurance.ContentIngestResult{}, &assurance.Error{Code: "content_policy_invalid", Message: "signed idempotency lifetime is shorter than the reservation lease"}
	}
	request.RetainUntil = now.Add(time.Duration(policy.Retention.IdempotencyLifetimeSeconds) * time.Second)
	reservation, err := s.store.Reserve(ctx, request, now)
	if err != nil {
		return assurance.ContentIngestResult{}, err
	}
	if reservation.Disposition != assurance.ReservationOwned {
		return contentIngestReplay(reservation)
	}
	return finalizer.FinalizePolicyBoundContentIngestWithRequestDigest(ctx, reservation, projection.Input, projection.RequestDigest, s.resolver, auditID, now)
}

type publicDigestContentIngestFinalizer interface {
	FinalizePolicyBoundContentIngestWithRequestDigest(context.Context, assurance.ReservationResult, assurance.ContentIngest, assurance.Digest, *assurance.ContentPolicyResolver, string, time.Time) (assurance.ContentIngestResult, error)
}

func hasUnverifiedWorkflowOrEvidence(raw []byte) bool {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return true
	}
	if _, ok := top["workflow_context"]; ok {
		return true
	}
	if _, ok := top["verified_evidence_refs"]; ok {
		return true
	}
	var drafts []struct {
		VerifiedEvidenceRefs []string `json:"verified_evidence_refs"`
	}
	if json.Unmarshal(top["draft_artifacts"], &drafts) != nil {
		return true
	}
	for _, draft := range drafts {
		if len(draft.VerifiedEvidenceRefs) > 0 {
			return true
		}
	}
	return false
}

func requireIngressMembers(raw []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return errors.New("content ingress: request must be an object")
	}
	for _, key := range []string{"phase", "idempotency_key", "origin", "extracted_items", "draft_artifacts"} {
		if value, ok := top[key]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("content ingress: required member %q is missing or null", key)
		}
	}
	if source, ok := top["source"]; ok && bytes.Equal(bytes.TrimSpace(source), []byte("null")) {
		return errors.New("content ingress: source cannot be null")
	}
	if source, ok := top["source"]; ok && !bytes.Equal(bytes.TrimSpace(source), []byte("null")) {
		var obj map[string]json.RawMessage
		_ = json.Unmarshal(source, &obj)
		if obj == nil {
			return errors.New("content ingress: source must be an object")
		}
		if _, text := obj["source_text"]; !text {
			if _, b64 := obj["source_bytes_base64"]; !b64 {
				return errors.New("content ingress: source representation is required")
			}
		}
		for _, key := range []string{"media_type"} {
			if v, ok := obj[key]; !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return fmt.Errorf("content ingress: source member %q is missing or null", key)
			}
		}
	}
	for _, key := range []string{"extracted_items", "draft_artifacts"} {
		var arr []json.RawMessage
		if err := json.Unmarshal(top[key], &arr); err != nil || arr == nil {
			return fmt.Errorf("content ingress: %s must be an array", key)
		}
		for _, entry := range arr {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(entry, &obj); err != nil || obj == nil {
				return fmt.Errorf("content ingress: %s entries must be objects", key)
			}
			required := []string{"local_id", "text"}
			if key == "extracted_items" {
				required = append(required, "kind_hint", "segments")
			} else {
				required = append(required, "origin")
			}
			for _, field := range required {
				v, ok := obj[field]
				if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
					return fmt.Errorf("content ingress: %s member %q is missing or null", key, field)
				}
			}
			if key == "extracted_items" {
				var segments []json.RawMessage
				if err := json.Unmarshal(obj["segments"], &segments); err != nil || segments == nil {
					return errors.New("content ingress: item segments must be an array")
				}
			}
		}
	}
	return nil
}

func validateOptionalIngressMembers(raw []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return err
	}
	if value, ok := top["existing_source_artifact_id"]; ok {
		var id string
		if json.Unmarshal(value, &id) != nil || !validIngressOpaqueID(id) {
			return errors.New("content ingress: existing source artifact ID is invalid")
		}
	}
	if value, ok := top["expected_existing_source_sha256"]; ok {
		var hash string
		if json.Unmarshal(value, &hash) != nil || !validIngressSHA256(hash) {
			return errors.New("content ingress: expected existing source digest is invalid")
		}
	}
	var drafts []map[string]json.RawMessage
	if err := json.Unmarshal(top["draft_artifacts"], &drafts); err != nil {
		return err
	}
	for _, draft := range drafts {
		if value, ok := draft["item_local_ids"]; ok {
			var refs []string
			if json.Unmarshal(value, &refs) != nil || len(refs) == 0 {
				return errors.New("content ingress: draft item references, when present, must be a nonempty array")
			}
		}
	}
	return nil
}

func validateIngressNullSemantics(raw []byte) error {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var walk func(any, bool) error
	walk = func(current any, inInterpretation bool) error {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				if child == nil {
					if !inInterpretation || !oneOf(key, "period", "currency", "unit", "scale", "percent_basis", "precision") {
						return fmt.Errorf("content ingress: null is not allowed for member %q", key)
					}
					continue
				}
				if err := walk(child, key == "interpretation"); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range typed {
				if err := walk(child, inInterpretation); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(value, false)
}

// rejectDuplicateJSONKeys prevents ambiguous object-member interpretation
// before DTO decoding or digest projection.
func rejectDuplicateJSONKeys(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := consumeJSONValue(d); err != nil {
		return fmt.Errorf("content ingress: invalid JSON: %w", err)
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("content ingress: trailing JSON data")
	}
	return nil
}
func consumeJSONValue(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[name]; exists {
				return fmt.Errorf("duplicate object key %q", name)
			}
			seen[name] = struct{}{}
			if err := consumeJSONValue(d); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	case '[':
		for d.More() {
			if err := consumeJSONValue(d); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	default:
		return errors.New("unexpected JSON delimiter")
	}
}
