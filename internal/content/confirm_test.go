package content

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/workiva"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
	_ "modernc.org/sqlite"
)

func TestConfirmSealedReplayPrecedesFreshTokenAndDependencies(t *testing.T) {
	store, _, baseCtx, now, _, _, _ := stageIntegrationFixture(t)
	principal, ok := identity.PrincipalFromContext(baseCtx)
	if !ok {
		t.Fatal("fixture principal missing")
	}
	principal.Permissions = append(principal.Permissions, identity.PermissionContentConfirm)
	ctx := identity.ContextWithPrincipal(context.Background(), principal)
	request := ConfirmRequest{PlacementIntentID: "placement-intent-fixture", ConfirmationToken: "expired-or-omitted", IdempotencyKey: "confirm-replay-key-0000001"}
	requestBytes, err := assurance.CanonicalJSON(struct {
		Phase string `json:"phase"`
		ID    string `json:"placement_intent_id"`
	}{"confirm", request.PlacementIntentID})
	if err != nil {
		t.Fatal(err)
	}
	reservationRequest := assurance.ReservationRequest{ActorID: principal.AuditActor(), Tool: confirmTool, Action: "confirm", IdempotencyDigest: assurance.DigestIdempotencyKey(request.IdempotencyKey), RequestDigest: assurance.HashBytes(requestBytes), RetentionClass: "workflow", RetainUntil: now.Add(time.Hour)}
	reservation, err := store.Reserve(ctx, reservationRequest, now)
	if err != nil {
		t.Fatal(err)
	}
	response := ConfirmResponse{Status: "machine_verified_visual_ack_pending", VisualState: "pending", TerminalFenceDigest: "aabbccdd"}
	envelope, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SealReservation(ctx, reservation.RecordID, reservation.OwnerNonce, envelope, response.Status, "placement-intent-fixture", "audit-confirm-replay", now); err != nil {
		t.Fatal(err)
	}
	service := ConfirmationService{Store: store, Clock: fixedStageClock{now}}
	got, err := service.Confirm(ctx, "", request)
	if err != nil || got.Status != response.Status || got.TerminalFenceDigest != response.TerminalFenceDigest || got.ResultSHA256 == "" {
		t.Fatalf("replay=%+v err=%v", got, err)
	}
}

func TestUnknownConfirmationErrorCannotPermitResubmission(t *testing.T) {
	err := &UnknownConfirmationError{IntentID: "placement-1", Reason: "provider_outcome_unknown", OperationReference: "op-1", ReconciliationRequired: true, ResubmissionAllowed: false}
	if err.Error() == "" || !err.ReconciliationRequired || err.ResubmissionAllowed || err.OperationReference != "op-1" {
		t.Fatalf("unknown result lost fail-closed fields: %+v", err)
	}
}

type confirmationFlowProvider struct {
	mu        sync.Mutex
	metadata  workivaprovider.ContentMetadata
	updates   int
	payload   []byte
	submitErr error
}

func (p *confirmationFlowProvider) ReadContentMetadata(_ context.Context, spreadsheet, sheet, cell string) (workivaprovider.ContentMetadata, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	copy := p.metadata
	copy.SpreadsheetID, copy.SheetID, copy.Locator = spreadsheet, sheet, cell
	copy.Provenance.QueryRange = cell
	return copy, nil
}

func (p *confirmationFlowProvider) UpdateSheetWithRetryAfter(_ context.Context, spreadsheet, sheet string, update workiva.SheetUpdate) (string, time.Duration, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.updates++
	p.payload, _ = json.Marshal(update)
	var body struct {
		EditCells struct {
			Cells []struct {
				Column int             `json:"column"`
				Row    int             `json:"row"`
				Value  json.RawMessage `json:"value"`
			} `json:"cells"`
		} `json:"editCells"`
	}
	if err := json.Unmarshal(p.payload, &body); err != nil || len(body.EditCells.Cells) != 1 {
		return "", 0, errors.New("expected one editCells literal")
	}
	p.metadata.RawValue = append(json.RawMessage(nil), body.EditCells.Cells[0].Value...)
	p.metadata.SpreadsheetID, p.metadata.SheetID = spreadsheet, sheet
	return "operation-confirm-1", 0, p.submitErr
}

func (p *confirmationFlowProvider) WaitOperationWithInitialRetryAfter(context.Context, string, time.Duration) (string, error) {
	return "SUCCEEDED", nil
}

