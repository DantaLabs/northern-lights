package mapping

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

const forensicRawToken = "raw-token-forensic-residue-check-0123456789abcdef"

// createVersion5Database reconstructs the durable state left by a crash after
// the version-5 schema rewrite committed but before any forensic scrub ran.
// The raw token is first checkpointed into the main file, then the real v5
// table shape and marker are committed without VACUUM or a truncating
// checkpoint.
func createVersion5Database(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", sqlitedb.SharedFileDSN(path))
	if err != nil {
		t.Fatalf("open version-5 fixture: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close version-5 fixture: %v", err)
		}
	}()
	if err := sqlitedb.Migrate(ctx, db, "mapping", []string{migration0001, migration0002, migration0003, migration0004}); err != nil {
		t.Fatalf("apply version-4 schema: %v", err)
	}
	created := time.Date(2026, time.September, 24, 1, 2, 3, 0, time.UTC)
	if _, err := db.ExecContext(ctx, `INSERT INTO pending_writes
		(tenant_id, token, field_id, field_name, value, spreadsheet_id, sheet_id, cell_range, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, identity.LegacyTenantID, forensicRawToken, 7, "legacy-field", "8", "legacy-sp", "legacy-sh", "B3", formatTime(created)); err != nil {
		t.Fatalf("insert legacy pending write: %v", err)
	}
	var busy, logFrames, checkpointed int
	if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		t.Fatalf("checkpoint raw-token fixture: %v", err)
	}
	if busy != 0 {
		t.Fatalf("checkpoint raw-token fixture busy=%d log=%d checkpointed=%d", busy, logFrames, checkpointed)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin version-5 fixture: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE pending_writes_v5 (
	  tenant_id TEXT NOT NULL,
	  token_digest TEXT NOT NULL,
	  actor TEXT NOT NULL,
	  required_permission TEXT NOT NULL,
	  field_id INTEGER NOT NULL,
	  field_name TEXT NOT NULL,
	  value TEXT NOT NULL,
	  value_digest TEXT NOT NULL,
	  spreadsheet_id TEXT NOT NULL,
	  sheet_id TEXT NOT NULL,
	  cell_range TEXT NOT NULL,
	  created_at DATETIME NOT NULL,
	  expires_at DATETIME NOT NULL,
	  PRIMARY KEY (tenant_id, token_digest)
	)`); err != nil {
		t.Fatalf("create version-5 replacement: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pending_writes_v5
		(tenant_id, token_digest, actor, required_permission, field_id, field_name, value, value_digest, spreadsheet_id, sheet_id, cell_range, created_at, expires_at)
		VALUES (?, ?, '', '', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		identity.LegacyTenantID, tokenDigest(forensicRawToken), 7, "legacy-field", "8", tokenDigest("8"), "legacy-sp", "legacy-sh", "B3", formatTime(created), formatTime(created.Add(5*time.Minute))); err != nil {
		t.Fatalf("copy version-5 pending write: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE pending_writes; ALTER TABLE pending_writes_v5 RENAME TO pending_writes`); err != nil {
		t.Fatalf("replace version-5 pending writes: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(app,version) VALUES('mapping',5)`); err != nil {
		t.Fatalf("record version-5 fixture: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit version-5 fixture: %v", err)
	}
	committed = true
}

func migrationMarkerCount(t *testing.T, db *sql.DB, version int) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE app='mapping' AND version=?`, version).Scan(&count); err != nil {
		t.Fatalf("read migration marker %d: %v", version, err)
	}
	return count
}

func filesContainingRawToken(t *testing.T, path string) []string {
	t.Helper()
	var found []string
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		data, err := os.ReadFile(candidate)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("read %s: %v", candidate, err)
		}
		if bytes.Contains(data, []byte(forensicRawToken)) {
			found = append(found, filepath.Base(candidate))
		}
	}
	return found
}

func TestPendingWriteScrubRetriesVersion5DatabaseAndMarksCompletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forensic.db")
	createVersion5Database(t, path)
	if found := filesContainingRawToken(t, path); len(found) == 0 {
		t.Fatal("fixture broken: raw token is absent before the forensic scrub")
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open version-5 database: %v", err)
	}
	if got := migrationMarkerCount(t, store.db, 5); got != 1 {
		t.Fatalf("version-5 marker count=%d, want 1", got)
	}
	if got := migrationMarkerCount(t, store.db, 6); got != 1 {
		t.Fatalf("scrub-completion marker count=%d, want 1", got)
	}
	var fieldName, value string
	if err := store.db.QueryRow(`SELECT field_name, value FROM pending_writes WHERE tenant_id=? AND token_digest=?`, identity.LegacyTenantID, tokenDigest(forensicRawToken)).Scan(&fieldName, &value); err != nil {
		t.Fatalf("read preserved legacy pending write: %v", err)
	}
	if fieldName != "legacy-field" || value != "8" {
		t.Fatalf("preserved legacy pending write=%q/%q", fieldName, value)
	}
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(sidecar); err != nil {
			t.Fatalf("expected open WAL sidecar %s: %v", filepath.Base(sidecar), err)
		}
	}
	if found := filesContainingRawToken(t, path); len(found) != 0 {
		t.Fatalf("raw token remains while store is open in %v", found)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close scrubbed store: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen scrubbed database: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if got := migrationMarkerCount(t, reopened.db, 6); got != 1 {
		t.Fatalf("scrub marker count after idempotent reopen=%d, want 1", got)
	}
	if found := filesContainingRawToken(t, path); len(found) != 0 {
		t.Fatalf("raw token reappeared after clean reopen in %v", found)
	}
}

func TestPendingWriteScrubCheckpointBusyLeavesMarkerAbsentAndRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	createVersion5Database(t, path)

	blocker, err := sql.Open("sqlite", sqlitedb.SharedFileDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	blocker.SetMaxOpenConns(1)
	defer func() { _ = blocker.Close() }()
	readTx, err := blocker.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin checkpoint-blocking read: %v", err)
	}
	var rows int
	if err := readTx.QueryRow(`SELECT count(*) FROM pending_writes`).Scan(&rows); err != nil {
		t.Fatalf("establish checkpoint-blocking snapshot: %v", err)
	}

	store, openErr := Open(path)
	if store != nil {
		_ = store.Close()
	}
	if openErr == nil || !strings.Contains(openErr.Error(), "checkpoint") {
		t.Fatalf("Open with checkpoint-blocking reader error=%v, want checkpoint failure", openErr)
	}
	if err := readTx.Rollback(); err != nil {
		t.Fatalf("release checkpoint-blocking read: %v", err)
	}
	if got := migrationMarkerCount(t, blocker, 6); got != 0 {
		t.Fatalf("scrub marker recorded after failed checkpoint: count=%d", got)
	}

	retried, err := Open(path)
	if err != nil {
		t.Fatalf("Open retry after releasing reader: %v", err)
	}
	defer func() { _ = retried.Close() }()
	if got := migrationMarkerCount(t, retried.db, 6); got != 1 {
		t.Fatalf("scrub marker count after successful retry=%d, want 1", got)
	}
	if found := filesContainingRawToken(t, path); len(found) != 0 {
		t.Fatalf("raw token remains after successful retry in %v", found)
	}
}

func TestPendingWriteVersion5CollisionRollsBackDataAndMarker(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "collision.db")
	db, err := sql.Open("sqlite", sqlitedb.SharedFileDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := sqlitedb.Migrate(ctx, db, "mapping", []string{migration0001, migration0002, migration0003, migration0004}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO pending_writes(tenant_id,token,field_id,field_name,value,spreadsheet_id,sheet_id,cell_range,created_at)
		VALUES('legacy-api-key','preserved-token',1,'preserved-field','9','sp','sh','A1',CURRENT_TIMESTAMP);
		CREATE TABLE pending_writes_v5(sentinel TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if store, err := Open(path); err == nil {
		_ = store.Close()
		t.Fatal("Open succeeded despite injected version-5 table collision")
	}
	db, err = sql.Open("sqlite", sqlitedb.SharedFileDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var token, fieldName string
	if err := db.QueryRow(`SELECT token, field_name FROM pending_writes`).Scan(&token, &fieldName); err != nil {
		t.Fatalf("read original pending write: %v", err)
	}
	if token != "preserved-token" || fieldName != "preserved-field" {
		t.Fatalf("original pending write changed to %q/%q", token, fieldName)
	}
	if got := migrationMarkerCount(t, db, 5); got != 0 {
		t.Fatalf("version-5 marker recorded after collision: count=%d", got)
	}
	if got := migrationMarkerCount(t, db, 6); got != 0 {
		t.Fatalf("version-6 marker recorded after version-5 collision: count=%d", got)
	}
}

func TestPendingWriteScrubMarksMemoryDatabase(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open memory database: %v", err)
	}
	defer func() { _ = store.Close() }()
	if got := migrationMarkerCount(t, store.db, 6); got != 1 {
		t.Fatalf("memory scrub marker count=%d, want 1", got)
	}
}
