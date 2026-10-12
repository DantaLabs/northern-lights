package assurance

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
)

func TestContentSourceEncryptionPersistsCiphertextAndReopensWithSameKey(t *testing.T) {
	store, db := openTestStore(t)
	key := bytes.Repeat([]byte{0x5a}, 32)
	if err := store.SetContentStorageKey("unit-key-1", key); err != nil {
		t.Fatal(err)
	}
	if !store.ContentStorageEncrypted() {
		t.Fatal("configured store does not report encrypted source storage")
	}
	now := time.Now().UTC().Truncate(time.Second)
	ctx := contentPrincipalContext(testTenant, contentTestActorID, identity.TokenTypeDelegated, true, false)
	input := sampleContentIngest()
	reservation := reserveContentIngest(t, store, ctx, "encrypted-source-key-01", input, now)
	result, err := store.FinalizeContentIngest(ctx, reservation, input, syntheticContentRetention(now), "audit-encrypted-source", now)
	if err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := db.QueryRow(`SELECT source_bytes FROM assurance_content_sources WHERE tenant_id=? AND source_artifact_id=?`, testTenant, result.SourceArtifactID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, input.SourceBytes) || !bytes.HasPrefix(stored, contentStorageEnvelopeMagic) {
		t.Fatalf("source row is not a tagged ciphertext: %q", stored)
	}
	var plainHash string
	if err := db.QueryRow(`SELECT source_sha256 FROM assurance_content_sources WHERE tenant_id=? AND source_artifact_id=?`, testTenant, result.SourceArtifactID).Scan(&plainHash); err != nil {
		t.Fatal(err)
	}
	if plainHash != digestHex(HashBytes(input.SourceBytes)) || result.SourceSHA256 != plainHash {
		t.Fatalf("plaintext digest changed: stored=%s result=%s", plainHash, result.SourceSHA256)
	}

	reopened, err := NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.GetContentSource(ctx, result.SourceArtifactID); err == nil {
		t.Fatal("encrypted row was read without a configured key")
	}
	if err := reopened.SetContentStorageKey("unit-key-1", key); err != nil {
		t.Fatal(err)
	}
	source, err := reopened.GetContentSource(ctx, result.SourceArtifactID)
	if err != nil || !bytes.Equal(source.SourceBytes, input.SourceBytes) {
		t.Fatalf("reopened source=%+v err=%v", source, err)
	}
	if _, err := reopened.getContentSourceAt(ctx, result.SourceArtifactID, now.Add(38*time.Minute)); err == nil {
		t.Fatal("expired encrypted source remained available")
	}
	privateDigest, err := ContentIngestRequestDigest(input)
	if err != nil {
		t.Fatal(err)
	}
	replay, found, err := reopened.LookupReservation(ctx, ReservationRequest{
		ActorID: testTenant + "/" + contentTestActorID, Tool: "workiva_content_placement", Action: "ingest",
		IdempotencyDigest: HashBytes([]byte("encrypted-source-key-01")), RequestDigest: privateDigest,
	})
	if err != nil || !found || replay.Disposition != ReservationReplay {
		t.Fatalf("encrypted duplicate replay=%+v found=%t err=%v", replay, found, err)
	}
	var replayed ContentIngestResult
	if err := json.Unmarshal(replay.Envelope, &replayed); err != nil || replayed.SourceArtifactID != result.SourceArtifactID || replayed.SourceSHA256 != result.SourceSHA256 {
		t.Fatalf("encrypted replay changed sealed source result: %+v err=%v", replayed, err)
	}
	sealed, err := CanonicalJSON(result)
	if err != nil || !bytes.Equal(replay.Envelope, sealed) {
		t.Fatalf("encrypted duplicate replay changed sealed response: envelope=%s err=%v", replay.Envelope, err)
	}
}

