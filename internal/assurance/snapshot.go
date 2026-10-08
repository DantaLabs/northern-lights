package assurance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
)

// ConsistencyMode is the closed snapshot consistency request vocabulary.
type ConsistencyMode string

const (
	ConsistencyNone           ConsistencyMode = "none"
	ConsistencyBestEffort     ConsistencyMode = "best_effort"
	ConsistencyRevisionPinned ConsistencyMode = "revision_pinned"
)

// SnapshotStatus is the public snapshot result state.
type SnapshotStatus string

const (
	SnapshotCompleted         SnapshotStatus = "completed"
	SnapshotPartial           SnapshotStatus = "partial"
	SnapshotFailed            SnapshotStatus = "failed"
	SnapshotIdempotencyReplay SnapshotStatus = "idempotency_replay"
)

// Completeness is explicit and never inferred by consumers.
type Completeness string

const (
	CompletenessComplete     Completeness = "complete"
	CompletenessIncomplete   Completeness = "incomplete"
	CompletenessNotCreated   Completeness = "not_created"
	CompletenessNotEvaluable Completeness = "not_evaluable"
)

// SnapshotRequest is the exact materializing request. IdempotencyKey is
// transient ingress data and is excluded from every canonical persisted body.
type SnapshotRequest struct {
	ReportID             string          `json:"report_id"`
	Period               Period          `json:"period"`
	FieldIDs             []string        `json:"field_ids,omitempty"`
	AllowPartial         bool            `json:"allow_partial"`
	IncludeRelationships bool            `json:"include_relationships"`
	Consistency          ConsistencyMode `json:"consistency,omitempty"`
	RetentionClass       string          `json:"retention_class,omitempty"`
	IdempotencyKey       string          `json:"idempotency_key"`
}

// SourceRequest asks the provider boundary for one direct uncached field read.
type SourceRequest struct {
	ExternalResourceID string
	SubresourceID      string
	Locator            string
	Consistency        ConsistencyMode
}

// ProviderRead is provider evidence for one uncached read.
type ProviderRead struct {
	Value            ProviderValue
	ProviderRevision ProviderRevision
	CacheBypassed    bool
}

// SourceReader is the minimum provider contract needed by Wave 1 snapshots.
type SourceReader interface {
	ReadUncached(context.Context, SourceRequest) (ProviderRead, error)
}

// RevisionPinCapability is optional; unsupported pinning fails rather than downgrades.
type RevisionPinCapability interface {
	SupportsRevisionPinning() bool
}

// SnapshotObservation is one immutable normalized source observation.
type SnapshotObservation struct {
	ObservationID      string           `json:"observation_id"`
	FieldID            string           `json:"field_id"`
	ResourceID         string           `json:"resource_id"`
	ExternalResourceID string           `json:"external_resource_id"`
	Locator            string           `json:"locator"`
	TypedValue         TypedValue       `json:"typed_value"`
	ProviderRevision   ProviderRevision `json:"provider_revision"`
	SourceFingerprint  string           `json:"source_fingerprint"`
	ObservedAt         string           `json:"observed_at"`
}

