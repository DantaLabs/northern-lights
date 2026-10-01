package assurance

import (
	"context"
	"math/big"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
)

type CompareRequest struct {
	CurrentSnapshotID   string   `json:"current_snapshot_id"`
	PriorSnapshotID     string   `json:"prior_snapshot_id"`
	MaterialityPolicyID string   `json:"materiality_policy_id"`
	FieldIDs            []string `json:"field_ids,omitempty"`
	IncludeUnchanged    bool     `json:"include_unchanged"`
	RetentionClass      string   `json:"retention_class,omitempty"`
	IdempotencyKey      string   `json:"idempotency_key"`
}

type ComparisonItem struct {
	ComparisonItemID       string      `json:"comparison_item_id"`
	FieldID                string      `json:"field_id"`
	Prior                  *TypedValue `json:"prior,omitempty"`
	Current                *TypedValue `json:"current,omitempty"`
	AbsoluteDelta          string      `json:"absolute_delta,omitempty"`
	PercentageDelta        string      `json:"percentage_delta,omitempty"`
	ComparisonStatus       string      `json:"comparison_status"`
	MaterialityStatus      string      `json:"materiality_status"`
	EvidenceObservationIDs []string    `json:"evidence_observation_ids"`
}

type ComparisonResponse struct {
	NLAuditID           string           `json:"nl_audit_id"`
	Status              string           `json:"status"`
	ComparisonID        string           `json:"comparison_id"`
	CurrentSnapshotID   string           `json:"current_snapshot_id"`
	PriorSnapshotID     string           `json:"prior_snapshot_id"`
	Completeness        Completeness     `json:"completeness"`
	ComparisonBasis     string           `json:"comparison_basis"`
	MaterialityPolicyID string           `json:"materiality_policy_id"`
	MaterialityRevision int              `json:"materiality_revision"`
	Changes             []ComparisonItem `json:"changes"`
	MaterialCount       int              `json:"material_count"`
	currentReportID     string           `json:"-"`
	priorReportID       string           `json:"-"`
	currentRevision     int              `json:"-"`
	priorRevision       int              `json:"-"`
	partialPolicy       string           `json:"-"`
}

const (
	ComparisonCompleted         = "completed"
	ComparisonIncompatible      = "incompatible"
	ComparisonError             = "error"
	ComparisonIdempotencyReplay = "idempotency_replay"
	ComparisonBasisUnion        = "definition_membership_union"
	ComparisonBasisExplicit     = "explicit_field_ids"
)

type CompareService struct {
	Store *Store
	Audit *audit.Log
	Now   func() time.Time
}

func (service CompareService) now() time.Time {
	if service.Now != nil {
		return service.Now().UTC()
	}
	return time.Now().UTC()
}

