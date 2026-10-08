package transfer

import (
	"context"
	"database/sql"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokenAbsentFromSQLiteWALBackupAndAudit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assurance.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	s, err := NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	in := testIntent()
	token := "must-never-persist-this-confirm-token"
	if _, err := s.Stage(context.Background(), in, token, "digest-only-key", "request-digest", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(context.Background(), Claim{TenantID: in.TenantID, ActorID: in.ActorID, Permission: in.Permission, TransferID: in.ID, Token: token, LeaseID: "lease", Now: time.Now().UTC(), LeaseUntil: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if _, err := db.Exec(`VACUUM INTO ?`, backup); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm", backup} {
		b, e := os.ReadFile(p)
		if e != nil {
			if os.IsNotExist(e) {
				continue
			}
			t.Fatal(e)
		}
		if strings.Contains(string(b), token) {
			t.Fatalf("confirmation token found in %s", p)
		}
	}
	var details string
	if err := db.QueryRow(`SELECT details FROM transfer_audit_events WHERE tenant_id=? AND transfer_id=?`, in.TenantID, in.ID).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(details, token) {
		t.Fatalf("token found in audit: %s", details)
	}
}
