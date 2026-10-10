package assurance

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
)

const contentIngestCapability identity.Permission = "content.ingest"
const contentTestActorID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

func contentPrincipalContext(tenant, objectID string, tokenType identity.TokenType, permission bool, subjectFallback bool) context.Context {
	permissions := []identity.Permission(nil)
	if permission {
		permissions = []identity.Permission{contentIngestCapability}
	}
	return identity.ContextWithPrincipal(context.Background(), identity.Principal{
		TenantID: tenant, ObjectID: objectID, Subject: "subject-1", TokenType: tokenType,
		UsedSubjectFallback: subjectFallback, Permissions: permissions,
	})
}

func reserveContentIngest(t *testing.T, store *Store, ctx context.Context, key string, input ContentIngest, now time.Time) ReservationResult {
	t.Helper()
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok {
		t.Fatal("test principal missing")
	}
	requestDigest, err := ContentIngestRequestDigest(input)
	if err != nil {
		t.Fatalf("digest private content ingest request: %v", err)
	}
	reservation, err := store.Reserve(ctx, ReservationRequest{
		ActorID: principal.AuditActor(), Tool: "workiva_content_placement", Action: "ingest",
		IdempotencyDigest: HashBytes([]byte(key)), RequestDigest: requestDigest,
	}, now)
	if err != nil || reservation.Disposition != ReservationOwned || reservation.OwnerNonce == "" {
		t.Fatalf("reserve content ingest: reservation=%#v err=%v", reservation, err)
	}
	return reservation
}

func sampleContentIngest() ContentIngest {
	return ContentIngest{
		SourceBytes:  []byte("Revenue was 12 million USD in FY2025."),
		MediaType:    "text/plain",
		Filename:     "quarterly.txt",
		MetadataBLOB: []byte(`{"origin":{"kind":"analyst"},"provenance":{"availability":"available"}}`),
		Items: []ContentItemInput{{
			LocalID: "revenue", Text: "12 million USD", MetadataBLOB: []byte(`{"kind_hint":"number","interpretation":{"period":"FY2025","currency":"USD","unit":"currency","scale":"millions"}}`),
		}},
		Drafts: []ContentDraftInput{{
			LocalID: "summary", Text: "Revenue was 12 million USD.", ItemLocalIDs: []string{"revenue"},
			MetadataBLOB: []byte(`{"origin":{"kind":"copilot"}}`),
		}},
	}
}

func syntheticContentRetention(now time.Time) time.Time {
	// This is an explicit fixture value supplied as though resolved from trusted
	// policy. Production duration and policy resolution are intentionally absent.
	return now.Add(37 * time.Minute)
}

