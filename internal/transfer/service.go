package transfer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/workiva"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

type StageRequest struct {
	RouteID, IdempotencyDigest, RequestDigest string
	Now                                       time.Time
}

func newOpaqueID(prefix string, size int) (string, error) {
	if size <= 0 {
		return "", errors.New("transfer: opaque ID size must be positive")
	}
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw), nil
}

type ConfirmRequest struct {
	TenantID, ActorID, Permission, TransferID, Token string
	IdempotencyDigest, RequestDigest                 string
	Now                                              time.Time
}

type AcknowledgeRequest struct {
	TransferID, Observation, UILocationJSON, ObservedValueJSON, Note string
	Refreshed                                                        bool
	RefreshOverrideReason                                            string
	IdempotencyDigest, RequestDigest                                 string
	Now                                                              time.Time
}

type ReconcileRequest struct {
	TransferID, Action, Classification, Note string
	IdempotencyDigest, RequestDigest         string
	Now                                      time.Time
}
type Service struct {
	store              *Store
	assuranceStore     *assurance.Store
	provider           *workivaprovider.Router
	fence              Fence
	leaseRenewInterval time.Duration
}

// NewService refuses to expose the write path unless both the provider router and
// an explicitly configured fence implementation are present. Production wiring
// must supply a durable implementation; FakeFence is intended only for tests.
func NewService(store *Store, assuranceStore *assurance.Store, provider *workivaprovider.Router, fence Fence) (*Service, error) {
	if store == nil || assuranceStore == nil || assuranceStore.Ready() != nil || provider == nil || fence == nil || !fence.Durable() {
		return nil, errors.New("transfer: store, ready assurance store, provider router, and verified durable fence are required")
	}
	return &Service{store: store, assuranceStore: assuranceStore, provider: provider, fence: fence, leaseRenewInterval: 20 * time.Second}, nil
}

// NewTestService exposes the fake-backed service only to local tests.
func NewTestService(store *Store, assuranceStore *assurance.Store, provider *workivaprovider.Router, fence *FakeFence) (*Service, error) {
	if store == nil || assuranceStore == nil || assuranceStore.Ready() != nil || provider == nil || fence == nil {
		return nil, errors.New("transfer: test store, ready assurance store, router, and fake fence are required")
	}
	return &Service{store: store, assuranceStore: assuranceStore, provider: provider, fence: fence, leaseRenewInterval: 20 * time.Second}, nil
}

func (s *Service) startStageLeaseRenewal(ctx context.Context, reservation assurance.ReservationResult) (context.Context, <-chan error, func()) {
	workCtx, cancel := context.WithCancel(ctx)
	errs := make(chan error, 1)
	done := make(chan struct{})
	interval := s.leaseRenewInterval
	if interval <= 0 || interval > 20*time.Second {
		interval = 20 * time.Second
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-ticker.C:
				if err := s.assuranceStore.RenewReservation(workCtx, reservation.RecordID, reservation.OwnerNonce, time.Now().UTC()); err != nil {
					select {
					case errs <- err:
					default:
					}
					cancel()
					return
				}
			}
		}
	}()
	return workCtx, errs, func() { cancel(); <-done }
}

func (s *Service) startLeaseRenewal(ctx context.Context, tenant, transferID, leaseID string, reservation assurance.ReservationResult) (context.Context, <-chan error, func()) {
	workCtx, cancel := context.WithCancel(ctx)
	errs := make(chan error, 1)
	done := make(chan struct{})
	interval := s.leaseRenewInterval
	if interval <= 0 {
		interval = 20 * time.Second
	}
	renew := func() error {
		now := time.Now().UTC()
		if err := s.store.RenewLease(ctx, tenant, transferID, leaseID, now); err != nil {
			return err
		}
		return s.assuranceStore.RenewReservation(ctx, reservation.RecordID, reservation.OwnerNonce, now)
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-ticker.C:
				if err := renew(); err != nil {
					select {
					case errs <- err:
					default:
					}
					cancel()
					return
				}
			}
		}
	}()
	return workCtx, errs, func() { cancel(); <-done }
}

type stageReplayEnvelope struct {
	Status                string                     `json:"status"`
	TransferID            string                     `json:"transfer_id,omitempty"`
	ReplayRequiresRestage bool                       `json:"replay_requires_restage,omitempty"`
	TokenRecoverable      bool                       `json:"token_recoverable"`
	Error                 *assurance.StructuredError `json:"error,omitempty"`
}

func stageReservationProjection(reservation assurance.ReservationResult) (StageResult, error) {
	switch reservation.Disposition {
	case assurance.ReservationConflict:
		return StageResult{}, errors.New("idempotency_conflict")
	case assurance.ReservationInProgress:
		return StageResult{}, errors.New("idempotency_in_progress")
	case assurance.ReservationReplay:
		var replay stageReplayEnvelope
		if err := json.Unmarshal(reservation.Envelope, &replay); err != nil {
			return StageResult{}, err
		}
		if replay.Error != nil {
			return StageResult{}, errors.New(replay.Error.Code)
		}
		if replay.Status == "" && replay.TransferID == "" {
			var failure assurance.StructuredError
			if err := json.Unmarshal(reservation.Envelope, &failure); err == nil && failure.Code != "" {
				return StageResult{}, errors.New(failure.Code)
			}
		}
		return StageResult{TransferID: replay.TransferID, ReplayRequiresRestage: replay.ReplayRequiresRestage}, nil
	default:
		return StageResult{}, errors.New("idempotency_state_invalid")
	}
}

func stageReservationDigest(value string) assurance.Digest {
	// The public ingress supplies a SHA-256 hex digest, not the raw key.
	// Historical private callers supplied arbitrary strings; retain their
	// legacy hashing behavior without double-hashing valid ingress digests.
	if len(value) == 64 {
		var d assurance.Digest
		if raw, err := hex.DecodeString(value); err == nil && len(raw) == len(d) {
			copy(d[:], raw)
			return d
		}
	}
	return assurance.HashBytes([]byte(value))
}

// StageReplay classifies a trusted ingress key before execution-only route,
// audit or provider dependencies. A hit never reconstructs a token.
func (s *Service) StageReplay(ctx context.Context, keyDigest, requestDigest string) (StageResult, bool, error) {
	p, ok := identity.PrincipalFromContext(ctx)
	if !ok || p.TenantID == "" || p.AuditActor() == "" || !p.HasPermission(identity.PermissionWorkivaWritePreview) {
		return StageResult{}, false, errors.New("transfer: trusted preview capability required")
	}
	prior, found, err := s.assuranceStore.LookupReservation(ctx, assurance.ReservationRequest{
		ActorID: p.AuditActor(), Tool: "workiva_transfer_value", Action: "stage",
		IdempotencyDigest: stageReservationDigest(keyDigest), RequestDigest: stageReservationDigest(requestDigest),
	})
	if err != nil || !found {
		return StageResult{}, found, err
	}
	result, err := stageReservationProjection(prior)
	return result, true, err
}

