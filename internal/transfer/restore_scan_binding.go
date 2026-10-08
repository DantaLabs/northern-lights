package transfer

import (
	"encoding/hex"
	"encoding/json"
	"time"
)

type confirmReservationEvidence struct {
	count                      int
	actorID, idempotencyDigest string
	requestDigest, state       string
	responseStatus             string
}

func reservationDigestHex(value string) string {
	digest := stageReservationDigest(value)
	return hex.EncodeToString(digest[:])
}

func validateRestoredClaim(r restoreFenceRecord, o normalizedFenceObject, opts RestoreScanOptions, reservation confirmReservationEvidence) string {
	// Only completion-sealed confirmation evidence admits a successful restore.
	// Older or nonterminal rows without that independent confirm reservation stay
	// quarantined; stage-row digests are never a fallback confirmation authority.
	if reservation.count == 0 {
		return "missing_confirmation_reservation"
	}
	if reservation.count != 1 {
		return "ambiguous_confirmation_reservation"
	}
	if reservation.state != "sealed" || reservation.responseStatus != "machine_verified_visual_ack_pending" {
		return "invalid_confirmation_reservation"
	}
	if reservation.actorID != r.actorID {
		return "confirmation_actor_mismatch"
	}
	var p claimPayload
	if err := json.Unmarshal(o.body, &p); err != nil {
		return "invalid_claim_payload"
	}
	if p.EnvironmentDigest != opts.EnvironmentDigest || p.TenantDigest != opts.HighWater.TenantDigest || p.TransferID != r.transferID || p.ActorDigest != digest(r.actorID) || p.SourceHash != r.intent.Source.Fingerprint || p.TargetHash != r.intent.Target.Fingerprint || p.IntendedHash != digest(r.intent.Intended) || p.PolicyHash != r.intent.PolicyHash || p.MappingHash != r.intent.MappingHash || p.ClaimState != "claimed" || p.ClaimTime != r.claimTime || p.ClaimRowVersion != 2 || r.rowVersion < p.ClaimRowVersion || r.intent.ID != r.transferID || r.intent.TenantID != opts.TenantID || r.intent.ActorID != r.actorID || r.intent.Permission != r.permission {
		return "claim_binding_mismatch"
	}
	if reservation.idempotencyDigest != reservationDigestHex(p.IdempotencyDigest) || reservation.requestDigest != reservationDigestHex(p.RequestDigest) {
		return "claim_confirmation_digest_mismatch"
	}
	if _, err := time.Parse(time.RFC3339Nano, p.ClaimTime); err != nil {
		return "invalid_claim_time"
	}
	return ""
}

func isRestoreTerminalState(state State) bool {
	switch state {
	case StateMachineVerified, StateVisuallyAcknowledged, StateReconciledNotApplied:
		return true
	}
	return false
}

func validateRestoredTerminal(r restoreFenceRecord, claim, terminal normalizedFenceObject, opts RestoreScanOptions) string {
	var p terminalPayload
	if err := json.Unmarshal(terminal.body, &p); err != nil {
		return "invalid_terminal_payload"
	}
	if p.EnvironmentDigest != opts.EnvironmentDigest || p.TenantDigest != opts.HighWater.TenantDigest || p.TransferID != r.transferID || claim.digest == "" || p.ClaimDigest != claim.digest {
		return "terminal_claim_binding_mismatch"
	}
	if _, err := time.Parse(time.RFC3339Nano, p.TerminalTime); err != nil {
		return "invalid_terminal_time"
	}
	if p.OperationReferenceDigest != "" && (r.operationReference == "" || p.OperationReferenceDigest != digest(r.operationReference)) {
		return "terminal_operation_mismatch"
	}
	if p.ReadbackDigest != "" && (r.readback == "" || !r.readbackCacheBypassed || p.ReadbackDigest != digest(r.readback)) {
		return "terminal_readback_mismatch"
	}
	switch p.TerminalKind {
	case "accepted":
		if p.ProviderOutcomeDigest != digest("accepted") || p.Reason != "api_verified" || p.OperationReferenceDigest == "" || p.ReadbackDigest == "" || !r.operationCompleted || r.readback != r.intent.Intended {
			return "accepted_terminal_evidence_missing"
		}
		if isRestoreTerminalState(r.state) && r.state != StateMachineVerified && r.state != StateVisuallyAcknowledged {
			return "terminal_state_contradiction"
		}
	case "rejected_preacceptance", "failed", "unknown", "reconciled_applied", "reconciled_not_applied":
		if p.ProviderOutcomeDigest == "" || isRestoreTerminalState(r.state) && (r.state == StateMachineVerified || r.state == StateVisuallyAcknowledged) {
			return "terminal_state_contradiction"
		}
	default:
		return "invalid_terminal_kind"
	}
	return ""
}
