// Package mapping provides the persistent semantic mapping layer: it binds
// natural-language field names (spreadsheet, sheet, name, A1 range) and
// caches fetched cell values so repeat reads stay fast. Storage is SQLite
// via the pure-Go modernc.org/sqlite driver, so a ":memory:" database works
// for tests and a file path works in production.
package mapping

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

// migration0001 creates the mapping schema. The exact column layout is part
// of the public contract; newer migrations must be additive.
const migration0001 = `
CREATE TABLE IF NOT EXISTS spreadsheets (id TEXT PRIMARY KEY, name TEXT, region TEXT, synced_at DATETIME);
CREATE TABLE IF NOT EXISTS sheets (id TEXT, spreadsheet_id TEXT REFERENCES spreadsheets(id), name TEXT, PRIMARY KEY (id, spreadsheet_id));
CREATE TABLE IF NOT EXISTS fields (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  spreadsheet_id TEXT NOT NULL, sheet_id TEXT NOT NULL,
  name TEXT NOT NULL,
  aliases TEXT DEFAULT '',
  cell_range TEXT NOT NULL,
  field_type TEXT DEFAULT 'text',
  description TEXT DEFAULT '',
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE (spreadsheet_id, sheet_id, name)
);
CREATE INDEX IF NOT EXISTS idx_fields_name ON fields (name);
CREATE TABLE IF NOT EXISTS snapshots (
  spreadsheet_id TEXT, sheet_id TEXT, cell TEXT,
  value TEXT, fetched_at DATETIME,
  PRIMARY KEY (spreadsheet_id, sheet_id, cell)
);
`

// migration0002 adds the pending writes table used by the two-phase
// write confirmation flow of workiva_update_field. It is additive:
// CREATE TABLE IF NOT EXISTS, no changes to existing tables.
const migration0002 = `
CREATE TABLE IF NOT EXISTS pending_writes (
  token TEXT PRIMARY KEY,
  field_id INTEGER NOT NULL,
  field_name TEXT NOT NULL,
  value TEXT NOT NULL,
  created_at DATETIME NOT NULL
);
`

// migration0003 binds confirmation tokens to the exact mapped target. The
// empty defaults keep rows created before this migration consumable and safe:
// confirmation validation will reject them until they are restaged.
const migration0003 = `
ALTER TABLE pending_writes ADD COLUMN spreadsheet_id TEXT NOT NULL DEFAULT '';
ALTER TABLE pending_writes ADD COLUMN sheet_id TEXT NOT NULL DEFAULT '';
ALTER TABLE pending_writes ADD COLUMN cell_range TEXT NOT NULL DEFAULT '';
`