func (p *confirmationFlowProvider) ContentLiteralWriteContract(context.Context) (workivaprovider.ContentLiteralWriteContract, error) {
	return workivaprovider.ContentLiteralWriteContract{ProviderFamily: "workiva", APIVersion: "2026-01-01", Endpoint: "POST /spreadsheets/{spreadsheetId}/sheets/{sheetId}/update", FormatPreservation: "preserves", Authority: "explicit test writer"}, nil
}

type confirmationProviderWithoutWriterProof struct{ provider *confirmationFlowProvider }

func (p confirmationProviderWithoutWriterProof) ReadContentMetadata(ctx context.Context, spreadsheet, sheet, cell string) (workivaprovider.ContentMetadata, error) {
	return p.provider.ReadContentMetadata(ctx, spreadsheet, sheet, cell)
}
func (p confirmationProviderWithoutWriterProof) UpdateSheetWithRetryAfter(ctx context.Context, spreadsheet, sheet string, update workiva.SheetUpdate) (string, time.Duration, error) {
	return p.provider.UpdateSheetWithRetryAfter(ctx, spreadsheet, sheet, update)
}
func (p confirmationProviderWithoutWriterProof) WaitOperationWithInitialRetryAfter(ctx context.Context, operation string, delay time.Duration) (string, error) {
	return p.provider.WaitOperationWithInitialRetryAfter(ctx, operation, delay)
}

func TestSignedIntakeStageConfirmWritesOneExactCellAndReplaysWithoutFreshDependencies(t *testing.T) {
	store, ctx, now, resolver, provider, created, _ := confirmationFlowFixture(t, nil)
	staged := stageConfirmationPreview(t, store, ctx, now, resolver, provider, created, "flow")
	var err error
	fence, err := NewLocalTestFence(testTenant, "wave4-test-env")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewLocalTestConfirmationService(store, AssuranceCandidateResolver{Store: store}, resolver, provider, fence, fixedStageClock{now})
	if err != nil {
		t.Fatal(err)
	}
	request := ConfirmRequest{PlacementIntentID: placementIntentIDFromPreview(t, staged.Preview), ConfirmationToken: staged.ConfirmationToken, IdempotencyKey: "confirm-flow-idempotency-key-01"}
	var frozen PreviewPlan
	_ = json.Unmarshal(staged.Preview, &frozen)
	observed, _ := provider.ReadContentMetadata(ctx, frozen.Destination.ResourceID, frozen.Destination.SheetID, frozen.Destination.Cell)
	if err := validateConfirmationTarget(frozen, frozen.Destination, observed); err != nil {
		t.Fatalf("fixture provider does not match staged proof: %v observed=%+v frozen=%+v", err, observed, frozen.ProviderMetadata)
	}
	response, err := service.Confirm(ctx, "audit-flow-confirm", request)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if response.Status != "machine_verified_visual_ack_pending" || response.VisualState != "pending" || response.ResultSHA256 == "" || provider.updates != 1 || string(response.ReadbackJSON) == "" || hashBytes(response.ReadbackJSON) != response.ReadbackSHA256 {
		t.Fatalf("response=%+v updates=%d", response, provider.updates)
	}
	var payload struct {
		EditCells struct {
			Cells []struct {
				Column int    `json:"column"`
				Row    int    `json:"row"`
				Value  string `json:"value"`
			} `json:"cells"`
		} `json:"editCells"`
	}
	if err := json.Unmarshal(provider.payload, &payload); err != nil || len(payload.EditCells.Cells) != 1 || payload.EditCells.Cells[0].Column != 1 || payload.EditCells.Cells[0].Row != 3 || payload.EditCells.Cells[0].Value != " exact staged text\n" {
		t.Fatalf("update payload=%s err=%v", provider.payload, err)
	}
	replayService := ConfirmationService{Store: store, Clock: fixedStageClock{now}}
	replayRequest := request
	replayRequest.ConfirmationToken = ""
	replayed, err := replayService.Confirm(ctx, "", replayRequest)
	if err != nil || replayed.ResultSHA256 != response.ResultSHA256 || provider.updates != 1 {
		t.Fatalf("replay=%+v updates=%d err=%v", replayed, provider.updates, err)
	}
}

