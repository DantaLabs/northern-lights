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
type ConfirmRequest struct {
	TenantID, ActorID, Permission, TransferID, Token string
	Now                                              time.Time
}
type Service struct {
	store          *Store
	assuranceStore *assurance.Store
	provider       *workivaprovider.Router
	fence          Fence
}

// NewService refuses to expose the write path unless both the provider router and
// an explicitly configured fence implementation are present. Production wiring
// must supply a durable implementation; FakeFence is intended only for tests.
func NewService(store *Store, assuranceStore *assurance.Store, provider *workivaprovider.Router, fence Fence) (*Service, error) {
	if store == nil || assuranceStore == nil || assuranceStore.Ready() != nil || provider == nil || fence == nil || !fence.Durable() {
		return nil, errors.New("transfer: store, ready assurance store, provider router, and verified durable fence are required")
	}
	return &Service{store: store, assuranceStore: assuranceStore, provider: provider, fence: fence}, nil
}

// NewTestService exposes the fake-backed service only to local tests.
func NewTestService(store *Store, assuranceStore *assurance.Store, provider *workivaprovider.Router, fence *FakeFence) (*Service, error) {
	if store == nil || assuranceStore == nil || assuranceStore.Ready() != nil || provider == nil || fence == nil {
		return nil, errors.New("transfer: test store, ready assurance store, router, and fake fence are required")
	}
	return &Service{store: store, assuranceStore: assuranceStore, provider: provider, fence: fence}, nil
}
func (s *Service) Stage(ctx context.Context, r StageRequest) (StageResult, error) {
	var zero StageResult
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok || principal.TenantID == "" || principal.AuditActor() == "" || r.RouteID == "" || r.IdempotencyDigest == "" || r.RequestDigest == "" {
		return zero, errors.New("transfer: trusted tenant, actor, route, and request digests are required")
	}
	route, err := s.assuranceStore.ResolveTransferRoute(ctx, r.RouteID)
	if err != nil {
		return zero, err
	}
	policy, err := s.assuranceStore.ResolveConversionPolicy(ctx, route.ConversionPolicyID, route.ConversionPolicyVersion)
	if err != nil {
		return zero, err
	}
	sourceEndpoint := Endpoint{ResourceID: route.SourceResourceID, SheetID: route.SourceSheetID, Locator: route.SourceLocator}
	targetEndpoint := Endpoint{ResourceID: route.TargetResourceID, SheetID: route.TargetSheetID, Locator: route.TargetLocator}
	src, err := s.read(ctx, sourceEndpoint)
	if err != nil || !src.CacheBypassed {
		return zero, errors.New("transfer: uncached source read required")
	}
	dst, err := s.read(ctx, targetEndpoint)
	if err != nil || !dst.CacheBypassed {
		return zero, errors.New("transfer: uncached target read required")
	}
	sourceRaw, err := canonicalProviderValue(src.Value)
	if err != nil {
		return zero, err
	}
	targetRaw, err := canonicalProviderValue(dst.Value)
	if err != nil {
		return zero, err
	}
	field := assurance.FieldDefinition{Kind: policy.SourceKind, Unit: policy.SourceUnit}
	sourceTyped, err := assurance.NormalizeProviderValue(src.Value, field)
	if err != nil {
		return zero, err
	}
	_, err = assurance.NormalizeProviderValue(dst.Value, assurance.FieldDefinition{Kind: policy.TargetKind, Unit: policy.TargetUnit})
	if err != nil {
		return zero, err
	}
	_, err = policy.ConvertTypedValue(sourceTyped)
	if err != nil {
		return zero, err
	}
	beforeBytes := []byte(targetRaw)
	afterBytes := []byte(sourceRaw)
	rawToken := make([]byte, 32)
	if _, err := rand.Read(rawToken); err != nil {
		return zero, err
	}
	token := hex.EncodeToString(rawToken)
	now := r.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	intent := Intent{ID: fmt.Sprintf("tr-%x", rawToken[:12]), TenantID: principal.TenantID, ActorID: principal.AuditActor(), Permission: "workiva.write.confirm", Source: sourceEndpoint, Target: targetEndpoint, Before: string(beforeBytes), Intended: string(afterBytes), PolicyHash: policy.ContentHash, MappingHash: route.ContentHash, ExpiresAt: now.Add(15 * time.Minute)}
	intent.Source.Fingerprint = digest(sourceRaw)
	intent.Target.Fingerprint = digest(targetRaw)
	return s.store.Stage(ctx, intent, token, r.IdempotencyDigest, r.RequestDigest, now)
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
		col = col*26 + int64(cell[i]-'A'+1)
		if col > int64(^uint(0)>>1) {
			return 0, 0, errors.New("transfer: cell locator column overflow")
		}
		i++
	}
	if i == 0 || i == len(cell) {
		return 0, 0, errors.New("transfer: locator must identify exactly one cell")
	}
	row64 := int64(0)
	for ; i < len(cell); i++ {
		if cell[i] < '0' || cell[i] > '9' {
			return 0, 0, errors.New("transfer: invalid single-cell locator")
		}
		row64 = row64*10 + int64(cell[i]-'0')
		if row64 > int64(^uint(0)>>1) {
			return 0, 0, errors.New("transfer: cell locator row overflow")
		}
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
	b, err := json.Marshal(x)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
func (s *Service) Confirm(ctx context.Context, r ConfirmRequest) (Transfer, error) {
	var zero Transfer
	if r.TenantID == "" || r.ActorID == "" || r.Permission != "workiva.write.confirm" || r.TransferID == "" || r.Token == "" {
		return zero, errors.New("transfer: trusted tenant, actor, permission, transfer and token required")
	}
	before, err := s.store.Get(ctx, r.TenantID, r.TransferID)
	if err != nil {
		return zero, err
	}
	if before.Intent.ActorID != r.ActorID || before.Intent.Permission != r.Permission {
		return zero, errors.New("transfer: actor or permission mismatch")
	}
	claim, err := s.store.Claim(ctx, Claim{TenantID: r.TenantID, ActorID: r.ActorID, Permission: r.Permission, TransferID: r.TransferID, Token: r.Token, Now: r.Now, LeaseID: digest(fmt.Sprintf("%s:%d", r.TransferID, r.Now.UnixNano())), LeaseUntil: r.Now.Add(time.Minute)})
	if err != nil {
		return zero, err
	}
	if !claim {
		return s.store.Get(ctx, r.TenantID, r.TransferID)
	}
	fail := func(reason string) (Transfer, error) {
		_ = s.store.markUnknown(ctx, r.TenantID, r.TransferID, reason)
		return s.store.Get(ctx, r.TenantID, r.TransferID)
	}
	source, err := s.read(ctx, before.Intent.Source)
	if err != nil {
		return fail("source_read_failed")
	}
	sourceValue, err := canonicalProviderValue(source.Value)
	if err != nil || !source.CacheBypassed || digest(sourceValue) != before.Intent.Source.Fingerprint {
		return fail("stale_source")
	}
	target, err := s.read(ctx, before.Intent.Target)
	if err != nil {
		return fail("target_read_failed")
	}
	targetValue, err := canonicalProviderValue(target.Value)
	if err != nil || !target.CacheBypassed || digest(targetValue) != before.Intent.Target.Fingerprint {
		return fail("stale_target")
	}
	claimBytes, _ := json.Marshal(map[string]string{"transfer_id": r.TransferID, "tenant_digest": digest(r.TenantID), "actor_digest": digest(r.ActorID), "source_hash": before.Intent.Source.Fingerprint, "target_hash": before.Intent.Target.Fingerprint, "intended_hash": digest(before.Intent.Intended), "policy_hash": before.Intent.PolicyHash, "mapping_hash": before.Intent.MappingHash})
	cd, err := s.fence.CreateClaim(ctx, r.TransferID, claimBytes)
	if err != nil {
		return fail("claim_fence_unavailable")
	}
	if err = s.fence.VerifyClaim(ctx, r.TransferID, cd); err != nil {
		return fail("claim_fence_unverified")
	}
	if err = s.store.markFenced(ctx, r.TenantID, r.TransferID, cd); err != nil {
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
	op, delay, err := s.provider.UpdateSheetWithRetryAfter(ctx, before.Intent.Target.ResourceID, before.Intent.Target.SheetID, workiva.NewEditCellsUpdate([]workiva.CellEdit{{Column: column, Row: row, Value: value}}))
	if err != nil {
		return fail("provider_outcome_unknown")
	}
	if err = s.store.persistOperation(ctx, r.TenantID, r.TransferID, op); err != nil {
		return fail("operation_reference_persist_failed")
	}
	_, err = s.provider.WaitOperationWithInitialRetryAfter(ctx, op, delay)
	if err != nil {
		return fail("poll_outcome_unknown")
	}
	rb, err := s.read(ctx, before.Intent.Target)
	if err != nil {
		return fail("readback_unavailable")
	}
	rbv, err := canonicalProviderValue(rb.Value)
	if err != nil || !rb.CacheBypassed || rbv != before.Intent.Intended {
		return fail("readback_mismatch")
	}
	terminal, _ := json.Marshal(map[string]string{"claim_digest": cd, "transfer_id": r.TransferID, "provider_outcome_digest": digest("accepted"), "operation_reference_digest": digest(op), "readback_digest": digest(rbv), "terminal_kind": "accepted"})
	td, err := s.fence.CreateTerminal(ctx, r.TransferID, terminal)
	if err != nil {
		return fail("terminal_fence_unavailable")
	}
	if err = s.fence.VerifyTerminal(ctx, r.TransferID, td); err != nil {
		return fail("terminal_fence_unverified")
	}
	if err = s.store.finishVerified(ctx, r.TenantID, r.TransferID, td, op, r.Now); err != nil {
		return fail("terminal_persist_failed")
	}
	return s.store.Get(ctx, r.TenantID, r.TransferID)
}
func decodeScalar(raw string) (any, error) {
	var v struct {
		Kind    string `json:"kind"`
		Text    string `json:"text"`
		Number  string `json:"number"`
		Boolean bool   `json:"boolean"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		var number json.Number
		if numberErr := json.Unmarshal([]byte(raw), &number); numberErr == nil {
			return number, nil
		}
		return nil, err
	}
	if v.Kind == "" {
		var number json.Number
		if err := json.Unmarshal([]byte(raw), &number); err == nil {
			return number, nil
		}
	}
	switch v.Kind {
	case "text", "string":
		return v.Text, nil
	case "number":
		return json.Number(v.Number), nil
	case "boolean":
		return v.Boolean, nil
	default:
		return nil, fmt.Errorf("transfer: unsupported typed value kind %q", v.Kind)
	}
}
