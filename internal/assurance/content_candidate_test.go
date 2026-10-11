package assurance

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
)

const candidateTestObjectID = "abababab-abab-4bab-8bab-abababababab"

func candidatePrincipal(tenant, object string, tokenType identity.TokenType, permission identity.Permission, fallback bool) context.Context {
	permissions := []identity.Permission(nil)
	if permission != "" {
		permissions = []identity.Permission{permission}
	}
	return identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: tenant, ObjectID: object, Subject: "candidate-test", TokenType: tokenType, Permissions: permissions, UsedSubjectFallback: fallback})
}

func seedCandidateArtifacts(t *testing.T, now time.Time) (*Store, *sql.DB, ContentIngestResult, string) {
	t.Helper()
	store, db := openTestStore(t)
	ctx := contentPrincipalContext(testTenant, candidateTestObjectID, identity.TokenTypeDelegated, true, false)
	input := sampleContentIngest()
	input.Items = append(input.Items, ContentItemInput{LocalID: "context", Text: "FY2025 context", MetadataBLOB: []byte(`{"kind_hint":"text"}`)})
	reservation := reserveContentIngest(t, store, ctx, "candidate-seed-"+t.Name(), input, now)
	result, err := store.FinalizeContentIngest(ctx, reservation, input, syntheticContentRetention(now), "audit-candidate-"+t.Name(), now)
	if err != nil {
		t.Fatalf("seed content candidates: %v", err)
	}
	return store, db, result, result.ItemIDs["revenue"]
}

func TestGetContentCandidateAtReturnsOwnedItemAndDraftCopies(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	store, _, created, itemID := seedCandidateArtifacts(t, now)
	for _, tc := range []struct {
		kind         ContentCandidateKind
		id, wantText string
		refs         int
	}{
		{ContentCandidateExtractedItem, itemID, "12 million USD", 0},
		{ContentCandidateDraftArtifact, created.DraftIDs["summary"], "Revenue was 12 million USD.", 1},
	} {
		ctx := candidatePrincipal(testTenant, candidateTestObjectID, identity.TokenTypeDelegated, identity.PermissionContentStage, false)
		got, err := store.GetContentCandidateAt(ctx, tc.kind, tc.id, now)
		if err != nil {
			t.Fatalf("get %s: %v", tc.kind, err)
		}
		if got.ID != tc.id || got.Kind != tc.kind || got.OriginalText != tc.wantText || len(got.MetadataBLOB) == 0 || got.MetadataSHA256 != digestHex(HashBytes(got.MetadataBLOB)) || got.CandidateSHA256 != digestHex(HashBytes([]byte(tc.wantText))) || got.SourceArtifactID != created.SourceArtifactID || got.SourceSHA256 != created.SourceSHA256 || len(got.ItemIDs) != tc.refs || tc.kind == ContentCandidateDraftArtifact && got.LineageSHA256 == "" || !got.ExpiresAt.Equal(syntheticContentRetention(now)) {
			t.Fatalf("candidate=%+v", got)
		}
		metadataCopy := append([]byte(nil), got.MetadataBLOB...)
		refsCopy := append([]string(nil), got.ItemIDs...)
		got.MetadataBLOB[0] ^= 0xff
		if len(got.ItemIDs) > 0 {
			got.ItemIDs[0] = "mutated"
		}
		again, err := store.GetContentCandidateAt(ctx, tc.kind, tc.id, now)
		if err != nil || !bytes.Equal(again.MetadataBLOB, metadataCopy) || len(again.ItemIDs) != len(refsCopy) || len(refsCopy) > 0 && again.ItemIDs[0] != refsCopy[0] {
			t.Fatalf("returned copy mutated stored candidate: %+v err=%v", again, err)
		}
	}
}