func TestConfirmSubmitErrorWithKnownOperationIsUnknownAndNeverResubmitted(t *testing.T) {
	store, ctx, now, resolver, provider, created, _ := confirmationFlowFixture(t, errors.New("provider accepted but response failed"))
	staged := stageConfirmationPreview(t, store, ctx, now, resolver, provider, created, "error")
	fence, err := NewLocalTestFence(testTenant, "wave4-test-env")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewLocalTestConfirmationService(store, AssuranceCandidateResolver{Store: store}, resolver, provider, fence, fixedStageClock{now})
	if err != nil {
		t.Fatal(err)
	}
	intentID := placementIntentIDFromPreview(t, staged.Preview)
	_, err = service.Confirm(ctx, "audit-flow-confirm-error", ConfirmRequest{PlacementIntentID: intentID, ConfirmationToken: staged.ConfirmationToken, IdempotencyKey: "confirm-flow-error-idem-key-01"})
	var unknown *UnknownConfirmationError
	if !errors.As(err, &unknown) || unknown.OperationReference != "operation-confirm-1" || !unknown.ReconciliationRequired || unknown.ResubmissionAllowed || provider.updates != 1 {
		t.Fatalf("err=%v unknown=%+v updates=%d", err, unknown, provider.updates)
	}
	var operation, state string
	if err := store.DB().QueryRow(`SELECT operation_reference,state FROM assurance_content_placement_intents WHERE placement_intent_id=?`, intentID).Scan(&operation, &state); err != nil || operation != "operation-confirm-1" || state != "reconciliation_required" {
		t.Fatalf("operation=%q state=%q err=%v", operation, state, err)
	}
}

func TestConfirmTargetTamperAfterStagePreventsWrite(t *testing.T) {
	store, ctx, now, resolver, provider, created, _ := confirmationFlowFixture(t, nil)
	staged := stageConfirmationPreview(t, store, ctx, now, resolver, provider, created, "target-tamper")
	provider.mu.Lock()
	provider.metadata.RawValue = json.RawMessage(`"unexpected"`)
	provider.mu.Unlock()
	fence, err := NewLocalTestFence(testTenant, "wave4-test-env")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewLocalTestConfirmationService(store, AssuranceCandidateResolver{Store: store}, resolver, provider, fence, fixedStageClock{now})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Confirm(ctx, "audit-target-tamper", ConfirmRequest{PlacementIntentID: placementIntentIDFromPreview(t, staged.Preview), ConfirmationToken: staged.ConfirmationToken, IdempotencyKey: "confirm-target-tamper-key-001"})
	if err == nil || provider.updates != 0 {
		t.Fatalf("error=%v updates=%d", err, provider.updates)
	}
}

func TestConfirmWithoutExplicitConfiguredWriterProofStopsBeforeClaim(t *testing.T) {
	store, ctx, now, resolver, provider, created, _ := confirmationFlowFixture(t, nil)
	staged := stageConfirmationPreview(t, store, ctx, now, resolver, provider, created, "writer-unknown")
	fence, err := NewLocalTestFence(testTenant, "wave4-test-env")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewLocalTestConfirmationService(store, AssuranceCandidateResolver{Store: store}, resolver, confirmationProviderWithoutWriterProof{provider}, fence, fixedStageClock{now})
	if err != nil {
		t.Fatal(err)
	}
	intentID := placementIntentIDFromPreview(t, staged.Preview)
	_, err = service.Confirm(ctx, "audit-writer-unknown", ConfirmRequest{PlacementIntentID: intentID, ConfirmationToken: staged.ConfirmationToken, IdempotencyKey: "confirm-writer-unknown-idem-01"})
	if err == nil || provider.updates != 0 {
		t.Fatalf("error=%v updates=%d", err, provider.updates)
	}
	var state, confirmationID string
	if err := store.DB().QueryRow(`SELECT state,confirmation_record_id FROM assurance_content_placement_intents WHERE placement_intent_id=?`, intentID).Scan(&state, &confirmationID); err != nil || state != "staged" || confirmationID != "" {
		t.Fatalf("state=%q confirmation=%q err=%v", state, confirmationID, err)
	}
}

