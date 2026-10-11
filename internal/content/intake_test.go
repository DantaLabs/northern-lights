package content

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	_ "modernc.org/sqlite"
)

const (
	intakeTestTenant = "11111111-1111-4111-8111-111111111111"
	intakeTestActor  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

func TestIntakeUsesSignedPolicyAndReplaySurvivesRetirementAndAuditOutage(t *testing.T) {
	store, db, resolver, publicKey, privateKey := openSignedIntakeStore(t, 86400, 43200, 7200, true)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	ctx := intakeContext()
	service, err := NewIntakeService(store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	input := intakeInput()
	key := assurance.DigestIdempotencyKey("signed-policy-intake-key")
	created, err := service.Ingest(ctx, input, key, "audit-intake-policy", now)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	wantSourceExpiry, wantDraftExpiry := now.Add(24*time.Hour), now.Add(12*time.Hour)
	var sourceExpiry string
	var metadata []byte
	if err := db.QueryRow(`SELECT expires_at,metadata_blob FROM assurance_content_sources WHERE tenant_id=? AND source_artifact_id=?`, intakeTestTenant, created.SourceArtifactID).Scan(&sourceExpiry, &metadata); err != nil {
		t.Fatal(err)
	}
	if sourceExpiry != wantSourceExpiry.Format(time.RFC3339Nano) {
		t.Fatalf("source expiry=%q want %q", sourceExpiry, wantSourceExpiry.Format(time.RFC3339Nano))
	}
	var idempotencyExpiry string
	if err := db.QueryRow(`SELECT expires_at FROM assurance_idempotency_records WHERE tenant_id=? AND action='ingest'`, intakeTestTenant).Scan(&idempotencyExpiry); err != nil {
		t.Fatal(err)
	}
	if idempotencyExpiry != now.Add(2*time.Hour).Format(time.RFC3339Nano) {
		t.Fatalf("idempotency expiry=%q want signed policy horizon %q", idempotencyExpiry, now.Add(2*time.Hour).Format(time.RFC3339Nano))
	}
	var sourceMetadata map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &sourceMetadata); err != nil {
		t.Fatal(err)
	}
	var provenance struct {
		AccessPolicy struct {
			PolicyID    string `json:"policy_id"`
			ContentHash string `json:"content_hash"`
			ValidUntil  string `json:"valid_until"`
			Revision    int    `json:"revision"`
		} `json:"access_policy"`
		RetentionPolicy struct {
			PolicyID    string `json:"policy_id"`
			ContentHash string `json:"content_hash"`
			Revision    int    `json:"revision"`
		} `json:"retention_policy"`
		SourceExpiresAt string `json:"source_expires_at"`
		DraftExpiresAt  string `json:"draft_expires_at"`
	}
	if err := json.Unmarshal(sourceMetadata["intake_policy"], &provenance); err != nil {
		t.Fatal(err)
	}
	if provenance.AccessPolicy.PolicyID != "intake-access" || provenance.AccessPolicy.Revision != 1 || provenance.AccessPolicy.ContentHash == "" || provenance.AccessPolicy.ValidUntil != "2027-01-01T00:00:00Z" || provenance.RetentionPolicy.PolicyID != "intake-retention" || provenance.RetentionPolicy.Revision != 1 || provenance.RetentionPolicy.ContentHash == "" || provenance.SourceExpiresAt != wantSourceExpiry.Format(time.RFC3339Nano) || provenance.DraftExpiresAt != wantDraftExpiry.Format(time.RFC3339Nano) {
		t.Fatalf("source lacks signed policy references/expiries: %+v", provenance)
	}
	candidateCtx := identity.ContextWithPrincipal(ctx, mustPrincipal(t, ctx, identity.PermissionContentStage))
	draft, err := store.GetContentCandidateAt(candidateCtx, assurance.ContentCandidateDraftArtifact, created.DraftIDs["summary"], now)
	if err != nil {
		t.Fatalf("read signed-retention draft: %v", err)
	}
	if !draft.CreatedAt.Equal(now) || !draft.ExpiresAt.Equal(wantDraftExpiry) || len(draft.SourceMetadataBLOB) == 0 {
		t.Fatalf("candidate timestamps or source metadata missing: %+v", draft)
	}
	draft.SourceMetadataBLOB[0] ^= 0xff
	draftAgain, err := store.GetContentCandidateAt(candidateCtx, assurance.ContentCandidateDraftArtifact, created.DraftIDs["summary"], now)
	if err != nil || len(draftAgain.SourceMetadataBLOB) == 0 || draftAgain.SourceMetadataBLOB[0] == draft.SourceMetadataBLOB[0] {
		t.Fatalf("candidate source metadata was not defensively copied: err=%v", err)
	}
	if _, err := store.GetContentCandidateAt(candidateCtx, assurance.ContentCandidateDraftArtifact, created.DraftIDs["summary"], wantDraftExpiry); err == nil {
		t.Fatal("draft remained available at its signed draft expiry")
	}

	// Activate a genuine newly signed retired access policy, then remove the
	// execution-only audit dependency. A sealed replay must use neither.
	activateSignedIntakeBundle(t, store, ctx, publicKey, privateKey, 2, "retired", 86400, 43200, 7200, true)
	store.SetAuditLog(nil)
	replayed, err := service.Ingest(ctx, input, key, "audit-intake-replay", now.Add(time.Minute))
	if err != nil || replayed.SourceArtifactID != created.SourceArtifactID || replayed.SourceSHA256 != created.SourceSHA256 {
		t.Fatalf("replay after policy retirement/audit outage=%+v err=%v", replayed, err)
	}
	var sourceCount int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_content_sources WHERE tenant_id=?`, intakeTestTenant).Scan(&sourceCount); err != nil || sourceCount != 1 {
		t.Fatalf("replay created extra source rows=%d err=%v", sourceCount, err)
	}
}

func TestIntakeRejectsCallerAuthorityAndAuthenticatesBeforeLookup(t *testing.T) {
	store, _, resolver, _, _ := openSignedIntakeStore(t, 86400, 43200, 7200, true)
	service, err := NewIntakeService(store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		mutate func(*assurance.ContentIngest)
	}{
		{"source", func(i *assurance.ContentIngest) {
			i.MetadataBLOB = []byte(`{"intake_policy":{"access_policy":{"policy_id":"forged"}}}`)
		}},
		{"item", func(i *assurance.ContentIngest) { i.Items[0].MetadataBLOB = []byte(`{"intake_policy":null}`) }},
		{"draft", func(i *assurance.ContentIngest) { i.Drafts[0].MetadataBLOB = []byte(`{"intake_policy":false}`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := intakeInput()
			tc.mutate(&input)
			_, err := service.Ingest(intakeContext(), input, assurance.DigestIdempotencyKey("forged-"+tc.name), "audit-forged", now)
			if err == nil || !strings.Contains(err.Error(), "server-owned") {
				t.Fatalf("forged intake policy metadata accepted: %v", err)
			}
		})
	}
	unauthorized := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: intakeTestTenant, ObjectID: intakeTestActor, TokenType: identity.TokenTypeApplication, Permissions: []identity.Permission{identity.PermissionContentIngest}})
	if _, err := service.Ingest(unauthorized, intakeInput(), assurance.DigestIdempotencyKey("unauthorized"), "audit-unauthorized", now); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("application principal passed authorization: %v", err)
	}
}

func TestIntakeReplayClassificationPrecedesPolicyResolutionAndFreshAuditRequirement(t *testing.T) {
	store, db, resolver, publicKey, privateKey := openSignedIntakeStore(t, 86400, 43200, 7200, true)
	service, err := NewIntakeService(store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	ctx := intakeContext()
	input := intakeInput()
	// Bootstrap expires leases against wall time. Use a fixed far-future
	// reservation and assert its exact lease/state after activation so the test
	// does not depend on suite timing or a wall-clock expiry race.
	now := time.Date(2099, 10, 10, 12, 0, 0, 0, time.UTC)
	digest, err := assurance.ContentIngestRequestDigest(input)
	if err != nil {
		t.Fatal(err)
	}
	key := assurance.DigestIdempotencyKey("in-progress-intake-key")
	_, err = store.Reserve(ctx, assurance.ReservationRequest{ActorID: intakeTestTenant + "/" + intakeTestActor, Tool: contentIngestTool, Action: contentIngestAction,
		IdempotencyDigest: key, RequestDigest: digest, RetainUntil: now.Add(2 * time.Hour)}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateSignedIntakeBundle(t, store, ctx, publicKey, privateKey, 2, "retired", 86400, 43200, 7200, true)
	var state, leaseExpiry string
	if err := db.QueryRow(`SELECT state,lease_expires_at FROM assurance_idempotency_records WHERE tenant_id=? AND action=? AND idempotency_digest=?`, intakeTestTenant, contentIngestAction, hex.EncodeToString(key[:])).Scan(&state, &leaseExpiry); err != nil {
		t.Fatal(err)
	}
	lease, err := time.Parse(time.RFC3339Nano, leaseExpiry)
	if err != nil || state != string(assurance.ReservationStateReserved) || !lease.Equal(now.Add(time.Minute)) {
		t.Fatalf("policy activation did not preserve an in-progress lease: state=%q lease=%q err=%v", state, leaseExpiry, err)
	}
	store.SetAuditLog(nil)
	if _, err := service.Ingest(ctx, input, key, "audit-in-progress", now.Add(time.Second)); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("in-progress replay did not classify before inactive policy/audit: %v", err)
	}
}

func TestPublicIntakeBindsInternallyComputedCanonicalRequestDigest(t *testing.T) {
	store, db, resolver, _, _ := openSignedIntakeStore(t, 86400, 43200, 7200, true)
	service, err := NewIntakeService(store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	ctx := intakeContext()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	raw := []byte(`{"phase":"ingest","idempotency_key":"public-canonical-digest-0001","source":{"source_text":"Reviewed source text.","media_type":"text/plain"},"origin":{"kind":"analyst"},"extracted_items":[],"draft_artifacts":[]}`)
	projection, err := DecodeContentIngest(raw)
	if err != nil {
		t.Fatal(err)
	}
	privateDigest, err := assurance.ContentIngestRequestDigest(projection.Input)
	if err != nil {
		t.Fatal(err)
	}
	if privateDigest == projection.RequestDigest {
		t.Fatal("fixture must distinguish public canonical request digest from private persistence shape")
	}
	result, err := service.IngestPublic(ctx, raw, "audit-public-intake", now)
	if err != nil {
		t.Fatalf("IngestPublic: %v", err)
	}
	var storedDigest string
	if err := db.QueryRow(`SELECT request_digest FROM assurance_idempotency_records WHERE tenant_id=? AND action='ingest'`, intakeTestTenant).Scan(&storedDigest); err != nil {
		t.Fatal(err)
	}
	if storedDigest != hex.EncodeToString(projection.RequestDigest[:]) {
		t.Fatalf("reservation digest=%q, want internally computed public digest %x", storedDigest, projection.RequestDigest)
	}
	prior, found, err := store.LookupReservation(ctx, assurance.ReservationRequest{ActorID: intakeTestTenant + "/" + intakeTestActor, Tool: contentIngestTool, Action: contentIngestAction,
		IdempotencyDigest: projection.IdempotencyDigest, RequestDigest: projection.RequestDigest})
	if err != nil || !found || prior.Disposition != assurance.ReservationReplay {
		t.Fatalf("public replay lookup=%#v found=%v err=%v", prior, found, err)
	}
	replay, err := contentIngestReplay(prior)
	if err != nil || replay.SourceArtifactID != result.SourceArtifactID {
		t.Fatalf("public replay=%#v err=%v, initial=%#v", replay, err, result)
	}
}

func TestIntakeAuditUnavailableLeavesNoArtifactsAndNoFalseSeal(t *testing.T) {
	store, db, resolver, _, _ := openSignedIntakeStore(t, 86400, 43200, 7200, true)
	store.SetAuditLog(nil)
	service, err := NewIntakeService(store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	ctx := intakeContext()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	_, err = service.Ingest(ctx, intakeInput(), assurance.DigestIdempotencyKey("audit-outage-intake"), "audit-intake-outage", now)
	if err == nil || !strings.Contains(err.Error(), "audit_unavailable") {
		t.Fatalf("intake without audit dependency returned err=%v", err)
	}
	for _, table := range []string{"assurance_content_sources", "assurance_content_items", "assurance_content_drafts"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE tenant_id=?`, intakeTestTenant).Scan(&count); err != nil || count != 0 {
			t.Fatalf("audit-unavailable intake wrote %d rows in %s err=%v", count, table, err)
		}
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM assurance_idempotency_records WHERE tenant_id=? AND action='ingest'`, intakeTestTenant).Scan(&state); err != nil || state != string(assurance.ReservationStateReserved) {
		t.Fatalf("audit-unavailable intake reservation state=%q err=%v", state, err)
	}
}

func TestIntakeReservationFailureCreatesNoContentRows(t *testing.T) {
	store, db, resolver, _, _ := openSignedIntakeStore(t, 86400, 43200, 7200, true)
	if _, err := db.Exec(`CREATE TRIGGER fail_content_ingest_reservation BEFORE INSERT ON assurance_idempotency_records
	 WHEN NEW.action='ingest' BEGIN SELECT RAISE(ABORT,'injected reservation failure'); END`); err != nil {
		t.Fatal(err)
	}
	service, err := NewIntakeService(store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Ingest(intakeContext(), intakeInput(), assurance.DigestIdempotencyKey("reserve-fault"), "audit-reserve-fault", time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("injected reservation failure unexpectedly succeeded")
	}
	for _, table := range []string{"assurance_content_sources", "assurance_content_items", "assurance_content_drafts"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE tenant_id=?`, intakeTestTenant).Scan(&count); err != nil || count != 0 {
			t.Fatalf("reservation failure wrote %d rows in %s err=%v", count, table, err)
		}
	}
}

