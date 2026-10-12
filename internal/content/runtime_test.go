package content

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
	_ "modernc.org/sqlite"
)

func TestRuntimeRequiresEncryptedStorageAndSameDatabaseAudit(t *testing.T) {
	store, auditLog, resolver, db := runtimeTestDependencies(t)
	_ = db
	if _, err := NewRuntime(RuntimeConfig{Store: store, Resolver: resolver, Audit: auditLog, Gate: assurance.NewDrainGate()}); err != nil {
		t.Fatalf("valid intake-only runtime: %v", err)
	}

	otherDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := otherDB.Close(); err != nil {
			t.Errorf("close foreign audit db: %v", err)
		}
	}()
	foreignAudit, err := audit.NewWithDB(otherDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewRuntime(RuntimeConfig{Store: store, Resolver: resolver, Audit: foreignAudit, Gate: assurance.NewDrainGate()}); err == nil {
		t.Fatal("runtime accepted an audit log on a different SQLite handle")
	}

	unencrypted, unencryptedDB, _, _, _, _, unencryptedResolver := stageIntegrationFixture(t)
	unencryptedAudit, err := audit.NewWithDB(unencryptedDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewRuntime(RuntimeConfig{Store: unencrypted, Resolver: unencryptedResolver, Audit: unencryptedAudit, Gate: assurance.NewDrainGate()}); err == nil {
		t.Fatal("runtime accepted unencrypted source storage")
	}
}

func TestRuntimeRejectsUnreadmittedStoreAtConstruction(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close unreadmitted db: %v", err)
		}
	}()
	store, err := assurance.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := assurance.NewContentPolicyResolver(store, pub, "unreadmitted-access")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetContentStorageKey("unreadmitted-test", make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRuntime(RuntimeConfig{Store: store, Resolver: resolver, Audit: log, Gate: assurance.NewDrainGate()}); err == nil {
		t.Fatal("runtime admitted a store before signed-bundle startup admission")
	}
}

