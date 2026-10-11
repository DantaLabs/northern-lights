package assurance

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	_ "modernc.org/sqlite"
)

func TestContentMetadataV19UpgradePreservesLegacyAndVerifiesNewIntakeAfterReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "assurance-v18.sqlite")
	db := openContentMetadataMigrationDB(t, path)
	if err := sqlitedb.Migrate(ctx, db, "assurance", migrations[:18]); err != nil {
		t.Fatalf("install historical v1-v18 schema: %v", err)
	}

	legacyNow := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	legacySource := []byte("legacy source bytes\x00remain exact")
	legacySourceMetadata := []byte(`{"legacy":true,"source":"v18"}`)
	legacyItemText := "legacy extracted value"
	legacyItemMetadata := []byte(`{"kind":"text","legacy":true}`)
	legacyDraftText := "legacy draft text"
	legacyDraftMetadata := []byte(`{"legacy":true,"kind":"draft"}`)
	legacyItemIDs := []byte(`["itm-v18-exact"]`)
	legacyBundleJSON := []byte("{ \"signed\": \"legacy bundle bytes\", \"version\": 8 }\n")
	legacyRows := seedV18ContentRows(t, db, legacyNow, legacySource, legacySourceMetadata, legacyItemText, legacyItemMetadata, legacyDraftText, legacyDraftMetadata, legacyItemIDs, legacyBundleJSON)

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("upgrade v18 to v19: %v", err)
	}
	assertLegacyContentBytesUnchanged(t, db, legacyRows)
	var itemMetadataHash, draftMetadataHash, lineageHash string
	if err := db.QueryRow(`SELECT metadata_sha256 FROM assurance_content_items WHERE tenant_id=? AND item_id=?`, testTenant, "itm-v18-exact").Scan(&itemMetadataHash); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT metadata_sha256,lineage_sha256 FROM assurance_content_drafts WHERE tenant_id=? AND draft_artifact_id=?`, testTenant, "drf-v18-exact").Scan(&draftMetadataHash, &lineageHash); err != nil {
		t.Fatal(err)
	}
	if itemMetadataHash != "" || draftMetadataHash != "" || lineageHash != "" {
		t.Fatalf("v18 row got invented v19 integrity values: item=%q draft=%q lineage=%q", itemMetadataHash, draftMetadataHash, lineageHash)
	}
	store := &Store{db: db}
	legacyOwner := candidatePrincipal(testTenant, contentTestActorID, identity.TokenTypeDelegated, identity.PermissionContentStage, false)
	for _, candidate := range []struct {
		kind ContentCandidateKind
		id   string
	}{{ContentCandidateExtractedItem, "itm-v18-exact"}, {ContentCandidateDraftArtifact, "drf-v18-exact"}} {
		if _, err := store.GetContentCandidateAt(legacyOwner, candidate.kind, candidate.id, legacyNow); err == nil {
			t.Fatalf("unproven historical %s unexpectedly passed candidate integrity checks", candidate.kind)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen the upgraded file and create new records using the normal intake
	// transaction; all their integrity columns must be populated by ingestion.
	db = openContentMetadataMigrationDB(t, path)
	store, err := NewWithDB(db)
	if err != nil {
		t.Fatalf("reopen v19 store: %v", err)
	}
	auditLog, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatalf("reopen audit log: %v", err)
	}
	store.SetAuditLog(auditLog)
	newNow := legacyNow.Add(time.Minute)
	owner := contentPrincipalContext(testTenant, contentTestActorID, identity.TokenTypeDelegated, true, false)
	input := sampleContentIngest()
	reservation := reserveContentIngest(t, store, owner, "v19-new-intake-migration-test", input, newNow)
	created, err := store.FinalizeContentIngest(owner, reservation, input, syntheticContentRetention(newNow), "audit-v19-new-intake", newNow)
	if err != nil {
		t.Fatalf("ingest new post-v19 candidate: %v", err)
	}
	var gotItemMetadataHash, gotDraftMetadataHash, gotLineageHash string
	if err := db.QueryRow(`SELECT metadata_sha256 FROM assurance_content_items WHERE tenant_id=? AND item_id=?`, testTenant, created.ItemIDs["revenue"]).Scan(&gotItemMetadataHash); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT metadata_sha256,lineage_sha256 FROM assurance_content_drafts WHERE tenant_id=? AND draft_artifact_id=?`, testTenant, created.DraftIDs["summary"]).Scan(&gotDraftMetadataHash, &gotLineageHash); err != nil {
		t.Fatal(err)
	}
	itemMetadataCanonical, err := CanonicalJSONBytes(input.Items[0].MetadataBLOB)
	if err != nil {
		t.Fatal(err)
	}
	draftMetadataCanonical, err := CanonicalJSONBytes(input.Drafts[0].MetadataBLOB)
	if err != nil {
		t.Fatal(err)
	}
	if gotItemMetadataHash != digestHex(HashBytes(itemMetadataCanonical)) || gotDraftMetadataHash != digestHex(HashBytes(draftMetadataCanonical)) || gotLineageHash == "" {
		t.Fatalf("new intake integrity hashes item=%q draft=%q lineage=%q", gotItemMetadataHash, gotDraftMetadataHash, gotLineageHash)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db = openContentMetadataMigrationDB(t, path)
	store, err = NewWithDB(db)
	if err != nil {
		t.Fatalf("reopen after new intake: %v", err)
	}
	for _, candidate := range []struct {
		kind ContentCandidateKind
		id   string
	}{{ContentCandidateExtractedItem, created.ItemIDs["revenue"]}, {ContentCandidateDraftArtifact, created.DraftIDs["summary"]}} {
		got, err := store.GetContentCandidateAt(legacyOwner, candidate.kind, candidate.id, newNow)
		if err != nil {
			t.Fatalf("reopened new %s failed validation: %v", candidate.kind, err)
		}
		if got.ID != candidate.id || got.MetadataSHA256 == "" || got.CandidateSHA256 == "" || candidate.kind == ContentCandidateDraftArtifact && got.LineageSHA256 == "" {
			t.Fatalf("reopened candidate lacks its validated hashes: %+v", got)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func openContentMetadataMigrationDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type v18ContentRows struct {
	source, sourceMetadata, itemMetadata, itemText, draftMetadata, draftText, itemIDs, bundleJSON      []byte
	created, expires, sourceHash, sourceMetadataHash, itemHash, draftHash, bundleSignature, bundleHash string
}

func seedV18ContentRows(t *testing.T, db *sql.DB, created time.Time, source, sourceMetadata []byte, itemText string, itemMetadata []byte, draftText string, draftMetadata, itemIDs, bundleJSON []byte) v18ContentRows {
	t.Helper()
	actor := testTenant + "/" + contentTestActorID
	if _, err := db.Exec(`INSERT INTO assurance_content_sources
	 (tenant_id,source_artifact_id,actor_id,media_type,detected_media_type,filename,source_bytes,source_sha256,metadata_blob,metadata_sha256,expires_at,created_at)
	 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, testTenant, "src-v18-exact", actor, "text/plain", "text/plain", "legacy.txt", source, digestHex(HashBytes(source)), sourceMetadata, digestHex(HashBytes(sourceMetadata)), formatTimestamp(created.Add(time.Hour)), formatTimestamp(created)); err != nil {
		t.Fatalf("seed v18 source: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_content_items
	 (tenant_id,item_id,source_artifact_id,actor_id,item_text,item_sha256,metadata_blob,created_at)
	 VALUES(?,?,?,?,?,?,?,?)`, testTenant, "itm-v18-exact", "src-v18-exact", actor, itemText, digestHex(HashBytes([]byte(itemText))), itemMetadata, formatTimestamp(created)); err != nil {
		t.Fatalf("seed v18 item: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_content_drafts
	 (tenant_id,draft_artifact_id,source_artifact_id,actor_id,draft_text,draft_sha256,item_ids_json,metadata_blob,created_at)
	 VALUES(?,?,?,?,?,?,?,?,?)`, testTenant, "drf-v18-exact", "src-v18-exact", actor, draftText, digestHex(HashBytes([]byte(draftText))), string(itemIDs), draftMetadata, formatTimestamp(created)); err != nil {
		t.Fatalf("seed v18 draft: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_active_bundles
	 (tenant_id,singleton,bundle_id,bundle_version,schema_version,bundle_json,signature_hex,content_hash,activated_at)
	 VALUES(?,1,?,?,?,?,?,?,?)`, testTenant, "active-v18-bytes", 8, 1, string(bundleJSON), "signature-v18-exact", "hash-v18-exact", formatTimestamp(created)); err != nil {
		t.Fatalf("seed v18 bundle: %v", err)
	}
	return v18ContentRows{
		source: append([]byte(nil), source...), sourceMetadata: append([]byte(nil), sourceMetadata...), itemText: []byte(itemText), itemMetadata: append([]byte(nil), itemMetadata...),
		draftText: []byte(draftText), draftMetadata: append([]byte(nil), draftMetadata...), itemIDs: append([]byte(nil), itemIDs...), bundleJSON: append([]byte(nil), bundleJSON...),
		created: formatTimestamp(created), expires: formatTimestamp(created.Add(time.Hour)), sourceHash: digestHex(HashBytes(source)), sourceMetadataHash: digestHex(HashBytes(sourceMetadata)),
		itemHash: digestHex(HashBytes([]byte(itemText))), draftHash: digestHex(HashBytes([]byte(draftText))), bundleSignature: "signature-v18-exact", bundleHash: "hash-v18-exact",
	}
}

func assertLegacyContentBytesUnchanged(t *testing.T, db *sql.DB, want v18ContentRows) {
	t.Helper()
	var source, sourceMetadata, itemMetadata, draftMetadata []byte
	var sourceID, actor, mediaType, detectedType, filename, sourceHash, sourceMetadataHash, sourceCreated, sourceExpires string
	if err := db.QueryRow(`SELECT source_artifact_id,actor_id,media_type,detected_media_type,filename,source_bytes,source_sha256,metadata_blob,metadata_sha256,expires_at,created_at FROM assurance_content_sources WHERE tenant_id=? AND source_artifact_id=?`, testTenant, "src-v18-exact").Scan(&sourceID, &actor, &mediaType, &detectedType, &filename, &source, &sourceHash, &sourceMetadata, &sourceMetadataHash, &sourceExpires, &sourceCreated); err != nil {
		t.Fatal(err)
	}
	var itemID, itemSourceID, itemActor, itemText, itemHash, itemCreated string
	if err := db.QueryRow(`SELECT item_id,source_artifact_id,actor_id,item_text,item_sha256,metadata_blob,created_at FROM assurance_content_items WHERE tenant_id=? AND item_id=?`, testTenant, "itm-v18-exact").Scan(&itemID, &itemSourceID, &itemActor, &itemText, &itemHash, &itemMetadata, &itemCreated); err != nil {
		t.Fatal(err)
	}
	var draftID, draftSourceID, draftActor, draftText, draftHash, itemIDs, draftCreated string
	if err := db.QueryRow(`SELECT draft_artifact_id,source_artifact_id,actor_id,draft_text,draft_sha256,item_ids_json,metadata_blob,created_at FROM assurance_content_drafts WHERE tenant_id=? AND draft_artifact_id=?`, testTenant, "drf-v18-exact").Scan(&draftID, &draftSourceID, &draftActor, &draftText, &draftHash, &itemIDs, &draftMetadata, &draftCreated); err != nil {
		t.Fatal(err)
	}
	var bundleID, bundleVersion, schemaVersion, bundleJSON, bundleSignature, bundleHash, bundleActivated string
	if err := db.QueryRow(`SELECT bundle_id,bundle_version,schema_version,bundle_json,signature_hex,content_hash,activated_at FROM assurance_active_bundles WHERE tenant_id=? AND singleton=1`, testTenant).Scan(&bundleID, &bundleVersion, &schemaVersion, &bundleJSON, &bundleSignature, &bundleHash, &bundleActivated); err != nil {
		t.Fatal(err)
	}
	if sourceID != "src-v18-exact" || actor != testTenant+"/"+contentTestActorID || mediaType != "text/plain" || detectedType != "text/plain" || filename != "legacy.txt" || !bytes.Equal(source, want.source) || sourceHash != want.sourceHash || !bytes.Equal(sourceMetadata, want.sourceMetadata) || sourceMetadataHash != want.sourceMetadataHash || sourceExpires != want.expires || sourceCreated != want.created ||
		itemID != "itm-v18-exact" || itemSourceID != "src-v18-exact" || itemActor != actor || !bytes.Equal([]byte(itemText), want.itemText) || itemHash != want.itemHash || !bytes.Equal(itemMetadata, want.itemMetadata) || itemCreated != want.created ||
		draftID != "drf-v18-exact" || draftSourceID != "src-v18-exact" || draftActor != actor || !bytes.Equal([]byte(draftText), want.draftText) || draftHash != want.draftHash || !bytes.Equal([]byte(itemIDs), want.itemIDs) || !bytes.Equal(draftMetadata, want.draftMetadata) || draftCreated != want.created ||
		bundleID != "active-v18-bytes" || bundleVersion != "8" || schemaVersion != "1" || !bytes.Equal([]byte(bundleJSON), want.bundleJSON) || bundleSignature != want.bundleSignature || bundleHash != want.bundleHash || bundleActivated != want.created {
		t.Fatal("v18 source/item/draft or active bundle bytes changed during v19 upgrade")
	}
}