// ResolveStageRoute binds the selectors to an active signed mapping. It never
// interprets the caller's target as write authority.
func (s *Service) ResolveStageRoute(ctx context.Context, mappingID, sourceResourceID, sourceLocator, targetResourceID, targetLocator, policyID string) (assurance.TransferRoute, error) {
	return s.assuranceStore.MatchTransferRoute(ctx, mappingID, sourceResourceID, sourceLocator, targetResourceID, targetLocator, policyID)
}

// StagedIntent is for the first stage response only. A replay must use the
// sealed token-free projection and must not re-read mutable transfer state.
func (s *Service) StagedIntent(ctx context.Context, id string) (Intent, error) {
	p, ok := identity.PrincipalFromContext(ctx)
	if !ok || p.TenantID == "" || p.AuditActor() == "" {
		return Intent{}, errors.New("transfer: trusted principal required")
	}
	tr, err := s.store.Get(ctx, p.TenantID, id)
	if err != nil {
		return Intent{}, err
	}
	if tr.Intent.ActorID != p.AuditActor() {
		return Intent{}, errors.New("transfer: actor mismatch")
	}
	return tr.Intent, nil
}

// CompletionEvidence is read only on a first successful confirmation, never
// on a sealed replay. It verifies that the persisted readback is present.
func (s *Service) CompletionEvidence(ctx context.Context, id string) (string, string, string, error) {
	p, ok := identity.PrincipalFromContext(ctx)
	if !ok || p.TenantID == "" || p.AuditActor() == "" {
		return "", "", "", errors.New("transfer: trusted principal required")
	}
	var op, rbID, value string
	err := s.store.db.QueryRowContext(ctx, `SELECT t.operation_reference, r.readback_id, r.typed_value_json FROM transfer_intents t JOIN assurance_transfer_readbacks r ON r.tenant_id=t.tenant_id AND r.transfer_id=t.transfer_id WHERE t.tenant_id=? AND t.transfer_id=? AND t.actor_id=? AND t.state='machine_verified_visual_ack_pending'`, p.TenantID, id, p.AuditActor()).Scan(&op, &rbID, &value)
	return op, rbID, value, err
}

func (s *Service) finishStageReservation(ctx context.Context, reservation assurance.ReservationResult, err error, now time.Time) error {
	failure := stageReplayEnvelope{
		Status:           "failed",
		TokenRecoverable: false,
		Error: &assurance.StructuredError{
			Code:    "transfer_stage_failed",
			Message: err.Error(),
		},
	}
	if sealErr := s.assuranceStore.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, *failure.Error, now); sealErr != nil {
		return errors.Join(err, sealErr)
	}
	return err
}