// migration0004 rebuilds every mapping-owned table with tenant-scoped keys.
// SQLite DDL is transactional, so either all copied legacy rows and table
// swaps commit together or none of them do.
var migration0004 = strings.ReplaceAll(`
CREATE TABLE spreadsheets_v4 (
  tenant_id TEXT NOT NULL, id TEXT NOT NULL, name TEXT, region TEXT, synced_at DATETIME,
  PRIMARY KEY (tenant_id, id)
);
CREATE TABLE sheets_v4 (
  tenant_id TEXT NOT NULL, id TEXT NOT NULL, spreadsheet_id TEXT NOT NULL, name TEXT,
  PRIMARY KEY (tenant_id, id, spreadsheet_id)
);
CREATE TABLE fields_v4 (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id TEXT NOT NULL,
  spreadsheet_id TEXT NOT NULL, sheet_id TEXT NOT NULL,
  name TEXT NOT NULL,
  aliases TEXT DEFAULT '',
  cell_range TEXT NOT NULL,
  field_type TEXT DEFAULT 'text',
  description TEXT DEFAULT '',
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE (tenant_id, spreadsheet_id, sheet_id, name)
);
CREATE TABLE snapshots_v4 (
  tenant_id TEXT NOT NULL, spreadsheet_id TEXT, sheet_id TEXT, cell TEXT,
  value TEXT, fetched_at DATETIME,
  PRIMARY KEY (tenant_id, spreadsheet_id, sheet_id, cell)
);
CREATE TABLE pending_writes_v4 (
  tenant_id TEXT NOT NULL, token TEXT NOT NULL,
  field_id INTEGER NOT NULL, field_name TEXT NOT NULL, value TEXT NOT NULL,
  spreadsheet_id TEXT NOT NULL DEFAULT '', sheet_id TEXT NOT NULL DEFAULT '',
  cell_range TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL,
  PRIMARY KEY (tenant_id, token)
);

INSERT INTO spreadsheets_v4 SELECT '{{legacy}}', id, name, region, synced_at FROM spreadsheets;
INSERT INTO sheets_v4 SELECT '{{legacy}}', id, spreadsheet_id, name FROM sheets;
INSERT INTO fields_v4 (id, tenant_id, spreadsheet_id, sheet_id, name, aliases, cell_range, field_type, description, updated_at)
  SELECT id, '{{legacy}}', spreadsheet_id, sheet_id, name, aliases, cell_range, field_type, description, updated_at FROM fields;
INSERT INTO snapshots_v4 SELECT '{{legacy}}', spreadsheet_id, sheet_id, cell, value, fetched_at FROM snapshots;
INSERT INTO pending_writes_v4 SELECT '{{legacy}}', token, field_id, field_name, value, spreadsheet_id, sheet_id, cell_range, created_at FROM pending_writes;

DROP TABLE pending_writes;
DROP TABLE snapshots;
DROP TABLE fields;
DROP TABLE sheets;
DROP TABLE spreadsheets;
ALTER TABLE spreadsheets_v4 RENAME TO spreadsheets;
ALTER TABLE sheets_v4 RENAME TO sheets;
ALTER TABLE fields_v4 RENAME TO fields;
ALTER TABLE snapshots_v4 RENAME TO snapshots;
ALTER TABLE pending_writes_v4 RENAME TO pending_writes;
CREATE INDEX idx_fields_tenant_name ON fields (tenant_id, name);
`, "{{legacy}}", identity.LegacyTenantID)

// ErrPendingWriteExpired is returned by ConsumePendingWriteFor when a pending
// write exists but is older than the allowed age. The expired row is
// deleted as part of the consume.
var ErrPendingWriteExpired = errors.New("mapping: pending write expired")

// ErrPendingWriteBindingMismatch is returned without consuming the row when
// the authenticated actor or required permission differs from the staging
// bindings. Rows staged without bindings (migrated from the pre-binding
// schema) always fail closed with this error and must be restaged.
var ErrPendingWriteBindingMismatch = errors.New("mapping: pending write binding mismatch")

// ErrPendingWriteIntegrity is returned without consuming a row whose exact
// staged value no longer matches its persisted digest.
var ErrPendingWriteIntegrity = errors.New("mapping: pending write integrity check failed")

// timeFormat is the storage format for DATETIME values. It sorts
// lexicographically in chronological order, which lets SQL comparisons
// double as time comparisons.
const timeFormat = "2006-01-02 15:04:05"

func formatTime(t time.Time) string {
	return t.UTC().Format(timeFormat)
}

func tokenDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Spreadsheet is a mapped Workiva spreadsheet.
type Spreadsheet struct {
	ID       string
	Name     string
	Region   string
	SyncedAt time.Time
	// Sheets lists the sheets belonging to this spreadsheet, if any.
	Sheets []Sheet
}

// Sheet is a mapped Workiva sheet within a spreadsheet.
type Sheet struct {
	ID            string
	SpreadsheetID string
	Name          string
}

// Field is a semantic mapping from a natural-language name to an A1 range.
type Field struct {
	ID            int64
	SpreadsheetID string
	SheetID       string
	Name          string
	// Aliases holds comma-separated natural-language aliases.
	Aliases   string
	CellRange string
	FieldType string
	// Description explains the field to MCP clients (and their LLMs).
	Description string
}

// CellValue is a cached cell read from a sheet.
type CellValue struct {
	SpreadsheetID string
	SheetID       string
	Cell          string
	Value         string
	FetchedAt     time.Time
}

