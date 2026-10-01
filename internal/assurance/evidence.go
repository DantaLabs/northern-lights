package assurance

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"fmt"
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
		return nil, fmt.Errorf("evidence artifact is not available")
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
	CheckpointID   string `json:"checkpoint_id"`
	Sequence       int64  `json:"sequence"`
	Hash           string `json:"hash"`
	ExternalAnchor string `json:"external_anchor"`
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
	if request.SubjectKind == "transfer" {
		return EvidenceResponse{}, domainError("subject_unavailable", "transfer evidence is unavailable until Wave 3 transfer state exists")
	}
	policy, err := s.ResolveRetentionPolicy(ctx, request.RetentionClass)
	if err != nil {
		return EvidenceResponse{}, err
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
	manifest, err := s.BuildManifest(ctx, request.SubjectKind, request.SubjectID, profile)
	if err != nil {
		_ = s.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(err, auditID), time.Now().UTC())
		return EvidenceResponse{}, err
	}
	manifest.ManifestID = uuid.NewString()
	manifest.ExpiresAt = retentionExpiry(time.Now(), policy).Format(time.RFC3339Nano)
	if request.IncludeAuditChain {
		if s.auditLog == nil {
			failure := domainError("audit_unavailable", "audit-chain evidence was requested but the audit log is unavailable")
			_ = s.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(failure, auditID), time.Now().UTC())
			return EvidenceResponse{}, failure
		}
		first, last, boundsErr := s.auditLog.RangeBounds(ctx)
		if boundsErr != nil {
			failure := domainError("audit_incomplete", "audit-chain evidence could not establish a bounded tenant range")
			_ = s.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(failure, auditID), time.Now().UTC())
			return EvidenceResponse{}, failure
		}
		rangeResult, rangeErr := s.auditLog.ExportRange(ctx, first, last)
		verification, verifyErr := s.auditLog.VerifyRange(ctx, first, last)
		if rangeErr != nil || verifyErr != nil || !verification.ChainVerified {
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
		manifest.Audit = AuditManifest{FirstSeq: rangeResult.FirstSeq, LastSeq: rangeResult.LastSeq, FirstHash: rangeResult.FirstHash, LastHash: rangeResult.LastHash, Integrity: AuditIntegrity{Scope: "included_rows_hash_continuity", ChainVerified: true, HashVersionCoverage: coverage, TenantIdentityHashed: tenantIdentityHashed}, Checkpoint: AuditCheckpoint{Status: "not_requested"}, Completeness: AuditCompleteness{Status: verification.Completeness.Status, ExpectedCount: verification.Completeness.ExpectedCount, IncludedCount: verification.Completeness.IncludedCount, OmittedCount: verification.Completeness.OmittedCount, ExpectedEventCount: verification.Completeness.ExpectedEventCount, IncludedEventCount: verification.Completeness.IncludedEventCount, OmissionCount: verification.Completeness.OmissionCount, OmissionsRecorded: verification.Completeness.OmissionsRecorded, TerminalAnchor: verification.Completeness.TerminalAnchor, TerminalAnchorVerified: verification.Completeness.TerminalAnchorVerified, FinalRowDeletionDetectable: verification.Completeness.FinalRowDeletionDetectable}, Caveats: verification.Caveats}
	}
	storage := s.evidenceStorage
	if len(storages) > 1 {
		return EvidenceResponse{}, domainError("invalid_request", "only one evidence storage adapter is permitted")
	}
	if len(storages) == 1 {
		storage = storages[0]
	}
	if storage == nil {
		storage = NewMemoryEvidenceStorage()
	}
	subjectRef, err := storage.Put(ctx, "subject.json", "application/json", redacted)
	if err != nil {
		return EvidenceResponse{}, err
	}
	if err := VerifyStorageHash(ctx, storage, subjectRef, HashBytes(redacted)); err != nil {
		return EvidenceResponse{}, err
	}
	subjectArtifact := EvidenceArtifact{ArtifactID: uuid.NewString(), Name: "subject.json", MediaType: "application/json", ByteCount: len(redacted), SHA256: digestHex(HashBytes(redacted)), StorageRef: subjectRef}
	artifacts := []EvidenceArtifact{subjectArtifact}
	var csvData []byte
	if request.Format == "csv" {
		csvData = deterministicCSV(redacted)
		ref, putErr := storage.Put(ctx, "subject.csv", "text/csv", csvData)
		if putErr != nil {
			return EvidenceResponse{}, putErr
		}
		if verifyErr := VerifyStorageHash(ctx, storage, ref, HashBytes(csvData)); verifyErr != nil {
			return EvidenceResponse{}, verifyErr
		}
		artifacts = append(artifacts, EvidenceArtifact{ArtifactID: uuid.NewString(), Name: "subject.csv", MediaType: "text/csv", ByteCount: len(csvData), SHA256: digestHex(HashBytes(csvData)), StorageRef: ref})
	}
	manifest.Artifacts = artifacts
	manifest.PackageHash = digestHex(HashBytes(packageHash(artifacts)))
	manifestWithoutSelf := canonicalManifestBytes(manifest)
	manifestRef, err := storage.Put(ctx, "manifest.json", "application/json", manifestWithoutSelf)
	if err != nil {
		return EvidenceResponse{}, err
	}
	if err := VerifyStorageHash(ctx, storage, manifestRef, HashBytes(manifestWithoutSelf)); err != nil {
		return EvidenceResponse{}, err
	}
	manifestArtifact := EvidenceArtifact{ArtifactID: uuid.NewString(), Name: "manifest.json", MediaType: "application/json", ByteCount: len(manifestWithoutSelf), SHA256: digestHex(HashBytes(manifestWithoutSelf)), StorageRef: manifestRef}
	manifest.Artifacts = append([]EvidenceArtifact{manifestArtifact}, manifest.Artifacts...)
	if err := VerifyManifest(manifest, manifest.Artifacts); err != nil {
		_ = s.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(err, auditID), time.Now().UTC())
		return EvidenceResponse{}, err
	}
	response := EvidenceResponse{NLAuditID: auditID, Status: EvidenceCompleted, EvidenceManifestID: manifest.ManifestID, ManifestVersion: 2, Manifest: manifest, Artifacts: manifest.Artifacts, PackageHash: manifest.PackageHash, Audit: manifest.Audit, ExpiresAt: manifest.ExpiresAt}
	if err := s.finalizeEvidence(ctx, reservation, response, actorID, time.Now().UTC()); err != nil {
		_ = s.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(err, auditID), time.Now().UTC())
		return EvidenceResponse{}, err
	}
	return response, nil
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