func TestFinalizeContentIngestPersistsOwnedImmutableBytesAndSealsAtomically(t *testing.T) {
	store, db := openTestStore(t)
	ctx := contentPrincipalContext(testTenant, contentTestActorID, identity.TokenTypeDelegated, true, false)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	input := sampleContentIngest()
	reservation := reserveContentIngest(t, store, ctx, "content-ingest-key-0001", input, now)

	result, err := store.FinalizeContentIngest(ctx, reservation, input, syntheticContentRetention(now), "audit-content-1", now)
	if err != nil {
		t.Fatalf("FinalizeContentIngest: %v", err)
	}
	if result.SourceArtifactID == "" || result.SourceSHA256 != digestHex(HashBytes(input.SourceBytes)) {
		t.Fatalf("source result=%#v, want opaque ID and server-computed SHA-256", result)
	}
	if result.ItemIDs["revenue"] == "" || result.DraftIDs["summary"] == "" {
		t.Fatalf("local IDs were not replaced with opaque immutable IDs: %#v", result)
	}

	source, err := store.getContentSourceAt(ctx, result.SourceArtifactID, now)
	if err != nil {
		t.Fatalf("GetContentSource: %v", err)
	}
	if source.TenantID != testTenant || source.ActorID != "11111111-1111-1111-1111-111111111111/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" || source.SourceSHA256 != result.SourceSHA256 || string(source.SourceBytes) != string(input.SourceBytes) {
		t.Fatalf("persisted source=%#v, want owned byte-exact source and computed hash", source)
	}
	if !source.ExpiresAt.Equal(syntheticContentRetention(now)) {
		t.Fatalf("source expiry=%s, want explicit policy fixture %s", source.ExpiresAt, syntheticContentRetention(now))
	}
	// Returned byte slices must not provide a mutation path back into storage.
	source.SourceBytes[0] ^= 0xff
	readAgain, err := store.getContentSourceAt(ctx, result.SourceArtifactID, now)
	if err != nil || string(readAgain.SourceBytes) != string(input.SourceBytes) {
		t.Fatalf("stored bytes changed through returned slice: source=%q err=%v", readAgain.SourceBytes, err)
	}

	for table, want := range map[string]int{"assurance_content_sources": 1, "assurance_content_items": 1, "assurance_content_drafts": 1} {
		var got int
		if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE tenant_id=?`, testTenant).Scan(&got); err != nil || got != want {
			t.Fatalf("%s count=%d err=%v, want %d", table, got, err, want)
		}
	}
	var state, envelope string
	if err := db.QueryRow(`SELECT state,response_envelope FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=?`, testTenant, reservation.RecordID).Scan(&state, &envelope); err != nil {
		t.Fatal(err)
	}
	if state != string(ReservationStateSealed) || strings.Contains(envelope, string(input.SourceBytes)) || strings.Contains(strings.ToLower(envelope), "token") {
		t.Fatalf("reservation state=%q envelope=%q; want sealed token-free metadata without source bytes", state, envelope)
	}
	var artifactLinks, auditRows int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_audit_links WHERE tenant_id=? AND entity_kind='content_source_artifact' AND entity_id=? AND audit_id=?`, testTenant, result.SourceArtifactID, "audit-content-1").Scan(&artifactLinks); err != nil || artifactLinks != 1 {
		t.Fatalf("source audit links=%d err=%v, want one source link", artifactLinks, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM audit_log WHERE tenant_id=? AND audit_id=?`, testTenant, "audit-content-1").Scan(&auditRows); err != nil || auditRows != 1 {
		t.Fatalf("audit rows=%d err=%v, want one atomic rich audit row", auditRows, err)
	}
}

func TestPublicDigestIngestSealsProjectionFromSavedRowsAndReplaysAfterArtifactExpiry(t *testing.T) {
	store, db := openTestStore(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, sig := contentTestBundle(t, priv, "active", contentTestActor)
	validated, err := ValidateBundle(raw, sig, testTenant, pub)
	if err != nil {
		t.Fatal(err)
	}
	admin := contentStageContext(contentTestActor, identity.PermissionContentStage)
	if err := store.StageBundle(admin, validated); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestActivation(admin, "bundle-1", 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(admin, testTenant, pub); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewContentPolicyResolver(store, pub, "content-access")
	if err != nil {
		t.Fatal(err)
	}
	ctx := contentPrincipalContext(testTenant, contentTestActorID, identity.TokenTypeDelegated, true, false)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	input := sampleContentIngest()
	publicDigest := HashBytes([]byte(`{"source":{"media_type":"text/plain"},"items":1}`))
	principal, _ := identity.PrincipalFromContext(ctx)
	reservation, err := store.Reserve(ctx, ReservationRequest{
		ActorID: principal.AuditActor(), Tool: "workiva_content_placement", Action: "ingest",
		IdempotencyDigest: HashBytes([]byte("public-ingest-key")), RequestDigest: publicDigest,
		RetainUntil: now.Add(2 * time.Hour),
	}, now)
	if err != nil || reservation.Disposition != ReservationOwned {
		t.Fatalf("reserve public ingest: result=%+v err=%v", reservation, err)
	}
	result, err := store.FinalizePolicyBoundContentIngestWithRequestDigest(ctx, reservation, input, publicDigest, resolver, "audit-public-ingest", now)
	if err != nil {
		t.Fatalf("finalize public ingest: %v", err)
	}
	p := result.PublicProjection
	if p == nil || p.Kind != "ingest" || len(p.VerifiedEvidenceRefs) != 0 || len(p.ExtractedItems) != 1 || len(p.DraftArtifacts) != 1 {
		t.Fatalf("public projection=%+v", p)
	}
	if p.Source.SourceArtifactID != result.SourceArtifactID || p.Source.Revision != 1 || p.Source.ContentHash != result.SourceSHA256 || p.Source.ByteLength != len(input.SourceBytes) || p.Source.MediaType != "text/plain" || p.Source.RetentionPolicy.Class != "source" {
		t.Fatalf("source projection=%+v", p.Source)
	}
	item := p.ExtractedItems[0]
	if item.ExtractedItemID != result.ItemIDs["revenue"] || item.Revision != 1 || item.Source.ContentHash != p.Source.ContentHash || !item.CandidateOnly || item.RetentionPolicy.Class != "draft" {
		t.Fatalf("item projection=%+v", item)
	}
	draft := p.DraftArtifacts[0]
	if draft.DraftArtifactID != result.DraftIDs["summary"] || draft.Revision != 1 || draft.Lineage.Kind != "source_bytes" || draft.Lineage.Source != item.Source || draft.OriginKind != "copilot" || !draft.CandidateOnly || draft.RetentionPolicy.Class != "draft" {
		t.Fatalf("draft projection=%+v", draft)
	}
	stageCtx := contentStageContext(contentTestActor, identity.PermissionContentStage)
	derivedExpiryNow := now.Add(13 * time.Hour)
	if _, err := store.GetContentCandidateAt(stageCtx, ContentCandidateExtractedItem, item.ExtractedItemID, derivedExpiryNow); err == nil {
		t.Fatal("signed-derived extracted item remained accessible after draft retention while source remained live")
	}
	if _, err := store.GetContentCandidateAt(stageCtx, ContentCandidateDraftArtifact, draft.DraftArtifactID, derivedExpiryNow); err == nil {
		t.Fatal("signed-derived draft remained accessible after draft retention while source remained live")
	}
	reingestNow := now.Add(13 * time.Hour)
	reingest := sampleContentIngest()
	reingest.ExistingSourceArtifactID = result.SourceArtifactID
	reingest.ExpectedExistingSourceSHA256 = result.SourceSHA256
	reingestDigest := HashBytes([]byte("reingest-after-derived-cutoff"))
	reingestReservation, err := store.Reserve(ctx, ReservationRequest{
		ActorID: principal.AuditActor(), Tool: "workiva_content_placement", Action: "ingest",
		IdempotencyDigest: HashBytes([]byte("reingest-derived-cutoff-key")), RequestDigest: reingestDigest,
		RetainUntil: reingestNow.Add(2 * time.Hour),
	}, reingestNow)
	if err != nil || reingestReservation.Disposition != ReservationOwned {
		t.Fatalf("reserve derived-cutoff reingest: result=%+v err=%v", reingestReservation, err)
	}
	if _, err := store.FinalizePolicyBoundContentIngestWithRequestDigest(ctx, reingestReservation, reingest, reingestDigest, resolver, "audit-derived-cutoff-reingest", reingestNow); err == nil {
		t.Fatal("re-ingest extended signed derived-content retention past its original cutoff")
	}
	var itemCount int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_content_items WHERE tenant_id=? AND source_artifact_id=?`, testTenant, result.SourceArtifactID).Scan(&itemCount); err != nil || itemCount != 1 {
		t.Fatalf("reingest item count=%d err=%v; original source rows should remain unchanged", itemCount, err)
	}
	var responseEnvelope, responseHash, storedRequestDigest string
	if err := db.QueryRow(`SELECT response_envelope,response_hash,request_digest FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=?`, testTenant, reservation.RecordID).Scan(&responseEnvelope, &responseHash, &storedRequestDigest); err != nil {
		t.Fatal(err)
	}
	if storedRequestDigest != digestHex(publicDigest) || responseHash != digestHex(HashBytes([]byte(responseEnvelope))) || strings.Contains(responseEnvelope, string(input.SourceBytes)) || strings.Contains(responseEnvelope, "12 million USD") {
		t.Fatalf("sealed public result did not bind digest or contains private content: digest=%s hash=%s envelope=%s", storedRequestDigest, responseHash, responseEnvelope)
	}
	// Expire and corrupt the live artifact rows after sealing. Replay must return
	// the transactionally sealed public projection without re-reading them.
	if _, err := db.Exec(`UPDATE assurance_content_sources SET expires_at=? WHERE tenant_id=? AND source_artifact_id=?`, formatTimestamp(now.Add(-time.Second)), testTenant, result.SourceArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE assurance_content_sources SET source_bytes=? WHERE tenant_id=? AND source_artifact_id=?`, []byte("tampered"), testTenant, result.SourceArtifactID); err != nil {
		t.Fatal(err)
	}
	replay, found, err := store.LookupReservation(ctx, ReservationRequest{
		ActorID: principal.AuditActor(), Tool: "workiva_content_placement", Action: "ingest",
		IdempotencyDigest: HashBytes([]byte("public-ingest-key")), RequestDigest: publicDigest,
	})
	if err != nil || !found || replay.State != ReservationStateSealed {
		t.Fatalf("lookup sealed public replay: result=%+v found=%v err=%v", replay, found, err)
	}
	var replayed ContentIngestResult
	if err := json.Unmarshal(replay.Envelope, &replayed); err != nil || replayed.PublicProjection == nil || replayed.PublicProjection.Source != p.Source || replayed.PublicProjection.DraftArtifacts[0] != draft {
		t.Fatalf("replayed projection=%+v err=%v", replayed.PublicProjection, err)
	}
}

func TestPublicProjectionFailureRollsBackArtifactsAndReservation(t *testing.T) {
	store, db := openTestStore(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, sig := contentTestBundle(t, priv, "active", contentTestActor)
	validated, err := ValidateBundle(raw, sig, testTenant, pub)
	if err != nil {
		t.Fatal(err)
	}
	admin := contentStageContext(contentTestActor, identity.PermissionContentStage)
	if err := store.StageBundle(admin, validated); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestActivation(admin, "bundle-1", 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(admin, testTenant, pub); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewContentPolicyResolver(store, pub, "content-access")
	if err != nil {
		t.Fatal(err)
	}
	ctx := contentPrincipalContext(testTenant, contentTestActorID, identity.TokenTypeDelegated, true, false)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	input := sampleContentIngest()
	mutatedSource := []byte("consistently rewritten source")
	mutatedSourceHash := digestHex(HashBytes(mutatedSource))
	trigger := fmt.Sprintf(`CREATE TRIGGER rewrite_intake_source AFTER INSERT ON assurance_content_sources
		WHEN NEW.tenant_id='%s' BEGIN UPDATE assurance_content_sources SET source_bytes=%q,source_sha256='%s'
		WHERE tenant_id=NEW.tenant_id AND source_artifact_id=NEW.source_artifact_id; END`, testTenant, string(mutatedSource), mutatedSourceHash)
	if _, err := db.Exec(trigger); err != nil {
		t.Fatal(err)
	}
	digest := HashBytes([]byte("public-digest-consistently-mutated-source"))
	principal, _ := identity.PrincipalFromContext(ctx)
	reservation, err := store.Reserve(ctx, ReservationRequest{
		ActorID: principal.AuditActor(), Tool: "workiva_content_placement", Action: "ingest",
		IdempotencyDigest: HashBytes([]byte("public-ingest-mutated-source")), RequestDigest: digest, RetainUntil: now.Add(2 * time.Hour),
	}, now)
	if err != nil || reservation.Disposition != ReservationOwned {
		t.Fatalf("reserve public ingest: result=%+v err=%v", reservation, err)
	}
	if _, err := store.FinalizePolicyBoundContentIngestWithRequestDigest(ctx, reservation, input, digest, resolver, "audit-mutated-source", now); err == nil {
		t.Fatal("consistently rewritten source was sealed with a mismatched projection")
	}
	for table := range map[string]bool{"assurance_content_sources": true, "assurance_content_items": true, "assurance_content_drafts": true} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE tenant_id=?`, testTenant).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s rows=%d err=%v after projection failure", table, count, err)
		}
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=?`, testTenant, reservation.RecordID).Scan(&state); err != nil || state != string(ReservationStateReserved) {
		t.Fatalf("reservation state=%q err=%v after projection rollback", state, err)
	}
	var auditCount int
	if err := db.QueryRow(`SELECT count(*) FROM audit_log WHERE tenant_id=? AND audit_id=?`, testTenant, "audit-mutated-source").Scan(&auditCount); err != nil || auditCount != 0 {
		t.Fatalf("audit count=%d err=%v after projection rollback", auditCount, err)
	}
}

func TestContentIngestReplayAndConflictUseSealedReservationWithoutAuditDependency(t *testing.T) {
	store, db := openTestStore(t)
	ctx := contentPrincipalContext(testTenant, contentTestActorID, identity.TokenTypeDelegated, true, false)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	input := sampleContentIngest()
	keyDigest := HashBytes([]byte("replay-content-ingest-key-0001"))
	reservation := reserveContentIngest(t, store, ctx, "replay-content-ingest-key-0001", input, now)
	written, err := store.FinalizeContentIngest(ctx, reservation, input, syntheticContentRetention(now), "audit-replay-initial", now)
	if err != nil {
		t.Fatal(err)
	}
	var sourcesBefore, itemsBefore, draftsBefore, auditBefore int
	for table, target := range map[string]*int{"assurance_content_sources": &sourcesBefore, "assurance_content_items": &itemsBefore, "assurance_content_drafts": &draftsBefore} {
		if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE tenant_id=?`, testTenant).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.QueryRow(`SELECT count(*) FROM audit_log WHERE tenant_id=? AND action='ingest'`, testTenant).Scan(&auditBefore); err != nil {
		t.Fatal(err)
	}

	// A sealed replay remains classifiable when rich audit is unavailable;
	// replay must not ask for a new audit row or materialize another bundle.
	store.SetAuditLog(nil)
	requestDigest, err := ContentIngestRequestDigest(input)
	if err != nil {
		t.Fatal(err)
	}
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok {
		t.Fatal("missing principal in replay test context")
	}
	actorID := principal.AuditActor()
	request := ReservationRequest{ActorID: actorID, Tool: "workiva_content_placement", Action: "ingest", IdempotencyDigest: keyDigest, RequestDigest: requestDigest}
	prior, found, err := store.LookupReservation(ctx, request)
	if err != nil || !found || prior.Disposition != ReservationReplay {
		t.Fatalf("sealed replay lookup=%#v found=%v err=%v", prior, found, err)
	}
	var replay ContentIngestResult
	if err := json.Unmarshal(prior.Envelope, &replay); err != nil {
		t.Fatalf("decode token-free replay envelope: %v", err)
	}
	if replay.SourceArtifactID != written.SourceArtifactID || replay.SourceSHA256 != written.SourceSHA256 {
		t.Fatalf("replay=%#v, original=%#v", replay, written)
	}

	conflicting := sampleContentIngest()
	conflicting.SourceBytes = []byte("different source under same key")
	conflictDigest, err := ContentIngestRequestDigest(conflicting)
	if err != nil {
		t.Fatal(err)
	}
	request.RequestDigest = conflictDigest
	conflict, found, err := store.LookupReservation(ctx, request)
	if err != nil || !found || conflict.Disposition != ReservationConflict {
		t.Fatalf("conflicting replay lookup=%#v found=%v err=%v", conflict, found, err)
	}
	var sourcesAfter, itemsAfter, draftsAfter, auditAfter int
	for table, target := range map[string]*int{"assurance_content_sources": &sourcesAfter, "assurance_content_items": &itemsAfter, "assurance_content_drafts": &draftsAfter} {
		if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE tenant_id=?`, testTenant).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.QueryRow(`SELECT count(*) FROM audit_log WHERE tenant_id=? AND action='ingest'`, testTenant).Scan(&auditAfter); err != nil {
		t.Fatal(err)
	}
	if sourcesAfter != sourcesBefore || itemsAfter != itemsBefore || draftsAfter != draftsBefore || auditAfter != auditBefore {
		t.Fatalf("replay changed persistence/audit counts: before=(%d,%d,%d,%d) after=(%d,%d,%d,%d)", sourcesBefore, itemsBefore, draftsBefore, auditBefore, sourcesAfter, itemsAfter, draftsAfter, auditAfter)
	}
}

func TestFinalizeContentIngestRequiresTrustedDelegatedContentPrincipal(t *testing.T) {
	store, db := openTestStore(t)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	ownerCtx := contentPrincipalContext(testTenant, contentTestActorID, identity.TokenTypeDelegated, true, false)
	cases := []struct {
		name string
		ctx  context.Context
	}{
		{name: "missing principal", ctx: context.Background()},
		{name: "application token", ctx: contentPrincipalContext(testTenant, "cccccccc-cccc-4ccc-8ccc-cccccccccccc", identity.TokenTypeApplication, true, false)},
		{name: "subject fallback", ctx: contentPrincipalContext(testTenant, "", identity.TokenTypeDelegated, true, true)},
		{name: "missing capability", ctx: contentPrincipalContext(testTenant, "dddddddd-dddd-4ddd-8ddd-dddddddddddd", identity.TokenTypeDelegated, false, false)},
		{name: "missing object ID", ctx: contentPrincipalContext(testTenant, "", identity.TokenTypeDelegated, true, false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := sampleContentIngest()
			reservation := reserveContentIngest(t, store, ownerCtx, "unauthorized-key-"+strings.ReplaceAll(tc.name, " ", "-"), input, now)
			_, err := store.FinalizeContentIngest(tc.ctx, reservation, input, syntheticContentRetention(now), "audit-denied", now)
			if err == nil {
				t.Fatal("untrusted principal was allowed to finalize intake")
			}
			for _, table := range []string{"assurance_content_sources", "assurance_content_items", "assurance_content_drafts"} {
				var count int
				if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE tenant_id=?`, testTenant).Scan(&count); err != nil || count != 0 {
					t.Fatalf("denied request left %d rows in %s (err=%v)", count, table, err)
				}
			}
			var state string
			if err := db.QueryRow(`SELECT state FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=?`, testTenant, reservation.RecordID).Scan(&state); err != nil || state != string(ReservationStateReserved) {
				t.Fatalf("denied request changed reservation state=%q err=%v", state, err)
			}
		})
	}
	var auditRows int
	if err := db.QueryRow(`SELECT count(*) FROM audit_log WHERE tenant_id=? AND action='ingest'`, testTenant).Scan(&auditRows); err != nil || auditRows != 0 {
		t.Fatalf("denied requests wrote %d ingest audit rows (err=%v)", auditRows, err)
	}
}