// SnapshotItemError is bounded per-field capture diagnostics.
type SnapshotItemError struct {
	FieldID string `json:"field_id"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// SnapshotPeriod is the bounded public projection of an approved period.
type SnapshotPeriod struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// SnapshotResponse matches the public Wave 1 tool contract.
type SnapshotResponse struct {
	NLAuditID          string                `json:"nl_audit_id"`
	Status             SnapshotStatus        `json:"status"`
	SnapshotID         string                `json:"snapshot_id"`
	ReportID           string                `json:"report_id"`
	DefinitionRevision int                   `json:"definition_revision"`
	Period             SnapshotPeriod        `json:"period"`
	CapturedAt         string                `json:"captured_at"`
	Completeness       Completeness          `json:"completeness"`
	Observations       []SnapshotObservation `json:"observations"`
	ItemErrors         []SnapshotItemError   `json:"item_errors"`
	ContentHash        string                `json:"content_hash"`
	RelationshipCount  int                   `json:"relationship_count"`
}

// SnapshotService resolves server-owned definitions, reserves idempotency,
// performs direct reads outside transactions, and seals immutable state.
type SnapshotService struct {
	Store     *Store
	Reader    SourceReader
	Audit     *audit.Log
	Now       func() time.Time
	Authorize func(context.Context, FieldDefinition) error
}

// Capture materializes one immutable snapshot or an explicit failed capture.
func (service SnapshotService) Capture(ctx context.Context, actorID, auditID string, request SnapshotRequest) (SnapshotResponse, error) {
	if service.Store == nil || service.Reader == nil {
		return SnapshotResponse{}, domainError("dependency_unavailable", "assurance store or uncached provider reader is unavailable")
	}
	if err := service.Store.Ready(); err != nil {
		return SnapshotResponse{}, err
	}
	if err := service.Store.requireRichAudit(service.Audit); err != nil {
		return SnapshotResponse{}, err
	}
	if len(request.ReportID) == 0 || len(request.ReportID) > 128 || len(request.IdempotencyKey) == 0 || len(request.IdempotencyKey) > 256 || len(request.FieldIDs) > 1000 {
		return SnapshotResponse{}, domainError("invalid_request", "report_id, bounded field_ids, and idempotency_key are required")
	}
	if request.Consistency == "" {
		request.Consistency = ConsistencyBestEffort
	}
	if request.RetentionClass == "" {
		request.RetentionClass = "standard"
	}
	canonicalFields, canonicalErr := canonicalizeSetStrings(request.FieldIDs)
	if canonicalErr != nil {
		return SnapshotResponse{}, domainError("invalid_request", canonicalErr.Error())
	}
	request.FieldIDs = canonicalFields
	if request.Consistency != ConsistencyNone && request.Consistency != ConsistencyBestEffort && request.Consistency != ConsistencyRevisionPinned {
		return SnapshotResponse{}, domainError("invalid_consistency", "consistency must be none, best_effort, or revision_pinned")
	}
	if request.Consistency == ConsistencyRevisionPinned {
		capability, ok := service.Reader.(RevisionPinCapability)
		if !ok || !capability.SupportsRevisionPinning() {
			return SnapshotResponse{}, domainError("capability_unverified", "revision-pinned reads are not supported by the active provider")
		}
	}

	report, err := service.Store.ResolveReport(ctx, request.ReportID, request.Period, request.FieldIDs)
	if err != nil {
		return SnapshotResponse{}, err
	}
	for _, field := range report.Fields {
		if service.Authorize != nil {
			if err := service.Authorize(ctx, field); err != nil {
				return SnapshotResponse{}, domainError("resource_denied", "an approved report source is not authorized")
			}
		}
	}
	canonicalRequest := struct {
		ReportID             string          `json:"report_id"`
		Period               Period          `json:"period"`
		FieldIDs             []string        `json:"field_ids"`
		AllowPartial         bool            `json:"allow_partial"`
		IncludeRelationships bool            `json:"include_relationships"`
		Consistency          ConsistencyMode `json:"consistency"`
		RetentionClass       string          `json:"retention_class"`
	}{request.ReportID, report.Periods[0], canonicalFieldIDs(report.Fields), request.AllowPartial, request.IncludeRelationships, request.Consistency, request.RetentionClass}
	requestJSON, err := CanonicalJSON(canonicalRequest)
	if err != nil {
		return SnapshotResponse{}, err
	}
	mappingJSON, _ := CanonicalJSON(report.Fields)
	periodJSON, _ := marshalCanonical(report.Periods[0])
	now := service.now()
	internalSnapshotID := uuid.NewString()
	reservation, err := service.Store.reserveTx(ctx, nil, ReservationRequest{
		ActorID: actorID, Tool: "workiva_snapshot_report", Action: "capture",
		IdempotencyDigest: DigestIdempotencyKey(request.IdempotencyKey), RequestDigest: HashBytes(requestJSON), RetentionClass: request.RetentionClass,
	}, now, &snapshotStart{SnapshotID: internalSnapshotID, ReportID: report.ReportID, DefinitionRevision: report.Revision,
		PeriodJSON: periodJSON, MappingSetHash: digestHex(HashBytes(mappingJSON)), ProviderRoute: "rest", RetentionClass: request.RetentionClass, AuditID: auditID})
	// Drop the only service-owned reference as soon as the one-way digest exists.
	request.IdempotencyKey = ""
	if err != nil {
		return SnapshotResponse{}, err
	}
	switch reservation.Disposition {
	case ReservationConflict:
		return SnapshotResponse{}, domainError("idempotency_conflict", "idempotency key was already used for a different canonical request")
	case ReservationInProgress:
		err := domainError("idempotency_in_progress", "an identical request is still in progress")
		err.Retryable = true
		return SnapshotResponse{}, err
	case ReservationReplay:
		response, err := decodeEnvelope[SnapshotResponse](reservation.Envelope)
		if err != nil {
			return SnapshotResponse{}, wrapError("idempotency_replay_invalid", "sealed response envelope is invalid", err)
		}
		response.Status = SnapshotIdempotencyReplay
		return response, nil
	case ReservationOwned:
		// Continue as the sole executor.
	default:
		return SnapshotResponse{}, domainError("idempotency_state_invalid", "reservation disposition is invalid")
	}
	if err := service.Store.MarkExecutionStarted(ctx, reservation.RecordID, reservation.OwnerNonce, service.now()); err != nil {
		_ = service.Store.failRunningSnapshot(ctx, internalSnapshotID, "idempotency_execution_unknown", err.Error(), service.now())
		failure := asStructured(err, auditID)
		_ = service.Store.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, failure, service.now())
		return SnapshotResponse{}, err
	}
	captureCtx, cancelCapture := context.WithCancel(ctx)
	heartbeatErrors := make(chan error, 1)
	heartbeatDone := make(chan struct{})
	defer func() { cancelCapture(); <-heartbeatDone }()
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(reservationLease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-captureCtx.Done():
				return
			case now := <-ticker.C:
				if err := service.Store.RenewReservation(captureCtx, reservation.RecordID, reservation.OwnerNonce, now.UTC()); err != nil {
					heartbeatErrors <- err
					cancelCapture()
					return
				}
			}
		}
	}()
	checkHeartbeat := func() error {
		select {
		case err := <-heartbeatErrors:
			return fmt.Errorf("assurance: snapshot reservation lease lost: %w", err)
		default:
			return nil
		}
	}

	observations := make([]SnapshotObservation, 0, len(report.Fields))
	itemErrors := make([]SnapshotItemError, 0)
	for _, field := range report.Fields {
		if heartbeatErr := checkHeartbeat(); heartbeatErr != nil {
			_ = service.Store.failRunningSnapshot(ctx, internalSnapshotID, "idempotency_execution_unknown", heartbeatErr.Error(), service.now())
			_ = service.Store.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(heartbeatErr, auditID), service.now())
			return SnapshotResponse{}, heartbeatErr
		}
		read, readErr := service.Reader.ReadUncached(captureCtx, SourceRequest{
			ExternalResourceID: field.ExternalResourceID, SubresourceID: field.SubresourceID,
			Locator: field.Locator, Consistency: request.Consistency,
		})
		if readErr != nil {
			itemErrors = append(itemErrors, SnapshotItemError{FieldID: field.FieldID, Code: classifyReadError(readErr), Message: "source read failed"})
			continue
		}
		if !read.CacheBypassed {
			itemErrors = append(itemErrors, SnapshotItemError{FieldID: field.FieldID, Code: "invalid", Message: "provider did not prove an uncached read"})
			continue
		}
		typedValue, normalizeErr := NormalizeProviderValue(read.Value, field)
		if normalizeErr != nil {
			itemErrors = append(itemErrors, SnapshotItemError{FieldID: field.FieldID, Code: "invalid", Message: "source value does not match approved type"})
			continue
		}
		if read.ProviderRevision.Strength == "" {
			read.ProviderRevision.Strength = RevisionUnavailable
		}
		observedAt := formatTimestamp(service.now())
		fingerprintBody := struct {
			TenantID           string           `json:"tenant_id"`
			Provider           string           `json:"provider"`
			ExternalResourceID string           `json:"external_resource_id"`
			SubresourceID      string           `json:"subresource_id"`
			Locator            string           `json:"locator"`
			MappingRevision    int              `json:"mapping_revision"`
			ProviderRevision   ProviderRevision `json:"provider_revision"`
			TypedValue         TypedValue       `json:"typed_value"`
		}{identity.StorageTenant(ctx), "workiva-rest", field.ExternalResourceID, field.SubresourceID, field.Locator, field.MappingRevision, read.ProviderRevision, typedValue}
		fingerprintJSON, _ := CanonicalJSON(fingerprintBody)
		observations = append(observations, SnapshotObservation{
			ObservationID: uuid.NewString(), FieldID: field.FieldID, ResourceID: field.ResourceID,
			ExternalResourceID: field.ExternalResourceID, Locator: field.Locator, TypedValue: typedValue,
			ProviderRevision: read.ProviderRevision, SourceFingerprint: digestHex(HashBytes(fingerprintJSON)), ObservedAt: observedAt,
		})
	}
	if heartbeatErr := checkHeartbeat(); heartbeatErr != nil {
		_ = service.Store.failRunningSnapshot(ctx, internalSnapshotID, "idempotency_execution_unknown", heartbeatErr.Error(), service.now())
		_ = service.Store.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(heartbeatErr, auditID), service.now())
		return SnapshotResponse{}, heartbeatErr
	}

	response := SnapshotResponse{
		NLAuditID: auditID, ReportID: report.ReportID, DefinitionRevision: report.Revision,
		Period: SnapshotPeriod{Key: report.Periods[0].Key, Label: report.Periods[0].Label}, CapturedAt: formatTimestamp(service.now()), Observations: observations,
		ItemErrors: itemErrors, RelationshipCount: 0,
	}
	if len(itemErrors) == 0 {
		response.Status = SnapshotCompleted
		response.Completeness = CompletenessComplete
		response.SnapshotID = internalSnapshotID
	} else if request.AllowPartial {
		response.Status = SnapshotPartial
		response.Completeness = CompletenessIncomplete
		response.SnapshotID = internalSnapshotID
	} else {
		response.Status = SnapshotFailed
		response.Completeness = CompletenessNotCreated
		response.Observations = []SnapshotObservation{}
	}
	hashBody := struct {
		TenantID           string                `json:"tenant_id"`
		ReportID           string                `json:"report_id"`
		DefinitionRevision int                   `json:"definition_revision"`
		Period             Period                `json:"period"`
		Completeness       Completeness          `json:"completeness"`
		Observations       []SnapshotObservation `json:"observations"`
		ItemErrors         []SnapshotItemError   `json:"item_errors"`
	}{identity.StorageTenant(ctx), report.ReportID, report.Revision, report.Periods[0], response.Completeness, observations, itemErrors}
	hashJSON, _ := CanonicalJSON(hashBody)
	response.ContentHash = digestHex(HashBytes(hashJSON))
	auditLog := service.Audit
	if auditLog == nil {
		auditLog = service.Store.auditLog
	}
	if err := service.Store.finalizeSnapshot(ctx, reservation, internalSnapshotID, response, observations, itemErrors, now, auditLog, actorID); err != nil {
		_ = service.Store.failRunningSnapshot(ctx, internalSnapshotID, "snapshot_finalize_failed", "snapshot finalization failed", service.now())
		failure := asStructured(err, auditID)
		_ = service.Store.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, failure, service.now())
		return SnapshotResponse{}, err
	}
	return response, nil
}

func (service SnapshotService) now() time.Time {
	if service.Now != nil {
		return service.Now().UTC()
	}
	return time.Now().UTC()
}

func canonicalFieldIDs(fields []FieldDefinition) []string {
	ids := make([]string, 0, len(fields))
	for _, field := range fields {
		ids = append(ids, field.FieldID)
	}
	sort.Strings(ids)
	return ids
}

func classifyReadError(err error) string {
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "denied") || strings.Contains(text, "forbidden") || strings.Contains(text, "permission") {
		return "denied"
	}
	if strings.Contains(text, "ambiguous") {
		return "ambiguous"
	}
	if strings.Contains(text, "invalid") || strings.Contains(text, "malformed") {
		return "invalid"
	}
	return "source_unavailable"
}

func (s *Store) finalizeSnapshot(ctx context.Context, reservation ReservationResult, internalSnapshotID string, response SnapshotResponse, observations []SnapshotObservation, failures []SnapshotItemError, now time.Time, auditLog *audit.Log, auditActor string) error {
	if auditLog == nil {
		auditLog = s.auditLog
	}
	if err := s.requireRichAudit(auditLog); err != nil {
		return err
	}
	tenant := identity.StorageTenant(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, observation := range observations {
		typedJSON, _ := marshalCanonical(observation.TypedValue)
		revisionJSON, _ := marshalCanonical(observation.ProviderRevision)
		if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_snapshot_observations
 (tenant_id, snapshot_id, observation_id, field_id, resource_id, external_resource_id, locator, typed_value_json,
  provider_revision_json, source_fingerprint, observed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, tenant,
			internalSnapshotID, observation.ObservationID, observation.FieldID, observation.ResourceID, observation.ExternalResourceID,
			observation.Locator, typedJSON, revisionJSON, observation.SourceFingerprint, observation.ObservedAt); err != nil {
			return fmt.Errorf("assurance: insert snapshot observation: %w", err)
		}
	}
	for _, failure := range failures {
		if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_snapshot_failures
 (tenant_id, snapshot_id, failure_id, field_id, code, message, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, tenant,
			internalSnapshotID, uuid.NewString(), failure.FieldID, failure.Code, failure.Message, formatTimestamp(now)); err != nil {
			return fmt.Errorf("assurance: insert snapshot failure: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE assurance_snapshots SET status=?, completeness=?, content_hash=?, captured_at=?
 WHERE tenant_id=? AND snapshot_id=? AND status='running'`, response.Status, response.Completeness, response.ContentHash,
		response.CapturedAt, tenant, internalSnapshotID); err != nil {
		return fmt.Errorf("assurance: finalize snapshot: %w", err)
	}
	trustedActor, err := reservationActor(ctx, tx, reservation)
	if err != nil {
		return err
	}
	auditJSON, err := CanonicalJSON(response)
	if err != nil {
		return err
	}
	if _, err := auditLog.AppendTx(ctx, tx, audit.Entry{
		Actor: trustedActor, Tool: "workiva_snapshot_report", Action: "capture", Target: internalSnapshotID,
		AfterJSON: string(auditJSON), AuditID: response.NLAuditID,
	}); err != nil {
		return fmt.Errorf("assurance: append snapshot audit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_audit_links
	 (tenant_id, link_id, entity_kind, entity_id, audit_id, request_id, correlation_id, created_at) VALUES (?, ?, 'snapshot', ?, ?, ?, ?, ?)`,
		tenant, uuid.NewString(), internalSnapshotID, response.NLAuditID, RequestIDFromContext(ctx), reservation.CorrelationID, formatTimestamp(now)); err != nil {
		return fmt.Errorf("assurance: link snapshot audit: %w", err)
	}
	envelope, err := CanonicalJSON(response)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET state='sealed', response_envelope=?, response_hash=?,
 response_status=?, entity_reference=?, audit_id=?, terminal_at=?, lease_expires_at=?
 WHERE tenant_id=? AND record_id=? AND state='reserved' AND owner_nonce=?`, string(envelope), digestHex(HashBytes(envelope)),
		response.Status, internalSnapshotID, response.NLAuditID, formatTimestamp(now), formatTimestamp(now), tenant, reservation.RecordID, reservation.OwnerNonce)
	if err != nil {
		return fmt.Errorf("assurance: seal snapshot reservation: %w", err)
	}
	if err := requireOneTransition(result); err != nil {
		return err
	}
	return tx.Commit()
}