func VerifyManifest(manifest EvidenceManifest, artifacts []EvidenceArtifact) error {
	if manifest.ManifestVersion != 2 || manifest.ManifestID == "" || manifest.SubjectKind == "" || manifest.SubjectID == "" {
		return fmt.Errorf("invalid evidence manifest")
	}
	if len(artifacts) == 0 || len(artifacts) > 3 {
		return fmt.Errorf("evidence manifest has an invalid artifact count")
	}
	for _, artifact := range artifacts {
		if artifact.ArtifactID == "" || artifact.Name == "" || artifact.MediaType == "" || artifact.StorageRef == "" || artifact.SHA256 == "" || artifact.ByteCount < 0 {
			return fmt.Errorf("evidence manifest contains an invalid artifact")
		}
	}
	return nil
}

func packageHash(artifacts []EvidenceArtifact) []byte {
	var out bytes.Buffer
	for _, artifact := range artifacts {
		for _, value := range []string{artifact.Name, artifact.MediaType, fmt.Sprintf("%d", artifact.ByteCount), artifact.SHA256} {
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
			if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || lower == "idempotency_key" || lower == "authorization" || lower == "storage_path" {
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
func deterministicCSV(raw []byte) []byte {
	var out bytes.Buffer
	writer := csv.NewWriter(&out)
	_ = writer.Write([]string{"subject_json"})
	_ = writer.Write([]string{CSVSafeValue(string(raw))})
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
	if s.auditLog != nil && s.auditLog.SharesDB(s.db) {
		raw, err := CanonicalJSON(response)
		if err != nil {
			return err
		}
		if _, err := s.auditLog.AppendTx(ctx, tx, audit.Entry{Actor: actor, Tool: "workiva_export_evidence", Action: "export", Target: response.EvidenceManifestID, AfterJSON: string(raw), AuditID: response.NLAuditID}); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_audit_links (tenant_id, link_id, entity_kind, entity_id, audit_id, request_id, correlation_id, created_at) VALUES (?, ?, 'evidence_manifest', ?, ?, ?, ?, ?)`, tenant, uuid.NewString(), response.EvidenceManifestID, response.NLAuditID, uuid.NewString(), reservation.CorrelationID, formatTimestamp(now)); err != nil {
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
	return tx.Commit()
}

func (s *Store) PurgeExpiredEvidence(ctx context.Context, now time.Time) error {
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
		artifactRows, queryErr := s.db.QueryContext(ctx, `SELECT storage_reference FROM assurance_evidence_artifacts WHERE tenant_id=? AND manifest_id=?`, tenant, value.id)
		if queryErr != nil {
			return queryErr
		}
		var refs []string
		for artifactRows.Next() {
			var ref string
			if scanErr := artifactRows.Scan(&ref); scanErr != nil {
				_ = artifactRows.Close()
				return scanErr
			}
			refs = append(refs, ref)
		}
		if rowsErr := artifactRows.Err(); rowsErr != nil {
			_ = artifactRows.Close()
			return rowsErr
		}
		_ = artifactRows.Close()
		if s.evidenceStorage != nil {
			for _, ref := range refs {
				if deleteErr := s.evidenceStorage.Delete(ctx, ref); deleteErr != nil {
					return deleteErr
				}
			}
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO assurance_evidence_tombstones (tenant_id, tombstone_id, subject_kind, subject_id, manifest_id, reason, purged_at) VALUES (?, ?, ?, ?, ?, 'retention_expired', ?)`, tenant, uuid.NewString(), value.kind, value.subject, value.id, formatTimestamp(now)); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM assurance_evidence_artifacts WHERE tenant_id=? AND manifest_id=?`, tenant, value.id); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM assurance_evidence_subjects WHERE tenant_id=? AND manifest_id=?`, tenant, value.id); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM assurance_evidence_manifests WHERE tenant_id=? AND manifest_id=?`, tenant, value.id); err != nil {
			return err
		}
	}
	return nil
}