func TestGetContentCandidateAtRequiresDelegatedCanonicalOwnerAndCapability(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	store, _, created, itemID := seedCandidateArtifacts(t, now)
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"application", candidatePrincipal(testTenant, candidateTestObjectID, identity.TokenTypeApplication, identity.PermissionContentStage, false)},
		{"missing capability", candidatePrincipal(testTenant, candidateTestObjectID, identity.TokenTypeDelegated, "", false)},
		{"subject fallback", candidatePrincipal(testTenant, "", identity.TokenTypeDelegated, identity.PermissionContentConfirm, true)},
		{"noncanonical tenant", candidatePrincipal("not-a-uuid", candidateTestObjectID, identity.TokenTypeDelegated, identity.PermissionContentStage, false)},
		{"malformed object", candidatePrincipal(testTenant, "not-a-uuid", identity.TokenTypeDelegated, identity.PermissionContentConfirm, false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.GetContentCandidateAt(tc.ctx, ContentCandidateExtractedItem, itemID, now); err == nil || !strings.HasPrefix(err.Error(), "forbidden:") {
				t.Fatal("unauthorized candidate read succeeded")
			}
		})
	}
	confirm := candidatePrincipal(testTenant, candidateTestObjectID, identity.TokenTypeDelegated, identity.PermissionContentConfirm, false)
	if _, err := store.GetContentCandidateAt(confirm, ContentCandidateDraftArtifact, created.DraftIDs["summary"], now); err != nil {
		t.Fatalf("content.confirm should authorize candidate read: %v", err)
	}
}

func TestGetContentCandidateAtConcealsForeignAndMissingCandidates(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	store, _, _, itemID := seedCandidateArtifacts(t, now)
	for _, tc := range []struct{ name, tenant, object string }{
		{"foreign tenant", "cccccccc-cccc-4ccc-8ccc-cccccccccccc", candidateTestObjectID},
		{"foreign actor", testTenant, "cdcdcdcd-cdcd-4dcd-8dcd-cdcdcdcdcdcd"},
		{"missing", "dddddddd-dddd-4ddd-8ddd-dddddddddddd", candidateTestObjectID},
	} {
		ctx := candidatePrincipal(tc.tenant, tc.object, identity.TokenTypeDelegated, identity.PermissionContentStage, false)
		_, err := store.GetContentCandidateAt(ctx, ContentCandidateExtractedItem, itemID, now)
		if err == nil || !strings.Contains(err.Error(), "content candidate is unavailable") {
			t.Fatalf("%s result error=%v", tc.name, err)
		}
	}
}

func TestGetContentCandidateAtEnforcesSourceExpiryAtExplicitNow(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	store, db, created, itemID := seedCandidateArtifacts(t, now)
	if _, err := db.Exec(`UPDATE assurance_content_sources SET expires_at=? WHERE tenant_id=? AND source_artifact_id=?`, formatTimestamp(now), testTenant, created.SourceArtifactID); err != nil {
		t.Fatal(err)
	}
	ctx := candidatePrincipal(testTenant, candidateTestObjectID, identity.TokenTypeDelegated, identity.PermissionContentStage, false)
	_, err := store.GetContentCandidateAt(ctx, ContentCandidateExtractedItem, itemID, now)
	if err == nil || !strings.Contains(err.Error(), "content candidate is unavailable") {
		t.Fatalf("expiry error=%v", err)
	}
}

