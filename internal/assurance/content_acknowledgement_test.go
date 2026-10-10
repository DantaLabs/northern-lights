package assurance

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
)

func TestContentFormattingHashBindsPresenceAndCanonicalPayload(t *testing.T) {
	formats := json.RawMessage(`{"font":{"weight":400},"color":"#fff"}`)
	effective := json.RawMessage(`{"color":"#fff"}`)
	a, err := ContentFormattingHash(formats, true, effective, true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ContentFormattingHash(json.RawMessage(`{"color":"#fff","font":{"weight":400}}`), true, effective, true)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("object key order changed canonical formatting hash")
	}
	c, err := ContentFormattingHash(json.RawMessage(`null`), true, effective, true)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ContentFormattingHash(nil, false, effective, true)
	if err != nil {
		t.Fatal(err)
	}
	if c == d {
		t.Fatal("omitted and explicit-null formats shared a hash")
	}
	if _, err := ContentFormattingHash(json.RawMessage(`null`), false, effective, true); err == nil {
		t.Fatal("format bytes with absent presence bit accepted")
	}
}

func TestAcknowledgementIntentUsesSignedIntentLifetimeAfterPreviewExpiry(t *testing.T) {
	store, db := openTestStore(t)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close fixture database: %v", err)
		}
	})
	now := time.Date(2026, 10, 10, 12, 5, 0, 0, time.UTC)
	created := now.Add(-5 * time.Minute)
	preview := map[string]any{
		"placement_intent_id": "11111111-1111-4111-8111-111111111111",
		"content_hash":        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"actor_binding_id":    testTenant + "/" + contentTestActor,
		"created_at":          created.Format(time.RFC3339Nano),
		"expires_at":          created.Add(time.Minute).Format(time.RFC3339Nano),
		"intended_value":      json.RawMessage(`"approved"`),
		"destination_profile": map[string]any{
			"destination_profile_id": "destination", "revision": 1,
			"resource_id": "resource", "sheet_id": "sheet", "cell": "B4",
			"content_hash":            "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"intent_lifetime_seconds": 600,
		},
	}
	raw, err := CanonicalJSON(preview)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, decodeErr := decodeAcknowledgementPreview(raw, digestHex(HashBytes(raw)), "11111111-1111-4111-8111-111111111111", testTenant+"/"+contentTestActor); decodeErr != nil || !decoded.CreatedAt.Equal(created) || decoded.Destination.IntentLifetimeSeconds != 600 {
		t.Fatalf("preview decode failed: %#v %v", decoded, decodeErr)
	}
	if _, err := db.Exec(`INSERT INTO assurance_content_placement_intents
		(tenant_id,placement_intent_id,actor_id,preview_json,preview_sha256,token_digest,state,expires_at,created_at)
		VALUES(?,?,?,?,?,?,'machine_verified_visual_ack_pending',?,?)`, testTenant,
		"11111111-1111-4111-8111-111111111111", testTenant+"/"+contentTestActor, raw, digestHex(HashBytes(raw)), strings.Repeat("a", 64),
		created.Add(time.Minute).Format(time.RFC3339Nano), created.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	ctx := contentStageContext(contentTestActor, identity.PermissionContentAcknowledge)
	if _, err := store.GetContentAcknowledgementIntentAt(ctx, "11111111-1111-4111-8111-111111111111", now); err != nil {
		t.Fatalf("intent should remain acknowledgeable after preview/token expiry: %v", err)
	}
	tooLate := created.Add(10*time.Minute + time.Nanosecond)
	if _, err := store.GetContentAcknowledgementIntentAt(ctx, "11111111-1111-4111-8111-111111111111", tooLate); err == nil {
		t.Fatal("intent remained acknowledgeable beyond its signed intent lifetime")
	}
}

func TestAcknowledgementReadbackRecomputesFormattingHash(t *testing.T) {
	formats := json.RawMessage(`{"number_format":"0.00"}`)
	effective := json.RawMessage(`{"number_format":"0.00"}`)
	formatHash, err := ContentFormattingHash(formats, true, effective, true)
	if err != nil {
		t.Fatal(err)
	}
	preview := acknowledgementPreview{
		PlacementIntentID: "11111111-1111-4111-8111-111111111111",
		ContentHash:       "target", ActorBindingID: "actor",
		IntendedValue: json.RawMessage(`123.4500`),
		Destination:   acknowledgementPreviewDestination{ID: "dest", Revision: 1, ResourceID: "res", SheetID: "sheet", Cell: "B4"},
	}
	preview.ProviderMetadata.FormattingSHA256 = formatHash
	observation := map[string]any{
		"spreadsheet_id": "res", "sheet_id": "sheet", "locator": "B4", "raw_value": json.RawMessage(`123.4500`), "value_present": true,
		"formats": formats, "formats_present": true, "effective_formats": effective, "effective_formats_present": true,
		"formatting_sha256": formatHash, "protection": "unprotected", "writable": "writable",
		"provenance": map[string]string{"provider": "workiva_rest", "api_version": "2026-01-01", "endpoint": "GET /spreadsheets/{spreadsheetId}/sheets/{sheetId}/sheetdata", "query_range": "B4", "cache": "bypassed"},
	}
	raw, err := CanonicalJSON(observation)
	if err != nil {
		t.Fatal(err)
	}
	hash := digestHex(HashBytes(raw))
	if err := verifyAcknowledgementReadback(raw, hash, preview); err != nil {
		t.Fatalf("valid readback rejected: %v", err)
	}
	observation["formatting_sha256"] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tampered, err := CanonicalJSON(observation)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyAcknowledgementReadback(tampered, digestHex(HashBytes(tampered)), preview); err == nil {
		t.Fatal("readback accepted a formatting digest not recomputed from payload")
	}
}