func TestEncryptedContentSourceSupportsSignedProjectionCandidateAndReingest(t *testing.T) {
	store, db := openTestStore(t)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, signature := contentTestBundle(t, privateKey, "active", contentTestActor)
	validated, err := ValidateBundle(raw, signature, testTenant, publicKey)
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
	if err := store.Bootstrap(admin, testTenant, publicKey); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewContentPolicyResolver(store, publicKey, "content-access")
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x73}, 32)
	if err := store.SetContentStorageKey("signed-intake-key", key); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{
		TenantID: testTenant, ObjectID: contentTestActor, Subject: "crypto-test", TokenType: identity.TokenTypeDelegated,
		Permissions: []identity.Permission{contentIngestCapability, identity.PermissionContentStage},
	})
	input := sampleContentIngest()
	digest := HashBytes([]byte("encrypted-public-ingest-digest"))
	reservation, err := store.Reserve(ctx, ReservationRequest{
		ActorID: testTenant + "/" + contentTestActor, Tool: "workiva_content_placement", Action: "ingest",
		IdempotencyDigest: HashBytes([]byte("encrypted-public-ingest-key")), RequestDigest: digest, RetainUntil: now.Add(2 * time.Hour),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.FinalizePolicyBoundContentIngestWithRequestDigest(ctx, reservation, input, digest, resolver, "audit-encrypted-public-ingest", now)
	if err != nil || created.PublicProjection == nil {
		t.Fatalf("encrypted public intake result=%+v err=%v", created, err)
	}
	var stored []byte
	if err := db.QueryRow(`SELECT source_bytes FROM assurance_content_sources WHERE tenant_id=? AND source_artifact_id=?`, testTenant, created.SourceArtifactID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, input.SourceBytes) || created.PublicProjection.Source.ByteLength != len(input.SourceBytes) || created.PublicProjection.Source.ContentHash != digestHex(HashBytes(input.SourceBytes)) {
		t.Fatal("encrypted public projection lost plaintext byte length/hash or database contains plaintext")
	}
	candidate, err := store.GetContentCandidateAt(ctx, ContentCandidateExtractedItem, created.ItemIDs["revenue"], now)
	if err != nil || candidate.SourceSHA256 != created.SourceSHA256 {
		t.Fatalf("encrypted candidate read=%+v err=%v", candidate, err)
	}

	reingest := sampleContentIngest()
	reingest.ExistingSourceArtifactID = created.SourceArtifactID
	reingest.ExpectedExistingSourceSHA256 = created.SourceSHA256
	reingest.Items[0].LocalID = "revenue-revised"
	reingest.Drafts[0].ItemLocalIDs = []string{"revenue-revised"}
	reingestDigest := HashBytes([]byte("encrypted-source-reingest-digest"))
	reingestReservation, err := store.Reserve(ctx, ReservationRequest{
		ActorID: testTenant + "/" + contentTestActor, Tool: "workiva_content_placement", Action: "ingest",
		IdempotencyDigest: HashBytes([]byte("encrypted-source-reingest-key")), RequestDigest: reingestDigest, RetainUntil: now.Add(2*time.Hour + time.Minute),
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	updated, err := store.FinalizePolicyBoundContentIngestWithRequestDigest(ctx, reingestReservation, reingest, reingestDigest, resolver, "audit-encrypted-source-reingest", now.Add(time.Minute))
	if err != nil || updated.SourceArtifactID != created.SourceArtifactID || updated.PublicProjection == nil {
		t.Fatalf("encrypted source re-ingest result=%+v err=%v", updated, err)
	}
	var storedAfter []byte
	if err := db.QueryRow(`SELECT source_bytes FROM assurance_content_sources WHERE tenant_id=? AND source_artifact_id=?`, testTenant, created.SourceArtifactID).Scan(&storedAfter); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, storedAfter) {
		t.Fatal("source-bound re-ingest rewrote the original encrypted source")
	}
}

func TestContentSourceEncryptionRejectsWrongKeyTamperAndCrossActorSwap(t *testing.T) {
	store, db := openTestStore(t)
	key := bytes.Repeat([]byte{0x31}, 32)
	if err := store.SetContentStorageKey("unit-key", key); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	ctx := contentPrincipalContext(testTenant, contentTestActorID, identity.TokenTypeDelegated, true, false)
	input := sampleContentIngest()
	reservation := reserveContentIngest(t, store, ctx, "encrypted-source-tamper-01", input, now)
	result, err := store.FinalizeContentIngest(ctx, reservation, input, syntheticContentRetention(now), "audit-encrypted-tamper", now)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := wrong.SetContentStorageKey("unit-key", bytes.Repeat([]byte{0x32}, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.GetContentSource(ctx, result.SourceArtifactID); err == nil {
		t.Fatal("wrong key decrypted source")
	}

	var ciphertext []byte
	if err := db.QueryRow(`SELECT source_bytes FROM assurance_content_sources WHERE tenant_id=? AND source_artifact_id=?`, testTenant, result.SourceArtifactID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 1
	if _, err := db.Exec(`UPDATE assurance_content_sources SET source_bytes=? WHERE tenant_id=? AND source_artifact_id=?`, tampered, testTenant, result.SourceArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetContentSource(ctx, result.SourceArtifactID); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
	if _, err := db.Exec(`UPDATE assurance_content_sources SET source_bytes=? WHERE tenant_id=? AND source_artifact_id=?`, ciphertext, testTenant, result.SourceArtifactID); err != nil {
		t.Fatal(err)
	}
	foreignActor := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	if _, err := db.Exec(`UPDATE assurance_content_sources SET actor_id=? WHERE tenant_id=? AND source_artifact_id=?`, foreignActor, testTenant, result.SourceArtifactID); err != nil {
		t.Fatal(err)
	}
	foreignCtx := contentPrincipalContext(testTenant, foreignActor, identity.TokenTypeDelegated, true, false)
	if _, err := store.GetContentSource(foreignCtx, result.SourceArtifactID); err == nil {
		t.Fatal("ciphertext moved across actor binding was accepted")
	}
}

func TestContentSourceEncryptionIsOneTimeAndRejectsPlaintextRowsInEncryptedMode(t *testing.T) {
	store, db := openTestStore(t)
	key := bytes.Repeat([]byte{0x41}, 32)
	if err := store.SetContentStorageKey("unit-key", key); err != nil {
		t.Fatal(err)
	}
	if err := store.SetContentStorageKey("unit-key", key); err == nil {
		t.Fatal("key was silently reconfigured")
	}
	now := time.Now().UTC().Truncate(time.Second)
	ctx := contentPrincipalContext(testTenant, contentTestActorID, identity.TokenTypeDelegated, true, false)
	plain := []byte("historical plaintext source")
	metadata := []byte(`{}`)
	plainID := "src_historical_plaintext"
	_, err := db.Exec(`INSERT INTO assurance_content_sources(tenant_id,source_artifact_id,actor_id,media_type,detected_media_type,filename,source_bytes,source_sha256,metadata_blob,metadata_sha256,expires_at,created_at)
	 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, testTenant, plainID, contentTestActorID, "text/plain", "text/plain", "", plain, digestHex(HashBytes(plain)), metadata, digestHex(HashBytes(metadata)), formatTimestamp(now.Add(time.Hour)), formatTimestamp(now))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetContentSource(ctx, plainID); err == nil {
		t.Fatal("plaintext historical row was silently accepted in encrypted mode")
	}
}