// Snapshot retrieves one immutable assurance snapshot by trusted tenant.
func (s *Store) Snapshot(ctx context.Context, snapshotID string) (SnapshotResponse, error) {
	tenant := identity.StorageTenant(ctx)
	var response SnapshotResponse
	var periodJSON string
	err := s.db.QueryRowContext(ctx, `SELECT report_id, definition_revision, period_json, status, completeness, content_hash, captured_at, audit_id
 FROM assurance_snapshots WHERE tenant_id=? AND snapshot_id=? AND status IN ('completed','partial')`, tenant, snapshotID).Scan(
		&response.ReportID, &response.DefinitionRevision, &periodJSON, &response.Status, &response.Completeness, &response.ContentHash, &response.CapturedAt, &response.NLAuditID)
	if errors.Is(err, sql.ErrNoRows) {
		return SnapshotResponse{}, domainError("snapshot_not_found", "immutable snapshot was not found")
	}
	if err != nil {
		return SnapshotResponse{}, err
	}
	response.SnapshotID = snapshotID
	var period Period
	if err := json.Unmarshal([]byte(periodJSON), &period); err != nil {
		return SnapshotResponse{}, err
	}
	response.Period = SnapshotPeriod{Key: period.Key, Label: period.Label}
	response.Observations = []SnapshotObservation{}
	observationRows, err := s.db.QueryContext(ctx, `SELECT observation_id, field_id, resource_id, external_resource_id, locator,
 typed_value_json, provider_revision_json, source_fingerprint, observed_at
 FROM assurance_snapshot_observations WHERE tenant_id=? AND snapshot_id=? ORDER BY rowid`, tenant, snapshotID)
	if err != nil {
		return SnapshotResponse{}, err
	}
	for observationRows.Next() {
		var observation SnapshotObservation
		var typedJSON, revisionJSON string
		if err := observationRows.Scan(&observation.ObservationID, &observation.FieldID, &observation.ResourceID, &observation.ExternalResourceID,
			&observation.Locator, &typedJSON, &revisionJSON, &observation.SourceFingerprint, &observation.ObservedAt); err != nil {
			_ = observationRows.Close()
			return SnapshotResponse{}, err
		}
		if err := json.Unmarshal([]byte(typedJSON), &observation.TypedValue); err != nil {
			_ = observationRows.Close()
			return SnapshotResponse{}, fmt.Errorf("assurance: decode snapshot typed value: %w", err)
		}
		if err := json.Unmarshal([]byte(revisionJSON), &observation.ProviderRevision); err != nil {
			_ = observationRows.Close()
			return SnapshotResponse{}, fmt.Errorf("assurance: decode snapshot provider revision: %w", err)
		}
		response.Observations = append(response.Observations, observation)
	}
	if err := observationRows.Err(); err != nil {
		_ = observationRows.Close()
		return SnapshotResponse{}, err
	}
	if err := observationRows.Close(); err != nil {
		return SnapshotResponse{}, err
	}
	response.ItemErrors = []SnapshotItemError{}
	failureRows, err := s.db.QueryContext(ctx, `SELECT field_id, code, message FROM assurance_snapshot_failures
 WHERE tenant_id=? AND snapshot_id=? ORDER BY rowid`, tenant, snapshotID)
	if err != nil {
		return SnapshotResponse{}, err
	}
	for failureRows.Next() {
		var failure SnapshotItemError
		if err := failureRows.Scan(&failure.FieldID, &failure.Code, &failure.Message); err != nil {
			_ = failureRows.Close()
			return SnapshotResponse{}, err
		}
		response.ItemErrors = append(response.ItemErrors, failure)
	}
	if err := failureRows.Err(); err != nil {
		_ = failureRows.Close()
		return SnapshotResponse{}, err
	}
	if err := failureRows.Close(); err != nil {
		return SnapshotResponse{}, err
	}
	return response, nil
}