func TestGetContentCandidateAtDetectsTamperedStoredContent(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		kind   ContentCandidateKind
		mutate func(*testing.T, *sql.DB, ContentIngestResult, string)
	}{
		{"source bytes", ContentCandidateExtractedItem, func(t *testing.T, db *sql.DB, r ContentIngestResult, item string) {
			_, err := db.Exec(`UPDATE assurance_content_sources SET source_bytes=? WHERE tenant_id=? AND source_artifact_id=?`, []byte("tampered"), testTenant, r.SourceArtifactID)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"source metadata", ContentCandidateExtractedItem, func(t *testing.T, db *sql.DB, r ContentIngestResult, item string) {
			_, err := db.Exec(`UPDATE assurance_content_sources SET metadata_blob=? WHERE tenant_id=? AND source_artifact_id=?`, []byte(`{"tampered":true}`), testTenant, r.SourceArtifactID)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"candidate body", ContentCandidateExtractedItem, func(t *testing.T, db *sql.DB, r ContentIngestResult, item string) {
			_, err := db.Exec(`UPDATE assurance_content_items SET item_text='tampered' WHERE tenant_id=? AND item_id=?`, testTenant, item)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"candidate metadata", ContentCandidateExtractedItem, func(t *testing.T, db *sql.DB, r ContentIngestResult, item string) {
			_, err := db.Exec(`UPDATE assurance_content_items SET metadata_blob=? WHERE tenant_id=? AND item_id=?`, []byte(`{"z":1,"a":2}`), testTenant, item)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"draft duplicate refs", ContentCandidateDraftArtifact, func(t *testing.T, db *sql.DB, r ContentIngestResult, item string) {
			_, err := db.Exec(`UPDATE assurance_content_drafts SET item_ids_json=? WHERE tenant_id=? AND draft_artifact_id=?`, fmt.Sprintf(`["%s","%s"]`, item, item), testTenant, r.DraftIDs["summary"])
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"draft cross-source ref", ContentCandidateDraftArtifact, func(t *testing.T, db *sql.DB, r ContentIngestResult, item string) {
			_, err := db.Exec(`UPDATE assurance_content_drafts SET item_ids_json='["itm_foreign"]' WHERE tenant_id=? AND draft_artifact_id=?`, testTenant, r.DraftIDs["summary"])
			if err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, db, created, itemID := seedCandidateArtifacts(t, now)
			tc.mutate(t, db, created, itemID)
			id := itemID
			if tc.kind == ContentCandidateDraftArtifact {
				id = created.DraftIDs["summary"]
			}
			ctx := candidatePrincipal(testTenant, candidateTestObjectID, identity.TokenTypeDelegated, identity.PermissionContentConfirm, false)
			if _, err := store.GetContentCandidateAt(ctx, tc.kind, id, now); err == nil {
				t.Fatal("tampered candidate was returned")
			}
		})
	}
}

func TestGetContentCandidateAtRejectsDraftReferenceFromDifferentOwnedSource(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	store, db, first, _ := seedCandidateArtifacts(t, now)
	ctx := contentPrincipalContext(testTenant, candidateTestObjectID, identity.TokenTypeDelegated, true, false)
	secondInput := sampleContentIngest()
	secondInput.SourceBytes = []byte("A separate source for reference isolation.")
	reservation := reserveContentIngest(t, store, ctx, "candidate-second-source-"+t.Name(), secondInput, now)
	second, err := store.FinalizeContentIngest(ctx, reservation, secondInput, syntheticContentRetention(now), "audit-candidate-second-"+t.Name(), now)
	if err != nil {
		t.Fatal(err)
	}
	foreignItemID := second.ItemIDs["revenue"]
	if _, err := db.Exec(`UPDATE assurance_content_drafts SET item_ids_json=? WHERE tenant_id=? AND draft_artifact_id=?`, fmt.Sprintf(`["%s"]`, foreignItemID), testTenant, first.DraftIDs["summary"]); err != nil {
		t.Fatal(err)
	}
	reader := candidatePrincipal(testTenant, candidateTestObjectID, identity.TokenTypeDelegated, identity.PermissionContentConfirm, false)
	if _, err := store.GetContentCandidateAt(reader, ContentCandidateDraftArtifact, first.DraftIDs["summary"], now); err == nil {
		t.Fatal("draft referencing another owned source was returned")
	}
}

func TestGetContentCandidateAtDetectsSwitchToAnotherValidOwnedItem(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	store, db, created, _ := seedCandidateArtifacts(t, now)
	validOtherItemID := created.ItemIDs["context"]
	if _, err := db.Exec(`UPDATE assurance_content_drafts SET item_ids_json=? WHERE tenant_id=? AND draft_artifact_id=?`, fmt.Sprintf(`["%s"]`, validOtherItemID), testTenant, created.DraftIDs["summary"]); err != nil {
		t.Fatal(err)
	}
	ctx := candidatePrincipal(testTenant, candidateTestObjectID, identity.TokenTypeDelegated, identity.PermissionContentStage, false)
	if _, err := store.GetContentCandidateAt(ctx, ContentCandidateDraftArtifact, created.DraftIDs["summary"], now); err == nil {
		t.Fatal("draft with changed but valid owned-item reference was returned")
	}
}