func TestConfirmSignedPolicyTamperAfterStagePreventsWrite(t *testing.T) {
	store, ctx, now, resolver, provider, created, priv := confirmationFlowFixture(t, nil)
	staged := stageConfirmationPreview(t, store, ctx, now, resolver, provider, created, "policy-tamper")
	var bundle assurance.Bundle
	if err := json.Unmarshal(stageSignedBundle(t, priv), &bundle); err != nil {
		t.Fatal(err)
	}
	bundle.BundleVersion = 2
	access := &bundle.ContentAccessPolicies[0]
	access.Revision = 2
	access.ContentHash = policyFixtureHash(t, *access)
	resource := &bundle.ContentResourcePolicies[0]
	resource.Revision = 2
	resource.AccessPolicyRevision = 2
	resource.AccessPolicyContentHash = access.ContentHash
	resource.ContentHash = policyFixtureHash(t, *resource)
	bundle.DestinationProfiles[0].Revision = intptr(2)
	bundle.DestinationProfiles[0].ResourcePolicy.Revision = 2
	bundle.DestinationProfiles[0].ResourcePolicy.ContentHash = resource.ContentHash
	bundle.DestinationProfiles[0].ContentHash = ptrProfileHash(t, bundle.DestinationProfiles[0])
	raw, err := assurance.CanonicalJSON(bundle)
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	validated, err := assurance.ValidateBundle(raw, ed25519.Sign(priv, raw), testTenant, pub)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StageBundle(ctx, validated); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestActivation(ctx, "content-stage-bundle", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(ctx, testTenant, pub); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolver.ResolveContentDestinationForCapability(ctx, "content-stage-profile", 1, now, identity.PermissionContentConfirm); err == nil {
		t.Fatal("tampered active bundle still grants the frozen confirmation capability")
	}
	fence, err := NewLocalTestFence(testTenant, "wave4-test-env")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewLocalTestConfirmationService(store, AssuranceCandidateResolver{Store: store}, resolver, provider, fence, fixedStageClock{now})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Confirm(ctx, "audit-policy-tamper", ConfirmRequest{PlacementIntentID: placementIntentIDFromPreview(t, staged.Preview), ConfirmationToken: staged.ConfirmationToken, IdempotencyKey: "confirm-policy-tamper-key-001"})
	if err == nil || provider.updates != 0 {
		t.Fatalf("error=%v updates=%d", err, provider.updates)
	}
}

func stageConfirmationPreview(t *testing.T, store *assurance.Store, ctx context.Context, now time.Time, resolver *assurance.ContentPolicyResolver, provider *confirmationFlowProvider, created assurance.ContentIngestResult, suffix string) StageResponse {
	t.Helper()
	service := StageService{Store: store, Candidates: AssuranceCandidateResolver{Store: store}, Profiles: AssuranceProfileResolver{Resolver: resolver}, Provider: provider, Clock: fixedStageClock{now}}
	response, err := service.Stage(ctx, "audit-flow-stage-"+suffix, StageRequest{CandidateKind: "extracted_item", CandidateID: created.ItemIDs["text"], SourceArtifactID: created.SourceArtifactID, DestinationProfileID: "content-stage-profile", DestinationProfileRevision: 1, IdempotencyKey: "stage-confirm-flow-" + suffix + "-key-00001"})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	return response
}

func confirmationFlowFixture(t *testing.T, submitErr error, additionalPermissions ...identity.Permission) (*assurance.Store, context.Context, time.Time, *assurance.ContentPolicyResolver, *confirmationFlowProvider, assurance.ContentIngestResult, ed25519.PrivateKey) {
	store, _, ctx, now, resolver, provider, created, priv := confirmationFlowFixtureWithDB(t, submitErr, additionalPermissions...)
	return store, ctx, now, resolver, provider, created, priv
}

func confirmationFlowFixtureWithDB(t *testing.T, submitErr error, additionalPermissions ...identity.Permission) (*assurance.Store, *sql.DB, context.Context, time.Time, *assurance.ContentPolicyResolver, *confirmationFlowProvider, assurance.ContentIngestResult, ed25519.PrivateKey) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	store, err := assurance.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	store.SetAuditLog(log)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	permissions := []identity.Permission{identity.PermissionContentStage, identity.PermissionContentIngest, identity.PermissionContentConfirm}
	for _, permission := range additionalPermissions {
		if permission != identity.PermissionContentAcknowledge && permission != identity.PermissionContentReconcile {
			t.Fatalf("unsupported extra confirmation fixture capability: %q", permission)
		}
		permissions = append(permissions, permission)
	}
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: testTenant, ObjectID: testActor, TokenType: identity.TokenTypeDelegated, Permissions: permissions})
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var bundle assurance.Bundle
	if err := json.Unmarshal(stageSignedBundle(t, priv), &bundle); err != nil {
		t.Fatal(err)
	}
	capabilities := []string{"content.confirm"}
	for _, permission := range additionalPermissions {
		capabilities = append(capabilities, string(permission))
	}
	bundle.ContentAccessPolicies[0].Capabilities = append(bundle.ContentAccessPolicies[0].Capabilities, capabilities...)
	bundle.ContentAccessPolicies[0].ContentHash = policyFixtureHash(t, bundle.ContentAccessPolicies[0])
	bundle.ContentResourcePolicies[0].AccessPolicyContentHash = bundle.ContentAccessPolicies[0].ContentHash
	bundle.ContentResourcePolicies[0].ContentHash = policyFixtureHash(t, bundle.ContentResourcePolicies[0])
	bundle.DestinationProfiles[0].ResourcePolicy.ContentHash = bundle.ContentResourcePolicies[0].ContentHash
	bundle.DestinationProfiles[0].ContentHash = ptrProfileHash(t, bundle.DestinationProfiles[0])
	raw, err := assurance.CanonicalJSON(bundle)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := assurance.ValidateBundle(raw, ed25519.Sign(priv, raw), testTenant, pub)
	if err != nil {
		t.Fatalf("validate signed fixture: %v", err)
	}
	if err = store.StageBundle(ctx, validated); err != nil {
		t.Fatal(err)
	}
	if err = store.RequestActivation(ctx, "content-stage-bundle", 1); err != nil {
		t.Fatal(err)
	}
	if err = store.Bootstrap(ctx, testTenant, pub); err != nil {
		t.Fatal(err)
	}
	resolver, err := assurance.NewContentPolicyResolver(store, pub, "content-stage-access")
	if err != nil {
		t.Fatal(err)
	}
	provider := &confirmationFlowProvider{submitErr: submitErr, metadata: workivaprovider.ContentMetadata{RawValue: json.RawMessage("null"), ValuePresent: true, RawFormats: json.RawMessage(`{}`), FormatsPresent: true, EffectiveFormats: json.RawMessage(`{}`), EffectiveFormatsPresent: true, Provenance: workivaprovider.ContentMetadataProvenance{Provider: "workiva_rest", APIVersion: "2026-01-01", Endpoint: "GET /spreadsheets/{spreadsheetId}/sheets/{sheetId}/sheetdata", QueryRange: "B4", Cache: "bypassed"}, Protection: "unprotected", Writable: "writable", LiteralWriteFormatPreservation: "preserves", LiteralWriteAPIVersion: "2026-01-01", LiteralWriteEndpoint: "POST /spreadsheets/{spreadsheetId}/sheets/{sheetId}/update"}}
	provider.metadata.FormattingSHA256, _ = workivaprovider.ContentFormattingHash(provider.metadata.RawFormats, true, provider.metadata.EffectiveFormats, true)
	input := assurance.ContentIngest{SourceBytes: []byte("source fixture"), MediaType: "text/plain", MetadataBLOB: []byte(`{"origin":{"kind":"analyst"},"provenance":{"availability":"unavailable"}}`), Items: []assurance.ContentItemInput{{LocalID: "text", Text: " exact staged text\n", MetadataBLOB: []byte(`{"kind_hint":"text","interpretation":{"period":null,"currency":null,"unit":null,"scale":null,"percent_basis":null,"precision":null}}`)}}}
	digest, err := assurance.ContentIngestRequestDigest(input)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := store.Reserve(ctx, assurance.ReservationRequest{ActorID: testTenant + "/" + testActor, Tool: "workiva_content_placement", Action: "ingest", IdempotencyDigest: assurance.DigestIdempotencyKey("confirm-flow-ingest-idempotency-key-01"), RequestDigest: digest, RetainUntil: now.Add(1200 * time.Second)}, now)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.FinalizePolicyBoundContentIngest(ctx, reservation, input, digest, resolver, "audit-confirm-flow-ingest", now)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return store, db, ctx, now, resolver, provider, created, priv
}

func placementIntentIDFromPreview(t *testing.T, preview []byte) string {
	t.Helper()
	var plan PreviewPlan
	if err := json.Unmarshal(preview, &plan); err != nil || plan.PlacementIntentID == "" {
		t.Fatalf("preview has no intent id: %v", err)
	}
	return plan.PlacementIntentID
}