func TestGetContentSourceRequiresCapabilityAndHidesForeignOwnership(t *testing.T) {
	store, _ := openTestStore(t)
	ownerCtx := contentPrincipalContext(testTenant, "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", identity.TokenTypeDelegated, true, false)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	input := sampleContentIngest()
	reservation := reserveContentIngest(t, store, ownerCtx, "source-owner-key-00001", input, now)
	result, err := store.FinalizeContentIngest(ownerCtx, reservation, input, syntheticContentRetention(now), "audit-owner", now)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{name: "foreign actor", ctx: contentPrincipalContext(testTenant, "ffffffff-ffff-4fff-8fff-ffffffffffff", identity.TokenTypeDelegated, true, false)},
		{name: "foreign tenant", ctx: contentPrincipalContext("22222222-2222-4222-8222-222222222222", "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", identity.TokenTypeDelegated, true, false)},
		{name: "no capability", ctx: contentPrincipalContext(testTenant, "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", identity.TokenTypeDelegated, false, false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.getContentSourceAt(tc.ctx, result.SourceArtifactID, now); err == nil {
				t.Fatal("foreign or unauthorized source lookup succeeded")
			}
		})
	}
	if _, err := store.getContentSourceAt(ownerCtx, result.SourceArtifactID, now); err != nil {
		t.Fatalf("owner cannot read owned source: %v", err)
	}
}