func TestIntakeMissingSignedRetentionPolicyDoesNotReserve(t *testing.T) {
	store, db, resolver, publicKey, privateKey := openSignedIntakeStore(t, 86400, 43200, 7200, true)
	ctx := intakeContext()
	activateSignedIntakeBundle(t, store, ctx, publicKey, privateKey, 2, "active", 86400, 43200, 7200, false)
	service, err := NewIntakeService(store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Ingest(ctx, intakeInput(), assurance.DigestIdempotencyKey("missing-signed-retention"), "audit-no-retention", time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "content_policy_missing") {
		t.Fatalf("missing signed retention policy result err=%v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_idempotency_records WHERE tenant_id=? AND action='ingest'`, intakeTestTenant).Scan(&count); err != nil || count != 0 {
		t.Fatalf("missing policy created %d reservations err=%v", count, err)
	}
}

func openSignedIntakeStore(t *testing.T, sourceSeconds, draftSeconds, idempotencySeconds int64, accessActive bool) (*assurance.Store, *sql.DB, *assurance.ContentPolicyResolver, ed25519.PublicKey, ed25519.PrivateKey) {
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
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ctx := intakeContext()
	state := "active"
	if !accessActive {
		state = "retired"
	}
	activateSignedIntakeBundle(t, store, ctx, publicKey, privateKey, 1, state, sourceSeconds, draftSeconds, idempotencySeconds, true)
	resolver, err := assurance.NewContentPolicyResolver(store, publicKey, "intake-access")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return store, db, resolver, publicKey, privateKey
}

func activateSignedIntakeBundle(t *testing.T, store *assurance.Store, ctx context.Context, publicKey ed25519.PublicKey, privateKey ed25519.PrivateKey, version int, accessState string, sourceSeconds, draftSeconds, idempotencySeconds int64, includeRetention bool) {
	t.Helper()
	access := assurance.ContentAccessPolicy{PolicyID: "intake-access", Revision: version, State: accessState, TenantID: intakeTestTenant, ActorObjectID: intakeTestActor,
		Capabilities: []string{"content.ingest"}, ResourceIDs: []string{"resource-intake"}, ValidUntil: "2027-01-01T00:00:00Z"}
	retention := assurance.ContentRetentionPolicy{PolicyID: "intake-retention", Revision: version, State: "active", SourceLifetimeSeconds: sourceSeconds,
		DraftLifetimeSeconds: draftSeconds, IntentLifetimeSeconds: 300, IdempotencyLifetimeSeconds: idempotencySeconds, RecoveryLifetimeSeconds: 600}
	bundle := assurance.Bundle{
		SchemaVersion: 1, BundleID: "signed-intake-policy", BundleVersion: version, TenantID: intakeTestTenant,
		Reports: []assurance.ReportRevision{{ReportID: "intake-fixture", Revision: version, Name: "Intake fixture", Owner: "assurance", Status: "active", RetentionClass: "standard",
			ResourcePolicyHash: strings.Repeat("a", 64), Periods: []assurance.Period{{Key: "2026-Q4", Label: "Q4 2026", Start: "2026-10-01", End: "2026-12-31"}},
			Fields: []assurance.FieldDefinition{{FieldID: "intake-value", ResourceID: "resource-intake", ExternalResourceID: "external-intake", SubresourceID: "sheet-intake", Locator: "A1", Kind: assurance.ValueText, Required: true, Order: 1}}}},
		ContentAccessPolicies: []assurance.ContentAccessPolicy{access},
	}
	if includeRetention {
		bundle.ContentRetentionPolicies = []assurance.ContentRetentionPolicy{retention}
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := assurance.CanonicalJSONBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := assurance.ValidateBundle(canonical, ed25519.Sign(privateKey, canonical), intakeTestTenant, publicKey)
	if err != nil {
		t.Fatalf("validate signed intake policy bundle: %v", err)
	}
	if err := store.StageBundle(ctx, validated); err != nil {
		t.Fatalf("stage signed intake policy bundle: %v", err)
	}
	if err := store.RequestActivation(ctx, bundle.BundleID, bundle.BundleVersion); err != nil {
		t.Fatalf("request signed intake policy activation: %v", err)
	}
	if err := store.Bootstrap(ctx, intakeTestTenant, publicKey); err != nil {
		t.Fatalf("activate signed intake policy bundle: %v", err)
	}
}

func intakeContext() context.Context {
	return identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: intakeTestTenant, ObjectID: intakeTestActor, TokenType: identity.TokenTypeDelegated,
		Permissions: []identity.Permission{identity.PermissionContentIngest}})
}

func mustPrincipal(t *testing.T, ctx context.Context, permission identity.Permission) identity.Principal {
	t.Helper()
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok {
		t.Fatal("test principal missing")
	}
	principal.Permissions = append(principal.Permissions, permission)
	return principal
}

func intakeInput() assurance.ContentIngest {
	return assurance.ContentIngest{SourceBytes: []byte("A reviewed source value 42."), MediaType: "text/plain", Filename: "source.txt",
		MetadataBLOB: []byte(`{"origin":"trusted-upload"}`), Items: []assurance.ContentItemInput{{LocalID: "value", Text: "42", MetadataBLOB: []byte(`{"kind":"number"}`)}},
		Drafts: []assurance.ContentDraftInput{{LocalID: "summary", Text: "Value: 42", ItemLocalIDs: []string{"value"}, MetadataBLOB: []byte(`{"kind":"draft"}`)}}}
}

func TestIntakeRejectsActiveBundleRotationInsidePersistenceTransaction(t *testing.T) {
	store, db, resolver, _, _ := openSignedIntakeStore(t, 86400, 43200, 7200, true)
	if _, err := db.Exec(`CREATE TRIGGER rotate_bundle_during_intake AFTER INSERT ON assurance_content_sources
		BEGIN UPDATE assurance_active_bundles SET content_hash=lower(hex(randomblob(32))),bundle_version=bundle_version+1 WHERE tenant_id='` + intakeTestTenant + `' AND singleton=1; END`); err != nil {
		t.Fatal(err)
	}
	service, err := NewIntakeService(store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Ingest(intakeContext(), intakeInput(), assurance.DigestIdempotencyKey("bundle-rotation"), "audit-bundle-rotation", time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "active signed policy bundle changed") {
		t.Fatalf("active bundle rotation was not rejected: %v", err)
	}
	for _, table := range []string{"assurance_content_sources", "assurance_content_items", "assurance_content_drafts", "assurance_audit_entries", "assurance_content_audit_links"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE tenant_id=?`, intakeTestTenant).Scan(&count); err == nil && count != 0 {
			t.Fatalf("bundle rotation committed %d rows in %s", count, table)
		}
	}
}

func TestIntakeRejectsReservationHorizonDifferentFromSignedLifetime(t *testing.T) {
	store, db, resolver, _, _ := openSignedIntakeStore(t, 86400, 43200, 7200, true)
	ctx := intakeContext()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	input := intakeInput()
	digest, err := assurance.ContentIngestRequestDigest(input)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := store.Reserve(ctx, assurance.ReservationRequest{ActorID: intakeTestTenant + "/" + intakeTestActor, Tool: contentIngestTool, Action: contentIngestAction,
		IdempotencyDigest: assurance.DigestIdempotencyKey("wrong-horizon"), RequestDigest: digest, RetainUntil: now.Add(3 * time.Hour)}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.FinalizePolicyBoundContentIngest(ctx, reservation, input, digest, resolver, "audit-wrong-horizon", now)
	if err == nil || !strings.Contains(err.Error(), "reservation horizon does not match") {
		t.Fatalf("mismatched signed retention horizon accepted: %v", err)
	}
	var sourceCount int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_content_sources WHERE tenant_id=?`, intakeTestTenant).Scan(&sourceCount); err != nil || sourceCount != 0 {
		t.Fatalf("wrong-horizon finalize wrote %d source rows, err=%v", sourceCount, err)
	}
}

func TestPolicyBoundReingestRejectsLegacySourceWithoutServerProvenance(t *testing.T) {
	store, db, resolver, _, _ := openSignedIntakeStore(t, 86400, 43200, 7200, true)
	ctx := intakeContext()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	legacyInput := intakeInput()
	legacyDigest, err := assurance.ContentIngestRequestDigest(legacyInput)
	if err != nil {
		t.Fatal(err)
	}
	legacyReservation, err := store.Reserve(ctx, assurance.ReservationRequest{ActorID: intakeTestTenant + "/" + intakeTestActor, Tool: contentIngestTool, Action: contentIngestAction,
		IdempotencyDigest: assurance.DigestIdempotencyKey("legacy-source"), RequestDigest: legacyDigest, RetainUntil: now.Add(2 * time.Hour)}, now)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := store.FinalizeContentIngest(ctx, legacyReservation, legacyInput, now.Add(24*time.Hour), "audit-legacy-source", now)
	if err != nil {
		t.Fatal(err)
	}
	reingest := assurance.ContentIngest{SourceBytes: append([]byte(nil), legacyInput.SourceBytes...), MediaType: legacyInput.MediaType,
		ExistingSourceArtifactID: legacy.SourceArtifactID, ExpectedExistingSourceSHA256: legacy.SourceSHA256,
		Items: []assurance.ContentItemInput{{LocalID: "new", Text: "new item", MetadataBLOB: []byte(`{}`)}}}
	// Existing source provenance must have been created by the policy-bound path.
	service, err := NewIntakeService(store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Ingest(ctx, reingest, assurance.DigestIdempotencyKey("legacy-reingest"), "audit-legacy-reingest", now.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "lacks verified server intake policy provenance") {
		t.Fatalf("legacy source reingest was accepted: %v", err)
	}
	var itemCount int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_content_items WHERE tenant_id=?`, intakeTestTenant).Scan(&itemCount); err != nil || itemCount != 1 {
		t.Fatalf("legacy reingest changed item count to %d, err=%v", itemCount, err)
	}
}
