package assurance

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
)

// EvidenceStorage is the narrow storage boundary used by evidence delivery.
// References are opaque application values; implementations must not expose
// local filesystem paths to callers.
type EvidenceStorage interface {
	Put(context.Context, string, string, []byte) (string, error)
	Read(context.Context, string) ([]byte, error)
	Delete(context.Context, string) error
}

type memoryEvidenceStorage struct {
	mu    sync.RWMutex
	items map[string][]byte
}

func NewMemoryEvidenceStorage() *memoryEvidenceStorage {
	return &memoryEvidenceStorage{items: make(map[string][]byte)}
}

func (s *memoryEvidenceStorage) Put(_ context.Context, _ string, _ string, data []byte) (string, error) {
	if s == nil {
		return "", fmt.Errorf("evidence storage is unavailable")
	}
	ref := "mem:" + uuid.NewString()
	s.mu.Lock()
	s.items[ref] = append([]byte(nil), data...)
	s.mu.Unlock()
	return ref, nil
}

func (s *memoryEvidenceStorage) Read(_ context.Context, ref string) ([]byte, error) {
	s.mu.RLock()
	data, ok := s.items[ref]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrEvidenceNotFound
	}
	return append([]byte(nil), data...), nil
}

func (s *memoryEvidenceStorage) Get(ctx context.Context, ref string) ([]byte, error) {
	return s.Read(ctx, ref)
}

func (s *memoryEvidenceStorage) Delete(_ context.Context, ref string) error {
	s.mu.Lock()
	delete(s.items, ref)
	s.mu.Unlock()
	return nil
}

func VerifyStorageHash(ctx context.Context, storage EvidenceStorage, ref string, expected Digest) error {
	if storage == nil {
		return fmt.Errorf("evidence storage is unavailable")
	}
	data, err := storage.Read(ctx, ref)
	if err != nil {
		return err
	}
	if HashBytes(data) != expected {
		return fmt.Errorf("evidence artifact read-back hash mismatch")
	}
	return nil
}

type RedactionProfile string

const (
	RedactionStandard RedactionProfile = "standard"
	RedactionStrict   RedactionProfile = "strict"
)

var ErrEvidenceCommitAmbiguous = errors.New("evidence final database commit outcome is ambiguous")
var ErrEvidenceNotFound = errors.New("evidence artifact is not available")

type RedactionRecord struct {
	Profile string `json:"profile"`
	Field   string `json:"field"`
	Action  string `json:"action"`
}
type OmissionRecord struct {
	Kind   string `json:"kind"`
	Count  int    `json:"count"`
	Reason string `json:"reason"`
}
type HashVersionCoverage struct {
	HashVersion          int   `json:"hash_version"`
	FirstSeq             int64 `json:"first_seq"`
	LastSeq              int64 `json:"last_seq"`
	TenantIdentityHashed bool  `json:"tenant_identity_hashed"`
}
type AuditIntegrity struct {
	Scope                string                `json:"scope,omitempty"`
	ChainVerified        bool                  `json:"chain_verified"`
	HashVersionCoverage  []HashVersionCoverage `json:"hash_version_coverage"`
	TenantIdentityHashed bool                  `json:"tenant_identity_hashed"`
}
type AuditCheckpoint struct {
	Status         string `json:"status"`
	CheckpointID   string `json:"checkpoint_id,omitempty"`
	Sequence       int64  `json:"sequence,omitempty"`
	Hash           string `json:"hash,omitempty"`
	ExternalAnchor string `json:"external_anchor,omitempty"`
}
type AuditCompleteness struct {
	Status                     string `json:"status"`
	ExpectedCount              int    `json:"expected_count"`
	IncludedCount              int    `json:"included_count"`
	OmittedCount               int    `json:"omitted_count"`
	ExpectedEventCount         int    `json:"expected_event_count,omitempty"`
	IncludedEventCount         int    `json:"included_event_count,omitempty"`
	OmissionCount              int    `json:"omission_count,omitempty"`
	OmissionsRecorded          bool   `json:"omissions_recorded"`
	TerminalAnchor             string `json:"terminal_anchor"`
	TerminalAnchorVerified     bool   `json:"terminal_anchor_verified"`
	FinalRowDeletionDetectable bool   `json:"final_row_deletion_detectable"`
}
type AuditManifest struct {
	FirstSeq     int64             `json:"first_seq"`
	LastSeq      int64             `json:"last_seq"`
	FirstHash    string            `json:"first_hash,omitempty"`
	LastHash     string            `json:"last_hash,omitempty"`
	Integrity    AuditIntegrity    `json:"integrity"`
	Checkpoint   AuditCheckpoint   `json:"checkpoint"`
	Completeness AuditCompleteness `json:"completeness"`
	Caveats      []string          `json:"caveats"`
}
type EvidenceArtifact struct {
	ArtifactID string `json:"artifact_id"`
	Name       string `json:"name"`
	MediaType  string `json:"media_type"`
	ByteCount  int    `json:"byte_count"`
	SHA256     string `json:"sha256"`
	StorageRef string `json:"storage_ref"`
}
type Manifest = EvidenceManifest
type EvidenceManifest struct {
	ManifestID      string             `json:"manifest_id"`
	ManifestVersion int                `json:"manifest_version"`
	SubjectKind     string             `json:"subject_kind"`
	SubjectID       string             `json:"subject_id"`
	PackageHash     string             `json:"package_hash"`
	Artifacts       []EvidenceArtifact `json:"artifacts"`
	Audit           AuditManifest      `json:"audit"`
	Redactions      []RedactionRecord  `json:"redactions"`
	Omissions       []OmissionRecord   `json:"omissions"`
	CSV             *CSVMetadata       `json:"csv,omitempty"`
	ExpiresAt       string             `json:"expires_at"`
}
type EvidenceRequest struct {
	SubjectKind       string           `json:"subject_kind"`
	SubjectID         string           `json:"subject_id"`
	Format            string           `json:"format"`
	RedactionProfile  RedactionProfile `json:"redaction_profile"`
	IncludeAuditChain bool             `json:"include_audit_chain"`
	RetentionClass    string           `json:"retention_class"`
	IdempotencyKey    string           `json:"idempotency_key"`
}
type EvidenceResponse struct {
	NLAuditID          string             `json:"nl_audit_id"`
	Status             string             `json:"status"`
	EvidenceManifestID string             `json:"evidence_manifest_id"`
	ManifestVersion    int                `json:"manifest_version"`
	Manifest           EvidenceManifest   `json:"-"`
	Artifacts          []EvidenceArtifact `json:"artifacts"`
	PackageHash        string             `json:"package_hash"`
	Audit              AuditManifest      `json:"audit"`
	ExpiresAt          string             `json:"expires_at"`
}

type EvidenceExportRequest = EvidenceRequest
type EvidenceExportResponse = EvidenceResponse

const (
	EvidenceCompleted         = "completed"
	EvidenceTooLarge          = "too_large"
	EvidenceDenied            = "denied"
	EvidenceError             = "error"
	EvidenceIdempotencyReplay = "idempotency_replay"
)

