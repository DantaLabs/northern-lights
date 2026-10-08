package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/transfer"
)

// ingressDigest hashes the exact key bytes. Never copy the raw key into a
// service request, persistence record, provider call, audit entry or response.
func ingressDigest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func transferRequestDigest(in transferContractInput) (string, error) {
	var request any
	switch in.Phase {
	case "stage":
		request = struct {
			Phase  string                 `json:"phase"`
			Source *transferEndpointInput `json:"source"`
			Target *transferTargetInput   `json:"target"`
			Policy string                 `json:"conversion_policy_id,omitempty"`
			Reason string                 `json:"reason,omitempty"`
		}{in.Phase, in.Source, in.Target, in.ConversionPolicyID, in.Reason}
	case "confirm":
		// The one-time token is an execution credential, never request authority.
		// A sealed replay has no need to present or revalidate it.
		request = struct {
			Phase      string `json:"phase"`
			TransferID string `json:"transfer_id"`
		}{in.Phase, in.TransferID}
	case "acknowledge":
		request = struct {
			Phase                 string                   `json:"phase"`
			TransferID            string                   `json:"transfer_id"`
			Observation           string                   `json:"observation"`
			Refreshed             bool                     `json:"refreshed"`
			RefreshOverrideReason string                   `json:"refresh_override_reason,omitempty"`
			UILocation            *transferUILocationInput `json:"ui_location,omitempty"`
			ObservedValue         *transferTypedValueInput `json:"observed_value,omitempty"`
			Note                  string                   `json:"note,omitempty"`
		}{in.Phase, in.TransferID, in.Observation, in.Refreshed != nil && *in.Refreshed, in.RefreshOverrideReason, in.UILocation, in.ObservedValue, in.Note}
	case "reconcile":
		request = struct {
			Phase          string `json:"phase"`
			TransferID     string `json:"transfer_id"`
			Action         string `json:"action"`
			Classification string `json:"classification,omitempty"`
			Note           string `json:"note,omitempty"`
		}{Phase: in.Phase, TransferID: in.TransferID, Action: in.Action, Classification: func() string {
			if in.Action == "classify" || in.Action == "close" {
				return in.Classification
			}
			return ""
		}(), Note: func() string {
			if in.Action == "classify" || in.Action == "close" {
				return in.Note
			}
			return ""
		}()}
	default:
		return "", errors.New("unsupported transfer phase")
	}
	canonical, err := assurance.CanonicalJSON(request)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func transferSuccess(ctx context.Context, phase, status string, ignored []string, noMutation, reconcile bool) map[string]any {
	if ignored == nil {
		ignored = []string{}
	}
	return map[string]any{"nl_audit_id": transferAuditID(ctx), "phase": phase, "status": status, "ignored_fields": ignored, "errors": []any{}, "no_mutation_submitted": noMutation, "reconciliation_required": reconcile}
}
func transferFailure(ctx context.Context, phase string, ignored []string, err error) map[string]any {
	var unknown *transfer.ConfirmUnknownError
	if errors.As(err, &unknown) {
		out := transferContractOutput(ctx, phase, "reconciliation_required", transferContractIssue("write_outcome_unknown", "", "transfer outcome requires reconciliation; do not repeat"), ignored)
		out["no_mutation_submitted"] = false
		out["reconciliation_required"] = true
		if unknown.OperationReference != "" {
			out["operation_reference"] = unknown.OperationReference
		}
		return out
	}
	status := "error"
	code := "transfer_failed"
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "idempotency_conflict"):
			status = "idempotency_conflict"
			code = status
		case strings.Contains(err.Error(), "idempotency_in_progress"):
			status = "idempotency_in_progress"
			code = status
		}
	}
	out := transferContractOutput(ctx, phase, status, transferContractIssue(code, "", "transfer action failed"), ignored)
	if status == "idempotency_in_progress" {
		out["retry_after_ms"] = 1000
	}
	return out
}

func transferTyped(raw string) (map[string]any, error) {
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil || value["kind"] == nil {
		return nil, errors.New("invalid frozen typed value")
	}
	// The public schema does not advertise internal formula metadata. Never
	// project provider-only fields or raw JSON as a public typed object.
	allowed := map[string]bool{"kind": true, "text": true, "number": true, "boolean": true, "unit": true, "scale": true, "precision": true, "formula": true, "error_code": true}
	for k := range value {
		if !allowed[k] {
			delete(value, k)
		}
	}
	return value, nil
}