func (service CompareService) Compare(ctx context.Context, actorID, auditID string, request CompareRequest) (ComparisonResponse, error) {
	if service.Store == nil {
		return ComparisonResponse{}, domainError("dependency_unavailable", "assurance store is unavailable")
	}
	if request.CurrentSnapshotID == "" || request.PriorSnapshotID == "" || request.MaterialityPolicyID == "" || request.IdempotencyKey == "" || len(request.FieldIDs) > maxFieldCount {
		return ComparisonResponse{}, domainError("invalid_request", "snapshot IDs, materiality policy, and idempotency key are required")
	}
	if request.RetentionClass == "" {
		request.RetentionClass = "standard"
	}
	current, err := service.Store.SnapshotForAnalysis(ctx, request.CurrentSnapshotID)
	if err != nil {
		return ComparisonResponse{}, err
	}
	prior, err := service.Store.SnapshotForAnalysis(ctx, request.PriorSnapshotID)
	if err != nil {
		return ComparisonResponse{}, err
	}
	policy, err := service.Store.ResolveMaterialityPolicy(ctx, request.MaterialityPolicyID, 0)
	if err != nil {
		return ComparisonResponse{}, err
	}
	if (current.Response.Completeness == CompletenessIncomplete || prior.Response.Completeness == CompletenessIncomplete) && !policy.permitsPartialComparison() {
		return ComparisonResponse{}, domainError("partial_comparison_not_permitted", "materiality policy does not permit partial comparison")
	}
	fieldIDs, basis, err := comparisonFieldIDs(current, prior, request.FieldIDs)
	if err != nil {
		return ComparisonResponse{}, err
	}
	canonicalRequest, err := CanonicalJSON(struct {
		Current   string   `json:"current_snapshot_id"`
		Prior     string   `json:"prior_snapshot_id"`
		Policy    string   `json:"materiality_policy_id"`
		Fields    []string `json:"field_ids"`
		Include   bool     `json:"include_unchanged"`
		Retention string   `json:"retention_class"`
	}{request.CurrentSnapshotID, request.PriorSnapshotID, request.MaterialityPolicyID, fieldIDs, request.IncludeUnchanged, request.RetentionClass})
	if err != nil {
		return ComparisonResponse{}, err
	}
	reservation, err := service.Store.Reserve(ctx, ReservationRequest{ActorID: actorID, Tool: "workiva_compare_periods", Action: "compare", IdempotencyDigest: DigestIdempotencyKey(request.IdempotencyKey), RequestDigest: HashBytes(canonicalRequest), RetentionClass: request.RetentionClass}, service.now())
	idempotencyDigest := digestHex(DigestIdempotencyKey(request.IdempotencyKey))
	request.IdempotencyKey = ""
	if err != nil {
		return ComparisonResponse{}, err
	}
	switch reservation.Disposition {
	case ReservationConflict:
		return ComparisonResponse{}, domainError("idempotency_conflict", "idempotency key was already used for a different canonical request")
	case ReservationInProgress:
		e := domainError("idempotency_in_progress", "an identical request is still in progress")
		e.Retryable = true
		return ComparisonResponse{}, e
	case ReservationReplay:
		response, decodeErr := decodeEnvelope[ComparisonResponse](reservation.Envelope)
		if decodeErr != nil {
			return ComparisonResponse{}, decodeErr
		}
		response.Status = ComparisonIdempotencyReplay
		return response, nil
	case ReservationOwned:
	default:
		return ComparisonResponse{}, domainError("idempotency_state_invalid", "reservation disposition is invalid")
	}
	if err := service.Store.MarkExecutionStarted(ctx, reservation.RecordID, reservation.OwnerNonce, service.now()); err != nil {
		_ = service.Store.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(err, auditID), service.now())
		return ComparisonResponse{}, err
	}
	response := ComparisonResponse{NLAuditID: auditID, Status: ComparisonCompleted, ComparisonID: uuid.NewString(), CurrentSnapshotID: request.CurrentSnapshotID, PriorSnapshotID: request.PriorSnapshotID, Completeness: CompletenessComplete, ComparisonBasis: basis, MaterialityPolicyID: policy.PolicyID, MaterialityRevision: policy.Revision, Changes: []ComparisonItem{}}
	response.currentReportID, response.priorReportID = current.ReportID, prior.ReportID
	response.currentRevision, response.priorRevision = current.Revision, prior.Revision
	response.partialPolicy = "reject"
	if current.Response.Completeness == CompletenessIncomplete || prior.Response.Completeness == CompletenessIncomplete {
		response.Completeness = CompletenessIncomplete
		response.partialPolicy = "label_incomplete"
	}
	if current.ReportID != prior.ReportID {
		response.Status = ComparisonIncompatible
		response.Completeness = CompletenessNotCreated
	} else {
		response.Changes, response.MaterialCount = buildComparisonItems(current, prior, policy, fieldIDs, request.IncludeUnchanged)
	}
	if err := service.Store.finalizeComparison(ctx, reservation, response, idempotencyDigest, service.Audit, actorID, service.now()); err != nil {
		_ = service.Store.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(err, auditID), service.now())
		return ComparisonResponse{}, err
	}
	return response, nil
}

func comparisonFieldIDs(current, prior SnapshotAnalysis, requested []string) ([]string, string, error) {
	fields := membershipByField(current.Membership)
	for id, field := range membershipByField(prior.Membership) {
		fields[id] = field
	}
	if len(requested) == 0 {
		return sortedFieldIDs(fields), ComparisonBasisUnion, nil
	}
	seen := make(map[string]bool, len(requested))
	ids := append([]string(nil), requested...)
	for _, id := range ids {
		if id == "" || seen[id] {
			return nil, "", domainError("field_selection_invalid", "field_ids contains a duplicate or empty ID")
		}
		if _, ok := fields[id]; !ok {
			return nil, "", domainError("field_not_approved", "requested field is not in either approved definition revision")
		}
		seen[id] = true
	}
	sort.Strings(ids)
	return ids, ComparisonBasisExplicit, nil
}

