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

const acknowledgeAction = "acknowledge"

type AcknowledgeRequest = assurance.ContentAcknowledgementRequest
type AcknowledgeResponse = assurance.ContentAcknowledgementResult

type AcknowledgeService struct {
	Store    *assurance.Store
	Resolver *assurance.ContentPolicyResolver
}

func NewAcknowledgeService(store *assurance.Store, resolver *assurance.ContentPolicyResolver) (*AcknowledgeService, error) {
	if store == nil {
		return nil, errors.New("content: assurance store is required")
	}
	return &AcknowledgeService{Store: store, Resolver: resolver}, nil
}

// Acknowledge records an authorized visual decision against the exact sealed
// machine result. It makes no provider calls and submits no mutations.
func (s *AcknowledgeService) Acknowledge(ctx context.Context, auditID string, request AcknowledgeRequest, idempotencyDigest assurance.Digest, now time.Time) (AcknowledgeResponse, error) {
	p, ok := identity.PrincipalFromContext(ctx)
	if !ok || p.TokenType != identity.TokenTypeDelegated || p.UsedSubjectFallback || !p.HasPermission(identity.PermissionContentAcknowledge) || !isCanonicalUUID(p.TenantID) || !isCanonicalUUID(p.ObjectID) {
		return AcknowledgeResponse{}, &assurance.Error{Code: "forbidden", Message: "trusted delegated content.acknowledge capability required"}
	}
	if s == nil || s.Store == nil || idempotencyDigest == (assurance.Digest{}) {
		return AcknowledgeResponse{}, &assurance.Error{Code: "invalid_request", Message: "content acknowledgement service and idempotency digest are required"}
	}
	now = now.UTC()
	requestDigest, err := assurance.ContentAcknowledgementRequestDigest(request)
	if err != nil {
		return AcknowledgeResponse{}, err
	}
	reservationRequest := assurance.ReservationRequest{
		ActorID: p.AuditActor(), Tool: stageTool, Action: acknowledgeAction,
		IdempotencyDigest: idempotencyDigest, RequestDigest: requestDigest, RetentionClass: "signed_content_acknowledgement",
	}
	prior, found, err := s.Store.LookupReservation(ctx, reservationRequest)
	if err != nil {
		return AcknowledgeResponse{}, err
	}
	if found {
		return acknowledgeReplay(prior)
	}
	if s.Resolver == nil {
		return AcknowledgeResponse{}, &assurance.Error{Code: "content_policy_unavailable", Message: "configured signed policy resolver is required for a new acknowledgement"}
	}
	if err := validateAcknowledgeRequest(request); err != nil {
		return AcknowledgeResponse{}, err
	}
	if auditID == "" {
		return AcknowledgeResponse{}, &assurance.Error{Code: "invalid_request", Message: "content acknowledgement audit ID is required"}
	}
	bindings, err := s.Store.ResolveOwnedContentAcknowledgementPolicy(ctx, s.Resolver, request.PlacementIntentID, now)
	if err != nil {
		return AcknowledgeResponse{}, err
	}
	if bindings.Retention.IdempotencyLifetimeSeconds < 60 {
		return AcknowledgeResponse{}, &assurance.Error{Code: "content_policy_invalid", Message: "signed idempotency lifetime is shorter than the reservation lease"}
	}
	reservationRequest.RetainUntil = now.Add(time.Duration(bindings.Retention.IdempotencyLifetimeSeconds) * time.Second)
	reservation, err := s.Store.Reserve(ctx, reservationRequest, now)
	if err != nil {
		return AcknowledgeResponse{}, err
	}
	if reservation.Disposition != assurance.ReservationOwned {
		return acknowledgeReplay(reservation)
	}
	return s.Store.FinalizeContentAcknowledgement(ctx, reservation, request, requestDigest, s.Resolver, auditID, now)
}

func validateAcknowledgeRequest(request AcknowledgeRequest) error {
	if request.PlacementIntentID == "" || !sha256Pattern.MatchString(request.ResultSHA256) || len([]byte(request.VisualNotes)) > 2048 {
		return &assurance.Error{Code: "invalid_request", Message: "placement intent, machine result digest, and bounded visual notes are required"}
	}
	switch request.Observation {
	case "visually_confirmed", "visual_conflict", "not_reviewed":
		return nil
	default:
		return &assurance.Error{Code: "invalid_request", Message: "visual acknowledgement observation is invalid"}
	}
}

func acknowledgeReplay(reservation assurance.ReservationResult) (AcknowledgeResponse, error) {
	switch reservation.Disposition {
	case assurance.ReservationConflict:
		return AcknowledgeResponse{}, &assurance.Error{Code: "idempotency_conflict", Message: "idempotency key was already used for different content acknowledgement"}
	case assurance.ReservationInProgress, assurance.ReservationOwned:
		return AcknowledgeResponse{}, &assurance.Error{Code: "idempotency_in_progress", Message: "content acknowledgement is already in progress", Retryable: true}
	case assurance.ReservationReplay:
		if reservation.State != assurance.ReservationStateSealed || len(reservation.Envelope) == 0 {
			return AcknowledgeResponse{}, &assurance.Error{Code: "idempotency_terminal", Message: "prior acknowledgement has no sealed result"}
		}
		canonical, err := assurance.CanonicalJSONBytes(reservation.Envelope)
		if err != nil || !bytes.Equal(canonical, reservation.Envelope) {
			return AcknowledgeResponse{}, &assurance.Error{Code: "idempotency_integrity_failed", Message: "stored acknowledgement replay envelope is invalid"}
		}
		var result AcknowledgeResponse
		decoder := json.NewDecoder(bytes.NewReader(canonical))
		if err := decoder.Decode(&result); err != nil {
			return AcknowledgeResponse{}, &assurance.Error{Code: "idempotency_integrity_failed", Message: "stored acknowledgement replay envelope is invalid"}
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) || result.Kind != "acknowledge" || result.AcknowledgementID == "" || result.AcknowledgementID != reservation.RecordID || result.PlacementIntentID == "" || result.AuditID == "" {
			return AcknowledgeResponse{}, &assurance.Error{Code: "idempotency_integrity_failed", Message: "stored acknowledgement replay envelope is invalid"}
		}
		return result, nil
	default:
		return AcknowledgeResponse{}, fmt.Errorf("content: unknown acknowledgement reservation disposition %q", reservation.Disposition)
	}
}