// Store wraps the SQLite handle holding mappings and the cell cache.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies
// the schema migration. Use ":memory:" for an ephemeral database.
// File-backed databases enable WAL and a busy timeout so the store can
// share one file with the audit log.
func Open(path string) (*Store, error) {
	if err := sqlitedb.PreparePrivateDatabase(path); err != nil {
		return nil, fmt.Errorf("mapping: %w", err)
	}
	db, err := sql.Open("sqlite", sqlitedb.SharedFileDSN(path))
	if err != nil {
		return nil, fmt.Errorf("mapping: open %q: %w", path, err)
	}
	// SQLite allows one writer at a time; a single connection avoids
	// SQLITE_BUSY errors on concurrent writes while keeping reads simple.
	db.SetMaxOpenConns(1)
	if migrateErr := sqlitedb.Migrate(context.Background(), db, "mapping", []string{migration0001, migration0002, migration0003, migration0004}); migrateErr != nil {
		if closeErr := db.Close(); closeErr != nil {
			migrateErr = errors.Join(migrateErr, fmt.Errorf("mapping: close after migrate failure: %w", closeErr))
		}
		return nil, fmt.Errorf("mapping: migrate: %w", migrateErr)
	}
	if migrateErr := migratePendingWriteSecurity(context.Background(), db); migrateErr != nil {
		if closeErr := db.Close(); closeErr != nil {
			migrateErr = errors.Join(migrateErr, fmt.Errorf("mapping: close after pending-write migrate failure: %w", closeErr))
		}
		return nil, fmt.Errorf("mapping: migrate: %w", migrateErr)
	}
	return &Store{db: db}, nil
}

// migratePendingWriteSecurity first hashes every pre-Wave-2 raw token inside
// the same transaction that rebuilds the table and records migration version
// 5. A separate version-6 marker records completion of the non-transactional
// forensic scrub, so a crash or failure after version 5 always retries it.
func migratePendingWriteSecurity(ctx context.Context, db *sql.DB) error {
	if err := migratePendingWriteSchema(ctx, db); err != nil {
		return err
	}
	return scrubPendingWriteResidue(ctx, db)
}

func migratePendingWriteSchema(ctx context.Context, db *sql.DB) (err error) {
	var exists int
	err = db.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations WHERE app='mapping' AND version=5`).Scan(&exists)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("check pending-write migration: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin pending-write migration: %w", err)
	}
	defer func() {
		if err != nil {
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback pending-write migration: %w", rollbackErr))
			}
		}
	}()
	if _, err = tx.ExecContext(ctx, `CREATE TABLE pending_writes_v5 (
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
		return fmt.Errorf("create pending-write replacement: %w", err)
	}
	type legacyPending struct {
		tenant, token, fieldName, value, spreadsheet, sheet, cellRange string
		fieldID                                                        int64
		created                                                        time.Time
	}
	rows, err := tx.QueryContext(ctx, `SELECT tenant_id, token, field_id, field_name, value, spreadsheet_id, sheet_id, cell_range, created_at FROM pending_writes`)
	if err != nil {
		return fmt.Errorf("read legacy pending writes: %w", err)
	}
	var pending []legacyPending
	for rows.Next() {
		var row legacyPending
		if err = rows.Scan(&row.tenant, &row.token, &row.fieldID, &row.fieldName, &row.value, &row.spreadsheet, &row.sheet, &row.cellRange, &row.created); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan legacy pending write: %w", err)
		}
		pending = append(pending, row)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read legacy pending writes: %w", err)
	}
	if err = rows.Close(); err != nil {
		return fmt.Errorf("close legacy pending writes: %w", err)
	}
	for _, row := range pending {
		if _, err = tx.ExecContext(ctx, `INSERT INTO pending_writes_v5
 (tenant_id, token_digest, actor, required_permission, field_id, field_name, value, value_digest, spreadsheet_id, sheet_id, cell_range, created_at, expires_at)
 VALUES (?, ?, '', '', ?, ?, ?, ?, ?, ?, ?, ?, ?)`, row.tenant, tokenDigest(row.token), row.fieldID, row.fieldName, row.value,
			tokenDigest(row.value), row.spreadsheet, row.sheet, row.cellRange, formatTime(row.created), formatTime(row.created.Add(5*time.Minute))); err != nil {
			return fmt.Errorf("copy legacy pending write: %w", err)
		}
	}
	if _, err = tx.ExecContext(ctx, `DROP TABLE pending_writes; ALTER TABLE pending_writes_v5 RENAME TO pending_writes`); err != nil {
		return fmt.Errorf("replace pending writes: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(app,version) VALUES('mapping',5)`); err != nil {
		return fmt.Errorf("record pending-write migration: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit pending-write migration: %w", err)
	}
	return nil
}

