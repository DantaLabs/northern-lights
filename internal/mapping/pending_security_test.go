package mapping

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
)

func TestPendingWritePersistsDigestAndSecurityBindingsWithoutRawToken(t *testing.T) {
	s := openTestStore(t)
	ctx := tenantContext("11111111-1111-1111-1111-111111111111", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	rawToken := "test-only-raw-confirmation-token"
	valueDigest := tokenDigest("approved-value")
	w := PendingWrite{
		Token:              rawToken,
		Actor:              "11111111-1111-1111-1111-111111111111/bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		RequiredPermission: string(identity.PermissionWorkivaWriteConfirm),
		FieldID:            42,
		FieldName:          "energy",
		Value:              "approved-value",
		ValueDigest:        valueDigest,
		SpreadsheetID:      "sp",
		SheetID:            "sh",
		CellRange:          "B3",
		CreatedAt:          time.Now().UTC(),
		ExpiresAt:          time.Now().UTC().Add(5 * time.Minute),
	}
	if err := s.CreatePendingWrite(ctx, w); err != nil {
		t.Fatal(err)
	}

	var digest, actor, permission, storedValueDigest string
	var rawMatches int
	if err := s.db.QueryRow(`SELECT token_digest, actor, required_permission, value_digest,
		CASE WHEN token_digest=? OR field_name=? OR value=? OR spreadsheet_id=? OR sheet_id=? OR cell_range=? THEN 1 ELSE 0 END
		FROM pending_writes`, rawToken, rawToken, rawToken, rawToken, rawToken, rawToken).
		Scan(&digest, &actor, &permission, &storedValueDigest, &rawMatches); err != nil {
		t.Fatal(err)
	}
	if digest != tokenDigest(rawToken) || len(digest) != hex.EncodedLen(32) {
		t.Fatalf("stored token digest = %q", digest)
	}
	if rawMatches != 0 {
		t.Fatal("raw confirmation token appeared in a persisted text field")
	}
	if actor != w.Actor || permission != w.RequiredPermission || storedValueDigest != valueDigest {
		t.Fatalf("stored bindings = actor %q permission %q value digest %q", actor, permission, storedValueDigest)
	}
}

func TestForeignActorCannotConsumeOwnersPendingWrite(t *testing.T) {
	s := openTestStore(t)
	tenant := "11111111-1111-1111-1111-111111111111"
	ctx := tenantContext(tenant, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	bob := tenant + "/bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	alice := tenant + "/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	w := PendingWrite{
		Token:              "bob-token",
		Actor:              bob,
		RequiredPermission: string(identity.PermissionWorkivaWriteConfirm),
		FieldID:            1,
		FieldName:          "energy",
		Value:              "2",
		ValueDigest:        tokenDigest("2"),
		SpreadsheetID:      "sp",
		SheetID:            "sh",
		CellRange:          "B3",
		CreatedAt:          time.Now().UTC(),
		ExpiresAt:          time.Now().UTC().Add(5 * time.Minute),
	}
	if err := s.CreatePendingWrite(ctx, w); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ConsumePendingWriteFor(ctx, w.Token, alice, w.RequiredPermission, 5*time.Minute); err == nil || got != nil {
		t.Fatalf("Alice consumed Bob's token: got=%+v err=%v", got, err)
	}
	got, err := s.ConsumePendingWriteFor(ctx, w.Token, bob, w.RequiredPermission, 5*time.Minute)
	if err != nil || got == nil || got.Actor != bob {
		t.Fatalf("Bob could not consume intact token: got=%+v err=%v", got, err)
	}
}

func TestForeignTenantCannotConsumePendingWrite(t *testing.T) {
	s := openTestStore(t)
	a := tenantContext("11111111-1111-1111-1111-111111111111", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	b := tenantContext("22222222-2222-2222-2222-222222222222", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	w := PendingWrite{Token: "tenant-token", Actor: "actor-a", RequiredPermission: "workiva.write.confirm", FieldID: 1, FieldName: "f", Value: "v", ValueDigest: tokenDigest("v"), CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}
	if err := s.CreatePendingWrite(a, w); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ConsumePendingWriteFor(b, w.Token, w.Actor, w.RequiredPermission, time.Minute); err != nil || got != nil {
		t.Fatalf("foreign tenant result = %+v err=%v, want closed unknown", got, err)
	}
	if got, err := s.ConsumePendingWriteFor(a, w.Token, w.Actor, w.RequiredPermission, time.Minute); err != nil || got == nil {
		t.Fatalf("owner tenant token was not intact: got=%+v err=%v", got, err)
	}
}

func TestPendingValueDigestTamperFailsClosed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	w := PendingWrite{Token: "tamper-token", Actor: "api-key-actor", RequiredPermission: "workiva.write.confirm", FieldID: 1, FieldName: "f", Value: "approved", ValueDigest: tokenDigest("approved"), CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}
	if err := s.CreatePendingWrite(ctx, w); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE pending_writes SET value='tampered'`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ConsumePendingWriteFor(ctx, w.Token, w.Actor, w.RequiredPermission, time.Minute); err == nil || got != nil {
		t.Fatalf("tampered value consumed: got=%+v err=%v", got, err)
	}
}

func TestConsumePendingWriteRejectsZeroRowDelete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	w := PendingWrite{
		Token:              "delete-race-token",
		Actor:              "api-key-actor",
		RequiredPermission: "workiva.write.confirm",
		FieldID:            1,
		FieldName:          "f",
		Value:              "approved",
		CreatedAt:          time.Now(),
		ExpiresAt:          time.Now().Add(time.Minute),
	}
	if err := s.CreatePendingWrite(ctx, w); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TEMP TRIGGER ignore_pending_delete
		BEFORE DELETE ON pending_writes BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatalf("install zero-row delete trigger: %v", err)
	}

	got, err := s.ConsumePendingWriteFor(ctx, w.Token, w.Actor, w.RequiredPermission, time.Minute)
	if got != nil || err == nil || !strings.Contains(err.Error(), "affected 0 rows") {
		t.Fatalf("consume with ignored delete got=%+v err=%v, want zero-row failure", got, err)
	}
	var remaining int
	if err := s.db.QueryRow(`SELECT count(*) FROM pending_writes WHERE token_digest=?`, tokenDigest(w.Token)).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("pending write count=%d after rejected zero-row delete, want 1", remaining)
	}
}