func (s *Service) Stage(ctx context.Context, r StageRequest) (StageResult, error) {
	var zero StageResult
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok || principal.TenantID == "" || principal.AuditActor() == "" || !principal.HasPermission(identity.PermissionWorkivaWritePreview) || r.RouteID == "" || r.IdempotencyDigest == "" || r.RequestDigest == "" {
		return zero, errors.New("transfer: trusted tenant, actor, preview capability, route, and request digests are required")
	}
	now := r.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	reservationRequest := assurance.ReservationRequest{
		ActorID:           principal.AuditActor(),
		Tool:              "workiva_transfer_value",
		Action:            "stage",
		IdempotencyDigest: stageReservationDigest(r.IdempotencyDigest),
		RequestDigest:     stageReservationDigest(r.RequestDigest),
	}
	prior, found, err := s.assuranceStore.LookupReservation(ctx, reservationRequest)
	if err != nil {
		return zero, err
	}
	if found {
		return stageReservationProjection(prior)
	}
	if err := s.assuranceStore.RequireRichAudit(ctx); err != nil {
		return zero, err
	}
	reservation, err := s.assuranceStore.Reserve(ctx, reservationRequest, now)
	if err != nil {
		return zero, err
	}
	switch reservation.Disposition {
	case assurance.ReservationConflict:
		return zero, errors.New("idempotency_conflict")
	case assurance.ReservationInProgress:
		return zero, errors.New("idempotency_in_progress")
	case assurance.ReservationReplay:
		var replay stageReplayEnvelope
		if err := json.Unmarshal(reservation.Envelope, &replay); err != nil {
			return zero, err
		}
		if replay.Error != nil {
			return zero, errors.New(replay.Error.Code)
		}
		if replay.Status == "" && replay.TransferID == "" {
			var failure assurance.StructuredError
			if err := json.Unmarshal(reservation.Envelope, &failure); err == nil && failure.Code != "" {
				return zero, errors.New(failure.Code)
			}
		}
		return StageResult{TransferID: replay.TransferID, ReplayRequiresRestage: replay.ReplayRequiresRestage}, nil
	case assurance.ReservationOwned:
		// Replay/conflict/in-progress must not inspect the audit dependency.
		// The owner fails closed before its first provider read.
		if err := s.assuranceStore.RequireRichAudit(ctx); err != nil {
			return zero, s.finishStageReservation(ctx, reservation, err, now)
		}
		// The reservation is durable before route resolution or any provider read.
		if err := s.assuranceStore.MarkExecutionStarted(ctx, reservation.RecordID, reservation.OwnerNonce, now); err != nil {
			return zero, s.finishStageReservation(ctx, reservation, err, now)
		}
	default:
		return zero, errors.New("idempotency_state_invalid")
	}
	workCtx, leaseErrors, stopLeaseRenewal := s.startStageLeaseRenewal(ctx, reservation)
	defer stopLeaseRenewal()
	fail := func(err error) (StageResult, error) {
		return zero, s.finishStageReservation(ctx, reservation, err, time.Now().UTC())
	}
	renewBeforeStep := func() error {
		select {
		case err := <-leaseErrors:
			return fmt.Errorf("transfer: stage lease renewal failed: %w", err)
		default:
		}
		if err := workCtx.Err(); err != nil {
			return err
		}
		return s.assuranceStore.RenewReservation(workCtx, reservation.RecordID, reservation.OwnerNonce, time.Now().UTC())
	}
	if err := renewBeforeStep(); err != nil {
		return fail(err)
	}
	route, err := s.assuranceStore.ResolveTransferRoute(workCtx, r.RouteID)
	if err != nil {
		return fail(err)
	}
	policy, err := s.assuranceStore.ResolveConversionPolicy(workCtx, route.ConversionPolicyID, route.ConversionPolicyVersion)
	if err != nil {
		return fail(err)
	}
	sourceEndpoint := Endpoint{ResourceID: route.SourceResourceID, SheetID: route.SourceSheetID, Locator: route.SourceLocator}
	targetEndpoint := Endpoint{ResourceID: route.TargetResourceID, SheetID: route.TargetSheetID, Locator: route.TargetLocator}
	if err := renewBeforeStep(); err != nil {
		return fail(err)
	}
	src, err := s.read(workCtx, sourceEndpoint)
	if err != nil || !src.CacheBypassed {
		return fail(errors.New("transfer: uncached source read required"))
	}
	if err := renewBeforeStep(); err != nil {
		return fail(err)
	}
	dst, err := s.read(workCtx, targetEndpoint)
	if err != nil || !dst.CacheBypassed {
		return fail(errors.New("transfer: uncached target read required"))
	}
	field := assurance.FieldDefinition{Kind: policy.SourceKind, Unit: policy.SourceUnit}
	sourceTyped, err := assurance.NormalizeProviderValue(src.Value, field)
	if err != nil {
		return fail(err)
	}
	targetTyped, err := assurance.NormalizeProviderValue(dst.Value, assurance.FieldDefinition{Kind: policy.TargetKind, Unit: policy.TargetUnit})
	if err != nil {
		return fail(err)
	}
	intendedTyped, err := policy.ConvertTypedValue(sourceTyped)
	if err != nil {
		return fail(err)
	}
	before, err := canonicalTypedValue(targetTyped)
	if err != nil {
		return fail(err)
	}
	intended, err := canonicalTypedValue(intendedTyped)
	if err != nil {
		return fail(err)
	}
	transferID, err := newOpaqueID("tr-", 16)
	if err != nil {
		return fail(err)
	}
	rawToken := make([]byte, 32)
	if _, err := rand.Read(rawToken); err != nil {
		return fail(err)
	}
	token := hex.EncodeToString(rawToken)
	intent := Intent{ID: transferID, TenantID: principal.TenantID, ActorID: principal.AuditActor(), Permission: "workiva.write.confirm", RouteID: route.RouteID, RouteRevision: route.Revision, MappingID: route.SourceMappingID, Source: sourceEndpoint, Target: targetEndpoint, Before: before, Intended: intended, PolicyID: policy.PolicyID, PolicyRevision: policy.Revision, PolicyHash: policy.ContentHash, MappingHash: route.ContentHash, ExpiresAt: now.Add(15 * time.Minute)}
	intent.Source.Fingerprint = digest(intended)
	intent.Target.Fingerprint = digest(before)
	replayEnvelope, err := assurance.CanonicalJSON(stageReplayEnvelope{Status: "idempotency_replay", TransferID: intent.ID, ReplayRequiresRestage: true, TokenRecoverable: false})
	if err != nil {
		return fail(err)
	}
	intentJSON, err := json.Marshal(intent)
	if err != nil {
		return fail(err)
	}
	if err := renewBeforeStep(); err != nil {
		return fail(err)
	}
	stopLeaseRenewal()
	if err := s.assuranceStore.FinalizeTransferStage(ctx, reservation, assurance.TransferStageRecord{
		TransferID: intent.ID, ActorID: intent.ActorID, Permission: intent.Permission,
		IntentJSON: string(intentJSON), Token: token, IdempotencyDigest: r.IdempotencyDigest,
		RequestDigest: r.RequestDigest, ExpiresAt: intent.ExpiresAt,
	}, replayEnvelope, now); err != nil {
		return zero, err
	}
	return StageResult{TransferID: intent.ID, Token: token}, nil
}
func (s *Service) read(ctx context.Context, e Endpoint) (assurance.ProviderRead, error) {
	return s.provider.ReadUncached(ctx, assurance.SourceRequest{ExternalResourceID: e.ResourceID, SubresourceID: e.SheetID, Locator: e.Locator, Consistency: assurance.ConsistencyNone})
}

// parseCellLocator converts a single Workiva cell locator to its zero-based
// editCells coordinates. The sheet ID is carried separately in Endpoint.
func parseCellLocator(locator string) (column, row int, err error) {
	cell := locator
	if bang := strings.LastIndexByte(cell, '!'); bang >= 0 {
		cell = cell[bang+1:]
	}
	cell = strings.ReplaceAll(cell, "$", "")
	cell = strings.ToUpper(cell)
	if cell == "" {
		return 0, 0, errors.New("transfer: empty cell locator")
	}
	i := 0
	col := int64(0)
	for i < len(cell) && cell[i] >= 'A' && cell[i] <= 'Z' {
		if i == 3 {
			return 0, 0, errors.New("transfer: cell locator column exceeds three letters")
		}
		digit := int64(cell[i] - 'A' + 1)
		maxInt := int64(^uint(0) >> 1)
		if col > (maxInt-digit)/26 {
			return 0, 0, errors.New("transfer: cell locator column overflow")
		}
		col = col*26 + digit
		i++
	}
	if i == 0 || i == len(cell) {
		return 0, 0, errors.New("transfer: locator must identify exactly one cell")
	}
	row64 := int64(0)
	rowDigits := 0
	for ; i < len(cell); i++ {
		if cell[i] < '0' || cell[i] > '9' {
			return 0, 0, errors.New("transfer: invalid single-cell locator")
		}
		rowDigits++
		if rowDigits > 7 {
			return 0, 0, errors.New("transfer: cell locator row exceeds seven digits")
		}
		digit := int64(cell[i] - '0')
		maxInt := int64(^uint(0) >> 1)
		if row64 > (maxInt-digit)/10 {
			return 0, 0, errors.New("transfer: cell locator row overflow")
		}
		row64 = row64*10 + digit
	}
	if row64 < 1 {
		return 0, 0, errors.New("transfer: cell locator row must be positive")
	}
	return int(col - 1), int(row64 - 1), nil
}
func canonicalProviderValue(v assurance.ProviderValue) (string, error) {
	x := v.Value
	if v.Formula != "" {
		x = v.CalculatedValue
	}
	switch value := x.(type) {
	case json.Number:
		typed, err := assurance.NormalizeProviderValue(assurance.ProviderValue{Value: value}, assurance.FieldDefinition{Kind: assurance.ValueNumber})
		if err != nil {
			return "", err
		}
		return typed.Number, nil
	case string, bool, nil:
		b, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		return string(b), nil
	case int:
		typed, err := assurance.NormalizeProviderValue(assurance.ProviderValue{Value: value}, assurance.FieldDefinition{Kind: assurance.ValueNumber})
		if err != nil {
			return "", err
		}
		return typed.Number, nil
	case int64:
		typed, err := assurance.NormalizeProviderValue(assurance.ProviderValue{Value: value}, assurance.FieldDefinition{Kind: assurance.ValueNumber})
		if err != nil {
			return "", err
		}
		return typed.Number, nil
	default:
		return "", fmt.Errorf("transfer: unsupported provider scalar %T", x)
	}
}

