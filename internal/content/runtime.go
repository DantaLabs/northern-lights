package content

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

// RuntimeConfig contains the shared, server-owned dependencies for the MCP
// content placement capability. Provider and Fence may be nil for intake-only
// operation. A durable fence alone is deliberately insufficient to enable
// provider confirmation or reconciliation without restored-database admission.
type RuntimeConfig struct {
	Store    *assurance.Store
	Resolver *assurance.ContentPolicyResolver
	Provider workivaprovider.Backend
	Fence    ContentFence
	Audit    *audit.Log
	Gate     *assurance.DrainGate
}

// Runtime is the cohesive application facade used by the MCP tool adapter.
// It owns no caller-supplied authorization or policy authority.
type Runtime struct {
	intake *IntakeService
	stage  *StageService
	ack    *AcknowledgeService
	store  *assurance.Store
	gate   *assurance.DrainGate
}

func NewRuntime(config RuntimeConfig) (*Runtime, error) {
	if config.Store == nil || config.Resolver == nil || config.Audit == nil || config.Gate == nil {
		return nil, errors.New("content: store, signed policy resolver, audit log, and shared drain gate are required")
	}
	if err := config.Store.Ready(); err != nil {
		return nil, errors.New("content: assurance store has not completed startup admission")
	}
	if !config.Store.ContentStorageEncrypted() || !config.Audit.SharesDB(config.Store.DB()) {
		return nil, errors.New("content: encrypted source storage and audit sharing the assurance database are required")
	}
	config.Store.SetAuditLog(config.Audit)
	intake, err := NewIntakeService(config.Store, config.Resolver)
	if err != nil {
		return nil, err
	}
	ack, err := NewAcknowledgeService(config.Store, config.Resolver)
	if err != nil {
		return nil, err
	}
	runtime := &Runtime{intake: intake, ack: ack, store: config.Store, gate: config.Gate}
	if config.Provider != nil {
		router, routeErr := workivaprovider.NewRouterChecked(config.Provider)
		if routeErr != nil {
			return nil, errors.New("content: configured provider is invalid")
		}
		metadata, ok := any(router).(workivaprovider.ContentMetadataReader)
		if !ok {
			return nil, errors.New("content: provider metadata capability is unavailable")
		}
		runtime.stage = &StageService{
			Store: config.Store, Candidates: AssuranceCandidateResolver{Store: config.Store},
			Profiles: AssuranceProfileResolver{Resolver: config.Resolver}, Provider: metadata,
		}
	}
	return runtime, nil
}

func (r *Runtime) IngestPublic(ctx context.Context, raw []byte, auditID string) (assurance.ContentIngestResult, error) {
	result, _, err := r.IngestPublicWithInvocation(ctx, raw, auditID)
	return result, err
}

// IngestPublicWithInvocation returns the audit ID and replay disposition
// resolved from the sealed idempotency-record audit link.
func (r *Runtime) IngestPublicWithInvocation(ctx context.Context, raw []byte, auditID string) (assurance.ContentIngestResult, PlacementInvocation, error) {
	if err := requireRuntimePrincipal(ctx, identity.PermissionContentIngest); err != nil {
		return assurance.ContentIngestResult{}, PlacementInvocation{}, err
	}
	release, err := r.enter(ctx)
	if err != nil {
		return assurance.ContentIngestResult{}, PlacementInvocation{}, err
	}
	defer release()
	decoded, err := DecodePlacementTransportRequest(raw)
	if err != nil || decoded.Ingest == nil {
		return assurance.ContentIngestResult{}, PlacementInvocation{}, &assurance.Error{Code: "invalid_request", Message: "content ingest request is invalid"}
	}
	p, _ := identity.PrincipalFromContext(ctx)
	reservation := assurance.ReservationRequest{ActorID: p.AuditActor(), Tool: "workiva_content_placement", Action: "ingest", IdempotencyDigest: decoded.Ingest.IdempotencyDigest, RequestDigest: decoded.Ingest.RequestDigest}
	prior, priorFound, _ := r.store.LookupReservation(ctx, reservation)
	result, err := r.intake.IngestPublic(ctx, raw, auditID, time.Now().UTC())
	if err != nil {
		return assurance.ContentIngestResult{}, PlacementInvocation{}, err
	}
	classified, found, err := r.store.LookupReservation(ctx, reservation)
	if err != nil || !found || classified.State != assurance.ReservationStateSealed {
		return assurance.ContentIngestResult{}, PlacementInvocation{}, &assurance.Error{Code: "idempotency_integrity_failed", Message: "sealed ingest invocation metadata is unavailable"}
	}
	storedAudit, err := r.auditForReservation(ctx, classified.RecordID)
	if err != nil {
		return assurance.ContentIngestResult{}, PlacementInvocation{}, err
	}
	disposition := "fresh"
	if priorFound && prior.Disposition == assurance.ReservationReplay || storedAudit != auditID {
		disposition = "sealed_replay"
	}
	return result, PlacementInvocation{AuditID: storedAudit, ReplayDisposition: disposition}, nil
}