func TestFinalizeContentIngestReingestRequiresOwnedSourceAndBothHashBindings(t *testing.T) {
	store, db := openTestStore(t)
	ctx := contentPrincipalContext(testTenant, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", identity.TokenTypeDelegated, true, false)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	first := sampleContentIngest()
	reservation := reserveContentIngest(t, store, ctx, "initial-source-key-001", first, now)
	created, err := store.FinalizeContentIngest(ctx, reservation, first, syntheticContentRetention(now), "audit-initial", now)
	if err != nil {
		t.Fatal(err)
	}

	valid := sampleContentIngest()
	valid.ExistingSourceArtifactID = created.SourceArtifactID
	valid.ExpectedExistingSourceSHA256 = created.SourceSHA256
	valid.Items[0].LocalID = "revenue-revised"
	valid.Drafts[0].ItemLocalIDs = []string{"revenue-revised"}
	reingestReservation := reserveContentIngest(t, store, ctx, "reingest-source-key-01", valid, now.Add(time.Second))
	reingested, err := store.FinalizeContentIngest(ctx, reingestReservation, valid, syntheticContentRetention(now), "audit-reingest", now.Add(time.Second))
	if err != nil || reingested.SourceArtifactID != created.SourceArtifactID {
		t.Fatalf("valid source-bound reingest result=%#v err=%v", reingested, err)
	}
	var sourceCount, itemCount int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_content_sources WHERE tenant_id=?`, testTenant).Scan(&sourceCount); err != nil || sourceCount != 1 {
		t.Fatalf("reingest source count=%d err=%v, want original immutable source only", sourceCount, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM assurance_content_items WHERE tenant_id=? AND source_artifact_id=?`, testTenant, created.SourceArtifactID).Scan(&itemCount); err != nil || itemCount != 2 {
		t.Fatalf("source-bound immutable item revisions=%d err=%v, want 2", itemCount, err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*ContentIngest)
	}{
		{name: "missing expected hash", mutate: func(x *ContentIngest) { x.ExpectedExistingSourceSHA256 = "" }},
		{name: "wrong expected hash", mutate: func(x *ContentIngest) { x.ExpectedExistingSourceSHA256 = strings.Repeat("0", 64) }},
		{name: "changed supplied bytes", mutate: func(x *ContentIngest) { x.SourceBytes = []byte("different source") }},
		{name: "missing source ID with expected hash", mutate: func(x *ContentIngest) { x.ExistingSourceArtifactID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := sampleContentIngest()
			bad.ExistingSourceArtifactID = created.SourceArtifactID
			bad.ExpectedExistingSourceSHA256 = created.SourceSHA256
			tc.mutate(&bad)
			badReservation := reserveContentIngest(t, store, ctx, "bad-reingest-"+strings.ReplaceAll(tc.name, " ", "-"), bad, now.Add(2*time.Second))
			if _, err := store.FinalizeContentIngest(ctx, badReservation, bad, syntheticContentRetention(now), "audit-reingest-bad", now.Add(2*time.Second)); err == nil {
				t.Fatal("invalid source re-ingest binding accepted")
			}
		})
	}
}

