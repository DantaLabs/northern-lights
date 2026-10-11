package assurance

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	_ "modernc.org/sqlite"
)

func TestContentConfirmationMigrationsPreserveV19IntentWithoutInventingEvidence(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close migration test database: %v", err)
		}
	}()
	db.SetMaxOpenConns(1)
	if err := sqlitedb.Migrate(ctx, db, "assurance", migrations[:19]); err != nil {
		t.Fatal(err)
	}
	const preview = `{"frozen":"exact bytes"}`
	const expires = "2026-10-10T12:10:00Z"
	const created = "2026-10-10T12:00:00Z"
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := db.Exec(`INSERT INTO assurance_content_placement_intents
	 (tenant_id,placement_intent_id,actor_id,preview_json,preview_sha256,token_digest,state,expires_at,created_at)
	 VALUES('tenant','intent','actor',?,?,?,'staged',?,?)`, preview, hash, hash, expires, created); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var gotPreview, gotHash, gotToken, state, gotExpiry, gotCreated string
	if err := db.QueryRow(`SELECT preview_json,preview_sha256,token_digest,state,expires_at,created_at FROM assurance_content_placement_intents WHERE tenant_id='tenant' AND placement_intent_id='intent'`).Scan(&gotPreview, &gotHash, &gotToken, &state, &gotExpiry, &gotCreated); err != nil {
		t.Fatal(err)
	}
	if gotPreview != preview || gotHash != hash || gotToken != hash || state != "staged" || gotExpiry != expires || gotCreated != created {
		t.Fatalf("v19 intent changed during upgrade: %q %q %q %q %q %q", gotPreview, gotHash, gotToken, state, gotExpiry, gotCreated)
	}
	var confirmation, machineResult, lease, nonce, target, operation, unknown, readback, readbackHash, claimFence, terminalFence, episode, classification string
	var claimVersion, rowVersion int64
	if err := db.QueryRow(`SELECT confirmation_record_id,machine_result_record_id,lease_id,owner_nonce,target_hash,operation_reference,unknown_reason,readback_json,readback_sha256,claim_fence_digest,terminal_fence_digest,uncertainty_episode_id,reconciliation_classification,claim_row_version,row_version FROM assurance_content_placement_intents WHERE tenant_id='tenant' AND placement_intent_id='intent'`).Scan(&confirmation, &machineResult, &lease, &nonce, &target, &operation, &unknown, &readback, &readbackHash, &claimFence, &terminalFence, &episode, &classification, &claimVersion, &rowVersion); err != nil {
		t.Fatal(err)
	}
	if confirmation != "" || machineResult != "" || lease != "" || nonce != "" || target != "" || operation != "" || unknown != "" || readback != "" || readbackHash != "" || claimFence != "" || terminalFence != "" || episode != "" || classification != "" || claimVersion != 0 || rowVersion != 0 {
		t.Fatalf("migration fabricated confirmation evidence: confirm=%q machine=%q lease=%q nonce=%q target=%q op=%q unknown=%q readback=%q/%q fences=%q/%q episode=%q class=%q versions=%d/%d", confirmation, machineResult, lease, nonce, target, operation, unknown, readback, readbackHash, claimFence, terminalFence, episode, classification, claimVersion, rowVersion)
	}
	var version int
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations WHERE app='assurance'`).Scan(&version); err != nil || version != 22 {
		t.Fatalf("schema marker=%d err=%v, want 22", version, err)
	}
}

func TestContentConfirmationPreviewValueMustBeLiteralScalar(t *testing.T) {
	for _, raw := range []string{`"literal"`, `true`, `0`, `-12.5`} {
		if !validConfirmationJSONScalar([]byte(raw)) {
			t.Errorf("valid scalar %s rejected", raw)
		}
	}
	for _, raw := range []string{`null`, `[]`, `{}`, `"=SUM(A1:A2)"`, `true false`} {
		if validConfirmationJSONScalar([]byte(raw)) {
			t.Errorf("invalid confirmation scalar %s accepted", raw)
		}
	}
}

func TestSignedContentConfirmationClaimRenewSubmitAndFinalize(t *testing.T) {
	store, db := openTestStore(t) // MaxOpenConns(1) catches resolver calls under a held write transaction.
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bundleJSON, signature := contentTestBundle(t, priv, "active", contentTestActor, identity.PermissionContentStage, identity.PermissionContentConfirm)
	bundle, err := ValidateBundle(bundleJSON, signature, testTenant, pub)
	if err != nil {
		t.Fatal(err)
	}
	admin := contentStageContext(contentTestActor, identity.PermissionContentStage, identity.PermissionContentConfirm)
	if err = store.StageBundle(admin, bundle); err != nil {
		t.Fatal(err)
	}
	if err = store.RequestActivation(admin, "bundle-1", 1); err != nil {
		t.Fatal(err)
	}
	if err = store.Bootstrap(admin, testTenant, pub); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewContentPolicyResolver(store, pub, "content-access")
	if err != nil {
		t.Fatal(err)
	}
	stageCtx := contentStageContext(contentTestActor, identity.PermissionContentStage)
	profile, stageBindings, err := resolver.ResolveContentDestination(stageCtx, "destination-q3", 1, now)
	if err != nil {
		t.Fatal(err)
	}
	formats := json.RawMessage(`{"number_format":"TEXT"}`)
	effective := json.RawMessage(`{"number_format":"TEXT"}`)
	formatHash, err := ContentFormattingHash(formats, true, effective, true)
	if err != nil {
		t.Fatal(err)
	}
	intentID, rawToken := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", strings.Repeat("t", 48)
	created, expires := now.Format(time.RFC3339Nano), now.Add(5*time.Minute).Format(time.RFC3339Nano)
	preview, err := CanonicalJSON(map[string]any{
		"placement_intent_id": intentID, "content_hash": strings.Repeat("c", 64), "actor_binding_id": testTenant + "/" + contentTestActor, "created_at": created, "expires_at": expires, "intended_value": "approved literal",
		"destination_profile": map[string]any{"destination_profile_id": *profile.DestinationProfileID, "revision": *profile.Revision, "content_hash": *profile.ContentHash, "resource_id": *profile.ResourceID, "sheet_id": *profile.SheetID, "cell": *profile.Cell, "intent_lifetime_seconds": stageBindings.Retention.IntentLifetimeSeconds},
		"policy_binding":      map[string]any{"active_bundle_hash": stageBindings.ActiveBundleHash, "active_bundle_version": stageBindings.ActiveBundleVersion, "access_policy_ref": map[string]any{"policy_id": stageBindings.AccessPolicy.PolicyID, "revision": stageBindings.AccessPolicy.Revision, "content_hash": stageBindings.AccessPolicy.ContentHash}},
		"provider_metadata":   map[string]any{"formatting_sha256": formatHash},
	})
	if err != nil {
		t.Fatal(err)
	}
	stageRequestDigest := HashBytes([]byte(`{"phase":"stage-confirm-fixture"}`))
	stageReservation, err := store.Reserve(stageCtx, ReservationRequest{ActorID: testTenant + "/" + contentTestActor, Tool: contentPlacementTool, Action: "stage", IdempotencyDigest: HashBytes([]byte("confirmation-stage-0001")), RequestDigest: stageRequestDigest, RetentionClass: "workflow"}, now)
	if err != nil || stageReservation.Disposition != ReservationOwned {
		t.Fatalf("reserve stage fixture: %+v %v", stageReservation, err)
	}
	_, err = store.FinalizeContentStage(stageCtx, stageReservation, stageRequestDigest, ContentStageIntent{PlacementIntentID: intentID, PreviewJSON: preview, PreviewSHA256: digestHex(HashBytes(preview)), TokenDigest: contentPlacementTokenDigest(rawToken, preview), CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute)}, "audit-confirm-stage", now)
	if err != nil {
		t.Fatal(err)
	}
	confirmCtx := contentStageContext(contentTestActor, identity.PermissionContentConfirm)
	request := ReservationRequest{ActorID: testTenant + "/" + contentTestActor, Tool: contentPlacementTool, Action: "confirm", IdempotencyDigest: HashBytes([]byte("confirmation-key-0001")), RequestDigest: HashBytes([]byte(`{"phase":"confirm"}`)), RetentionClass: "workflow", RetainUntil: now.Add(2 * time.Hour)}
	for _, principal := range []identity.Principal{
		{TenantID: "22222222-2222-4222-8222-222222222222", ObjectID: contentTestActor, TokenType: identity.TokenTypeDelegated, Permissions: []identity.Permission{identity.PermissionContentConfirm}},
		{TenantID: testTenant, ObjectID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", TokenType: identity.TokenTypeDelegated, Permissions: []identity.Permission{identity.PermissionContentConfirm}},
		{TenantID: testTenant, ObjectID: contentTestActor, TokenType: identity.TokenTypeDelegated, Permissions: []identity.Permission{identity.PermissionContentStage}},
	} {
		if _, err := store.GetOwnedContentPlacementIntent(identity.ContextWithPrincipal(context.Background(), principal), intentID); err == nil {
			t.Fatal("wrong-tenant, cross-actor, or wrong-capability intent getter succeeded")
		}
	}
	wrongTokenRequest := request
	wrongTokenRequest.IdempotencyDigest = HashBytes([]byte("confirmation-wrong-token"))
	if _, err := store.ClaimContentPlacementConfirmation(confirmCtx, resolver, wrongTokenRequest, intentID, strings.Repeat("x", 48), "cccccccc-cccc-4ccc-8ccc-cccccccccccc", "audit-confirm-wrong-token", now); err == nil {
		t.Fatal("incorrect one-time token unexpectedly claimed")
	}
	var confirmRows int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_idempotency_records WHERE tenant_id=? AND action='confirm'`, testTenant).Scan(&confirmRows); err != nil || confirmRows != 0 {
		t.Fatalf("wrong-token attempt created confirmation reservation: count=%d err=%v", confirmRows, err)
	}
	claim, err := store.ClaimContentPlacementConfirmation(confirmCtx, resolver, request, intentID, rawToken, "cccccccc-cccc-4ccc-8ccc-cccccccccccc", "audit-confirm-claim", now)
	if err != nil {
		t.Fatalf("signed confirmation claim with one DB connection: %v", err)
	}
	if claim.Record.State != "claimed" || claim.Record.TargetHash == "" || claim.Record.RowVersion != 1 || claim.Record.ConfirmationRecordID != claim.Reservation.RecordID {
		t.Fatalf("claim evidence=%+v", claim.Record)
	}
	// A different signed intent for the same exact target cannot claim the
	// active target lock, and its reservation rolls back with the claim tx.
	var secondPreviewMap map[string]any
	if err := json.Unmarshal(preview, &secondPreviewMap); err != nil {
		t.Fatal(err)
	}
	secondIntentID, secondToken := "dddddddd-dddd-4ddd-8ddd-dddddddddddd", strings.Repeat("u", 48)
	secondPreviewMap["placement_intent_id"] = secondIntentID
	secondPreviewMap["expires_at"] = now.Add(10 * time.Minute).Format(time.RFC3339Nano)
	secondPreview, err := CanonicalJSON(secondPreviewMap)
	if err != nil {
		t.Fatal(err)
	}
	secondStageRequestDigest := HashBytes([]byte(`{"phase":"stage-confirm-fixture-2"}`))
	secondStageReservation, err := store.Reserve(stageCtx, ReservationRequest{ActorID: testTenant + "/" + contentTestActor, Tool: contentPlacementTool, Action: "stage", IdempotencyDigest: HashBytes([]byte("confirmation-stage-0002")), RequestDigest: secondStageRequestDigest, RetentionClass: "workflow"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeContentStage(stageCtx, secondStageReservation, secondStageRequestDigest, ContentStageIntent{PlacementIntentID: secondIntentID, PreviewJSON: secondPreview, PreviewSHA256: digestHex(HashBytes(secondPreview)), TokenDigest: contentPlacementTokenDigest(secondToken, secondPreview), CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}, "audit-confirm-stage-2", now); err != nil {
		t.Fatal(err)
	}
	secondRequest := request
	secondRequest.IdempotencyDigest = HashBytes([]byte("confirmation-key-0002"))
	if _, err := store.ClaimContentPlacementConfirmation(confirmCtx, resolver, secondRequest, secondIntentID, secondToken, "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", "audit-confirm-busy", now); err == nil {
		t.Fatal("concurrent same-target intent unexpectedly claimed")
	}
	if err := db.QueryRow(`SELECT count(*) FROM assurance_idempotency_records WHERE tenant_id=? AND action='confirm'`, testTenant).Scan(&confirmRows); err != nil || confirmRows != 1 {
		t.Fatalf("same-target failed claim left reservation: count=%d err=%v", confirmRows, err)
	}
	version, err := store.RenewContentPlacementConfirmation(confirmCtx, intentID, claim.Reservation.RecordID, claim.Reservation.OwnerNonce, claim.LeaseID, "audit-confirm-renew", claim.Record.RowVersion, now.Add(time.Second))
	if err != nil {
		t.Fatalf("renew confirmation lease: %v", err)
	}
	var activeHash string
	if err := db.QueryRow(`SELECT content_hash FROM assurance_active_bundles WHERE tenant_id=? AND singleton=1`, testTenant).Scan(&activeHash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE assurance_active_bundles SET content_hash=? WHERE tenant_id=? AND singleton=1`, strings.Repeat("9", 64), testTenant); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkContentPlacementSubmitting(confirmCtx, intentID, claim.Reservation.RecordID, claim.Reservation.OwnerNonce, claim.LeaseID, strings.Repeat("e", 64), "audit-confirm-stale-policy", version, now.Add(2*time.Second)); err == nil {
		t.Fatal("submit fence ignored active policy rotation")
	}
	if _, err := db.Exec(`UPDATE assurance_active_bundles SET content_hash=? WHERE tenant_id=? AND singleton=1`, activeHash, testTenant); err != nil {
		t.Fatal(err)
	}
	version, err = store.MarkContentPlacementSubmitting(confirmCtx, intentID, claim.Reservation.RecordID, claim.Reservation.OwnerNonce, claim.LeaseID, strings.Repeat("e", 64), "audit-confirm-submit", version, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("mark submitting after renewal: %v", err)
	}
	for _, offset := range []time.Duration{50 * time.Second, 100 * time.Second, 150 * time.Second, 200 * time.Second, 250 * time.Second, 300 * time.Second} {
		version, err = store.RenewContentPlacementConfirmation(confirmCtx, intentID, claim.Reservation.RecordID, claim.Reservation.OwnerNonce, claim.LeaseID, "audit-confirm-poll-renew", version, now.Add(offset))
		if err != nil {
			t.Fatalf("renew through bounded operation polling at %s: %v", offset, err)
		}
	}
	version, err = store.PersistContentPlacementOperation(confirmCtx, intentID, claim.Reservation.RecordID, claim.Reservation.OwnerNonce, "op-123", "audit-confirm-operation", version, now.Add(301*time.Second))
	if err != nil {
		t.Fatalf("persist operation: %v", err)
	}
	readback, _ := CanonicalJSON(map[string]any{"spreadsheet_id": *profile.ResourceID, "sheet_id": *profile.SheetID, "locator": *profile.Cell, "raw_value": "approved literal", "value_present": true, "formats": formats, "formats_present": true, "effective_formats": effective, "effective_formats_present": true, "formatting_sha256": formatHash, "protection": "unprotected", "writable": "writable", "literal_write_format_preservation": "preserves", "literal_write_api_version": "2026-01-01", "literal_write_endpoint": "POST /spreadsheets/{spreadsheetId}/sheets/{sheetId}/update", "provenance": map[string]any{"provider": "workiva_rest", "api_version": "2026-01-01", "endpoint": "GET /spreadsheets/{spreadsheetId}/sheets/{sheetId}/sheetdata", "query_range": *profile.Cell, "cache": "bypassed"}})
	if _, err := db.Exec(`CREATE TRIGGER fail_content_confirm_seal BEFORE UPDATE OF state ON assurance_idempotency_records WHEN NEW.state='sealed' AND NEW.action='confirm' BEGIN SELECT RAISE(ABORT,'injected confirmation seal failure'); END`); err != nil {
		t.Fatal(err)
	}
	terminalRequest := ContentPlacementReadback{PlacementIntentID: intentID, RecordID: claim.Reservation.RecordID, OwnerNonce: claim.Reservation.OwnerNonce, ExpectedVersion: version, Outcome: "accepted", OperationReference: "op-123", ReadbackJSON: readback, ReadbackSHA256: digestHex(HashBytes(readback)), TerminalFenceDigest: strings.Repeat("f", 64)}
	if _, err := store.FinalizeContentPlacementReadback(confirmCtx, terminalRequest, "audit-confirm-terminal-fault", now.Add(302*time.Second)); err == nil {
		t.Fatal("injected terminal seal failure was ignored")
	}
	var pendingState, pendingReadback string
	if err := db.QueryRow(`SELECT state,readback_json FROM assurance_content_placement_intents WHERE tenant_id=? AND placement_intent_id=?`, testTenant, intentID).Scan(&pendingState, &pendingReadback); err != nil || pendingState != "submitting" || pendingReadback != "" {
		t.Fatalf("terminal failure partially committed intent state=%s readback=%q err=%v", pendingState, pendingReadback, err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_content_confirm_seal`); err != nil {
		t.Fatal(err)
	}
	result, err := store.FinalizeContentPlacementReadback(confirmCtx, terminalRequest, "audit-confirm-terminal", now.Add(302*time.Second))
	if err != nil {
		t.Fatalf("finalize signed typed readback: %v", err)
	}
	var state, operation, machineRecord, responseHash, envelope string
	if err := db.QueryRow(`SELECT state,operation_reference,machine_result_record_id FROM assurance_content_placement_intents WHERE tenant_id=? AND placement_intent_id=?`, testTenant, intentID).Scan(&state, &operation, &machineRecord); err != nil || state != "machine_verified_visual_ack_pending" || operation != "op-123" || machineRecord != claim.Reservation.RecordID {
		t.Fatalf("terminal state=%s operation=%s machine_record=%s err=%v", state, operation, machineRecord, err)
	}
	if err := db.QueryRow(`SELECT response_hash,response_envelope FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=?`, testTenant, claim.Reservation.RecordID).Scan(&responseHash, &envelope); err != nil || result.ResultSHA256 != responseHash || digestHex(HashBytes([]byte(envelope))) != responseHash || strings.Contains(envelope, "result_sha256") {
		t.Fatalf("terminal sealed hash mismatch: result=%s stored=%s envelope=%s err=%v", result.ResultSHA256, responseHash, envelope, err)
	}
	replayed, err := store.ClaimContentPlacementConfirmation(confirmCtx, nil, request, intentID, "", "", "", now.Add(303*time.Second))
	if err != nil || !replayed.Replay || digestHex(HashBytes(replayed.Reservation.Envelope)) != responseHash {
		t.Fatalf("sealed replay was not classified before dependencies: replay=%+v err=%v", replayed, err)
	}
	// Once the first target is terminal, the other staged intent may claim it.
	// An audit outage while freezing a post-submit uncertainty must leave the
	// durable submitting lock intact and close process readiness.
	secondRequest.RetainUntil = now.Add(303 * time.Second).Add(2 * time.Hour)
	secondClaim, err := store.ClaimContentPlacementConfirmation(confirmCtx, resolver, secondRequest, secondIntentID, secondToken, "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", "audit-confirm-second-claim", now.Add(303*time.Second))
	if err != nil {
		t.Fatalf("claim released target for next intent: %v", err)
	}
	secondVersion, err := store.MarkContentPlacementSubmitting(confirmCtx, secondIntentID, secondClaim.Reservation.RecordID, secondClaim.Reservation.OwnerNonce, secondClaim.LeaseID, strings.Repeat("a", 64), "audit-confirm-second-submit", secondClaim.Record.RowVersion, now.Add(304*time.Second))
	if err != nil {
		t.Fatalf("mark second target submitting: %v", err)
	}
	richAudit := store.auditLog
	store.SetAuditLog(nil)
	freezeErr := store.FreezeContentPlacementUnknown(confirmCtx, secondIntentID, secondClaim.Reservation.RecordID, secondClaim.Reservation.OwnerNonce, "provider_outcome_unknown", "audit-confirm-freeze-fail", secondVersion, now.Add(370*time.Second))
	store.SetAuditLog(richAudit)
	if freezeErr == nil {
		t.Fatal("freeze without required rich audit unexpectedly succeeded")
	}
	if err := store.Ready(); err == nil {
		t.Fatal("audit failure while freezing did not block readiness")
	}
	var secondState, secondTarget string
	if err := db.QueryRow(`SELECT state,target_hash FROM assurance_content_placement_intents WHERE tenant_id=? AND placement_intent_id=?`, testTenant, secondIntentID).Scan(&secondState, &secondTarget); err != nil || secondState != "submitting" || secondTarget == "" {
		t.Fatalf("audit outage lost non-repeat target lock: state=%s target=%q err=%v", secondState, secondTarget, err)
	}
}