func TestRuntimeConfirmationAlwaysFailsClosedWithoutAdmissionCapability(t *testing.T) {
	principal := identity.Principal{TenantID: "11111111-1111-4111-8111-111111111111", ObjectID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", TokenType: identity.TokenTypeDelegated, Permissions: []identity.Permission{identity.PermissionContentConfirm}}
	ctx := identity.ContextWithPrincipal(context.Background(), principal)
	store, auditLog, resolver, db := runtimeTestDependencies(t)
	_ = db
	runtime, err := NewRuntime(RuntimeConfig{Store: store, Resolver: resolver, Audit: auditLog, Gate: assurance.NewDrainGate(), Fence: runtimeDurableFence{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Confirm(ctx, "audit-confirm-runtime", ConfirmRequest{PlacementIntentID: "intent-1", IdempotencyKey: "confirm-runtime-key-001"})
	var typed *assurance.Error
	if !errors.As(err, &typed) || typed.Code != "capability_unverified" {
		t.Fatalf("confirmation error=%v, want capability_unverified", err)
	}
}

func TestStageInvocationAuditIDComesFromItsReservation(t *testing.T) {
	store, db, ctx, now, profileID, _, resolver := stageIntegrationFixture(t)
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetContentStorageKey("runtime-stage-test", make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(RuntimeConfig{Store: store, Resolver: resolver, Provider: runtimeStageBackend{verifiedStageProvider: &verifiedStageProvider{}}, Audit: log, Gate: assurance.NewDrainGate()})
	if err != nil {
		t.Fatal(err)
	}
	runtime.stage.Clock = fixedStageClock{now}

	input := assurance.ContentIngest{SourceBytes: []byte("stage audit binding"), MediaType: "text/plain", MetadataBLOB: []byte(`{"origin":{"kind":"analyst"},"provenance":{"availability":"unavailable"}}`), Items: []assurance.ContentItemInput{{LocalID: "text", Text: "stage audit binding", MetadataBLOB: []byte(`{"kind_hint":"text"}`)}}}
	digest, err := assurance.ContentIngestRequestDigest(input)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := store.Reserve(ctx, assurance.ReservationRequest{ActorID: testTenant + "/" + testActor, Tool: stageTool, Action: "ingest", IdempotencyDigest: assurance.DigestIdempotencyKey("runtime-stage-source-key-001"), RequestDigest: digest, RetainUntil: now.Add(1200 * time.Second)}, now)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.FinalizePolicyBoundContentIngest(ctx, reservation, input, digest, resolver, "stage-source-audit", now)
	if err != nil {
		t.Fatal(err)
	}
	request := StageRequest{CandidateKind: "extracted_item", CandidateID: created.ItemIDs["text"], SourceArtifactID: created.SourceArtifactID, DestinationProfileID: profileID, DestinationProfileRevision: 1, IdempotencyKey: "runtime-stage-invocation-key-001"}
	first, invocation, err := runtime.StageWithInvocation(ctx, "stage-audit-original", request)
	if err != nil || invocation.AuditID != "stage-audit-original" || invocation.ReplayDisposition != "fresh" {
		t.Fatalf("first invocation=%+v err=%v", invocation, err)
	}
	var preview struct {
		PlacementIntentID string `json:"placement_intent_id"`
	}
	if err := json.Unmarshal(first.Preview, &preview); err != nil {
		t.Fatal(err)
	}
	// A later intent audit link whose fractional timestamp sorts before the
	// reservation link must not steal the stage invocation's public audit ID.
	if _, err := db.Exec(`INSERT INTO assurance_audit_links (tenant_id,link_id,entity_kind,entity_id,audit_id,created_at) VALUES (?,?,?,?,?,?)`, testTenant, "later-intent-link", "content_placement_intent", preview.PlacementIntentID, "later-confirmation-audit", "2026-10-10T12:00:00.1Z"); err != nil {
		t.Fatal(err)
	}
	_, replay, err := runtime.StageWithInvocation(ctx, "stage-audit-retry", request)
	if err != nil || replay.AuditID != "stage-audit-original" || replay.ReplayDisposition != "sealed_replay" {
		t.Fatalf("replay invocation=%+v err=%v", replay, err)
	}
}

type runtimeStageBackend struct {
	workivaprovider.Backend
	*verifiedStageProvider
}

type runtimeDurableFence struct{}

func (runtimeDurableFence) Identity(string) (string, string, error) { return "env", "tenant", nil }
func (runtimeDurableFence) Durable() bool                           { return true }
func (runtimeDurableFence) CreateClaim(context.Context, ContentClaim) (string, error) {
	panic("must not be called")
}
func (runtimeDurableFence) VerifyClaim(context.Context, ContentClaim, string) error {
	panic("must not be called")
}
func (runtimeDurableFence) CreateTerminal(context.Context, ContentClaim, ContentTerminal) (string, error) {
	panic("must not be called")
}
func (runtimeDurableFence) VerifyTerminal(context.Context, ContentClaim, ContentTerminal, string) error {
	panic("must not be called")
}
func (runtimeDurableFence) ReadVerifiedTerminal(context.Context, ContentClaim, string) (ContentTerminal, string, error) {
	panic("must not be called")
}
func (runtimeDurableFence) CaptureHighWater(context.Context, string) (ContentFenceHighWater, error) {
	panic("must not be called")
}
func (runtimeDurableFence) Scan(context.Context, ContentFenceScanRequest) ([]ContentFenceObject, error) {
	panic("must not be called")
}

func runtimeTestDependencies(t *testing.T) (*assurance.Store, *audit.Log, *assurance.ContentPolicyResolver, *sql.DB) {
	t.Helper()
	store, db, _, _, _, _, resolver := stageIntegrationFixture(t)
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetContentStorageKey("runtime-test-key", make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	return store, log, resolver, db
}
