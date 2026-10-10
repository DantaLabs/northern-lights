package assurance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
)

const (
	contentStageTestTenant = "11111111-1111-4111-8111-111111111111"
	contentStageTestActor  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

func TestFinalizeContentStageAtomicallyPersistsPrivatePreviewAuditAndTokenDigest(t *testing.T) {
	store, db := openTestStore(t)
	ctx := contentPlacementStageContext()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	requestDigest, reservation := reserveContentStage(t, store, ctx, "content-stage-key-000001", now)
	intent, rawToken := stageIntentFixture(t, now)

	result, err := store.FinalizeContentStage(ctx, reservation, requestDigest, intent, "audit-stage-1", now)
	if err != nil {
		t.Fatalf("FinalizeContentStage: %v", err)
	}
	if result.Kind != "stage" || result.ReplayRequiresRestage || !json.Valid(result.Preview) {
		t.Fatalf("stage result=%+v, want initial token-free preview envelope", result)
	}
	var storedPreview []byte
	var previewHash, tokenDigest, state string
	if err := db.QueryRow(`SELECT preview_json,preview_sha256,token_digest,state FROM assurance_content_placement_intents WHERE tenant_id=? AND placement_intent_id=?`, contentStageTestTenant, intent.PlacementIntentID).Scan(&storedPreview, &previewHash, &tokenDigest, &state); err != nil {
		t.Fatal(err)
	}
	if string(storedPreview) != string(intent.PreviewJSON) || previewHash != intent.PreviewSHA256 || state != "staged" {
		t.Fatalf("stored intent does not match frozen preview: hash=%s state=%s", previewHash, state)
	}
	if tokenDigest != intent.TokenDigest || strings.Contains(string(storedPreview), rawToken) {
		t.Fatal("raw confirmation token was stored or digest mismatch")
	}
	var idempotencyState, envelope string
	if err := db.QueryRow(`SELECT state,response_envelope FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=?`, contentStageTestTenant, reservation.RecordID).Scan(&idempotencyState, &envelope); err != nil {
		t.Fatal(err)
	}
	if idempotencyState != string(ReservationStateSealed) || strings.Contains(envelope, rawToken) || strings.Contains(envelope, "confirmation_token") {
		t.Fatalf("sealed replay envelope is not token-free: state=%s envelope=%s", idempotencyState, envelope)
	}
	var auditRows, intentLinks, reservationLinks int
	if err := db.QueryRow(`SELECT count(*) FROM audit_log WHERE tenant_id=? AND audit_id=? AND action='stage'`, contentStageTestTenant, "audit-stage-1").Scan(&auditRows); err != nil || auditRows != 1 {
		t.Fatalf("rich audit rows=%d err=%v", auditRows, err)
	}
	for _, target := range []struct {
		kind, id string
		count    *int
	}{
		{"content_placement_intent", intent.PlacementIntentID, &intentLinks},
		{"idempotency_record", reservation.RecordID, &reservationLinks},
	} {
		if err := db.QueryRow(`SELECT count(*) FROM assurance_audit_links WHERE tenant_id=? AND entity_kind=? AND entity_id=? AND audit_id=?`, contentStageTestTenant, target.kind, target.id, "audit-stage-1").Scan(target.count); err != nil || *target.count != 1 {
			t.Fatalf("%s audit links=%d err=%v", target.kind, *target.count, err)
		}
	}
	var auditAfter string
	if err := db.QueryRow(`SELECT after_json FROM audit_log WHERE tenant_id=? AND audit_id=?`, contentStageTestTenant, "audit-stage-1").Scan(&auditAfter); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(auditAfter, "exact private source text") || strings.Contains(auditAfter, rawToken) || !strings.Contains(auditAfter, intent.PreviewSHA256) {
		t.Fatalf("audit contains private/raw values or lacks preview digest: %s", auditAfter)
	}
	keyDigest := HashBytes([]byte("content-stage-key-000001"))
	lookup, found, err := store.LookupReservation(ctx, ReservationRequest{
		ActorID: contentStageActor(), Tool: "workiva_content_placement", Action: "stage",
		IdempotencyDigest: keyDigest, RequestDigest: requestDigest,
	})
	if err != nil || !found || lookup.Disposition != ReservationReplay || strings.Contains(string(lookup.Envelope), rawToken) {
		t.Fatalf("sealed stage replay=%+v found=%v err=%v", lookup, found, err)
	}
}

func TestFinalizeContentStageRollsBackAfterAuditLinksAndReservationSealFaults(t *testing.T) {
	for _, fault := range []string{"audit_link", "seal_after_links"} {
		t.Run(fault, func(t *testing.T) {
			store, db := openTestStore(t)
			ctx := contentPlacementStageContext()
			now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
			requestDigest, reservation := reserveContentStage(t, store, ctx, "content-stage-fault-0001", now)
			intent, _ := stageIntentFixture(t, now)
			var trigger string
			if fault == "audit_link" {
				trigger = `CREATE TRIGGER fail_stage_intent_link BEFORE INSERT ON assurance_audit_links WHEN NEW.entity_kind='content_placement_intent' BEGIN SELECT RAISE(ABORT,'injected stage link failure'); END`
			} else {
				trigger = `CREATE TRIGGER fail_stage_after_seal AFTER UPDATE OF state ON assurance_idempotency_records WHEN NEW.state='sealed' AND NEW.action='stage' BEGIN SELECT RAISE(ABORT,'injected post-link seal failure'); END`
			}
			if _, err := db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if _, err := store.FinalizeContentStage(ctx, reservation, requestDigest, intent, "audit-stage-fault", now); err == nil {
				t.Fatal("injected finalization fault was ignored")
			}
			for table, want := range map[string]int{"assurance_content_placement_intents": 0, "audit_log": 0, "assurance_audit_links": 0} {
				var count int
				if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != want {
					t.Fatalf("%s count=%d err=%v want=%d", table, count, err, want)
				}
			}
			var state string
			if err := db.QueryRow(`SELECT state FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=?`, contentStageTestTenant, reservation.RecordID).Scan(&state); err != nil || state != string(ReservationStateReserved) {
				t.Fatalf("reservation state=%s err=%v, want reserved", state, err)
			}
		})
	}
}

func TestFinalizeContentStageRejectsWrongOwnerAndExpiredLeaseBeforeWrites(t *testing.T) {
	store, db := openTestStore(t)
	ctx := contentPlacementStageContext()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	requestDigest, reservation := reserveContentStage(t, store, ctx, "content-stage-lease-0001", now)
	intent, _ := stageIntentFixture(t, now)
	other := identity.ContextWithPrincipal(context.Background(), identity.Principal{
		TenantID: contentStageTestTenant, ObjectID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		TokenType: identity.TokenTypeDelegated, Permissions: []identity.Permission{identity.PermissionContentStage},
	})
	if _, err := store.FinalizeContentStage(other, reservation, requestDigest, intent, "audit-denied", now); err == nil {
		t.Fatal("cross-actor finalization accepted")
	}
	if _, err := db.Exec(`UPDATE assurance_idempotency_records SET lease_expires_at=? WHERE tenant_id=? AND record_id=?`, formatTimestamp(now), contentStageTestTenant, reservation.RecordID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeContentStage(ctx, reservation, requestDigest, intent, "audit-expired", now); err == nil {
		t.Fatal("expired reservation lease accepted")
	}
	var intents, audits int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_content_placement_intents`).Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM audit_log`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if intents != 0 || audits != 0 {
		t.Fatalf("denied finalization wrote intent/audit: %d/%d", intents, audits)
	}
}

func contentPlacementStageContext() context.Context {
	return identity.ContextWithPrincipal(context.Background(), identity.Principal{
		TenantID: contentStageTestTenant, ObjectID: contentStageTestActor,
		TokenType: identity.TokenTypeDelegated, Permissions: []identity.Permission{identity.PermissionContentStage},
	})
}

func contentStageActor() string { return contentStageTestTenant + "/" + contentStageTestActor }

func reserveContentStage(t *testing.T, store *Store, ctx context.Context, key string, now time.Time) (Digest, ReservationResult) {
	t.Helper()
	request := struct {
		Phase          string `json:"phase"`
		CandidateKind  string `json:"candidate_kind"`
		CandidateID    string `json:"candidate_id"`
		ProfileID      string `json:"profile_id"`
		ProfileVersion int    `json:"profile_revision"`
	}{"stage", "extracted_item", "item-1", "profile-1", 1}
	canonical, err := CanonicalJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	digest := HashBytes(canonical)
	reservation, err := store.Reserve(ctx, ReservationRequest{
		ActorID: contentStageActor(), Tool: "workiva_content_placement", Action: "stage",
		IdempotencyDigest: HashBytes([]byte(key)), RequestDigest: digest, RetentionClass: "workflow",
	}, now)
	if err != nil || reservation.Disposition != ReservationOwned || reservation.OwnerNonce == "" {
		t.Fatalf("reserve stage=%+v err=%v", reservation, err)
	}
	return digest, reservation
}

func stageIntentFixture(t *testing.T, now time.Time) (ContentStageIntent, string) {
	t.Helper()
	intentID := "11111111-2222-4333-8444-555555555555"
	rawToken := "initial-stage-token-never-persisted"
	preview := struct {
		PlacementIntentID string    `json:"placement_intent_id"`
		ContentHash       string    `json:"content_hash"`
		ActorBindingID    string    `json:"actor_binding_id"`
		OriginalText      string    `json:"original_text"`
		CreatedAt         time.Time `json:"created_at"`
		ExpiresAt         time.Time `json:"expires_at"`
	}{intentID, strings.Repeat("a", 64), contentStageActor(), "exact private source text", now, now.Add(5 * time.Minute)}
	bytes, err := CanonicalJSON(preview)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(rawToken + "\x00" + string(bytes)))
	return ContentStageIntent{
		PlacementIntentID: intentID, PreviewJSON: bytes, PreviewSHA256: digestHex(HashBytes(bytes)),
		TokenDigest: hex.EncodeToString(digest[:]), ExpiresAt: now.Add(5 * time.Minute), CreatedAt: now,
	}, rawToken
}