func (r *Runtime) Stage(ctx context.Context, auditID string, request StageRequest) (StageResponse, error) {
	result, _, err := r.StageWithInvocation(ctx, auditID, request)
	return result, err
}

func (r *Runtime) StageWithInvocation(ctx context.Context, auditID string, request StageRequest) (StageResponse, PlacementInvocation, error) {
	if err := requireRuntimePrincipal(ctx, identity.PermissionContentStage); err != nil {
		return StageResponse{}, PlacementInvocation{}, err
	}
	if r == nil || r.stage == nil {
		return StageResponse{}, PlacementInvocation{}, &assurance.Error{Code: "feature_disabled", Message: "content staging provider capability is unavailable"}
	}
	release, err := r.enter(ctx)
	if err != nil {
		return StageResponse{}, PlacementInvocation{}, err
	}
	defer release()
	p, _ := identity.PrincipalFromContext(ctx)
	requestDigest, err := stageRequestDigest(request)
	if err != nil {
		return StageResponse{}, PlacementInvocation{}, err
	}
	reservation := assurance.ReservationRequest{ActorID: p.AuditActor(), Tool: stageTool, Action: "stage", IdempotencyDigest: assurance.DigestIdempotencyKey(request.IdempotencyKey), RequestDigest: requestDigest}
	prior, priorFound, err := r.store.LookupReservation(ctx, reservation)
	if err != nil {
		return StageResponse{}, PlacementInvocation{}, err
	}
	result, err := r.stage.Stage(ctx, auditID, request)
	if err != nil {
		return StageResponse{}, PlacementInvocation{}, err
	}
	var preview struct {
		PlacementIntentID string `json:"placement_intent_id"`
	}
	if err := json.Unmarshal(result.Preview, &preview); err != nil || preview.PlacementIntentID == "" {
		return StageResponse{}, PlacementInvocation{}, &assurance.Error{Code: "idempotency_integrity_failed", Message: "sealed stage invocation metadata is unavailable"}
	}
	classified, found, err := r.store.LookupReservation(ctx, reservation)
	if err != nil || !found || classified.State != assurance.ReservationStateSealed {
		return StageResponse{}, PlacementInvocation{}, &assurance.Error{Code: "idempotency_integrity_failed", Message: "sealed stage invocation metadata is unavailable"}
	}
	storedAudit, err := r.auditForReservation(ctx, classified.RecordID)
	if err != nil {
		return StageResponse{}, PlacementInvocation{}, err
	}
	disposition := "fresh"
	if priorFound && prior.Disposition == assurance.ReservationReplay || result.ReplayRequiresRestage || storedAudit != auditID {
		disposition = "sealed_replay"
	}
	return result, PlacementInvocation{AuditID: storedAudit, ReplayDisposition: disposition}, nil
}

// Confirm intentionally remains unavailable until startup proves restored
// database and external fence admission. It never calls the configured writer.
func (r *Runtime) Confirm(ctx context.Context, _ string, _ ConfirmRequest) (ConfirmResponse, error) {
	if err := requireRuntimePrincipal(ctx, identity.PermissionContentConfirm); err != nil {
		return ConfirmResponse{}, err
	}
	return ConfirmResponse{}, &assurance.Error{Code: "capability_unverified", Message: "restored-database content-fence admission is unavailable; provider confirmation is disabled"}
}

func (r *Runtime) Acknowledge(ctx context.Context, auditID string, request AcknowledgeRequest, idempotencyDigest assurance.Digest) (AcknowledgeResponse, error) {
	result, _, err := r.AcknowledgeWithInvocation(ctx, auditID, request, idempotencyDigest)
	return result, err
}