func buildComparisonItems(current, prior SnapshotAnalysis, policy MaterialityPolicy, ids []string, includeUnchanged bool) ([]ComparisonItem, int) {
	currentFields, priorFields := membershipByField(current.Membership), membershipByField(prior.Membership)
	currentObs, priorObs := observationMap(current.Response.Observations), observationMap(prior.Response.Observations)
	items := make([]ComparisonItem, 0, len(ids))
	material := 0
	for _, id := range ids {
		item := ComparisonItem{ComparisonItemID: uuid.NewString(), FieldID: id, EvidenceObservationIDs: []string{}}
		cur, curOK := currentObs[id]
		prev, prevOK := priorObs[id]
		if curOK {
			value := cur.TypedValue
			item.Current = &value
			item.EvidenceObservationIDs = append(item.EvidenceObservationIDs, cur.ObservationID)
		}
		if prevOK {
			value := prev.TypedValue
			item.Prior = &value
			item.EvidenceObservationIDs = append(item.EvidenceObservationIDs, prev.ObservationID)
		}
		switch {
		case !curOK && !prevOK:
			item.ComparisonStatus, item.MaterialityStatus = "not_comparable", "not_comparable"
		case !curOK:
			if _, inMembership := currentFields[id]; inMembership {
				item.ComparisonStatus = "missing_in_current"
			} else {
				item.ComparisonStatus = "removed"
			}
			item.MaterialityStatus = missingMateriality(policy)
		case !prevOK:
			if _, inMembership := priorFields[id]; inMembership {
				item.ComparisonStatus = "missing_in_prior"
			} else {
				item.ComparisonStatus = "added"
			}
			item.MaterialityStatus = missingMateriality(policy)
		case cur.TypedValue.Kind != prev.TypedValue.Kind || cur.TypedValue.Unit != prev.TypedValue.Unit:
			item.ComparisonStatus = "not_comparable"
			item.MaterialityStatus = typeChangeMateriality(policy)
		default:
			if cur.TypedValue.Number != "" && prev.TypedValue.Number != "" {
				delta, err := SubtractDecimal(cur.TypedValue.Number, prev.TypedValue.Number)
				if err != nil {
					item.ComparisonStatus, item.MaterialityStatus = "not_comparable", "not_comparable"
				} else {
					item.AbsoluteDelta, _ = AbsoluteDecimal(delta)
					if prev.TypedValue.Number != "0" {
						ratio, _ := DivideDecimal(delta, prev.TypedValue.Number, roundingMode(policy.Rounding))
						item.PercentageDelta, _ = multiplyDecimal(ratio, "100")
					}
					if delta == "0" {
						item.ComparisonStatus = "unchanged"
					} else {
						item.ComparisonStatus = "changed"
					}
					item.MaterialityStatus = materialityStatus(policy, item.AbsoluteDelta, delta, prev.TypedValue.Number)
					if item.MaterialityStatus == "material" {
						material++
					}
				}
			} else if typedValuesEqual(cur.TypedValue, prev.TypedValue) {
				item.ComparisonStatus, item.MaterialityStatus = "unchanged", "immaterial"
			} else {
				item.ComparisonStatus, item.MaterialityStatus = "changed", "unassessed"
			}
		}
		if item.ComparisonStatus == "unchanged" && !includeUnchanged {
			continue
		}
		items = append(items, item)
	}
	return items, material
}

