package assurance

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
)

type ValidationRequest struct {
	SnapshotID     string   `json:"snapshot_id"`
	RuleSetID      string   `json:"rule_set_id"`
	RuleIDs        []string `json:"rule_ids,omitempty"`
	FailOnWarning  bool     `json:"fail_on_warning"`
	RetentionClass string   `json:"retention_class,omitempty"`
	IdempotencyKey string   `json:"idempotency_key"`
}

type ValidationResult struct {
	ResultID               string      `json:"result_id"`
	RuleID                 string      `json:"rule_id"`
	RuleRevision           int         `json:"rule_revision"`
	Code                   string      `json:"code"`
	Status                 string      `json:"status"`
	Actual                 *TypedValue `json:"actual,omitempty"`
	Expected               *TypedValue `json:"expected,omitempty"`
	AbsoluteTolerance      string      `json:"absolute_tolerance,omitempty"`
	RelativeTolerance      string      `json:"relative_tolerance,omitempty"`
	FieldIDs               []string    `json:"field_ids"`
	EvidenceObservationIDs []string    `json:"evidence_observation_ids"`
}

type ValidationResponse struct {
	NLAuditID       string             `json:"nl_audit_id"`
	Status          string             `json:"status"`
	ValidationRunID string             `json:"validation_run_id"`
	SnapshotID      string             `json:"snapshot_id"`
	RuleSetID       string             `json:"rule_set_id"`
	RuleSetRevision int                `json:"rule_set_revision"`
	Counts          map[string]int     `json:"counts"`
	Results         []ValidationResult `json:"results"`
}

const (
	ValidationPassed            = "passed"
	ValidationFailed            = "failed"
	ValidationError             = "error"
	ValidationNotEvaluable      = "not_evaluable"
	ValidationIdempotencyReplay = "idempotency_replay"
)

type ValidationService struct {
	Store *Store
	Audit *audit.Log
	Now   func() time.Time
}

func (service ValidationService) now() time.Time {
	if service.Now != nil {
		return service.Now().UTC()
	}
	return time.Now().UTC()
}

func (service ValidationService) Validate(ctx context.Context, actorID, auditID string, request ValidationRequest) (ValidationResponse, error) {
	if service.Store == nil {
		return ValidationResponse{}, domainError("dependency_unavailable", "assurance store is unavailable")
	}
	if err := service.Store.Ready(); err != nil {
		return ValidationResponse{}, err
	}
	if err := service.Store.requireRichAudit(service.Audit); err != nil {
		return ValidationResponse{}, err
	}
	if request.SnapshotID == "" || request.RuleSetID == "" || request.IdempotencyKey == "" || len(request.RuleIDs) > maxFieldCount {
		return ValidationResponse{}, domainError("invalid_request", "snapshot_id, rule_set_id, and idempotency_key are required")
	}
	if request.RetentionClass == "" {
		request.RetentionClass = "standard"
	}
	canonicalRequest, err := CanonicalJSON(struct {
		SnapshotID     string   `json:"snapshot_id"`
		RuleSetID      string   `json:"rule_set_id"`
		RuleIDs        []string `json:"rule_ids"`
		FailOnWarning  bool     `json:"fail_on_warning"`
		RetentionClass string   `json:"retention_class"`
	}{request.SnapshotID, request.RuleSetID, append([]string(nil), request.RuleIDs...), request.FailOnWarning, request.RetentionClass})
	if err != nil {
		return ValidationResponse{}, err
	}
	now := service.now()
	idempotencyDigest := digestHex(DigestIdempotencyKey(request.IdempotencyKey))
	reservation, err := service.Store.Reserve(ctx, ReservationRequest{ActorID: actorID, Tool: "workiva_validate_report", Action: "validate", IdempotencyDigest: DigestIdempotencyKey(request.IdempotencyKey), RequestDigest: HashBytes(canonicalRequest), RetentionClass: request.RetentionClass}, now)
	request.IdempotencyKey = ""
	if err != nil {
		return ValidationResponse{}, err
	}
	switch reservation.Disposition {
	case ReservationConflict:
		return ValidationResponse{}, domainError("idempotency_conflict", "idempotency key was already used for a different canonical request")
	case ReservationInProgress:
		e := domainError("idempotency_in_progress", "an identical request is still in progress")
		e.Retryable = true
		return ValidationResponse{}, e
	case ReservationReplay:
		response, decodeErr := decodeEnvelope[ValidationResponse](reservation.Envelope)
		if decodeErr != nil {
			return ValidationResponse{}, decodeErr
		}
		response.Status = ValidationIdempotencyReplay
		return response, nil
	case ReservationOwned:
	default:
		return ValidationResponse{}, domainError("idempotency_state_invalid", "reservation disposition is invalid")
	}
	analysis, err := service.Store.SnapshotForAnalysis(ctx, request.SnapshotID)
	if err != nil {
		_ = service.Store.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(err, auditID), service.now())
		return ValidationResponse{}, err
	}
	ruleSet, err := service.Store.ResolveRuleSet(ctx, request.RuleSetID, 0)
	if err != nil {
		_ = service.Store.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(err, auditID), service.now())
		return ValidationResponse{}, err
	}
	rules, err := selectRules(ruleSet, request.RuleIDs)
	if err != nil {
		_ = service.Store.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(err, auditID), service.now())
		return ValidationResponse{}, err
	}
	if err := service.Store.MarkExecutionStarted(ctx, reservation.RecordID, reservation.OwnerNonce, now); err != nil {
		_ = service.Store.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(err, auditID), service.now())
		return ValidationResponse{}, err
	}
	results := evaluateRules(analysis, rules)
	counts := map[string]int{"pass": 0, "fail": 0, "warn": 0, "not_evaluable": 0, "error": 0}
	for _, result := range results {
		counts[result.Status]++
	}
	status := ValidationPassed
	if counts["error"] > 0 {
		status = ValidationError
	} else if counts["fail"] > 0 || (request.FailOnWarning && counts["warn"] > 0) {
		status = ValidationFailed
	} else if counts["not_evaluable"] > 0 {
		status = ValidationNotEvaluable
	}
	response := ValidationResponse{NLAuditID: auditID, Status: status, ValidationRunID: uuid.NewString(), SnapshotID: request.SnapshotID, RuleSetID: request.RuleSetID, RuleSetRevision: ruleSet.Revision, Counts: counts, Results: results}
	auditLog := service.Audit
	if auditLog == nil {
		auditLog = service.Store.auditLog
	}
	if err := service.Store.finalizeValidation(ctx, reservation, response, idempotencyDigest, request.FailOnWarning, auditLog, actorID, service.now()); err != nil {
		_ = service.Store.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, asStructured(err, auditID), service.now())
		return ValidationResponse{}, err
	}
	return response, nil
}