// scrubPendingWriteResidue rebuilds the database and then truncates the WAL.
// Both operations must finish before version 6 is recorded. If either fails
// (including a busy checkpoint), startup fails and the absent marker causes a
// retry on the next Open. Recording the marker may create a fresh WAL frame,
// but only after the residue-bearing WAL has been truncated.
func scrubPendingWriteResidue(ctx context.Context, db *sql.DB) error {
	var exists int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations WHERE app='mapping' AND version=6`).Scan(&exists)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("check pending-write scrub migration: %w", err)
	}
	if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum after pending-write migration: %w", err)
	}
	var busy, logFrames, checkpointed int
	if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint after pending-write migration: %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("checkpoint after pending-write migration: busy=%d log=%d checkpointed=%d", busy, logFrames, checkpointed)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(app,version) VALUES('mapping',6)`); err != nil {
		return fmt.Errorf("record pending-write scrub migration: %w", err)
	}
	return nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// Ping verifies that the mapping database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// UpsertSpreadsheet inserts or updates a spreadsheet by ID.
func (s *Store) UpsertSpreadsheet(ctx context.Context, sp Spreadsheet) error {
	tenant := identity.StorageTenant(ctx)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO spreadsheets (tenant_id, id, name, region, synced_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, id) DO UPDATE SET name = excluded.name, region = excluded.region, synced_at = excluded.synced_at`,
		tenant, sp.ID, sp.Name, sp.Region, formatTime(sp.SyncedAt))
	if err != nil {
		return fmt.Errorf("mapping: upsert spreadsheet %q: %w", sp.ID, err)
	}
	return nil
}

// UpsertSheet inserts or updates a sheet by (id, spreadsheet_id).
func (s *Store) UpsertSheet(ctx context.Context, sh Sheet) error {
	tenant := identity.StorageTenant(ctx)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sheets (tenant_id, id, spreadsheet_id, name) VALUES (?, ?, ?, ?)
		 ON CONFLICT(tenant_id, id, spreadsheet_id) DO UPDATE SET name = excluded.name`,
		tenant, sh.ID, sh.SpreadsheetID, sh.Name)
	if err != nil {
		return fmt.Errorf("mapping: upsert sheet %q in %q: %w", sh.ID, sh.SpreadsheetID, err)
	}
	return nil
}