func (s *Store) BuildManifest(ctx context.Context, subjectKind, subjectID string, profile RedactionProfile) (EvidenceManifest, error) {
	if profile == "" {
		profile = RedactionStandard
	}
	if profile != RedactionStandard && profile != RedactionStrict {
		return EvidenceManifest{}, domainError("redaction_profile_invalid", "redaction profile must be standard or strict")
	}
	if subjectKind == "transfer" {
		return EvidenceManifest{}, domainError("subject_unavailable", "transfer evidence is unavailable until Wave 3 transfer state exists")
	}
	if _, err := s.subjectJSON(ctx, subjectKind, subjectID); err != nil {
		return EvidenceManifest{}, err
	}
	return EvidenceManifest{ManifestID: uuid.NewString(), ManifestVersion: 2, SubjectKind: subjectKind, SubjectID: subjectID, Artifacts: []EvidenceArtifact{}, Audit: defaultAuditManifest(), Redactions: []RedactionRecord{{Profile: string(profile), Action: "allowlisted canonical subject"}}, Omissions: []OmissionRecord{}}, nil
}

func (s *Store) ExportEvidence(ctx context.Context, actorID, auditID string, request EvidenceRequest, storages ...EvidenceStorage) (EvidenceResponse, error) {
	if s == nil {
		return EvidenceResponse{}, domainError("dependency_unavailable", "assurance store is unavailable")
	}
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok || principal.TenantID == "" || principal.ObjectID == "" || !principal.HasPermission(identity.PermissionEvidenceExport) {
		return EvidenceResponse{}, domainError("strong_identity_required", "validated Entra tid/oid and evidence.export authorization are required")
	}
	if err := s.Ready(); err != nil {
		return EvidenceResponse{}, err
	}
	if err := s.requireRichAudit(); err != nil {
		return EvidenceResponse{}, err
	}
	var configuredStorage EvidenceStorage
	if request.RetentionClass == "" {
		request.RetentionClass = "long_term"
	}
	if request.Format == "" {
		request.Format = "json"
	}
	if request.Format != "json" && request.Format != "csv" {
		return EvidenceResponse{}, domainError("invalid_format", "evidence format must be json or csv")
	}
	if request.IdempotencyKey == "" || request.SubjectID == "" {
		return EvidenceResponse{}, domainError("invalid_request", "subject_id and idempotency_key are required")
	}
	if len(storages) > 1 {
		return EvidenceResponse{}, domainError("invalid_request", "only one evidence storage adapter is permitted")
	}
	profile := request.RedactionProfile
	if profile == "" {
		profile = RedactionStandard
	}
	canonicalRequest, err := CanonicalJSON(struct {
		SubjectKind  string           `json:"subject_kind"`
		SubjectID    string           `json:"subject_id"`
		Format       string           `json:"format"`
		Profile      RedactionProfile `json:"redaction_profile"`
		IncludeAudit bool             `json:"include_audit_chain"`
		Retention    string           `json:"retention_class"`
	}{request.SubjectKind, request.SubjectID, request.Format, profile, request.IncludeAuditChain, request.RetentionClass})
	if err != nil {
		return EvidenceResponse{}, err
	}
	reservation, err := s.Reserve(ctx, ReservationRequest{ActorID: actorID, Tool: "workiva_export_evidence", Action: "export", IdempotencyDigest: DigestIdempotencyKey(request.IdempotencyKey), RequestDigest: HashBytes(canonicalRequest), RetentionClass: request.RetentionClass}, time.Now().UTC())
	request.IdempotencyKey = ""
	if err != nil {
		return EvidenceResponse{}, err
	}
	switch reservation.Disposition {
	case ReservationConflict:
		return EvidenceResponse{}, domainError("idempotency_conflict", "idempotency key was already used for a different canonical request")
	case ReservationInProgress:
		e := domainError("idempotency_in_progress", "an identical request is still in progress")
		e.Retryable = true
		return EvidenceResponse{}, e
	case ReservationReplay:
		response, decodeErr := decodeEnvelope[EvidenceResponse](reservation.Envelope)
		if decodeErr != nil {
			return EvidenceResponse{}, decodeErr
		}
		response.Status = EvidenceIdempotencyReplay
		return response, nil
	case ReservationOwned:
	default:
		return EvidenceResponse{}, domainError("idempotency_state_invalid", "reservation disposition is invalid")
	}
	if request.SubjectKind == "transfer" {
		failure := domainError("subject_unavailable", "transfer evidence is unavailable until Wave 3 transfer state exists")
		_ = s.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(failure, auditID), time.Now().UTC())
		return EvidenceResponse{}, failure
	}
	configuredStorage = s.evidenceStorage
	if len(storages) == 1 {
		configuredStorage = storages[0]
	}
	if configuredStorage == nil {
		return EvidenceResponse{}, domainError("evidence_storage_unconfigured", "a durable evidence storage adapter must be explicitly configured")
	}
	storage := configuredStorage
	policy, err := s.ResolveRetentionPolicy(ctx, request.RetentionClass)
	if err != nil {
		_ = s.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(err, auditID), time.Now().UTC())
		return EvidenceResponse{}, err
	}
	profilePolicy, err := s.resolveExportProfile(ctx, request.SubjectKind, request.SubjectID, profile, request.RetentionClass)
	if err != nil {
		_ = s.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(err, auditID), time.Now().UTC())
		return EvidenceResponse{}, err
	}
	if err := s.MarkExecutionStarted(ctx, reservation.RecordID, reservation.OwnerNonce, time.Now().UTC()); err != nil {
		_ = s.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(err, auditID), time.Now().UTC())
		return EvidenceResponse{}, err
	}
	subject, err := s.subjectJSON(ctx, request.SubjectKind, request.SubjectID)
	if err != nil {
		_ = s.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(err, auditID), time.Now().UTC())
		return EvidenceResponse{}, err
	}
	redacted, err := redactCanonical(subject, profile)
	if err != nil {
		_ = s.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(err, auditID), time.Now().UTC())
		return EvidenceResponse{}, err
	}
	if profilePolicy.MaxBytes > 0 && len(redacted) > profilePolicy.MaxBytes {
		return s.sealTooLarge(ctx, reservation, auditID, "evidence exceeds the approved export profile byte bound", defaultAuditManifest(), time.Now().UTC())
	}
	if profilePolicy.MaxRows > 0 && estimateSubjectRows(redacted) > profilePolicy.MaxRows {
		return s.sealTooLarge(ctx, reservation, auditID, "evidence exceeds the approved export profile row bound", defaultAuditManifest(), time.Now().UTC())
	}
	manifest, err := s.BuildManifest(ctx, request.SubjectKind, request.SubjectID, profile)
	if err != nil {
		_ = s.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(err, auditID), time.Now().UTC())
		return EvidenceResponse{}, err
	}
	manifest.Redactions = append(manifest.Redactions, collectRedactionRecords(subject, profile)...)
	manifest.ManifestID = uuid.NewString()
	manifest.ExpiresAt = retentionExpiry(time.Now(), policy).Format(time.RFC3339Nano)
	if request.IncludeAuditChain {
		if s.auditLog == nil {
			failure := domainError("audit_unavailable", "audit-chain evidence was requested but the audit log is unavailable")
			_ = s.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(failure, auditID), time.Now().UTC())
			return EvidenceResponse{}, failure
		}
		auditIDs, linksErr := s.subjectAuditIDs(ctx, request.SubjectKind, request.SubjectID)
		if linksErr != nil {
			failure := domainError("audit_incomplete", "audit-chain evidence could not establish a bounded tenant range")
			_ = s.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(failure, auditID), time.Now().UTC())
			return EvidenceResponse{}, failure
		}
		rangeResult, rangeErr := s.auditLog.ExportLinked(ctx, auditIDs)
		if rangeErr != nil {
			failure := domainError("audit_incomplete", "audit-chain evidence could not select linked subject events")
			_ = s.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(failure, auditID), time.Now().UTC())
			return EvidenceResponse{}, failure
		}
		verification := s.auditLog.VerifyLinked(ctx, rangeResult.Entries)
		if !verification.ChainVerified {
			failure := domainError("audit_integrity_failed", "audit-chain evidence failed closed")
			_ = s.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(failure, auditID), time.Now().UTC())
			return EvidenceResponse{}, failure
		}
		coverage := make([]HashVersionCoverage, len(verification.HashVersionCoverage))
		for index, item := range verification.HashVersionCoverage {
			coverage[index] = HashVersionCoverage{HashVersion: item.HashVersion, FirstSeq: item.FirstSeq, LastSeq: item.LastSeq, TenantIdentityHashed: item.TenantIdentityHashed}
		}
		tenantIdentityHashed := true
		for _, item := range coverage {
			if !item.TenantIdentityHashed {
				tenantIdentityHashed = false
			}
		}
		caveats := append([]string(nil), verification.Caveats...)
		if !tenantIdentityHashed {
			caveats = append(caveats, "all-v1 audit rows lack tenant identity hashing")
		}
		manifest.Audit = AuditManifest{FirstSeq: rangeResult.FirstSeq, LastSeq: rangeResult.LastSeq, FirstHash: rangeResult.FirstHash, LastHash: rangeResult.LastHash, Integrity: AuditIntegrity{Scope: "included_rows_hash_continuity", ChainVerified: true, HashVersionCoverage: coverage, TenantIdentityHashed: tenantIdentityHashed}, Checkpoint: AuditCheckpoint{Status: "not_requested"}, Completeness: AuditCompleteness{Status: verification.Completeness.Status, ExpectedCount: verification.Completeness.ExpectedCount, IncludedCount: verification.Completeness.IncludedCount, OmittedCount: verification.Completeness.OmittedCount, ExpectedEventCount: verification.Completeness.ExpectedEventCount, IncludedEventCount: verification.Completeness.IncludedEventCount, OmissionCount: verification.Completeness.OmissionCount, OmissionsRecorded: verification.Completeness.OmissionsRecorded, TerminalAnchor: verification.Completeness.TerminalAnchor, TerminalAnchorVerified: verification.Completeness.TerminalAnchorVerified, FinalRowDeletionDetectable: verification.Completeness.FinalRowDeletionDetectable}, Caveats: caveats}
		checkpoint, checkpointErr := s.auditLog.VerifyCheckpoint(ctx, rangeResult.LastSeq, rangeResult.LastHash, "audit-terminal")
		if checkpointErr != nil {
			failure := domainError("checkpoint_unavailable", "audit checkpoint verification failed")
			_ = s.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(failure, auditID), time.Now().UTC())
			return EvidenceResponse{}, failure
		}
		manifest.Audit.Checkpoint = AuditCheckpoint{Status: string(checkpoint.Status), CheckpointID: checkpoint.CheckpointID, Sequence: checkpoint.Sequence, Hash: checkpoint.Hash, ExternalAnchor: checkpoint.ExternalAnchor}
		if checkpoint.Status == audit.CheckpointVerified && verification.Completeness.OmissionsRecorded && verification.Completeness.OmittedCount == 0 {
			manifest.Audit.Completeness.Status = "complete"
			manifest.Audit.Completeness.TerminalAnchor = "verified"
			manifest.Audit.Completeness.TerminalAnchorVerified = true
			manifest.Audit.Completeness.FinalRowDeletionDetectable = true
		} else {
			manifest.Audit.Completeness.Status = "unknown"
			manifest.Audit.Completeness.TerminalAnchorVerified = false
			manifest.Audit.Completeness.FinalRowDeletionDetectable = false
		}
	}
	createdRefs := []string{}
	failMaterialization := func(failure error) (EvidenceResponse, error) {
		cleanupEvidenceRefs(ctx, s, storage, createdRefs, reservation.RecordID, failure)
		_ = s.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(failure, auditID), time.Now().UTC())
		return EvidenceResponse{}, failure
	}
	if err := s.RenewReservation(ctx, reservation.RecordID, reservation.OwnerNonce, time.Now().UTC()); err != nil {
		return failMaterialization(err)
	}
	if err := s.RenewReservation(ctx, reservation.RecordID, reservation.OwnerNonce, time.Now().UTC()); err != nil {
		return failMaterialization(err)
	}
	subjectRef, err := storage.Put(ctx, "subject.json", "application/json", redacted)
	if err != nil {
		return failMaterialization(err)
	}
	createdRefs = append(createdRefs, subjectRef)
	if err := s.RenewReservation(ctx, reservation.RecordID, reservation.OwnerNonce, time.Now().UTC()); err != nil {
		return failMaterialization(err)
	}
	if err := VerifyStorageHash(ctx, storage, subjectRef, HashBytes(redacted)); err != nil {
		return failMaterialization(err)
	}
	subjectArtifact := EvidenceArtifact{ArtifactID: uuid.NewString(), Name: "subject.json", MediaType: "application/json", ByteCount: len(redacted), SHA256: digestHex(HashBytes(redacted)), StorageRef: subjectRef}
	artifacts := []EvidenceArtifact{subjectArtifact}
	var csvData []byte
	if request.Format == "csv" {
		csvData = deterministicCSV(redacted)
		manifest.CSV = &CSVMetadata{FormulaEscaping: "prefix_formula_values_with_apostrophe", Authoritative: false}
		manifest.Omissions = append(manifest.Omissions, OmissionRecord{Kind: "csv", Count: 1, Reason: "cell_formula_values_prefixed_with_apostrophe; JSON remains authoritative"})
		if err := s.RenewReservation(ctx, reservation.RecordID, reservation.OwnerNonce, time.Now().UTC()); err != nil {
			return failMaterialization(err)
		}
		ref, putErr := storage.Put(ctx, "subject.csv", "text/csv", csvData)
		if putErr != nil {
			return failMaterialization(putErr)
		}
		createdRefs = append(createdRefs, ref)
		if err := s.RenewReservation(ctx, reservation.RecordID, reservation.OwnerNonce, time.Now().UTC()); err != nil {
			return failMaterialization(err)
		}
		if verifyErr := VerifyStorageHash(ctx, storage, ref, HashBytes(csvData)); verifyErr != nil {
			return failMaterialization(verifyErr)
		}
		artifacts = append(artifacts, EvidenceArtifact{ArtifactID: uuid.NewString(), Name: "subject.csv", MediaType: "text/csv", ByteCount: len(csvData), SHA256: digestHex(HashBytes(csvData)), StorageRef: ref})
	}
	manifest.Artifacts = artifacts
	manifest.PackageHash = digestHex(HashBytes(packageHash(artifacts)))
	manifestWithoutSelf := canonicalManifestBytes(manifest)
	if !packageWithinBoundsWithRows(len(redacted), len(csvData), len(manifestWithoutSelf), estimateSubjectRows(redacted), csvRowCount(csvData), profilePolicy.MaxBytes, profilePolicy.MaxRows) {
		return s.cleanupAndSealTooLarge(ctx, reservation, storage, createdRefs, auditID, "evidence package exceeds the approved final byte or row bound", manifest.Audit, time.Now().UTC())
	}
	if err := s.RenewReservation(ctx, reservation.RecordID, reservation.OwnerNonce, time.Now().UTC()); err != nil {
		return failMaterialization(err)
	}
	manifestRef, err := storage.Put(ctx, "manifest.json", "application/json", manifestWithoutSelf)
	if err != nil {
		return failMaterialization(err)
	}
	createdRefs = append(createdRefs, manifestRef)
	if err := s.RenewReservation(ctx, reservation.RecordID, reservation.OwnerNonce, time.Now().UTC()); err != nil {
		return failMaterialization(err)
	}
	if err := VerifyStorageHash(ctx, storage, manifestRef, HashBytes(manifestWithoutSelf)); err != nil {
		return failMaterialization(err)
	}
	manifestArtifact := EvidenceArtifact{ArtifactID: uuid.NewString(), Name: "manifest.json", MediaType: "application/json", ByteCount: len(manifestWithoutSelf), SHA256: digestHex(HashBytes(manifestWithoutSelf)), StorageRef: manifestRef}
	if err := s.RenewReservation(ctx, reservation.RecordID, reservation.OwnerNonce, time.Now().UTC()); err != nil {
		return failMaterialization(err)
	}
	if err := VerifyManifestWithRenewal(manifest, append([]EvidenceArtifact{manifestArtifact}, manifest.Artifacts...), storage, func() error {
		return s.RenewReservation(ctx, reservation.RecordID, reservation.OwnerNonce, time.Now().UTC())
	}); err != nil {
		return failMaterialization(err)
	}
	response := EvidenceResponse{NLAuditID: auditID, Status: EvidenceCompleted, EvidenceManifestID: manifest.ManifestID, ManifestVersion: 2, Manifest: manifest, Artifacts: append([]EvidenceArtifact{manifestArtifact}, manifest.Artifacts...), PackageHash: manifest.PackageHash, Audit: manifest.Audit, ExpiresAt: manifest.ExpiresAt}
	if err := s.RenewReservation(ctx, reservation.RecordID, reservation.OwnerNonce, time.Now().UTC()); err != nil {
		return failMaterialization(err)
	}
	if err := s.finalizeEvidence(ctx, reservation, response, actorID, time.Now().UTC()); err != nil {
		return failMaterialization(err)
	}
	return response, nil
}

