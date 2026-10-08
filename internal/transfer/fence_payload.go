package transfer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Fence payloads are closed, canonical records. The digest is SHA-256 of these exact bytes.
type claimPayload struct {
	EnvironmentDigest string `json:"environment_digest"`
	TenantDigest      string `json:"tenant_digest"`
	TransferID        string `json:"transfer_id"`
	IdempotencyDigest string `json:"idempotency_digest"`
	RequestDigest     string `json:"request_digest"`
	ActorDigest       string `json:"actor_digest"`
	SourceHash        string `json:"source_hash"`
	TargetHash        string `json:"target_hash"`
	IntendedHash      string `json:"intended_hash"`
	PolicyHash        string `json:"policy_hash"`
	MappingHash       string `json:"mapping_hash"`
	ClaimState        string `json:"claim_state"`
	ClaimTime         string `json:"claim_time"`
	ClaimRowVersion   int64  `json:"claim_row_version"`
}
type terminalPayload struct {
	ClaimDigest              string `json:"claim_digest"`
	EnvironmentDigest        string `json:"environment_digest"`
	TenantDigest             string `json:"tenant_digest"`
	TransferID               string `json:"transfer_id"`
	ProviderOutcomeDigest    string `json:"provider_outcome_digest"`
	OperationReferenceDigest string `json:"operation_reference_digest,omitempty"`
	ReadbackDigest           string `json:"readback_digest,omitempty"`
	TerminalKind             string `json:"terminal_kind"`
	Reason                   string `json:"reason"`
	TerminalTime             string `json:"terminal_time"`
}

func canonicalFenceBody(body []byte, kind, id, env, tenant string) error {
	if len(body) == 0 || len(body) > 8192 {
		return errors.New("transfer: fence payload size invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	switch kind {
	case "claim":
		var p claimPayload
		if err := decoder.Decode(&p); err != nil {
			return err
		}
		if p.TransferID != id || p.EnvironmentDigest != env || p.TenantDigest != tenant || p.IdempotencyDigest == "" || p.RequestDigest == "" || p.ActorDigest == "" || p.SourceHash == "" || p.TargetHash == "" || p.IntendedHash == "" || p.PolicyHash == "" || p.MappingHash == "" || p.ClaimState != "claimed" || p.ClaimTime == "" || p.ClaimRowVersion < 1 {
			return errors.New("transfer: claim fence binding mismatch")
		}
		canonical, _ := json.Marshal(p)
		if !bytes.Equal(body, canonical) {
			return errors.New("transfer: noncanonical claim fence")
		}
	case "terminal":
		var p terminalPayload
		if err := decoder.Decode(&p); err != nil {
			return err
		}
		if p.TransferID != id || p.EnvironmentDigest != env || p.TenantDigest != tenant || p.ClaimDigest == "" || p.ProviderOutcomeDigest == "" || p.TerminalKind == "" || p.Reason == "" || p.TerminalTime == "" {
			return errors.New("transfer: terminal fence binding mismatch")
		}
		canonical, _ := json.Marshal(p)
		if !bytes.Equal(body, canonical) {
			return errors.New("transfer: noncanonical terminal fence")
		}
	default:
		return fmt.Errorf("transfer: invalid fence kind %q", kind)
	}
	return nil
}
