package assurance

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

func TestC045C073RawIdempotencyKeyAbsentFromDBWALBackupAndReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forensic.db")
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const rawKey = "RAW-KEY-DO-NOT-STORE-73d50a88"
	request := ReservationRequest{
		ActorID: "caller-asserted", Tool: "workiva_snapshot_report", Action: "capture",
		IdempotencyDigest: DigestIdempotencyKey(rawKey), RequestDigest: HashBytes([]byte("request")),
	}
	owner, err := store.Reserve(ctx, request, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SealReservation(ctx, owner.RecordID, owner.OwnerNonce, []byte(`{"status":"failed","snapshot_id":""}`), "failed", "", "audit", time.Now()); err != nil {
		t.Fatal(err)
	}
	replay, err := store.Reserve(ctx, request, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(replay.Envelope, []byte(rawKey)) {
		t.Fatal("raw key found in replay envelope")
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	mainBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	if err := os.WriteFile(backupPath, mainBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + "-wal", path + "-journal", backupPath} {
		data, err := os.ReadFile(candidate)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(rawKey)) {
			t.Fatalf("raw idempotency key residue found in %s", filepath.Base(candidate))
		}
	}
}