func TestFinalizeContentIngestRejectsUnboundedBytesMetadataAndInvalidReferences(t *testing.T) {
	store, db := openTestStore(t)
	ctx := contentPrincipalContext(testTenant, "99999999-9999-4999-8999-999999999999", identity.TokenTypeDelegated, true, false)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	mutations := []struct {
		name   string
		mutate func(*ContentIngest)
	}{
		{name: "source exceeds decoded byte bound", mutate: func(x *ContentIngest) { x.SourceBytes = make([]byte, 786433) }},
		{name: "metadata blob exceeds request bound", mutate: func(x *ContentIngest) { x.MetadataBLOB = make([]byte, 1048577) }},
		{name: "duplicate local item ID", mutate: func(x *ContentIngest) { x.Items = append(x.Items, x.Items[0]) }},
		{name: "draft references absent item", mutate: func(x *ContentIngest) { x.Drafts[0].ItemLocalIDs = []string{"missing"} }},
		{name: "duplicate draft local ID", mutate: func(x *ContentIngest) { x.Drafts = append(x.Drafts, x.Drafts[0]) }},
	}
	for i, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			input := sampleContentIngest()
			tc.mutate(&input)
			reservation := reserveContentIngest(t, store, ctx, fmt.Sprintf("bounded-intake-key-%02d", i), input, now)
			if _, err := store.FinalizeContentIngest(ctx, reservation, input, syntheticContentRetention(now), fmt.Sprintf("audit-bounded-%d", i), now); err == nil {
				t.Fatal("invalid bounded intake was accepted")
			}
		})
	}
	for _, table := range []string{"assurance_content_sources", "assurance_content_items", "assurance_content_drafts"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rejected intakes left %d rows in %s (err=%v)", count, table, err)
		}
	}
}