func (s *Store) subjectAuditIDs(ctx context.Context, subjectKind, subjectID string) ([]string, error) {
	tenant := identity.StorageTenant(ctx)
	rows, err := s.db.QueryContext(ctx, `SELECT audit_id FROM assurance_audit_links WHERE tenant_id=? AND entity_kind=? AND entity_id=? AND audit_id<>'' ORDER BY created_at, link_id`, tenant, subjectKind, subjectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := make([]string, 0, 16)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
		if len(ids) > 1000 {
			return nil, fmt.Errorf("audit subject selection exceeds bound")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("audit subject has no linked rows")
	}
	return ids, nil
}

func estimateSubjectRows(raw []byte) int {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if decoder.Decode(&value) != nil {
		return 1
	}
	if object, ok := value.(map[string]any); ok {
		for _, key := range []string{"observations", "results", "changes"} {
			if rows, ok := object[key].([]any); ok {
				return len(rows)
			}
		}
	}
	if rows, ok := value.([]any); ok {
		return len(rows)
	}
	return 1
}

func packageWithinBounds(subjectBytes, csvBytes, manifestBytes, maxBytes, rows int) bool {
	if maxBytes > 0 && subjectBytes+csvBytes+manifestBytes > maxBytes {
		return false
	}
	if rows <= 0 {
		return false
	}
	artifactRows := 1
	if csvBytes > 0 {
		artifactRows++
	}
	if manifestBytes > 0 {
		artifactRows++
	}
	return artifactRows <= rows
}

func packageWithinBoundsWithRows(subjectBytes, csvBytes, manifestBytes, subjectRows, csvRows, maxBytes, maxRows int) bool {
	if maxBytes > 0 && subjectBytes+csvBytes+manifestBytes > maxBytes {
		return false
	}
	if maxRows > 0 && subjectRows+csvRows+1 > maxRows {
		return false
	}
	return true
}

func csvRowCount(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	return bytes.Count(data, []byte{'\n'})
}

type CleanupReconciliationResult struct {
	Attempted int
	Deleted   int
	Pending   int
}

// ReconcileEvidenceCleanup retries only the recorded external deletes. It
// never reconstructs a subject, re-exports evidence, or changes a manifest.
// The tenant predicate and bounded limit make it safe for startup and operator
// recovery loops.
func (s *Store) ReconcileEvidenceCleanup(ctx context.Context, storage EvidenceStorage, now time.Time, limit int) (CleanupReconciliationResult, error) {
	result := CleanupReconciliationResult{}
	if s == nil || s.db == nil {
		return result, domainError("dependency_unavailable", "assurance store is unavailable")
	}
	if storage == nil {
		return result, domainError("evidence_storage_unconfigured", "evidence storage is not configured for cleanup reconciliation")
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	tenant := identity.StorageTenant(ctx)
	rows, err := s.db.QueryContext(ctx, `SELECT cleanup_id, storage_reference FROM assurance_evidence_cleanup WHERE tenant_id=? AND state='reconciliation_required' ORDER BY updated_at, cleanup_id LIMIT ?`, tenant, limit)
	if err != nil {
		return result, err
	}
	defer func() { _ = rows.Close() }()
	type item struct{ id, ref string }
	items := make([]item, 0, limit)
	for rows.Next() {
		var value item
		if err := rows.Scan(&value.id, &value.ref); err != nil {
			return result, err
		}
		items = append(items, value)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	var firstErr error
	for _, item := range items {
		result.Attempted++
		deleteErr := storage.Delete(ctx, item.ref)
		if deleteErr != nil && !errors.Is(deleteErr, ErrEvidenceNotFound) {
			result.Pending++
			if firstErr == nil {
				firstErr = deleteErr
			}
			_, _ = s.db.ExecContext(ctx, `UPDATE assurance_evidence_cleanup SET state='reconciliation_required', error=?, updated_at=? WHERE tenant_id=? AND cleanup_id=?`, deleteErr.Error(), formatTimestamp(now.UTC()), tenant, item.id)
			continue
		}
		result.Deleted++
		if _, updateErr := s.db.ExecContext(ctx, `UPDATE assurance_evidence_cleanup SET state='deleted', error='', updated_at=? WHERE tenant_id=? AND cleanup_id=? AND state='reconciliation_required'`, formatTimestamp(now.UTC()), tenant, item.id); updateErr != nil {
			result.Pending++
			if firstErr == nil {
				firstErr = updateErr
			}
		}
	}
	return result, firstErr
}

func recordEvidenceCleanup(ctx context.Context, store *Store, ref, recordID, state, cleanupErr string, now time.Time) {
	if store == nil || store.db == nil || ref == "" {
		return
	}
	tenant := identity.StorageTenant(ctx)
	var cleanupID string
	lookupErr := store.db.QueryRowContext(ctx, `SELECT cleanup_id FROM assurance_evidence_cleanup WHERE tenant_id=? AND record_id=? AND storage_reference=? ORDER BY updated_at DESC, cleanup_id DESC LIMIT 1`, tenant, recordID, ref).Scan(&cleanupID)
	if lookupErr == nil {
		_, _ = store.db.ExecContext(ctx, `UPDATE assurance_evidence_cleanup SET state=?, error=?, updated_at=? WHERE tenant_id=? AND cleanup_id=?`, state, cleanupErr, formatTimestamp(now.UTC()), tenant, cleanupID)
		return
	}
	_, _ = store.db.ExecContext(ctx, `INSERT INTO assurance_evidence_cleanup (tenant_id, cleanup_id, record_id, storage_reference, state, error, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, tenant, uuid.NewString(), recordID, ref, state, cleanupErr, formatTimestamp(now.UTC()), formatTimestamp(now.UTC()))
}

func cleanupEvidenceRefs(ctx context.Context, store *Store, storage EvidenceStorage, refs []string, recordID string, cause error) {
	for _, ref := range refs {
		state := "deleted"
		cleanupErr := ""
		if errors.Is(cause, ErrEvidenceCommitAmbiguous) {
			state = "reconciliation_required"
			cleanupErr = cause.Error()
		} else if storage == nil {
			state = "reconciliation_required"
			cleanupErr = "evidence storage is unavailable"
		} else if err := storage.Delete(ctx, ref); err != nil && !errors.Is(err, ErrEvidenceNotFound) {
			state = "reconciliation_required"
			cleanupErr = err.Error()
		}
		recordEvidenceCleanup(ctx, store, ref, recordID, state, cleanupErr, time.Now().UTC())
	}
}

func (s *Store) sealTooLarge(ctx context.Context, reservation ReservationResult, auditID, message string, auditManifest AuditManifest, now time.Time) (EvidenceResponse, error) {
	response := EvidenceResponse{NLAuditID: auditID, Status: EvidenceTooLarge, ManifestVersion: 2, Artifacts: []EvidenceArtifact{}, Audit: auditManifest}
	envelope, err := CanonicalJSON(response)
	if err != nil {
		return EvidenceResponse{}, err
	}
	if err := s.sealTerminalOutcome(ctx, reservation, envelope, EvidenceTooLarge, "", auditID, now); err != nil {
		return EvidenceResponse{}, err
	}
	_ = message
	return response, nil
}

func (s *Store) cleanupAndSealTooLarge(ctx context.Context, reservation ReservationResult, storage EvidenceStorage, refs []string, auditID, message string, auditManifest AuditManifest, now time.Time) (EvidenceResponse, error) {
	cleanupEvidenceRefs(ctx, s, storage, refs, reservation.RecordID, domainError(EvidenceTooLarge, message))
	return s.sealTooLarge(ctx, reservation, auditID, message, auditManifest, now)
}

func (s *Store) resolveExportProfile(ctx context.Context, kind, id string, requested RedactionProfile, retention string) (ExportProfile, error) {
	tenant := identity.StorageTenant(ctx)
	var snapshotID string
	switch kind {
	case "snapshot":
		snapshotID = id
	case "validation_run":
		if err := s.db.QueryRowContext(ctx, `SELECT snapshot_id FROM assurance_validation_runs WHERE tenant_id=? AND validation_run_id=?`, tenant, id).Scan(&snapshotID); err != nil {
			return ExportProfile{}, domainError("subject_not_found", "evidence subject was not found")
		}
	case "comparison":
		if err := s.db.QueryRowContext(ctx, `SELECT current_snapshot_id FROM assurance_comparisons WHERE tenant_id=? AND comparison_id=?`, tenant, id).Scan(&snapshotID); err != nil {
			return ExportProfile{}, domainError("subject_not_found", "evidence subject was not found")
		}
	default:
		return ExportProfile{}, domainError("subject_unavailable", "evidence subject is not exportable")
	}
	var reportID string
	var revision int
	if err := s.db.QueryRowContext(ctx, `SELECT report_id, definition_revision FROM assurance_snapshots WHERE tenant_id=? AND snapshot_id=?`, tenant, snapshotID).Scan(&reportID, &revision); err != nil {
		return ExportProfile{}, domainError("subject_not_found", "evidence subject was not found")
	}
	report, err := s.LoadReportRevision(ctx, reportID, revision)
	if err != nil {
		return ExportProfile{}, err
	}
	if len(report.ExportProfiles) == 0 {
		return ExportProfile{}, domainError("export_profile_not_approved", "report has no approved export profile")
	}
	var matches []ExportProfile
	seenProfileIDs := make(map[string]struct{}, len(report.ExportProfiles))
	for _, profileID := range report.ExportProfiles {
		if _, exists := seenProfileIDs[profileID]; exists {
			return ExportProfile{}, domainError("export_profile_ambiguous", "report contains a duplicate export profile reference")
		}
		seenProfileIDs[profileID] = struct{}{}
		var revision int
		var status, raw, storedHash string
		if err := s.db.QueryRowContext(ctx, `SELECT revision, status, profile_json, content_hash FROM assurance_export_profiles p JOIN assurance_active_bundle_objects a ON a.tenant_id=p.tenant_id AND a.object_kind='export_profile' AND a.object_id=p.profile_id AND a.object_revision=p.revision WHERE p.tenant_id=? AND p.profile_id=? AND p.status='active' ORDER BY p.revision DESC LIMIT 1`, tenant, profileID).Scan(&revision, &status, &raw, &storedHash); err != nil {
			continue
		}
		if digestHex(HashBytes([]byte(raw))) != storedHash {
			return ExportProfile{}, domainError("export_profile_integrity_failed", "stored export profile hash does not match")
		}
		var profile ExportProfile
		if json.Unmarshal([]byte(raw), &profile) != nil {
			return ExportProfile{}, domainError("export_profile_integrity_failed", "stored export profile JSON is invalid")
		}
		profile.Revision = revision
		profile.Status = status
		permitted := false
		for _, subject := range profile.PermittedSubjects {
			if subject == kind {
				permitted = true
				break
			}
		}
		if permitted && profile.RedactionProfile == string(requested) && profile.RetentionClass == retention && profile.DeliveryPolicy == "opaque_reference" {
			matches = append(matches, profile)
		}
	}
	if len(matches) > 1 {
		return ExportProfile{}, domainError("export_profile_ambiguous", "more than one active export profile matches the historical report revision")
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return ExportProfile{}, domainError("export_profile_not_approved", "requested subject, redaction, retention, or delivery policy is not approved")
}

func defaultAuditManifest() AuditManifest {
	return AuditManifest{Integrity: AuditIntegrity{Scope: "included_rows_hash_continuity", ChainVerified: false, HashVersionCoverage: []HashVersionCoverage{}, TenantIdentityHashed: true}, Checkpoint: AuditCheckpoint{Status: "not_requested"}, Completeness: AuditCompleteness{Status: "unknown", TerminalAnchor: "unknown", FinalRowDeletionDetectable: false}, Caveats: []string{"audit chain not requested"}}
}
func canonicalManifestBytes(manifest EvidenceManifest) []byte {
	raw, _ := CanonicalJSON(manifest)
	return raw
}

func CanonicalManifestJSON(manifest EvidenceManifest) ([]byte, error) {
	return CanonicalJSON(manifest)
}

func VerifyManifest(manifest EvidenceManifest, artifacts []EvidenceArtifact, storage ...EvidenceStorage) error {
	return verifyManifest(manifest, artifacts, nil, storage...)
}

func VerifyManifestWithRenewal(manifest EvidenceManifest, artifacts []EvidenceArtifact, storage EvidenceStorage, beforeRead func() error) error {
	return verifyManifest(manifest, artifacts, beforeRead, storage)
}

func verifyManifest(manifest EvidenceManifest, artifacts []EvidenceArtifact, beforeRead func() error, storage ...EvidenceStorage) error {
	if manifest.ManifestVersion != 2 || manifest.ManifestID == "" || manifest.SubjectKind == "" || manifest.SubjectID == "" {
		return fmt.Errorf("invalid evidence manifest")
	}
	if len(artifacts) == 0 || len(artifacts) > 3 {
		return fmt.Errorf("evidence manifest has an invalid artifact count")
	}
	payload := make([]EvidenceArtifact, 0, len(artifacts))
	seenIDs := make(map[string]struct{}, len(artifacts))
	seenNames := make(map[string]struct{}, len(artifacts))
	manifestCount := 0
	for _, artifact := range artifacts {
		if artifact.ArtifactID == "" || artifact.Name == "" || artifact.MediaType == "" || artifact.StorageRef == "" || artifact.SHA256 == "" || artifact.ByteCount < 0 {
			return fmt.Errorf("evidence manifest contains an invalid artifact")
		}
		if _, err := hex.DecodeString(artifact.SHA256); err != nil || len(artifact.SHA256) != 64 {
			return fmt.Errorf("evidence manifest contains an invalid artifact hash")
		}
		if _, exists := seenIDs[artifact.ArtifactID]; exists {
			return fmt.Errorf("evidence manifest contains duplicate artifact ID")
		}
		seenIDs[artifact.ArtifactID] = struct{}{}
		if _, exists := seenNames[artifact.Name]; exists {
			return fmt.Errorf("evidence manifest contains duplicate artifact name")
		}
		seenNames[artifact.Name] = struct{}{}
		if artifact.Name == "manifest.json" {
			manifestCount++
			if manifestCount > 1 {
				return fmt.Errorf("evidence manifest contains duplicate manifest artifact")
			}
		}
		if artifact.Name != "manifest.json" {
			payload = append(payload, artifact)
		}
		if len(storage) > 1 {
			return fmt.Errorf("exactly one storage adapter is permitted")
		}
		if len(storage) == 1 {
			if beforeRead != nil {
				if err := beforeRead(); err != nil {
					return err
				}
			}
			data, err := storage[0].Read(context.Background(), artifact.StorageRef)
			if err != nil {
				return fmt.Errorf("read artifact %s: %w", artifact.Name, err)
			}
			if len(data) != artifact.ByteCount || digestHex(HashBytes(data)) != artifact.SHA256 {
				return fmt.Errorf("artifact %s failed read-back hash verification", artifact.Name)
			}
			if artifact.Name == "manifest.json" {
				expected, err := CanonicalJSON(manifest)
				if err != nil || string(expected) != string(data) {
					return fmt.Errorf("stored manifest bytes do not match canonical manifest")
				}
			}
		}
	}
	if len(payload) != len(manifest.Artifacts) {
		return fmt.Errorf("manifest payload artifact set does not match")
	}
	for index := range payload {
		if payload[index] != manifest.Artifacts[index] {
			return fmt.Errorf("manifest payload artifact metadata does not match")
		}
	}
	if manifest.PackageHash != digestHex(HashBytes(packageHash(payload))) {
		return fmt.Errorf("manifest package hash does not match artifacts")
	}
	return nil
}

func packageHash(artifacts []EvidenceArtifact) []byte {
	var out bytes.Buffer
	for _, artifact := range artifacts {
		for _, value := range []string{artifact.ArtifactID, artifact.Name, artifact.MediaType, fmt.Sprintf("%d", artifact.ByteCount), artifact.SHA256, artifact.StorageRef} {
			var length [8]byte
			binary.BigEndian.PutUint64(length[:], uint64(len(value)))
			out.Write(length[:])
			out.WriteString(value)
		}
	}
	return out.Bytes()
}

type CSVMetadata struct {
	FormulaEscaping string `json:"formula_escaping"`
	Authoritative   bool   `json:"authoritative"`
}

func RenderCSV(subject any, profile RedactionProfile) ([]byte, CSVMetadata, error) {
	raw, err := CanonicalJSON(subject)
	if err != nil {
		return nil, CSVMetadata{}, err
	}
	redacted, err := redactCanonical(raw, profile)
	if err != nil {
		return nil, CSVMetadata{}, err
	}
	return deterministicCSV(redacted), CSVMetadata{FormulaEscaping: "prefix_formula_values_with_apostrophe", Authoritative: false}, nil
}
func redactCanonical(raw []byte, profile RedactionProfile) ([]byte, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	redactValue(value, profile)
	return CanonicalJSON(value)
}
func redactValue(value any, profile RedactionProfile) {
	switch typed := value.(type) {
	case map[string]any:
		for key := range typed {
			lower := strings.ToLower(key)
			if redactEvidenceKey(lower) {
				delete(typed, key)
				continue
			}
			if profile == RedactionStrict && (lower == "formula_text" || lower == "provider_revision") {
				delete(typed, key)
				continue
			}
			redactValue(typed[key], profile)
		}
	case []any:
		for _, item := range typed {
			redactValue(item, profile)
		}
	}
}

func redactEvidenceKey(key string) bool {
	normalized := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(key))
	if normalized == "token" || strings.Contains(normalized, "token") ||
		normalized == "secret" || strings.Contains(normalized, "password") ||
		normalized == "credential" || strings.Contains(normalized, "credential") ||
		normalized == "authorization" || normalized == "authheader" ||
		normalized == "idempotencykey" {
		return true
	}
	if normalized == "path" || normalized == "filepath" || normalized == "filesystempath" ||
		normalized == "localpath" || normalized == "unrestrictedpath" || normalized == "storagepath" ||
		normalized == "tenantid" || normalized == "tenantmetadata" {
		return true
	}
	return false
}

func collectRedactionRecords(raw []byte, profile RedactionProfile) []RedactionRecord {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return nil
	}
	records := make([]RedactionRecord, 0, 8)
	var walk func(string, any)
	walk = func(path string, item any) {
		switch typed := item.(type) {
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				field := key
				if path != "" {
					field = path + "." + key
				}
				if redactEvidenceKey(key) || (profile == RedactionStrict && (strings.EqualFold(key, "formula_text") || strings.EqualFold(key, "provider_revision"))) {
					if len(records) < 100 {
						records = append(records, RedactionRecord{Profile: string(profile), Field: field, Action: "removed"})
					}
					continue
				}
				walk(field, typed[key])
			}
		case []any:
			for index, child := range typed {
				walk(fmt.Sprintf("%s[%d]", path, index), child)
			}
		}
	}
	walk("", value)
	return records
}
func deterministicCSV(raw []byte) []byte {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return nil
	}
	rows := make(map[string]string)
	var walk func(string, any)
	walk = func(path string, item any) {
		switch typed := item.(type) {
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				next := key
				if path != "" {
					next = path + "." + key
				}
				walk(next, typed[key])
			}
		case []any:
			for index, child := range typed {
				walk(fmt.Sprintf("%s[%d]", path, index), child)
			}
		case nil:
			rows[path] = ""
		default:
			rows[path] = fmt.Sprint(typed)
		}
	}
	walk("subject", value)
	var out bytes.Buffer
	writer := csv.NewWriter(&out)
	_ = writer.Write([]string{"path", "value"})
	paths := make([]string, 0, len(rows))
	for path := range rows {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		_ = writer.Write([]string{CSVSafeValue(path), CSVSafeValue(rows[path])})
	}
	writer.Flush()
	return out.Bytes()
}
func CSVSafeValue(value string) string {
	if value == "" {
		return value
	}
	switch value[0] {
	case '=', '+', '-', '@':
		return "'" + value
	}
	return value
}

func (s *Store) subjectJSON(ctx context.Context, kind, id string) ([]byte, error) {
	switch kind {
	case "snapshot":
		response, err := s.Snapshot(ctx, id)
		if err != nil {
			return nil, err
		}
		return CanonicalJSON(response)
	case "validation_run":
		tenant := identity.StorageTenant(ctx)
		var snapshotID, ruleSetID, status string
		var revision, failOnWarning int
		if err := s.db.QueryRowContext(ctx, `SELECT snapshot_id, rule_set_id, rule_set_revision, status, fail_on_warning FROM assurance_validation_runs WHERE tenant_id=? AND validation_run_id=?`, tenant, id).Scan(&snapshotID, &ruleSetID, &revision, &status, &failOnWarning); err != nil {
			return nil, domainError("subject_not_found", "validation run was not found")
		}
		rows, err := s.db.QueryContext(ctx, `SELECT result_json FROM assurance_validation_results WHERE tenant_id=? AND validation_run_id=? ORDER BY rowid`, tenant, id)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		results := []json.RawMessage{}
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				return nil, err
			}
			results = append(results, json.RawMessage(raw))
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return CanonicalJSON(map[string]any{"validation_run_id": id, "snapshot_id": snapshotID, "rule_set_id": ruleSetID, "rule_set_revision": revision, "status": status, "fail_on_warning": failOnWarning != 0, "results": results})
	case "comparison":
		tenant := identity.StorageTenant(ctx)
		var currentID, priorID, status, completeness, basis, policyID string
		var policyRevision, materialCount int
		if err := s.db.QueryRowContext(ctx, `SELECT current_snapshot_id, prior_snapshot_id, status, completeness, comparison_basis, policy_revision, materiality_policy_id, material_count FROM assurance_comparisons WHERE tenant_id=? AND comparison_id=?`, tenant, id).Scan(&currentID, &priorID, &status, &completeness, &basis, &policyRevision, &policyID, &materialCount); err != nil {
			return nil, domainError("subject_not_found", "comparison was not found")
		}
		rows, err := s.db.QueryContext(ctx, `SELECT item_json FROM assurance_comparison_items WHERE tenant_id=? AND comparison_id=? ORDER BY rowid`, tenant, id)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		items := []json.RawMessage{}
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				return nil, err
			}
			items = append(items, json.RawMessage(raw))
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return CanonicalJSON(map[string]any{"comparison_id": id, "current_snapshot_id": currentID, "prior_snapshot_id": priorID, "status": status, "completeness": completeness, "comparison_basis": basis, "materiality_policy_id": policyID, "materiality_revision": policyRevision, "material_count": materialCount, "changes": items})
	default:
		return nil, domainError("subject_not_found", "evidence subject was not found")
	}
}

func (s *Store) finalizeEvidence(ctx context.Context, reservation ReservationResult, response EvidenceResponse, actor string, now time.Time) error {
	if err := s.requireRichAudit(); err != nil {
		return err
	}
	tenant := identity.StorageTenant(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	manifestJSON, err := CanonicalJSON(response.Manifest)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_evidence_manifests (tenant_id, manifest_id, manifest_version, subject_kind, subject_id, manifest_hash, manifest_json, audit_integrity, audit_completeness, expiry_at, idempotency_digest) SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, idempotency_digest FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=?`, tenant, response.EvidenceManifestID, 2, response.Manifest.SubjectKind, response.Manifest.SubjectID, digestHex(HashBytes(manifestJSON)), string(manifestJSON), `included_rows_only`, response.Manifest.Audit.Completeness.Status, response.Manifest.ExpiresAt, tenant, reservation.RecordID); err != nil {
		return err
	}
	for _, artifact := range response.Artifacts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_evidence_artifacts (tenant_id, artifact_id, manifest_id, artifact_hash, size_bytes, media_type, storage_reference, omissions_json, redaction_json) VALUES (?, ?, ?, ?, ?, ?, ?, '[]', '{}')`, tenant, artifact.ArtifactID, response.EvidenceManifestID, artifact.SHA256, artifact.ByteCount, artifact.MediaType, artifact.StorageRef); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_evidence_subjects (tenant_id, manifest_id, subject_kind, subject_id) VALUES (?, ?, ?, ?)`, tenant, response.EvidenceManifestID, response.Manifest.SubjectKind, response.Manifest.SubjectID); err != nil {
		return err
	}
	trustedActor, err := reservationActor(ctx, tx, reservation)
	if err != nil {
		return err
	}
	raw, err := CanonicalJSON(response)
	if err != nil {
		return err
	}
	if _, err := s.auditLog.AppendTx(ctx, tx, audit.Entry{Actor: trustedActor, Tool: "workiva_export_evidence", Action: "export", Target: response.EvidenceManifestID, AfterJSON: string(raw), AuditID: response.NLAuditID}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_audit_links (tenant_id, link_id, entity_kind, entity_id, audit_id, request_id, correlation_id, created_at) VALUES (?, ?, 'evidence_manifest', ?, ?, ?, ?, ?)`, tenant, uuid.NewString(), response.EvidenceManifestID, response.NLAuditID, RequestIDFromContext(ctx), reservation.CorrelationID, formatTimestamp(now)); err != nil {
		return err
	}
	envelope, err := CanonicalJSON(response)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET state='sealed', response_envelope=?, response_hash=?, response_status=?, entity_reference=?, audit_id=?, terminal_at=?, lease_expires_at=? WHERE tenant_id=? AND record_id=? AND state='reserved' AND owner_nonce=?`, string(envelope), digestHex(HashBytes(envelope)), response.Status, response.EvidenceManifestID, response.NLAuditID, formatTimestamp(now), formatTimestamp(now), tenant, reservation.RecordID, reservation.OwnerNonce)
	if err != nil {
		return err
	}
	if err := requireOneTransition(result); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: %v", ErrEvidenceCommitAmbiguous, err)
	}
	return nil
}

func markTombstoneReconciliation(ctx context.Context, store *Store, tenant, manifestID, reason string, dispositions map[string]string) {
	if store == nil || store.db == nil {
		return
	}
	raw, _ := json.Marshal(dispositions)
	_, _ = store.db.ExecContext(ctx, `UPDATE assurance_evidence_tombstones SET state='reconciliation_required', reason=?, artifact_disposition_json=? WHERE tenant_id=? AND manifest_id=?`, reason, string(raw), tenant, manifestID)
}

func (s *Store) PurgeExpiredEvidence(ctx context.Context, now time.Time) error {
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok || principal.TenantID == "" || principal.ObjectID == "" || !principal.HasPermission(identity.PermissionTenantAdmin) {
		return domainError("operator_authorization_required", "an authorized tenant operator is required")
	}
	if err := s.requireRichAudit(); err != nil {
		return err
	}
	tenant := identity.StorageTenant(ctx)
	rows, err := s.db.QueryContext(ctx, `SELECT manifest_id, subject_kind, subject_id FROM assurance_evidence_manifests WHERE tenant_id=? AND expiry_at<>'' AND expiry_at<=? AND legal_hold=0`, tenant, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	type item struct{ id, kind, subject string }
	var expired []item
	for rows.Next() {
		var value item
		if err := rows.Scan(&value.id, &value.kind, &value.subject); err != nil {
			return err
		}
		expired = append(expired, value)
	}
	for _, value := range expired {
		artifactRows, queryErr := s.db.QueryContext(ctx, `SELECT artifact_id, storage_reference FROM assurance_evidence_artifacts WHERE tenant_id=? AND manifest_id=? ORDER BY artifact_id`, tenant, value.id)
		if queryErr != nil {
			return queryErr
		}
		var refs []string
		var artifactIDs []string
		for artifactRows.Next() {
			var ref, artifactID string
			if scanErr := artifactRows.Scan(&artifactID, &ref); scanErr != nil {
				_ = artifactRows.Close()
				return scanErr
			}
			refs = append(refs, ref)
			artifactIDs = append(artifactIDs, artifactID)
		}
		if rowsErr := artifactRows.Err(); rowsErr != nil {
			_ = artifactRows.Close()
			return rowsErr
		}
		_ = artifactRows.Close()
		// Persist a tombstone before external deletion. A failed delete leaves a
		// durable retry marker and the DB rows intact, never a false purge.
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_evidence_tombstones (tenant_id, tombstone_id, subject_kind, subject_id, manifest_id, reason, purged_at, state) VALUES (?, ?, ?, ?, ?, 'retention_pending', ?, 'pending') ON CONFLICT(tenant_id, manifest_id) DO UPDATE SET reason='retention_pending', state='pending', purged_at=excluded.purged_at`, tenant, uuid.NewString(), value.kind, value.subject, value.id, formatTimestamp(now)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if s.evidenceStorage == nil {
			markTombstoneReconciliation(ctx, s, tenant, value.id, "evidence_storage_unconfigured", nil)
			return domainError("evidence_storage_unconfigured", "evidence storage is not configured for purge")
		}
		dispositions := make(map[string]string, len(refs))
		for index, ref := range refs {
			if _, err := s.db.ExecContext(ctx, `UPDATE assurance_evidence_tombstones SET state='deleting' WHERE tenant_id=? AND manifest_id=?`, tenant, value.id); err != nil {
				markTombstoneReconciliation(ctx, s, tenant, value.id, "database_state_update_failed", dispositions)
				return err
			}
			if deleteErr := s.evidenceStorage.Delete(ctx, ref); deleteErr != nil && !errors.Is(deleteErr, ErrEvidenceNotFound) {
				dispositions[artifactIDs[index]] = "unknown"
				raw, _ := json.Marshal(dispositions)
				_, _ = s.db.ExecContext(ctx, `UPDATE assurance_evidence_tombstones SET state='reconciliation_required', reason=?, artifact_disposition_json=? WHERE tenant_id=? AND manifest_id=?`, "storage_delete_unknown", string(raw), tenant, value.id)
				return deleteErr
			}
			dispositions[artifactIDs[index]] = "deleted"
			raw, _ := json.Marshal(dispositions)
			if _, err := s.db.ExecContext(ctx, `UPDATE assurance_evidence_tombstones SET artifact_disposition_json=? WHERE tenant_id=? AND manifest_id=?`, string(raw), tenant, value.id); err != nil {
				markTombstoneReconciliation(ctx, s, tenant, value.id, "database_disposition_update_failed", dispositions)
				return err
			}
		}
		tx, err = s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := s.auditLog.AppendTx(ctx, tx, audit.Entry{Actor: principal.AuditActor(), Tool: "assurance_retention", Action: "purge_evidence", Target: value.id}); err != nil {
			_ = tx.Rollback()
			_, _ = s.db.ExecContext(ctx, `UPDATE assurance_evidence_tombstones SET state='reconciliation_required', reason='audit_or_db_failure' WHERE tenant_id=? AND manifest_id=?`, tenant, value.id)
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM assurance_evidence_artifacts WHERE tenant_id=? AND manifest_id=?`, tenant, value.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM assurance_evidence_subjects WHERE tenant_id=? AND manifest_id=?`, tenant, value.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM assurance_evidence_manifests WHERE tenant_id=? AND manifest_id=?`, tenant, value.id); err != nil {
			return err
		}
		rawDispositions, marshalErr := json.Marshal(dispositions)
		if marshalErr != nil {
			return marshalErr
		}
		if _, err := tx.ExecContext(ctx, `UPDATE assurance_evidence_tombstones SET reason='retention_expired', state='deleted', artifact_disposition_json=? WHERE tenant_id=? AND manifest_id=?`, string(rawDispositions), tenant, value.id); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			_, _ = s.db.ExecContext(ctx, `UPDATE assurance_evidence_tombstones SET state='reconciliation_required', reason='audit_or_db_commit_unknown' WHERE tenant_id=? AND manifest_id=?`, tenant, value.id)
			return err
		}
	}
	return nil
}