func selectRules(set RuleSet, requested []string) ([]RuleDefinition, error) {
	all := ruleOrder(set)
	if len(requested) == 0 {
		return all, nil
	}
	wanted := make(map[string]bool, len(requested))
	for _, id := range requested {
		if wanted[id] {
			return nil, domainError("rule_selection_invalid", "rule_ids contains a duplicate")
		}
		wanted[id] = true
	}
	result := make([]RuleDefinition, 0, len(requested))
	for _, rule := range all {
		if wanted[rule.RuleID] {
			result = append(result, rule)
			delete(wanted, rule.RuleID)
		}
	}
	if len(wanted) != 0 {
		return nil, domainError("rule_not_approved", "requested rule is not in the approved rule set")
	}
	return result, nil
}

func evaluateRules(analysis SnapshotAnalysis, rules []RuleDefinition) []ValidationResult {
	observations := make(map[string]SnapshotObservation, len(analysis.Response.Observations))
	for _, observation := range analysis.Response.Observations {
		observations[observation.FieldID] = observation
	}
	results := make([]ValidationResult, 0, len(rules))
	for _, rule := range rules {
		result := ValidationResult{ResultID: uuid.NewString(), RuleID: rule.RuleID, RuleRevision: rule.Revision, Code: validationCode(rule.Kind), FieldIDs: append([]string(nil), rule.FieldIDs...), EvidenceObservationIDs: []string{}}
		for _, fieldID := range rule.FieldIDs {
			if observation, ok := observations[fieldID]; ok {
				result.EvidenceObservationIDs = append(result.EvidenceObservationIDs, observation.ObservationID)
			}
		}
		if analysis.Response.Completeness == CompletenessIncomplete {
			result.Status = "not_evaluable"
			results = append(results, result)
			continue
		}
		result.Status, result.Actual, result.Expected = evaluateRule(rule, observations)
		result.AbsoluteTolerance, result.RelativeTolerance = rule.AbsoluteTolerance, rule.RelativeTolerance
		results = append(results, result)
	}
	return results
}