type canonicalTransferValue struct {
	Kind             assurance.ValueKind `json:"kind"`
	Text             string              `json:"text,omitempty"`
	Number           string              `json:"number,omitempty"`
	Boolean          *bool               `json:"boolean,omitempty"`
	Unit             string              `json:"unit,omitempty"`
	Scale            string              `json:"scale,omitempty"`
	Precision        int                 `json:"precision,omitempty"`
	Formula          bool                `json:"formula"`
	FormulaText      string              `json:"formula_text,omitempty"`
	Calculated       bool                `json:"calculated,omitempty"`
	CalculatedSource string              `json:"calculated_source,omitempty"`
	ErrorCode        string              `json:"error_code,omitempty"`
}

func canonicalTypedValue(value assurance.TypedValue) (string, error) {
	canonical := canonicalTransferValue{
		Kind: value.Kind, Text: value.Text, Number: value.Number, Unit: value.Unit,
		Scale: value.Scale, Precision: value.Precision, Formula: value.Formula,
		FormulaText: value.FormulaText, Calculated: value.Calculated,
		CalculatedSource: value.CalculatedSource, ErrorCode: value.ErrorCode,
	}
	if value.Kind == assurance.ValueBoolean {
		boolean := value.Boolean
		canonical.Boolean = &boolean
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	canonicalJSON, err := assurance.CanonicalJSONBytes(raw)
	if err != nil {
		return "", err
	}
	return string(canonicalJSON), nil
}

func canonicalProviderIntentValue(value assurance.ProviderValue, frozen string) (string, error) {
	var expected assurance.TypedValue
	if err := json.Unmarshal([]byte(frozen), &expected); err != nil || expected.Kind == "" {
		return "", errors.New("transfer: frozen intent value is not canonical typed data")
	}
	typed, err := assurance.NormalizeProviderValue(value, assurance.FieldDefinition{Kind: expected.Kind, Unit: expected.Unit, Scale: expected.Scale, Precision: expected.Precision, PercentBasis: expected.PercentBasis, Timezone: expected.Timezone})
	if err != nil {
		return "", err
	}
	return canonicalTypedValue(typed)
}

func sameEndpoint(routeResourceID, routeSheetID, routeLocator string, endpoint Endpoint) bool {
	return routeResourceID == endpoint.ResourceID && routeSheetID == endpoint.SheetID && routeLocator == endpoint.Locator
}

// verifyStagedAuthority re-resolves the exact active route and policy captured
// by stage. ResolveTransferRoute also rechecks the active bundle, route hash,
// policy reference, and route capabilities for the trusted principal.
func (s *Service) verifyStagedAuthority(ctx context.Context, intent Intent) error {
	if intent.RouteID == "" || intent.RouteRevision <= 0 || intent.MappingID == "" || intent.PolicyID == "" || intent.PolicyRevision <= 0 {
		return errors.New("transfer: staged authority binding is incomplete")
	}
	route, err := s.assuranceStore.ResolveTransferRoute(ctx, intent.RouteID)
	if err != nil {
		return err
	}
	if route.RouteID != intent.RouteID || route.Revision != intent.RouteRevision || route.SourceMappingID != intent.MappingID || route.ContentHash != intent.MappingHash ||
		!sameEndpoint(route.SourceResourceID, route.SourceSheetID, route.SourceLocator, intent.Source) ||
		!sameEndpoint(route.TargetResourceID, route.TargetSheetID, route.TargetLocator, intent.Target) ||
		route.ConversionPolicyID != intent.PolicyID || route.ConversionPolicyVersion != intent.PolicyRevision {
		return errors.New("transfer: staged route or mapping authority changed")
	}
	policy, err := s.assuranceStore.ResolveConversionPolicy(ctx, intent.PolicyID, intent.PolicyRevision)
	if err != nil {
		return err
	}
	if policy.PolicyID != intent.PolicyID || policy.Revision != intent.PolicyRevision || policy.ContentHash != intent.PolicyHash {
		return errors.New("transfer: staged conversion policy authority changed")
	}
	return nil
}

// ConfirmUnknownError is an explicit do-not-repeat disposition. A provider
// mutation may have been accepted; callers must reconcile using the operation
// reference rather than stage or submit the same transfer again.
type ConfirmUnknownError struct {
	Reason                 string
	OperationReference     string
	ReconciliationRequired bool
	NoRepeat               bool
	Cause                  error
}

func (e *ConfirmUnknownError) Error() string {
	if e == nil {
		return "transfer outcome unknown"
	}
	return "transfer: " + e.Reason + "; reconciliation required; do not repeat"
}
func (e *ConfirmUnknownError) Unwrap() error { return e.Cause }

// ConfirmWasSealed classifies an ingress key without inspecting mutable transfer
// state or token; used by the public projection before calling Confirm.
func (s *Service) ConfirmWasSealed(ctx context.Context, keyDigest, requestDigest string) (bool, error) {
	p, ok := identity.PrincipalFromContext(ctx)
	if !ok || p.TenantID == "" || p.AuditActor() == "" || !p.HasPermission(identity.PermissionWorkivaWriteConfirm) {
		return false, errors.New("transfer: trusted confirm capability required")
	}
	result, found, err := s.assuranceStore.LookupReservation(ctx, assurance.ReservationRequest{
		ActorID: p.AuditActor(), Tool: "workiva_transfer_value", Action: "confirm",
		IdempotencyDigest: stageReservationDigest(keyDigest), RequestDigest: stageReservationDigest(requestDigest),
	})
	return found && result.Disposition == assurance.ReservationReplay, err
}

func (s *Service) Confirm(ctx context.Context, r ConfirmRequest) (Transfer, error) {
	var zero Transfer
	principal, ok := identity.PrincipalFromContext(ctx)
	trustedTenant, trustedActor := "", ""
	if ok {
		trustedTenant, trustedActor = principal.TenantID, principal.AuditActor()
	}
	if !ok || trustedTenant == "" || trustedActor == "" || !principal.HasPermission(identity.PermissionWorkivaWriteConfirm) || r.TransferID == "" || r.IdempotencyDigest == "" || r.RequestDigest == "" {
		return zero, errors.New("transfer: trusted tenant, actor, confirm capability, transfer and ingress digests required")
	}
	if (r.TenantID != "" && r.TenantID != trustedTenant) || (r.ActorID != "" && r.ActorID != trustedActor) || (r.Permission != "" && r.Permission != string(identity.PermissionWorkivaWriteConfirm)) {
		return zero, errors.New("transfer: caller identity or permission does not match trusted principal")
	}
	r.TenantID, r.ActorID, r.Permission = trustedTenant, trustedActor, string(identity.PermissionWorkivaWriteConfirm)
	if r.Now.IsZero() {
		r.Now = time.Now().UTC()
	} else {
		r.Now = r.Now.UTC()
	}
	scope := assurance.ReservationRequest{
		ActorID: trustedActor, Tool: "workiva_transfer_value", Action: "confirm",
		IdempotencyDigest: stageReservationDigest(r.IdempotencyDigest), RequestDigest: stageReservationDigest(r.RequestDigest),
	}
	prior, found, err := s.assuranceStore.LookupReservation(ctx, scope)
	if err != nil {
		return zero, err
	}
	if found {
		return confirmReservationProjection(prior)
	}
	if r.Token == "" {
		return zero, errors.New("transfer: confirmation token required on idempotency miss")
	}
	if err := s.assuranceStore.RequireRichAudit(ctx); err != nil {
		return zero, err
	}
	// The owner verifies frozen authority before the atomic reservation/claim;
	// a racing winner is classified inside the same transaction without CAS.
	before, err := s.store.Get(ctx, trustedTenant, r.TransferID)
	if err != nil {
		return zero, err
	}
	if before.Intent.ActorID != r.ActorID || before.Intent.Permission != r.Permission {
		return zero, errors.New("transfer: actor or permission mismatch")
	}
	if err := s.verifyStagedAuthority(ctx, before.Intent); err != nil {
		return zero, err
	}
	leaseID, err := newOpaqueID("lease-", 16)
	if err != nil {
		return zero, err
	}
	reservation, err := s.assuranceStore.ReserveAndClaimTransfer(ctx, scope, r.TransferID, r.Permission, r.Token, leaseID, r.Now)
	if err != nil {
		return zero, err
	}
	if reservation.Disposition != assurance.ReservationOwned {
		return confirmReservationProjection(reservation)
	}
	claimVersion, claimTime, err := s.store.CommittedClaim(ctx, trustedTenant, r.TransferID, trustedActor, leaseID)
	if err != nil {
		_ = s.store.markUnknown(ctx, trustedTenant, r.TransferID, "claim_evidence_unavailable")
		_ = s.failConfirm(ctx, reservation, err, time.Now().UTC())
		return zero, &ConfirmUnknownError{Reason: "claim_evidence_unavailable", ReconciliationRequired: true, NoRepeat: true, Cause: err}
	}
	environmentDigest, tenantDigest, err := s.fence.Identity(trustedTenant)
	if err != nil {
		_ = s.store.markUnknown(ctx, trustedTenant, r.TransferID, "fence_identity_mismatch")
		_ = s.failConfirm(ctx, reservation, err, time.Now().UTC())
		return zero, &ConfirmUnknownError{Reason: "fence_identity_mismatch", ReconciliationRequired: true, NoRepeat: true, Cause: err}
	}
	workCtx, leaseErrors, stopLeaseRenewal := s.startLeaseRenewal(ctx, trustedTenant, r.TransferID, leaseID, reservation)
	defer stopLeaseRenewal()
	renewBeforeStep := func() error {
		select {
		case err := <-leaseErrors:
			return err
		default:
		}
		now := time.Now().UTC()
		if err := s.store.RenewLease(ctx, trustedTenant, r.TransferID, leaseID, now); err != nil {
			return err
		}
		return s.assuranceStore.RenewReservation(ctx, reservation.RecordID, reservation.OwnerNonce, now)
	}
	operationReference := ""
	fail := func(reason string) (Transfer, error) {
		stateErr := s.store.markUnknown(ctx, trustedTenant, r.TransferID, reason)
		auditErr := s.failConfirm(ctx, reservation, errors.New(reason), time.Now().UTC())
		current, readErr := s.store.Get(ctx, trustedTenant, r.TransferID)
		failure := &ConfirmUnknownError{
			Reason: reason, OperationReference: operationReference,
			ReconciliationRequired: true, NoRepeat: true,
			Cause: errors.Join(stateErr, auditErr, readErr),
		}
		if readErr != nil {
			return zero, failure
		}
		return current, failure
	}
	if err := renewBeforeStep(); err != nil {
		return fail("lease_renewal_failed")
	}
	source, err := s.read(workCtx, before.Intent.Source)
	if err != nil {
		return fail("source_read_failed")
	}
	sourceValue, err := canonicalProviderIntentValue(source.Value, before.Intent.Intended)
	if err != nil || !source.CacheBypassed || digest(sourceValue) != before.Intent.Source.Fingerprint {
		return fail("stale_source")
	}
	if err := renewBeforeStep(); err != nil {
		return fail("lease_renewal_failed")
	}
	target, err := s.read(workCtx, before.Intent.Target)
	if err != nil {
		return fail("target_read_failed")
	}
	targetValue, err := canonicalProviderIntentValue(target.Value, before.Intent.Before)
	if err != nil || !target.CacheBypassed || digest(targetValue) != before.Intent.Target.Fingerprint {
		return fail("stale_target")
	}
	if err := renewBeforeStep(); err != nil {
		return fail("lease_renewal_failed")
	}
	if err := s.verifyStagedAuthority(ctx, before.Intent); err != nil {
		return fail("staged_authority_changed")
	}
	claimBytes, err := json.Marshal(claimPayload{
		EnvironmentDigest: environmentDigest, TenantDigest: tenantDigest, TransferID: r.TransferID,
		IdempotencyDigest: r.IdempotencyDigest, RequestDigest: r.RequestDigest, ActorDigest: digest(trustedActor),
		SourceHash: before.Intent.Source.Fingerprint, TargetHash: before.Intent.Target.Fingerprint,
		IntendedHash: digest(before.Intent.Intended), PolicyHash: before.Intent.PolicyHash, MappingHash: before.Intent.MappingHash,
		ClaimState: string(StateClaimed), ClaimTime: claimTime, ClaimRowVersion: claimVersion,
	})
	if err != nil {
		return fail("claim_payload_failed")
	}
	if err := canonicalFenceBody(claimBytes, "claim", r.TransferID, environmentDigest, tenantDigest); err != nil {
		return fail("claim_payload_invalid")
	}
	if err := renewBeforeStep(); err != nil {
		return fail("lease_renewal_failed")
	}
	cd, err := s.fence.CreateClaim(workCtx, r.TransferID, claimBytes)
	if err != nil {
		return fail("claim_fence_unavailable")
	}
	if cd != digest(string(claimBytes)) {
		return fail("claim_fence_digest_mismatch")
	}
	if err = s.fence.VerifyClaim(workCtx, r.TransferID, cd, claimBytes); err != nil {
		return fail("claim_fence_unverified")
	}
	if err = renewBeforeStep(); err != nil {
		return fail("lease_renewal_failed")
	}
	if err = s.store.markFencedOwned(ctx, r.TenantID, r.TransferID, leaseID, cd); err != nil {
		return fail("local_fence_commit_failed")
	}
	column, row, err := parseCellLocator(before.Intent.Target.Locator)
	if err != nil {
		return fail("invalid_target_locator")
	}
	value, err := decodeScalar(before.Intent.Intended)
	if err != nil {
		return fail("invalid_intended_value")
	}
	if err := renewBeforeStep(); err != nil {
		return fail("lease_renewal_failed")
	}
	if err = s.store.markSubmitting(ctx, r.TenantID, r.TransferID, leaseID); err != nil {
		return fail("local_submitting_commit_failed")
	}
	if err := renewBeforeStep(); err != nil {
		return fail("lease_renewal_failed")
	}
	op, delay, err := s.provider.UpdateSheetWithRetryAfter(workCtx, before.Intent.Target.ResourceID, before.Intent.Target.SheetID, workiva.NewEditCellsUpdate([]workiva.CellEdit{{Column: column, Row: row, Value: value}}))
	operationReference = op
	if err != nil {
		if op != "" {
			if persistErr := s.store.persistOperation(ctx, r.TenantID, r.TransferID, op); persistErr != nil {
				return fail("operation_reference_persist_failed")
			}
		}
		return fail("provider_outcome_unknown")
	}
	if err = s.store.persistOperation(ctx, r.TenantID, r.TransferID, op); err != nil {
		return fail("operation_reference_persist_failed")
	}
	if err := renewBeforeStep(); err != nil {
		return fail("lease_renewal_failed")
	}
	_, err = s.provider.WaitOperationWithInitialRetryAfter(workCtx, op, delay)
	if err != nil {
		return fail("poll_outcome_unknown")
	}
	if err := renewBeforeStep(); err != nil {
		return fail("lease_renewal_failed")
	}
	rb, err := s.read(workCtx, before.Intent.Target)
	if err != nil {
		return fail("readback_unavailable")
	}
	rbv, err := canonicalProviderIntentValue(rb.Value, before.Intent.Intended)
	if err != nil || !rb.CacheBypassed || rbv != before.Intent.Intended {
		return fail("readback_mismatch")
	}
	terminal, err := json.Marshal(terminalPayload{
		ClaimDigest: cd, EnvironmentDigest: environmentDigest, TenantDigest: tenantDigest, TransferID: r.TransferID,
		ProviderOutcomeDigest: digest("accepted"), OperationReferenceDigest: digest(op), ReadbackDigest: digest(rbv),
		TerminalKind: "accepted", Reason: "api_verified", TerminalTime: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return fail("terminal_payload_failed")
	}
	if err := canonicalFenceBody(terminal, "terminal", r.TransferID, environmentDigest, tenantDigest); err != nil {
		return fail("terminal_payload_invalid")
	}
	if err := renewBeforeStep(); err != nil {
		return fail("lease_renewal_failed")
	}
	td, err := s.fence.CreateTerminal(workCtx, r.TransferID, terminal)
	if err != nil {
		return fail("terminal_fence_unavailable")
	}
	if td != digest(string(terminal)) {
		return fail("terminal_fence_digest_mismatch")
	}
	if err = s.fence.VerifyTerminal(workCtx, r.TransferID, td, terminal); err != nil {
		return fail("terminal_fence_unverified")
	}
	projection := before
	projection.OperationReference = op
	projection.State = StateMachineVerified
	projection.Submissions = 1
	projection.ClaimFenceDigest = cd
	projection.TerminalFenceDigest = td
	projection.MachineOutcome = "api_verified"
	projection.MachineOutcomeProvenance = "provider_read_back"
	projection.VisualState = "pending"
	replayEnvelope, err := assurance.CanonicalJSON(confirmReplayEnvelope{Status: "machine_verified_visual_ack_pending", TransferID: r.TransferID, Transfer: &projection})
	if err != nil {
		return fail("replay_envelope_failed")
	}
	readbackID, err := newOpaqueID("rb-", 16)
	if err != nil {
		return fail("readback_id_failed")
	}
	if err = s.assuranceStore.FinalizeTransferCompletion(ctx, reservation, assurance.TransferCompletionRequest{
		TransferID: r.TransferID, OperationReference: op, TerminalFenceDigest: td,
		ReadbackID: readbackID, TypedReadbackJSON: rbv, ReplayEnvelope: replayEnvelope, Now: r.Now,
	}); err != nil {
		return fail("terminal_persist_failed")
	}
	return s.store.Get(ctx, r.TenantID, r.TransferID)
}

type confirmReplayEnvelope struct {
	Status     string                     `json:"status"`
	TransferID string                     `json:"transfer_id"`
	Transfer   *Transfer                  `json:"transfer,omitempty"`
	Error      *assurance.StructuredError `json:"error,omitempty"`
}

func confirmReservationProjection(reservation assurance.ReservationResult) (Transfer, error) {
	var zero Transfer
	switch reservation.Disposition {
	case assurance.ReservationConflict:
		return zero, errors.New("idempotency_conflict")
	case assurance.ReservationInProgress:
		return zero, errors.New("idempotency_in_progress")
	case assurance.ReservationReplay:
		var replay confirmReplayEnvelope
		if err := json.Unmarshal(reservation.Envelope, &replay); err != nil {
			return zero, err
		}
		if replay.Error != nil {
			return zero, errors.New(replay.Error.Code)
		}
		if replay.Transfer == nil {
			var failure assurance.StructuredError
			if err := json.Unmarshal(reservation.Envelope, &failure); err == nil && failure.Code != "" {
				return zero, errors.New(failure.Code)
			}
			return zero, errors.New("transfer: sealed confirm projection missing")
		}
		if replay.TransferID == "" || replay.Transfer.Intent.ID != replay.TransferID {
			return zero, errors.New("transfer: invalid sealed confirm projection")
		}
		return *replay.Transfer, nil
	default:
		return zero, errors.New("idempotency_state_invalid")
	}
}

func (s *Service) failConfirm(ctx context.Context, reservation assurance.ReservationResult, cause error, now time.Time) error {
	failure := assurance.StructuredError{Code: "transfer_confirm_failed", Message: cause.Error(), ReconciliationRequired: true}
	if err := s.assuranceStore.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, failure, now); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func transferActionReplayProjection(reservation assurance.ReservationResult) (assurance.TransferActionResult, error) {
	var result assurance.TransferActionResult
	if reservation.Disposition == assurance.ReservationConflict {
		return result, errors.New("idempotency_conflict")
	}
	if reservation.Disposition == assurance.ReservationInProgress {
		return result, errors.New("idempotency_in_progress")
	}
	if reservation.Disposition != assurance.ReservationReplay {
		return result, errors.New("idempotency_state_invalid")
	}
	if err := json.Unmarshal(reservation.Envelope, &result); err != nil {
		return result, err
	}
	if result.TransferID == "" || result.Status == "" {
		return result, errors.New("transfer: sealed action projection missing")
	}
	result.Status = "idempotency_replay"
	result.NoMutationSubmitted = true
	return result, nil
}

// Acknowledge records trusted human UI evidence. It performs the idempotency
// lookup before rich-audit checks, transfer reads, or any local claim and never
// calls a provider.
func (s *Service) Acknowledge(ctx context.Context, r AcknowledgeRequest) (assurance.TransferActionResult, error) {
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok || principal.TenantID == "" || principal.AuditActor() == "" || !principal.HasPermission(identity.PermissionWorkivaVisualAck) || r.TransferID == "" || r.IdempotencyDigest == "" || r.RequestDigest == "" {
		return assurance.TransferActionResult{}, errors.New("transfer: trusted tenant, actor, visual acknowledgement capability, transfer and ingress digests required")
	}
	if r.Now.IsZero() {
		r.Now = time.Now().UTC()
	} else {
		r.Now = r.Now.UTC()
	}
	scope := assurance.ReservationRequest{ActorID: principal.AuditActor(), Tool: "workiva_transfer_value", Action: "acknowledge", IdempotencyDigest: stageReservationDigest(r.IdempotencyDigest), RequestDigest: stageReservationDigest(r.RequestDigest)}
	prior, found, err := s.assuranceStore.LookupReservation(ctx, scope)
	if err != nil {
		return assurance.TransferActionResult{}, err
	}
	if found {
		return transferActionReplayProjection(prior)
	}
	if err := s.assuranceStore.RequireRichAudit(ctx); err != nil {
		return assurance.TransferActionResult{}, err
	}
	if r.UILocationJSON == "" && (r.Observation == "matches" || r.Observation == "mismatch") {
		return assurance.TransferActionResult{}, errors.New("transfer: UI location is required for a match or mismatch")
	}
	var uiLocation struct {
		ResourceID string `json:"resource_id"`
		Locator    string `json:"locator"`
	}
	if r.UILocationJSON != "" {
		if err := json.Unmarshal([]byte(r.UILocationJSON), &uiLocation); err != nil {
			return assurance.TransferActionResult{}, errors.New("transfer: UI location is invalid")
		}
	}
	intent, err := s.store.Get(ctx, principal.TenantID, r.TransferID)
	if err != nil {
		return assurance.TransferActionResult{}, err
	}
	if r.UILocationJSON != "" && (uiLocation.ResourceID != intent.Intent.Target.ResourceID || uiLocation.Locator != intent.Intent.Target.Locator) {
		return assurance.TransferActionResult{}, errors.New("transfer: UI location is not the staged target")
	}
	reservation, err := s.assuranceStore.Reserve(ctx, scope, r.Now)
	if err != nil {
		return assurance.TransferActionResult{}, err
	}
	if reservation.Disposition != assurance.ReservationOwned {
		return transferActionReplayProjection(reservation)
	}
	if err := s.assuranceStore.MarkExecutionStarted(ctx, reservation.RecordID, reservation.OwnerNonce, r.Now); err != nil {
		_ = s.assuranceStore.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, assurance.StructuredError{Code: "acknowledge_failed", Message: err.Error(), NLAuditID: mcpAuditID(ctx)}, r.Now)
		return assurance.TransferActionResult{}, err
	}
	if r.ObservedValueJSON != "" {
		canonical, e := assurance.CanonicalJSONBytes([]byte(r.ObservedValueJSON))
		if e != nil {
			_ = s.assuranceStore.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, assurance.StructuredError{Code: "invalid_observed_value", Message: e.Error(), NLAuditID: mcpAuditID(ctx)}, r.Now)
			return assurance.TransferActionResult{}, e
		}
		r.ObservedValueJSON = string(canonical)
	}
	result, err := s.assuranceStore.FinalizeTransferVisualAcknowledgement(ctx, reservation, assurance.TransferVisualAcknowledgementRequest{
		TransferID: r.TransferID, AckActorID: principal.AuditActor(), Observation: r.Observation, UILocationJSON: r.UILocationJSON,
		Note:      r.Note,
		Refreshed: r.Refreshed, RefreshOverride: r.RefreshOverrideReason, ObservedValueJSON: r.ObservedValueJSON,
		ObservedDigest: func() string {
			if r.ObservedValueJSON == "" {
				return ""
			}
			return digest(r.ObservedValueJSON)
		}(), Now: r.Now,
	})
	if err != nil {
		_ = s.assuranceStore.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, assurance.StructuredError{Code: "acknowledge_failed", Message: err.Error(), ReconciliationRequired: true, NLAuditID: mcpAuditID(ctx)}, r.Now)
		return assurance.TransferActionResult{}, err
	}
	return result, nil
}