func executeSingleTransfer(ctx context.Context, deps mcpserver.Deps, in transferContractInput, ignored []string) map[string]any {
	ctx = assurance.WithTransferAuditID(ctx, mcpserver.AuditIDFromContext(ctx))
	ctx = assurance.WithRequestID(ctx, mcpserver.RequestIDFromContext(ctx))
	requestDigest, err := transferRequestDigest(in)
	if err != nil {
		return transferFailure(ctx, in.Phase, ignored, err)
	}
	keyDigest := ingressDigest(in.IdempotencyKey)
	if in.Phase == "stage" {
		prior, found, err := deps.Transfer.StageReplay(ctx, keyDigest, requestDigest)
		if err != nil {
			return transferFailure(ctx, "stage", ignored, err)
		}
		if found {
			out := transferSuccess(ctx, "stage", "idempotency_replay", ignored, true, false)
			out["transfer_id"] = prior.TransferID
			out["confirmation_token"] = ""
			out["token_recoverable"] = false
			out["replay_requires_restage"] = true
			return out
		}
		route, err := deps.Transfer.ResolveStageRoute(ctx, in.Source.MappingID, in.Source.ResourceID, in.Source.Locator, in.Target.ResourceID, in.Target.Locator, in.ConversionPolicyID)
		if err != nil {
			return transferFailure(ctx, in.Phase, ignored, err)
		}
		staged, err := deps.Transfer.Stage(ctx, transfer.StageRequest{RouteID: route.RouteID, IdempotencyDigest: keyDigest, RequestDigest: requestDigest})
		if err != nil {
			return transferFailure(ctx, in.Phase, ignored, err)
		}
		if staged.ReplayRequiresRestage {
			out := transferSuccess(ctx, "stage", "idempotency_replay", ignored, true, false)
			out["transfer_id"] = staged.TransferID
			out["confirmation_token"] = ""
			out["token_recoverable"] = false
			out["replay_requires_restage"] = true
			return out
		}
		intent, err := deps.Transfer.StagedIntent(ctx, staged.TransferID)
		if err != nil {
			return transferFailure(ctx, in.Phase, ignored, err)
		}
		sourceValue, err := transferTyped(intent.Intended)
		if err != nil {
			return transferFailure(ctx, in.Phase, ignored, err)
		}
		before, err := transferTyped(intent.Before)
		if err != nil {
			return transferFailure(ctx, in.Phase, ignored, err)
		}
		out := transferSuccess(ctx, "stage", "staged", ignored, true, false)
		out["transfer_id"] = staged.TransferID
		out["confirmation_token"] = staged.Token
		out["token_recoverable"] = false
		out["replay_requires_restage"] = false
		out["expires_at"] = intent.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		out["source"] = map[string]any{"resource_id": intent.Source.ResourceID, "locator": intent.Source.Locator, "mapping_id": intent.MappingID, "mapping_revision_id": strconv.Itoa(intent.RouteRevision), "value": sourceValue, "source_fingerprint": intent.Source.Fingerprint, "observed_at": time.Now().UTC().Format(time.RFC3339)}
		out["target"] = map[string]any{"resource_id": intent.Target.ResourceID, "locator": intent.Target.Locator, "before": before, "after": sourceValue}
		return out
	}
	if in.Phase == "acknowledge" {
		observedJSON := ""
		if in.ObservedValue != nil {
			raw, marshalErr := json.Marshal(in.ObservedValue)
			if marshalErr != nil {
				return transferFailure(ctx, in.Phase, ignored, marshalErr)
			}
			observedJSON = string(raw)
		}
		result, actionErr := deps.Transfer.Acknowledge(ctx, transfer.AcknowledgeRequest{TransferID: in.TransferID, Observation: in.Observation, Refreshed: in.Refreshed != nil && *in.Refreshed, RefreshOverrideReason: in.RefreshOverrideReason, UILocationJSON: func() string {
			if in.UILocation == nil {
				return ""
			}
			raw, _ := json.Marshal(in.UILocation)
			return string(raw)
		}(), ObservedValueJSON: observedJSON, Note: in.Note, IdempotencyDigest: keyDigest, RequestDigest: requestDigest})
		if actionErr != nil {
			return transferFailure(ctx, in.Phase, ignored, actionErr)
		}
		return transferActionSuccess(ctx, in.Phase, result, ignored)
	}
	if in.Phase == "reconcile" {
		result, actionErr := deps.Transfer.Reconcile(ctx, transfer.ReconcileRequest{TransferID: in.TransferID, Action: in.Action, Classification: in.Classification, Note: in.Note, IdempotencyDigest: keyDigest, RequestDigest: requestDigest})
		if actionErr != nil {
			return transferFailure(ctx, in.Phase, ignored, actionErr)
		}
		return transferActionSuccess(ctx, in.Phase, result, ignored)
	}
	replay, err := deps.Transfer.ConfirmWasSealed(ctx, keyDigest, requestDigest)
	if err != nil {
		return transferFailure(ctx, "confirm", ignored, err)
	}
	tr, err := deps.Transfer.Confirm(ctx, transfer.ConfirmRequest{TransferID: in.TransferID, Token: in.ConfirmationToken, IdempotencyDigest: keyDigest, RequestDigest: requestDigest})
	if err != nil {
		return transferFailure(ctx, "confirm", ignored, err)
	}
	status := string(tr.State)
	if replay {
		status = "idempotency_replay"
	}
	out := transferSuccess(ctx, "confirm", status, ignored, false, false)
	out["transfer_id"] = tr.Intent.ID
	out["machine_outcome"] = tr.MachineOutcome
	out["machine_outcome_provenance"] = tr.MachineOutcomeProvenance
	out["visual_state"] = tr.VisualState
	if !replay {
		op, rbID, rb, err := deps.Transfer.CompletionEvidence(ctx, tr.Intent.ID)
		if err != nil {
			return transferFailure(ctx, "confirm", ignored, &transfer.ConfirmUnknownError{Reason: "completion_evidence_missing", ReconciliationRequired: true, NoRepeat: true, Cause: err})
		}
		typed, err := transferTyped(rb)
		if err != nil {
			return transferFailure(ctx, "confirm", ignored, &transfer.ConfirmUnknownError{Reason: "readback_invalid", ReconciliationRequired: true, NoRepeat: true, Cause: err})
		}
		out["operation_reference"] = op
		out["readback_id"] = rbID
		out["readback_cache_bypassed"] = true
		out["target"] = map[string]any{"resource_id": tr.Intent.Target.ResourceID, "locator": tr.Intent.Target.Locator, "read_back": typed}
	}
	return out
}