func TestFinalizeContentIngestAuditFailureRollsBackArtifactsAndReservationSeal(t *testing.T) {
	store, db := openTestStore(t)
	ctx := contentPrincipalContext(testTenant, "88888888-8888-4888-8888-888888888888", identity.TokenTypeDelegated, true, false)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	input := sampleContentIngest()
	reservation := reserveContentIngest(t, store, ctx, "atomic-ingest-key-0001", input, now)
	if _, err := db.Exec(`CREATE TRIGGER fail_content_audit_link BEFORE INSERT ON assurance_audit_links WHEN NEW.entity_kind='content_source_artifact' BEGIN SELECT RAISE(ABORT, 'injected content audit link failure'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := store.FinalizeContentIngest(ctx, reservation, input, syntheticContentRetention(now), "audit-atomic-fail", now)
	if err == nil {
		t.Fatal("injected audit-link failure did not fail intake")
	}
	for _, table := range []string{"assurance_content_sources", "assurance_content_items", "assurance_content_drafts"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE tenant_id=?`, testTenant).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed atomic intake left %d rows in %s (err=%v)", count, table, err)
		}
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM assurance_idempotency_records WHERE tenant_id=? AND record_id=?`, testTenant, reservation.RecordID).Scan(&state); err != nil || state != string(ReservationStateReserved) {
		t.Fatalf("failed atomic intake reservation state=%q err=%v, want still reserved", state, err)
	}
	var auditRows int
	if err := db.QueryRow(`SELECT count(*) FROM audit_log WHERE tenant_id=? AND audit_id='audit-atomic-fail'`, testTenant).Scan(&auditRows); err != nil || auditRows != 0 {
		t.Fatalf("failed atomic intake left %d audit rows (err=%v)", auditRows, err)
	}
}

func TestFinalizeContentIngestRequiresMatchingReservationActorToolAndAction(t *testing.T) {
	store, db := openTestStore(t)
	ownerCtx := contentPrincipalContext(testTenant, "77777777-7777-4777-8777-777777777777", identity.TokenTypeDelegated, true, false)
	otherCtx := contentPrincipalContext(testTenant, "66666666-6666-4666-8666-666666666666", identity.TokenTypeDelegated, true, false)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	input := sampleContentIngest()
	otherActorReservation := reserveContentIngest(t, store, otherCtx, "wrong-actor-key-00001", input, now)
	if _, err := store.FinalizeContentIngest(ownerCtx, otherActorReservation, input, syntheticContentRetention(now), "audit-wrong-actor", now); err == nil {
		t.Fatal("reservation owned by another trusted actor was accepted")
	}

	// Corrupt persisted action/tool and request digest independently of the
	// opaque reservation ID and owner nonce.
	for _, tc := range []struct {
		name, column, value string
	}{
		{name: "wrong action", column: "action", value: "stage"},
		{name: "wrong tool", column: "tool", value: "workiva_transfer_value"},
		{name: "wrong request digest", column: "request_digest", value: digestHex(HashBytes([]byte("different semantic request")))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reservation := reserveContentIngest(t, store, ownerCtx, "wrong-binding-"+strings.ReplaceAll(tc.name, " ", "-"), input, now)
			if _, err := db.Exec(`UPDATE assurance_idempotency_records SET `+tc.column+`=? WHERE tenant_id=? AND record_id=?`, tc.value, testTenant, reservation.RecordID); err != nil {
				t.Fatal(err)
			}
			if _, err := store.FinalizeContentIngest(ownerCtx, reservation, input, syntheticContentRetention(now), "audit-wrong-binding", now); err == nil {
				t.Fatal("reservation with mismatched trusted scope/digest was accepted")
			}
		})
	}

	// Changing actual submitted bytes after reserving the canonical request must
	// fail the digest binding, even though the caller supplied no hash field.
	reservation := reserveContentIngest(t, store, ownerCtx, "changed-bytes-key-0001", input, now)
	changedBytes := sampleContentIngest()
	changedBytes.SourceBytes = []byte("changed after request reservation")
	if _, err := store.FinalizeContentIngest(ownerCtx, reservation, changedBytes, syntheticContentRetention(now), "audit-changed-bytes", now); err == nil {
		t.Fatal("bytes changed after reservation were accepted")
	}
}

func TestContentSourceBLOBSurvivesSQLiteCloseAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content-intake.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	store.SetAuditLog(log)
	ctx := contentPrincipalContext(testTenant, "55555555-5555-4555-8555-555555555555", identity.TokenTypeDelegated, true, false)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	input := sampleContentIngest()
	reservation := reserveContentIngest(t, store, ctx, "file-reopen-ingest-key", input, now)
	result, err := store.FinalizeContentIngest(ctx, reservation, input, syntheticContentRetention(now), "audit-file-reopen", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	store, err = NewWithDB(db)
	if err != nil {
		t.Fatalf("NewWithDB after close/reopen: %v", err)
	}
	log, err = audit.NewWithDB(db)
	if err != nil {
		t.Fatalf("audit.NewWithDB after close/reopen: %v", err)
	}
	store.SetAuditLog(log)
	reopened, err := store.getContentSourceAt(ctx, result.SourceArtifactID, now)
	if err != nil {
		t.Fatalf("GetContentSource after close/reopen: %v", err)
	}
	if string(reopened.SourceBytes) != string(input.SourceBytes) || reopened.SourceSHA256 != digestHex(HashBytes(input.SourceBytes)) {
		t.Fatalf("reopened source bytes/hash differ: bytes=%q hash=%s", reopened.SourceBytes, reopened.SourceSHA256)
	}
}

func TestGetContentSourceUsesCurrentClock(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := contentPrincipalContext(testTenant, contentTestActorID, identity.TokenTypeDelegated, true, false)
	now := time.Now().UTC().Truncate(time.Second)
	input := sampleContentIngest()
	reservation := reserveContentIngest(t, store, ctx, "source-current-clock-key", input, now)
	result, err := store.FinalizeContentIngest(ctx, reservation, input, now.Add(time.Hour), "audit-current-clock", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetContentSource(ctx, result.SourceArtifactID); err != nil {
		t.Fatalf("public current-clock source read failed: %v", err)
	}
}