// ListSpreadsheets returns all mapped spreadsheets with their sheets.
func (s *Store) ListSpreadsheets(ctx context.Context) (out []Spreadsheet, err error) {
	tenant := identity.StorageTenant(ctx)
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id, s.name, s.region, s.synced_at, sh.id, sh.name
		 FROM spreadsheets s
		 LEFT JOIN sheets sh ON sh.tenant_id = s.tenant_id AND sh.spreadsheet_id = s.id
		 WHERE s.tenant_id = ?
		 ORDER BY s.id, sh.id`, tenant)
	if err != nil {
		return nil, fmt.Errorf("mapping: list spreadsheets: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("mapping: list spreadsheets: close rows: %w", closeErr))
		}
	}()

	byID := map[string]*Spreadsheet{}
	var order []string
	for rows.Next() {
		var (
			sp        Spreadsheet
			syncedAt  sql.NullTime
			sheetID   sql.NullString
			sheetName sql.NullString
		)
		if err := rows.Scan(&sp.ID, &sp.Name, &sp.Region, &syncedAt, &sheetID, &sheetName); err != nil {
			return nil, fmt.Errorf("mapping: scan spreadsheet: %w", err)
		}
		if syncedAt.Valid {
			sp.SyncedAt = syncedAt.Time
		}
		entry, ok := byID[sp.ID]
		if !ok {
			entry = &sp
			byID[sp.ID] = entry
			order = append(order, sp.ID)
		}
		if sheetID.Valid {
			entry.Sheets = append(entry.Sheets, Sheet{
				ID:            sheetID.String,
				SpreadsheetID: sp.ID,
				Name:          sheetName.String,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mapping: list spreadsheets: %w", err)
	}

	out = make([]Spreadsheet, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

// ResourceOwnership reports whether the current storage tenant owns the exact
// spreadsheet/sheet pair and whether any different tenant owns that pair.
// Ownership is asserted by any mapping row for the spreadsheet: a
// spreadsheets-only row (no sheets or fields yet) still owns the resource.
// It never returns an owning tenant identifier to callers.
func (s *Store) ResourceOwnership(ctx context.Context, spreadsheetID, sheetID string) (owned, foreign bool, err error) {
	tenant := identity.StorageTenant(ctx)
	rows, err := s.db.QueryContext(ctx,
		`SELECT tenant_id FROM spreadsheets WHERE id = ?
		 UNION SELECT tenant_id FROM sheets WHERE spreadsheet_id = ? AND id = ?
		 UNION SELECT tenant_id FROM fields WHERE spreadsheet_id = ? AND sheet_id = ?`,
		spreadsheetID,
		spreadsheetID, sheetID,
		spreadsheetID, sheetID)
	if err != nil {
		return false, false, fmt.Errorf("mapping: resource ownership: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("mapping: resource ownership: close rows: %w", closeErr))
		}
	}()
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return false, false, fmt.Errorf("mapping: resource ownership: scan: %w", err)
		}
		if owner == tenant {
			owned = true
		} else {
			foreign = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, false, fmt.Errorf("mapping: resource ownership: %w", err)
	}
	return owned, foreign, nil
}

// UpsertField inserts a field or updates the existing row with the same
// (spreadsheet_id, sheet_id, name) triple. It returns the stored field
// including its assigned ID.
func (s *Store) UpsertField(ctx context.Context, f Field) (Field, error) {
	tenant := identity.StorageTenant(ctx)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO fields (tenant_id, spreadsheet_id, sheet_id, name, aliases, cell_range, field_type, description)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, spreadsheet_id, sheet_id, name) DO UPDATE SET
		   aliases = excluded.aliases, cell_range = excluded.cell_range,
		   field_type = excluded.field_type, description = excluded.description,
		   updated_at = CURRENT_TIMESTAMP`,
		tenant, f.SpreadsheetID, f.SheetID, f.Name, f.Aliases, f.CellRange, f.FieldType, f.Description)
	if err != nil {
		return Field{}, fmt.Errorf("mapping: upsert field %q: %w", f.Name, err)
	}
	if f.ID == 0 {
		if id, err := res.LastInsertId(); err == nil {
			f.ID = id
		}
	}
	return f, nil
}

