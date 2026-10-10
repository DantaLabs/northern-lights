package content

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/identity"
)

const (
	contentIngestTool                 = "workiva_content_placement"
	contentIngestAction               = "ingest"
	minimumContentIdempotencyLifetime = 60 * time.Second
)

// IntakeService applies the active signed access and retention policies to
// private source ingestion. It does not expose a handler or provider write.
type IntakeService struct {
	store    *assurance.Store
	resolver *assurance.ContentPolicyResolver
}

func NewIntakeService(store *assurance.Store, resolver *assurance.ContentPolicyResolver) (*IntakeService, error) {
	if store == nil || resolver == nil {
		return nil, errors.New("content: assurance store and configured signed policy resolver are required")
	}
	return &IntakeService{store: store, resolver: resolver}, nil
}

// Ingest classifies a prior request before consulting current policy or audit
// dependencies. A fresh operation uses the caller's original canonical request
// digest for both reservation and atomic finalization.
func (s *IntakeService) Ingest(ctx context.Context, input assurance.ContentIngest, idempotencyDigest assurance.Digest, auditID string, now time.Time) (assurance.ContentIngestResult, error) {
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok || principal.TokenType != identity.TokenTypeDelegated || principal.UsedSubjectFallback || !principal.HasPermission(identity.PermissionContentIngest) || !isCanonicalUUID(principal.TenantID) || !isCanonicalUUID(principal.ObjectID) {
		return assurance.ContentIngestResult{}, &assurance.Error{Code: "forbidden", Message: "trusted delegated content.ingest capability required"}
	}
	if s == nil || s.store == nil || s.resolver == nil || idempotencyDigest == (assurance.Digest{}) {
		return assurance.ContentIngestResult{}, &assurance.Error{Code: "invalid_request", Message: "content intake service and idempotency digest are required"}
	}
	now = now.UTC()
	requestDigest, err := assurance.ContentIngestRequestDigest(input)
	if err != nil {
		return assurance.ContentIngestResult{}, err
	}
	request := assurance.ReservationRequest{
		ActorID: principal.AuditActor(), Tool: contentIngestTool, Action: contentIngestAction,
		IdempotencyDigest: idempotencyDigest, RequestDigest: requestDigest, RetentionClass: "signed_content_ingest",
	}
	prior, found, err := s.store.LookupReservation(ctx, request)
	if err != nil {
		return assurance.ContentIngestResult{}, err
	}
	if found {
		return contentIngestReplay(prior)
	}
	if err := rejectIntakeAuthorityMetadata(input); err != nil {
		return assurance.ContentIngestResult{}, err
	}
	if auditID == "" {
		return assurance.ContentIngestResult{}, &assurance.Error{Code: "invalid_request", Message: "content ingest audit ID is required"}
	}
	if err := assurance.ValidateContentIngest(input); err != nil {
		return assurance.ContentIngestResult{}, err
	}

	// The first resolution determines the durable idempotency horizon. The
	// assurance finalizer independently re-resolves and binds policy provenance
	// into the source within the transaction's trusted persistence boundary.
	policy, err := s.store.ResolveContentIntakePolicySnapshot(ctx, s.resolver, now)
	if err != nil {
		return assurance.ContentIngestResult{}, err
	}
	if policy.Retention.IdempotencyLifetimeSeconds < int64(minimumContentIdempotencyLifetime/time.Second) {
		return assurance.ContentIngestResult{}, &assurance.Error{Code: "content_policy_invalid", Message: "signed idempotency lifetime is shorter than the reservation lease"}
	}
	retainUntil := now.Add(time.Duration(policy.Retention.IdempotencyLifetimeSeconds) * time.Second)
	request.RetainUntil = retainUntil
	reservation, err := s.store.Reserve(ctx, request, now)
	if err != nil {
		return assurance.ContentIngestResult{}, err
	}
	if reservation.Disposition != assurance.ReservationOwned {
		return contentIngestReplay(reservation)
	}
	return s.store.FinalizePolicyBoundContentIngest(ctx, reservation, input, requestDigest, s.resolver, auditID, now)
}

func contentIngestReplay(reservation assurance.ReservationResult) (assurance.ContentIngestResult, error) {
	switch reservation.Disposition {
	case assurance.ReservationConflict:
		return assurance.ContentIngestResult{}, &assurance.Error{Code: "idempotency_conflict", Message: "idempotency key was already used for different content"}
	case assurance.ReservationInProgress, assurance.ReservationOwned:
		return assurance.ContentIngestResult{}, &assurance.Error{Code: "idempotency_in_progress", Message: "content ingest is already in progress", Retryable: true}
	case assurance.ReservationReplay:
		if reservation.State != assurance.ReservationStateSealed || len(reservation.Envelope) == 0 {
			return assurance.ContentIngestResult{}, &assurance.Error{Code: "idempotency_terminal", Message: "prior content ingest has no successful replay result"}
		}
		var result assurance.ContentIngestResult
		decoder := json.NewDecoder(bytes.NewReader(reservation.Envelope))
		if err := decoder.Decode(&result); err != nil {
			return assurance.ContentIngestResult{}, &assurance.Error{Code: "idempotency_integrity_failed", Message: "stored content ingest replay envelope is invalid"}
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) || result.SourceArtifactID == "" || result.SourceSHA256 == "" {
			return assurance.ContentIngestResult{}, &assurance.Error{Code: "idempotency_integrity_failed", Message: "stored content ingest replay envelope is invalid"}
		}
		return result, nil
	default:
		return assurance.ContentIngestResult{}, fmt.Errorf("content: unknown reservation disposition %q", reservation.Disposition)
	}
}

func rejectIntakeAuthorityMetadata(input assurance.ContentIngest) error {
	for _, raw := range [][]byte{input.MetadataBLOB} {
		if err := rejectIntakeAuthorityMetadataValue(raw); err != nil {
			return err
		}
	}
	for _, item := range input.Items {
		if err := rejectIntakeAuthorityMetadataValue(item.MetadataBLOB); err != nil {
			return err
		}
	}
	for _, draft := range input.Drafts {
		if err := rejectIntakeAuthorityMetadataValue(draft.MetadataBLOB); err != nil {
			return err
		}
	}
	return nil
}

func rejectIntakeAuthorityMetadataValue(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return &assurance.Error{Code: "invalid_request", Message: "content metadata must be a JSON object"}
	}
	if _, exists := object["intake_policy"]; exists {
		return &assurance.Error{Code: "invalid_request", Message: "intake_policy metadata is server-owned"}
	}
	return nil
}