func validationCode(kind RuleKind) string {
	switch kind {
	case RuleNumericRange:
		return "range"
	case RuleReconciliationSum:
		return "reconciliation"
	default:
		return string(kind)
	}
}

func evaluateRule(rule RuleDefinition, observations map[string]SnapshotObservation) (string, *TypedValue, *TypedValue) {
	first, exists := observations[firstField(rule)]
	if !exists {
		if rule.Kind == RuleRequired {
			return "fail", nil, nil
		}
		return "not_evaluable", nil, nil
	}
	actual := &first.TypedValue
	switch rule.Kind {
	case RuleRequired:
		if isBlankValue(first.TypedValue) {
			return ruleFailure(rule), actual, nil
		}
		return "pass", actual, nil
	case RuleType:
		if first.TypedValue.Kind == rule.ExpectedKind {
			return "pass", actual, nil
		}
		expected := &TypedValue{Kind: rule.ExpectedKind}
		return ruleFailure(rule), actual, expected
	case RuleUnit:
		if first.TypedValue.Unit == rule.Unit {
			return "pass", actual, nil
		}
		expected := &TypedValue{Kind: first.TypedValue.Kind, Unit: rule.Unit}
		return ruleFailure(rule), actual, expected
	case RuleNumericRange:
		if first.TypedValue.Number == "" {
			return "error", actual, nil
		}
		if rule.Lower != "" {
			if cmp, err := CompareDecimal(first.TypedValue.Number, rule.Lower); err != nil {
				return "error", actual, nil
			} else if cmp < 0 {
				return ruleFailure(rule), actual, &TypedValue{Kind: ValueNumber, Number: rule.Lower}
			}
		}
		if rule.Upper != "" {
			if cmp, err := CompareDecimal(first.TypedValue.Number, rule.Upper); err != nil {
				return "error", actual, nil
			} else if cmp > 0 {
				return ruleFailure(rule), actual, &TypedValue{Kind: ValueNumber, Number: rule.Upper}
			}
		}
		return "pass", actual, nil
	case RuleAllowedValues:
		for _, allowed := range rule.AllowedValues {
			if typedValuesEqual(first.TypedValue, allowed) {
				return "pass", actual, nil
			}
		}
		return ruleFailure(rule), actual, nil
	case RuleCompleteness:
		return "pass", actual, nil
	case RuleVariance:
		targetID := rule.TargetFieldID
		if targetID == "" && len(rule.FieldIDs) > 1 {
			targetID = rule.FieldIDs[1]
		}
		target, ok := observations[targetID]
		if !ok {
			return "not_evaluable", actual, nil
		}
		delta, err := AbsoluteDecimal(mustNumber(first.TypedValue))
		if err != nil {
			return "error", actual, nil
		}
		_ = delta
		diff, err := AbsoluteDecimal(mustSubtract(first.TypedValue, target.TypedValue))
		if err != nil {
			return "error", actual, nil
		}
		if !withinTolerance(diff, rule.AbsoluteTolerance, rule.RelativeTolerance, target.TypedValue.Number) {
			return ruleFailure(rule), actual, &target.TypedValue
		}
		return "pass", actual, &target.TypedValue
	case RuleReconciliationSum:
		return evaluateReconciliation(rule, observations)
	default:
		return "error", actual, nil
	}
}

func firstField(rule RuleDefinition) string {
	if len(rule.FieldIDs) == 0 {
		return ""
	}
	return rule.FieldIDs[0]
}
func mustNumber(value TypedValue) string { return value.Number }
func mustSubtract(a, b TypedValue) string {
	value, _ := SubtractDecimal(a.Number, b.Number)
	return value
}

func isBlankValue(value TypedValue) bool {
	if value.Kind == ValueBlank || value.Kind == ValueError {
		return true
	}
	if value.Kind == ValueText {
		return strings.TrimSpace(value.Text) == ""
	}
	return value.Number == "" && value.Date == "" && value.DateTime == ""
}

func ruleFailure(rule RuleDefinition) string {
	if rule.FailureSeverity == "warn" {
		return "warn"
	}
	return "fail"
}

func withinTolerance(delta, absolute, relative, baseline string) bool {
	if absolute != "" {
		if cmp, err := CompareDecimal(delta, absolute); err == nil && cmp <= 0 {
			return true
		}
	}
	if relative != "" && baseline != "" {
		base, err := AbsoluteDecimal(baseline)
		if err != nil || base == "0" {
			return false
		}
		ratio, err := DivideDecimal(delta, base, RoundHalfEven)
		if err != nil {
			return false
		}
		ratio, err = AbsoluteDecimal(ratio)
		if err != nil {
			return false
		}
		if cmp, err := CompareDecimal(ratio, relative); err == nil && cmp <= 0 {
			return true
		}
	}
	return absolute == "" && relative == ""
}