func transferActionSuccess(ctx context.Context, phase string, result assurance.TransferActionResult, ignored []string) map[string]any {
	status := result.Status
	if result.Status == "" {
		status = "error"
	}
	out := transferSuccess(ctx, phase, status, ignored, true, result.ReconciliationRequired)
	if result.AuditID != "" {
		out["nl_audit_id"] = result.AuditID
	}
	if result.TransferID != "" {
		out["transfer_id"] = result.TransferID
	}
	if result.MachineOutcome != "" {
		out["machine_outcome"] = result.MachineOutcome
	}
	if result.MachineOutcomeProvenance != "" {
		out["machine_outcome_provenance"] = result.MachineOutcomeProvenance
	}
	if result.VisualState != "" {
		out["visual_state"] = result.VisualState
	}
	if result.OperationReference != "" {
		out["operation_reference"] = result.OperationReference
	}
	if result.ReadbackID != "" {
		out["readback_id"] = result.ReadbackID
		out["readback_cache_bypassed"] = result.ReadbackCacheBypassed
	}
	if result.ReconciliationClassification != "" {
		out["reconciliation_classification"] = result.ReconciliationClassification
	}
	if result.Terminal {
		out["terminal"] = true
	}
	if result.TargetResourceID != "" && result.ReadbackJSON != "" {
		if typed, err := transferTyped(result.ReadbackJSON); err == nil {
			out["target"] = map[string]any{"resource_id": result.TargetResourceID, "locator": result.TargetLocator, "read_back": typed}
		}
	}
	if result.ErrorCode != "" {
		errObject := map[string]any{"code": result.ErrorCode, "message": result.ErrorMessage, "retryable": false, "reconciliation_required": result.ReconciliationRequired, "nl_audit_id": result.AuditID}
		out["errors"] = []any{errObject}
		out["error"] = errObject
	}
	return out
}