func observationMap(values []SnapshotObservation) map[string]SnapshotObservation {
	result := make(map[string]SnapshotObservation, len(values))
	for _, value := range values {
		result[value.FieldID] = value
	}
	return result
}
func roundingMode(value string) RoundingMode {
	if value == string(RoundHalfUp) {
		return RoundHalfUp
	}
	if value == string(RoundTruncate) {
		return RoundTruncate
	}
	return RoundHalfEven
}
func missingMateriality(policy MaterialityPolicy) string {
	switch policy.MissingBehavior {
	case "material":
		return "material"
	case "immaterial":
		return "immaterial"
	default:
		return "not_comparable"
	}
}
func typeChangeMateriality(policy MaterialityPolicy) string {
	if policy.TypeChangeBehavior == "material" {
		return "material"
	}
	return "not_comparable"
}
func materialityStatus(policy MaterialityPolicy, absolute, delta, prior string) string {
	if prior == "0" && policy.ZeroBaseline == "not_comparable" {
		return "not_comparable"
	}
	absEnabled, relEnabled := policy.AbsoluteThreshold != "", policy.RelativeThreshold != ""
	absMaterial, relMaterial := false, false
	if absEnabled {
		if cmp, _ := CompareDecimal(absolute, policy.AbsoluteThreshold); cmp >= 0 {
			absMaterial = true
		}
	}
	if relEnabled && prior != "0" {
		ratio, err := DivideDecimal(delta, prior, roundingMode(policy.Rounding))
		if err == nil {
			absRatio, _ := AbsoluteDecimal(ratio)
			if cmp, _ := CompareDecimal(absRatio, policy.RelativeThreshold); cmp >= 0 {
				relMaterial = true
			}
		}
	}
	switch policy.Direction {
	case "absolute":
		if absEnabled && absMaterial {
			return "material"
		}
	case "relative":
		if relEnabled && relMaterial {
			return "material"
		}
	case "absolute_and_relative":
		if absEnabled && relEnabled && absMaterial && relMaterial {
			return "material"
		}
	default:
		if absMaterial || relMaterial {
			return "material"
		}
	}
	return "immaterial"
}

func multiplyDecimal(a, b string) (string, error) {
	ac, as, err := ParseDecimal(a)
	if err != nil {
		return "", err
	}
	bc, bs, err := ParseDecimal(b)
	if err != nil {
		return "", err
	}
	return formatDecimal(new(big.Int).Mul(ac, bc), as+bs)
}

func (s *Store) finalizeComparison(ctx context.Context, reservation ReservationResult, response ComparisonResponse, idempotencyDigest string, auditLog *audit.Log, actor string, now time.Time) error {
	tenant := identity.StorageTenant(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO assurance_comparisons (tenant_id, comparison_id, current_snapshot_id, prior_snapshot_id, idempotency_digest, status, completeness, comparison_basis, policy_revision, created_at, materiality_policy_id, current_report_id, prior_report_id, current_definition_revision, prior_definition_revision, partial_policy, material_count) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, tenant, response.ComparisonID, response.CurrentSnapshotID, response.PriorSnapshotID, idempotencyDigest, response.Status, response.Completeness, response.ComparisonBasis, response.MaterialityRevision, formatTimestamp(now), response.MaterialityPolicyID, response.currentReportID, response.priorReportID, response.currentRevision, response.priorRevision, response.partialPolicy, response.MaterialCount)
	if err != nil {
		return err
	}
	for _, item := range response.Changes {
		raw, err := marshalCanonical(item)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_comparison_items (tenant_id, comparison_id, comparison_item_id, field_id, item_json) VALUES (?, ?, ?, ?, ?)`, tenant, response.ComparisonID, item.ComparisonItemID, item.FieldID, raw); err != nil {
			return err
		}
	}
	if auditLog != nil && auditLog.SharesDB(s.db) {
		raw, err := CanonicalJSON(response)
		if err != nil {
			return err
		}
		if _, err := auditLog.AppendTx(ctx, tx, audit.Entry{Actor: actor, Tool: "workiva_compare_periods", Action: "compare", Target: response.ComparisonID, AfterJSON: string(raw), AuditID: response.NLAuditID}); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_audit_links (tenant_id, link_id, entity_kind, entity_id, audit_id, request_id, correlation_id, created_at) VALUES (?, ?, 'comparison', ?, ?, ?, ?, ?)`, tenant, uuid.NewString(), response.ComparisonID, response.NLAuditID, uuid.NewString(), reservation.CorrelationID, formatTimestamp(now)); err != nil {
		return err
	}
	envelope, err := CanonicalJSON(response)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET state='sealed', response_envelope=?, response_hash=?, response_status=?, entity_reference=?, audit_id=?, terminal_at=?, lease_expires_at=? WHERE tenant_id=? AND record_id=? AND state='reserved' AND owner_nonce=?`, string(envelope), digestHex(HashBytes(envelope)), response.Status, response.ComparisonID, response.NLAuditID, formatTimestamp(now), formatTimestamp(now), tenant, reservation.RecordID, reservation.OwnerNonce)
	if err != nil {
		return err
	}
	if err := requireOneTransition(result); err != nil {
		return err
	}
	return tx.Commit()
}

// Keep the comparison implementation's decimal multiplication in this file's
// package without widening the public arithmetic surface.