// SyncMapping atomically upserts the spreadsheet, sheet, and complete field
// batch produced by one mapping sync.
func (s *Store) SyncMapping(ctx context.Context, sp Spreadsheet, sh Sheet, fields []Field) (err error) {
	tenant := identity.StorageTenant(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mapping: begin sync: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, `INSERT INTO spreadsheets (tenant_id, id, name, region, synced_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT(tenant_id, id) DO UPDATE SET name=excluded.name, region=excluded.region, synced_at=excluded.synced_at`, tenant, sp.ID, sp.Name, sp.Region, formatTime(sp.SyncedAt)); err != nil {
		return fmt.Errorf("mapping: sync spreadsheet %q: %w", sp.ID, err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sheets (tenant_id, id, spreadsheet_id, name) VALUES (?, ?, ?, ?) ON CONFLICT(tenant_id, id, spreadsheet_id) DO UPDATE SET name=excluded.name`, tenant, sh.ID, sh.SpreadsheetID, sh.Name); err != nil {
		return fmt.Errorf("mapping: sync sheet %q: %w", sh.ID, err)
	}
	for _, f := range fields {
		if _, err = tx.ExecContext(ctx, `INSERT INTO fields (tenant_id, spreadsheet_id, sheet_id, name, aliases, cell_range, field_type, description) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(tenant_id, spreadsheet_id, sheet_id, name) DO UPDATE SET aliases=excluded.aliases, cell_range=excluded.cell_range, field_type=excluded.field_type, description=excluded.description, updated_at=CURRENT_TIMESTAMP`, tenant, f.SpreadsheetID, f.SheetID, f.Name, f.Aliases, f.CellRange, f.FieldType, f.Description); err != nil {
			return fmt.Errorf("mapping: sync field %q: %w", f.Name, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("mapping: commit sync: %w", err)
	}
	return nil
}

const fieldColumns = `id, spreadsheet_id, sheet_id, name, aliases, cell_range, field_type, description`

func scanField(rows interface {
	Scan(dest ...any) error
}) (Field, error) {
	var f Field
	err := rows.Scan(&f.ID, &f.SpreadsheetID, &f.SheetID, &f.Name, &f.Aliases, &f.CellRange, &f.FieldType, &f.Description)
	return f, err
}

// GetField returns the field with the exact given name, or (nil, nil) when
// no field matches.
func (s *Store) GetField(ctx context.Context, name string) (*Field, error) {
	tenant := identity.StorageTenant(ctx)
	row := s.db.QueryRowContext(ctx, `SELECT `+fieldColumns+` FROM fields WHERE tenant_id = ? AND name = ?`, tenant, name)
	f, err := scanField(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mapping: get field %q: %w", name, err)
	}
	return &f, nil
}

// escapeLike escapes LIKE wildcard characters so user input is matched
// literally. The backslash is the escape character used with ESCAPE '\'.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// SearchFields returns fields matching query, ranked: exact name match
// first, then names containing the query, then alias matches last.
// Wildcard characters in the query are matched literally. If the raw phrase
// does not match, a normalized snake_case form is tried so natural language
// such as "scope 2 energy" can match synced names like scope_2_energy_kwh.
func (s *Store) SearchFields(ctx context.Context, query string) (out []Field, err error) {
	out, err = s.searchFieldsLike(ctx, query)
	if err != nil || len(out) > 0 {
		return out, err
	}
	if normalized := normalizeSearchKey(query); normalized != "" && normalized != query && strings.Contains(query, " ") {
		return s.searchFieldsLike(ctx, normalized)
	}
	return out, nil
}

func (s *Store) searchFieldsLike(ctx context.Context, query string) (out []Field, err error) {
	tenant := identity.StorageTenant(ctx)
	like := "%" + escapeLike(query) + "%"
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+fieldColumns+` FROM fields
		 WHERE tenant_id = ? AND (name = ? OR name LIKE ? ESCAPE '\' OR aliases LIKE ? ESCAPE '\')
		 ORDER BY CASE
		   WHEN name = ? THEN 0
		   WHEN name LIKE ? ESCAPE '\' THEN 1
		   ELSE 2
		 END, name`,
		tenant, query, like, like, query, like)
	if err != nil {
		return nil, fmt.Errorf("mapping: search fields %q: %w", query, err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("mapping: search fields %q: close rows: %w", query, closeErr))
		}
	}()

	for rows.Next() {
		f, err := scanField(rows)
		if err != nil {
			return nil, fmt.Errorf("mapping: scan field: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mapping: search fields %q: %w", query, err)
	}
	return out, nil
}

func normalizeSearchKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastUnderscore := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if b.Len() > 0 && !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}

// CacheCells upserts cell values into the snapshot cache.
func (s *Store) CacheCells(ctx context.Context, cells []CellValue) (err error) {
	tenant := identity.StorageTenant(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mapping: cache cells: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("mapping: cache cells rollback: %w", rollbackErr))
			}
		}
	}()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO snapshots (tenant_id, spreadsheet_id, sheet_id, cell, value, fetched_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, spreadsheet_id, sheet_id, cell) DO UPDATE SET
		   value = excluded.value, fetched_at = excluded.fetched_at`)
	if err != nil {
		return fmt.Errorf("mapping: cache cells: %w", err)
	}
	defer func() {
		if closeErr := stmt.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("mapping: cache cells close statement: %w", closeErr))
		}
	}()
	for _, c := range cells {
		if _, err := stmt.ExecContext(ctx, tenant, c.SpreadsheetID, c.SheetID, c.Cell, c.Value, formatTime(c.FetchedAt)); err != nil {
			return fmt.Errorf("mapping: cache cell %q: %w", c.Cell, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mapping: cache cells: %w", err)
	}
	committed = true
	return nil
}

// GetCachedCells returns cached cells for a sheet whose fetched_at is
// within maxAge of now. Stale rows are filtered out.
func (s *Store) GetCachedCells(ctx context.Context, spreadsheetID, sheetID string, maxAge time.Duration) (out []CellValue, err error) {
	tenant := identity.StorageTenant(ctx)
	cutoff := formatTime(time.Now().Add(-maxAge))
	rows, err := s.db.QueryContext(ctx,
		`SELECT spreadsheet_id, sheet_id, cell, value, fetched_at
		 FROM snapshots
		 WHERE tenant_id = ? AND spreadsheet_id = ? AND sheet_id = ? AND fetched_at >= ?
		 ORDER BY cell`,
		tenant, spreadsheetID, sheetID, cutoff)
	if err != nil {
		return nil, fmt.Errorf("mapping: get cached cells %s/%s: %w", spreadsheetID, sheetID, err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("mapping: get cached cells %s/%s: close rows: %w", spreadsheetID, sheetID, closeErr))
		}
	}()

	for rows.Next() {
		var (
			c         CellValue
			fetchedAt sql.NullTime
		)
		if err := rows.Scan(&c.SpreadsheetID, &c.SheetID, &c.Cell, &c.Value, &fetchedAt); err != nil {
			return nil, fmt.Errorf("mapping: scan cached cell: %w", err)
		}
		if fetchedAt.Valid {
			c.FetchedAt = fetchedAt.Time
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mapping: get cached cells %s/%s: %w", spreadsheetID, sheetID, err)
	}
	return out, nil
}

// PendingWrite is a staged write awaiting user confirmation in the
// two-phase flow of workiva_update_field. Token is the transient opaque value
// presented by the caller; only its SHA-256 digest is persisted.
type PendingWrite struct {
	Token              string
	Actor              string
	RequiredPermission string
	FieldID            int64
	FieldName          string
	Value              string
	ValueDigest        string
	SpreadsheetID      string
	SheetID            string
	CellRange          string
	CreatedAt          time.Time
	ExpiresAt          time.Time
}

// CreatePendingWrite stores one staged write. Reusing a token replaces
// the previous entry.
func (s *Store) CreatePendingWrite(ctx context.Context, w PendingWrite) error {
	tenant := identity.StorageTenant(ctx)
	// The store derives this binding from the exact staged value rather than
	// trusting a caller-supplied digest.
	w.ValueDigest = tokenDigest(w.Value)
	if w.ExpiresAt.IsZero() {
		w.ExpiresAt = w.CreatedAt.Add(5 * time.Minute)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pending_writes
		 (tenant_id, token_digest, actor, required_permission, field_id, field_name, value, value_digest, spreadsheet_id, sheet_id, cell_range, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, token_digest) DO UPDATE SET
		   actor = excluded.actor, required_permission = excluded.required_permission,
		   field_id = excluded.field_id, field_name = excluded.field_name,
		   value = excluded.value, value_digest = excluded.value_digest, spreadsheet_id = excluded.spreadsheet_id,
		   sheet_id = excluded.sheet_id, cell_range = excluded.cell_range,
		   created_at = excluded.created_at, expires_at = excluded.expires_at`,
		tenant, tokenDigest(w.Token), w.Actor, w.RequiredPermission, w.FieldID, w.FieldName, w.Value, w.ValueDigest,
		w.SpreadsheetID, w.SheetID, w.CellRange, formatTime(w.CreatedAt), formatTime(w.ExpiresAt))
	if err != nil {
		return fmt.Errorf("mapping: create pending write: %w", err)
	}
	return nil
}

// DeleteExpiredPendingWrites removes staged writes older than maxAge and
// returns how many rows were deleted. Called once at server startup so
// abandoned confirmations do not accumulate.
func (s *Store) DeleteExpiredPendingWrites(ctx context.Context, maxAge time.Duration) (int64, error) {
	tenant := identity.StorageTenant(ctx)
	_ = maxAge // retained for API compatibility; each row carries its exact TTL.
	cutoff := formatTime(time.Now().UTC())
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM pending_writes WHERE tenant_id = ? AND expires_at < ?`, tenant, cutoff)
	if err != nil {
		return 0, fmt.Errorf("mapping: delete expired pending writes: %w", err)
	}
	return res.RowsAffected()
}

// DeleteExpiredPendingWritesGlobal removes expired staged writes for every
// tenant and returns how many rows were deleted. Only the startup janitor
// uses it: it runs before any tenant context exists, and a tenant-scoped
// sweep would strand expired rows belonging to other tenants. Expired rows
// are unusable regardless of tenant, so deleting them crosses no trust
// boundary.
func (s *Store) DeleteExpiredPendingWritesGlobal(ctx context.Context, maxAge time.Duration) (int64, error) {
	_ = maxAge // retained for API compatibility; each row carries its exact TTL.
	cutoff := formatTime(time.Now().UTC())
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM pending_writes WHERE expires_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("mapping: delete expired pending writes: %w", err)
	}
	return res.RowsAffected()
}

// ConsumePendingWriteFor atomically validates and consumes one token digest.
// Actor or permission mismatches roll back without deleting the owner's row.
// Rows stored without bindings (migrated from the pre-binding schema) fail
// closed with ErrPendingWriteBindingMismatch and must be restaged.
func (s *Store) ConsumePendingWriteFor(ctx context.Context, token, actor, requiredPermission string, maxAge time.Duration) (write *PendingWrite, err error) {
	tenant := identity.StorageTenant(ctx)
	digest := tokenDigest(token)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("mapping: consume pending write: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("mapping: consume pending write rollback: %w", rollbackErr))
			}
		}
	}()

	var (
		w         PendingWrite
		createdAt time.Time
		expiresAt time.Time
	)
	err = tx.QueryRowContext(ctx,
		`SELECT actor, required_permission, field_id, field_name, value, value_digest, spreadsheet_id, sheet_id, cell_range, created_at, expires_at
		 FROM pending_writes WHERE tenant_id = ? AND token_digest = ?`,
		tenant, digest).Scan(&w.Actor, &w.RequiredPermission, &w.FieldID, &w.FieldName, &w.Value, &w.ValueDigest,
		&w.SpreadsheetID, &w.SheetID, &w.CellRange, &createdAt, &expiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mapping: consume pending write: %w", err)
	}

	w.Token = token
	w.CreatedAt = createdAt
	w.ExpiresAt = expiresAt
	// A row staged without bindings predates the binding schema. No caller
	// may consume it: fail closed so the write must be restaged under the
	// current actor, permission, and target bindings.
	if w.Actor == "" || w.RequiredPermission == "" {
		return nil, ErrPendingWriteBindingMismatch
	}
	if w.Actor != actor || w.RequiredPermission != requiredPermission {
		return nil, ErrPendingWriteBindingMismatch
	}
	if tokenDigest(w.Value) != w.ValueDigest {
		return nil, ErrPendingWriteIntegrity
	}
	expired := !expiresAt.IsZero() && time.Now().UTC().After(expiresAt)
	if expiresAt.IsZero() {
		expired = time.Since(createdAt) > maxAge
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM pending_writes WHERE tenant_id = ? AND token_digest = ?`, tenant, digest)
	if err != nil {
		return nil, fmt.Errorf("mapping: consume pending write: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("mapping: consume pending write: rows affected: %w", err)
	}
	if affected != 1 {
		return nil, fmt.Errorf("mapping: consume pending write: delete affected %d rows, want 1", affected)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("mapping: consume pending write: %w", err)
	}
	committed = true

	if expired {
		return nil, ErrPendingWriteExpired
	}
	return &w, nil
}