// Reconcile executes inspect_operation, read_back, classify, and close. The
// only provider capabilities used are a read-only operation GET and a distinct
// uncached typed read; no branch can submit a mutation.
func (s *Service) Reconcile(ctx context.Context, r ReconcileRequest) (assurance.TransferActionResult, error) {
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok || principal.TenantID == "" || principal.AuditActor() == "" || !principal.HasPermission(identity.PermissionReconciliationManage) || r.TransferID == "" || r.Action == "" || r.IdempotencyDigest == "" || r.RequestDigest == "" {
		return assurance.TransferActionResult{}, errors.New("transfer: trusted tenant, actor, reconciliation capability, transfer and ingress digests required")
	}
	if r.Now.IsZero() {
		r.Now = time.Now().UTC()
	} else {
		r.Now = r.Now.UTC()
	}
	scope := assurance.ReservationRequest{ActorID: principal.AuditActor(), Tool: "workiva_transfer_value", Action: "reconcile", IdempotencyDigest: stageReservationDigest(r.IdempotencyDigest), RequestDigest: stageReservationDigest(r.RequestDigest)}
	prior, found, err := s.assuranceStore.LookupReservation(ctx, scope)
	if err != nil {
		return assurance.TransferActionResult{}, err
	}
	if found {
		return transferActionReplayProjection(prior)
	}
	if err := s.assuranceStore.RequireRichAudit(ctx); err != nil {
		return assurance.TransferActionResult{}, err
	}
	reservation, err := s.assuranceStore.Reserve(ctx, scope, r.Now)
	if err != nil {
		return assurance.TransferActionResult{}, err
	}
	if reservation.Disposition != assurance.ReservationOwned {
		return transferActionReplayProjection(reservation)
	}
	if err := s.assuranceStore.MarkExecutionStarted(ctx, reservation.RecordID, reservation.OwnerNonce, r.Now); err != nil {
		_ = s.assuranceStore.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, assurance.StructuredError{Code: "reconcile_failed", Message: err.Error(), NLAuditID: mcpAuditID(ctx)}, r.Now)
		return assurance.TransferActionResult{}, err
	}
	intent, err := s.store.Get(ctx, principal.TenantID, r.TransferID)
	if err != nil {
		return s.failAction(ctx, reservation, err, r.Now)
	}
	if intent.State != StateReconciliationRequired {
		return s.failAction(ctx, reservation, errors.New("reconciliation requires reconciliation_required state"), r.Now)
	}
	request := assurance.TransferReconciliationRequest{TransferID: r.TransferID, Action: r.Action, Classification: r.Classification, Note: r.Note, Now: r.Now}
	switch r.Action {
	case "inspect_operation":
		if intent.Intent.ID == "" || intent.OperationReference == "" {
			return s.failAction(ctx, reservation, errors.New("no stored provider operation reference is available"), r.Now)
		}
		inspection, inspectErr := s.provider.InspectOperation(ctx, intent.OperationReference)
		if inspectErr != nil {
			return s.failAction(ctx, reservation, inspectErr, r.Now)
		}
		event, e := assurance.CanonicalJSON(map[string]any{"operation_reference": inspection.Reference, "status": inspection.Status, "resource_url": inspection.ResourceURL, "read_only": true})
		if e != nil {
			return s.failAction(ctx, reservation, e, r.Now)
		}
		request.OperationEventJSON = string(event)
	case "read_back":
		rb, readErr := s.read(ctx, intent.Intent.Target)
		if readErr != nil || !rb.CacheBypassed {
			if readErr == nil {
				readErr = errors.New("uncached reconciliation read-back required")
			}
			return s.failAction(ctx, reservation, readErr, r.Now)
		}
		value, valueErr := canonicalProviderIntentValue(rb.Value, intent.Intent.Intended)
		if valueErr != nil {
			return s.failAction(ctx, reservation, valueErr, r.Now)
		}
		readbackID, idErr := newOpaqueID("rb-", 16)
		if idErr != nil {
			return s.failAction(ctx, reservation, idErr, r.Now)
		}
		request.ReadbackID, request.TypedReadbackJSON, request.ReadbackCacheBypassed = readbackID, value, true
	case "classify", "close":
		// Classification is local-only. Generic operation failure is not
		// non-acceptance or proof of no effect; the store fails closed.
	default:
		return s.failAction(ctx, reservation, errors.New("unsupported reconciliation action"), r.Now)
	}
	result, err := s.assuranceStore.FinalizeTransferReconciliation(ctx, reservation, request)
	if err != nil {
		return s.failAction(ctx, reservation, err, r.Now)
	}
	return result, nil
}