func (r *Runtime) AcknowledgeWithInvocation(ctx context.Context, auditID string, request AcknowledgeRequest, idempotencyDigest assurance.Digest) (AcknowledgeResponse, PlacementInvocation, error) {
	if err := requireRuntimePrincipal(ctx, identity.PermissionContentAcknowledge); err != nil {
		return AcknowledgeResponse{}, PlacementInvocation{}, err
	}
	release, err := r.enter(ctx)
	if err != nil {
		return AcknowledgeResponse{}, PlacementInvocation{}, err
	}
	defer release()
	p, _ := identity.PrincipalFromContext(ctx)
	requestDigest, digestErr := assurance.ContentAcknowledgementRequestDigest(request)
	if digestErr != nil {
		return AcknowledgeResponse{}, PlacementInvocation{}, digestErr
	}
	reservation := assurance.ReservationRequest{ActorID: p.AuditActor(), Tool: "workiva_content_placement", Action: "acknowledge", IdempotencyDigest: idempotencyDigest, RequestDigest: requestDigest}
	prior, priorFound, _ := r.store.LookupReservation(ctx, reservation)
	result, err := r.ack.Acknowledge(ctx, auditID, request, idempotencyDigest, time.Now().UTC())
	if err != nil {
		return AcknowledgeResponse{}, PlacementInvocation{}, err
	}
	disposition := "fresh"
	if priorFound && prior.Disposition == assurance.ReservationReplay || result.AuditID != auditID {
		disposition = "sealed_replay"
	}
	return result, PlacementInvocation{AuditID: result.AuditID, ReplayDisposition: disposition}, nil
}

func (r *Runtime) Reconcile(ctx context.Context, _ string, _ ReconcileRequest) (assurance.ContentReconciliationResult, error) {
	if err := requireRuntimePrincipal(ctx, identity.PermissionContentReconcile); err != nil {
		return assurance.ContentReconciliationResult{}, err
	}
	return assurance.ContentReconciliationResult{}, &assurance.Error{Code: "capability_unverified", Message: "restored-database content-fence admission is unavailable; reconciliation is disabled"}
}

func (r *Runtime) enter(ctx context.Context) (func(), error) {
	if r == nil || r.gate == nil {
		return nil, &assurance.Error{Code: "storage_unavailable", Message: "shared content drain gate is unavailable"}
	}
	release, err := r.gate.EnterWrite(ctx)
	if err != nil {
		return nil, &assurance.Error{Code: "storage_unavailable", Message: "content operation is not admitted by the shared drain gate"}
	}
	return release, nil
}

func requireRuntimePrincipal(ctx context.Context, permission identity.Permission) error {
	p, ok := identity.PrincipalFromContext(ctx)
	if !ok || p.TokenType != identity.TokenTypeDelegated || p.UsedSubjectFallback || !p.HasPermission(permission) || !canonicalRuntimeUUID(p.TenantID) || !canonicalRuntimeUUID(p.ObjectID) {
		return &assurance.Error{Code: "forbidden", Message: "trusted delegated content capability is required"}
	}
	return nil
}

func canonicalRuntimeUUID(value string) bool { return isCanonicalUUID(value) }

func (r *Runtime) auditForReservation(ctx context.Context, recordID string) (string, error) {
	return r.auditForEntity(ctx, "idempotency_record", recordID)
}

func (r *Runtime) auditForEntity(ctx context.Context, kind, id string) (string, error) {
	p, ok := identity.PrincipalFromContext(ctx)
	if !ok || r == nil || r.store == nil {
		return "", &assurance.Error{Code: "storage_unavailable", Message: "sealed invocation metadata is unavailable"}
	}
	var auditID string
	err := r.store.DB().QueryRowContext(ctx, `SELECT audit_id FROM assurance_audit_links WHERE tenant_id=? AND entity_kind=? AND entity_id=? ORDER BY created_at ASC LIMIT 1`, p.TenantID, kind, id).Scan(&auditID)
	if err != nil || auditID == "" {
		return "", &assurance.Error{Code: "idempotency_integrity_failed", Message: "sealed invocation audit link is unavailable"}
	}
	return auditID, nil
}