func evaluateReconciliation(rule RuleDefinition, observations map[string]SnapshotObservation) (string, *TypedValue, *TypedValue) {
	targetID := rule.TargetFieldID
	if targetID == "" && len(rule.FieldIDs) > 1 {
		targetID = rule.FieldIDs[len(rule.FieldIDs)-1]
	}
	target, ok := observations[targetID]
	if !ok {
		return "not_evaluable", nil, nil
	}
	sum := "0"
	for _, fieldID := range rule.FieldIDs {
		if fieldID == targetID {
			continue
		}
		observation, exists := observations[fieldID]
		if !exists {
			return "not_evaluable", nil, nil
		}
		var err error
		sum, err = AddDecimal(sum, observation.TypedValue.Number)
		if err != nil {
			return "error", nil, nil
		}
	}
	if cmp, err := CompareDecimal(sum, target.TypedValue.Number); err != nil {
		return "error", nil, nil
	} else if cmp != 0 && !withinTolerance(mustSubtract(TypedValue{Number: sum}, target.TypedValue), rule.AbsoluteTolerance, rule.RelativeTolerance, target.TypedValue.Number) {
		return ruleFailure(rule), &target.TypedValue, &TypedValue{Kind: target.TypedValue.Kind, Number: sum}
	}
	return "pass", &target.TypedValue, &TypedValue{Kind: target.TypedValue.Kind, Number: sum}
}

func typedValuesEqual(a, b TypedValue) bool {
	left, _ := CanonicalJSON(a)
	right, _ := CanonicalJSON(b)
	return string(left) == string(right)
}

func (s *Store) finalizeValidation(ctx context.Context, reservation ReservationResult, response ValidationResponse, idempotencyDigest string, failOnWarning bool, auditLog *audit.Log, actor string, now time.Time) error {
	tenant := identity.StorageTenant(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO assurance_validation_runs (tenant_id, validation_run_id, snapshot_id, rule_set_id, rule_set_revision, idempotency_digest, status, created_at, fail_on_warning) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, tenant, response.ValidationRunID, response.SnapshotID, response.RuleSetID, response.RuleSetRevision, idempotencyDigest, response.Status, formatTimestamp(now), failOnWarning)
	if err != nil {
		return err
	}
	for _, result := range response.Results {
		raw, err := marshalCanonical(result)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_validation_results (tenant_id, validation_run_id, result_id, rule_id, status, result_json) VALUES (?, ?, ?, ?, ?, ?)`, tenant, response.ValidationRunID, result.ResultID, result.RuleID, result.Status, raw); err != nil {
			return err
		}
	}
	if auditLog != nil && auditLog.SharesDB(s.db) {
		raw, err := CanonicalJSON(response)
		if err != nil {
			return err
		}
		if _, err := auditLog.AppendTx(ctx, tx, audit.Entry{Actor: actor, Tool: "workiva_validate_report", Action: "validate", Target: response.ValidationRunID, AfterJSON: string(raw), AuditID: response.NLAuditID}); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO assurance_audit_links (tenant_id, link_id, entity_kind, entity_id, audit_id, request_id, correlation_id, created_at) VALUES (?, ?, 'validation_run', ?, ?, ?, ?, ?)`, tenant, uuid.NewString(), response.ValidationRunID, response.NLAuditID, RequestIDFromContext(ctx), reservation.CorrelationID, formatTimestamp(now)); err != nil {
		return err
	}
	envelope, err := CanonicalJSON(response)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE assurance_idempotency_records SET state='sealed', response_envelope=?, response_hash=?, response_status=?, entity_reference=?, audit_id=?, terminal_at=?, lease_expires_at=? WHERE tenant_id=? AND record_id=? AND state='reserved' AND owner_nonce=?`, string(envelope), digestHex(HashBytes(envelope)), response.Status, response.ValidationRunID, response.NLAuditID, formatTimestamp(now), formatTimestamp(now), tenant, reservation.RecordID, reservation.OwnerNonce)
	if err != nil {
		return err
	}
	if err := requireOneTransition(result); err != nil {
		return err
	}
	return tx.Commit()
}