func mcpAuditID(ctx context.Context) string {
	if id := assurance.TransferAuditIDFromContext(ctx); id != "" {
		return id
	}
	return ""
}

func (s *Service) failAction(ctx context.Context, reservation assurance.ReservationResult, cause error, now time.Time) (assurance.TransferActionResult, error) {
	code := "transfer_action_failed"
	if strings.Contains(cause.Error(), "capability_unverified") {
		code = "capability_unverified"
	}
	failure := assurance.StructuredError{Code: code, Message: cause.Error(), ReconciliationRequired: true, NLAuditID: mcpAuditID(ctx)}
	if err := s.assuranceStore.FailReservation(ctx, reservation.RecordID, reservation.OwnerNonce, failure, now); err != nil {
		return assurance.TransferActionResult{}, errors.Join(cause, err)
	}
	return assurance.TransferActionResult{}, cause
}
func decodeScalar(raw string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	switch value := value.(type) {
	case string, bool:
		return value, nil
	case json.Number:
		canonical, err := canonicalProviderValue(assurance.ProviderValue{Value: value})
		if err != nil {
			return nil, err
		}
		return json.Number(canonical), nil
	case map[string]any:
		kind, ok := value["kind"].(string)
		if !ok || kind == "" {
			return nil, errors.New("transfer: typed value kind is required")
		}
		switch kind {
		case "text", "string":
			text, ok := value["text"].(string)
			if !ok {
				return nil, errors.New("transfer: typed text value is missing text")
			}
			return text, nil
		case "number", "integer", "currency", "percent":
			number, ok := value["number"].(string)
			if !ok || number == "" {
				return nil, errors.New("transfer: typed numeric value is missing number")
			}
			canonical, err := canonicalProviderValue(assurance.ProviderValue{Value: json.Number(number)})
			if err != nil {
				return nil, err
			}
			return json.Number(canonical), nil
		case "boolean":
			boolean, ok := value["boolean"].(bool)
			if !ok {
				return nil, errors.New("transfer: typed boolean value is missing boolean")
			}
			return boolean, nil
		default:
			return nil, fmt.Errorf("transfer: unsupported typed value kind %q", kind)
		}
	default:
		return nil, fmt.Errorf("transfer: unsupported scalar JSON type %T", value)
	}
}
