// Package mapping provides the persistent semantic mapping layer: it binds
// natural-language field names (spreadsheet, sheet, name, A1 range) and
// caches fetched cell values so repeat reads stay fast. Storage is SQLite
// via the pure-Go modernc.org/sqlite driver, so a ":memory:" database works
// for tests and a file path works in production.
package mapping

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

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

// ErrPendingWriteExpired is returned by ConsumePendingWrite when a pending
// write exists but is older than the allowed age. The expired row is
// deleted as part of the consume.
var ErrPendingWriteExpired = errors.New("mapping: pending write expired")

// timeFormat is the storage format for DATETIME values. It sorts
// lexicographically in chronological order, which lets SQL comparisons
// double as time comparisons.
const timeFormat = "2006-01-02 15:04:05"

func formatTime(t time.Time) string {
	return t.UTC().Format(timeFormat)
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
	db, err := sql.Open("sqlite", sqlitedb.SharedFileDSN(path))
	if err != nil {
		return nil, fmt.Errorf("mapping: open %q: %w", path, err)
	}
	// SQLite allows one writer at a time; a single connection avoids
	// SQLITE_BUSY errors on concurrent writes while keeping reads simple.
	db.SetMaxOpenConns(1)
	if migrateErr := sqlitedb.Migrate(context.Background(), db, "mapping", []string{migration0001, migration0002, migration0003}); migrateErr != nil {
		if closeErr := db.Close(); closeErr != nil {
			migrateErr = errors.Join(migrateErr, fmt.Errorf("mapping: close after migrate failure: %w", closeErr))
		}
		return nil, fmt.Errorf("mapping: migrate: %w", migrateErr)
	}
	return &Store{db: db}, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// UpsertSpreadsheet inserts or updates a spreadsheet by ID.
func (s *Store) UpsertSpreadsheet(ctx context.Context, sp Spreadsheet) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO spreadsheets (id, name, region, synced_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET name = excluded.name, region = excluded.region, synced_at = excluded.synced_at`,
		sp.ID, sp.Name, sp.Region, formatTime(sp.SyncedAt))
	if err != nil {
		return fmt.Errorf("mapping: upsert spreadsheet %q: %w", sp.ID, err)
	}
	return nil
}

// UpsertSheet inserts or updates a sheet by (id, spreadsheet_id).
func (s *Store) UpsertSheet(ctx context.Context, sh Sheet) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sheets (id, spreadsheet_id, name) VALUES (?, ?, ?)
		 ON CONFLICT(id, spreadsheet_id) DO UPDATE SET name = excluded.name`,
		sh.ID, sh.SpreadsheetID, sh.Name)
	if err != nil {
		return fmt.Errorf("mapping: upsert sheet %q in %q: %w", sh.ID, sh.SpreadsheetID, err)
	}
	return nil
}

// ListSpreadsheets returns all mapped spreadsheets with their sheets.
func (s *Store) ListSpreadsheets(ctx context.Context) (out []Spreadsheet, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id, s.name, s.region, s.synced_at, sh.id, sh.name
		 FROM spreadsheets s
		 LEFT JOIN sheets sh ON sh.spreadsheet_id = s.id
		 ORDER BY s.id, sh.id`)
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

// UpsertField inserts a field or updates the existing row with the same
// (spreadsheet_id, sheet_id, name) triple. It returns the stored field
// including its assigned ID.
func (s *Store) UpsertField(ctx context.Context, f Field) (Field, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO fields (spreadsheet_id, sheet_id, name, aliases, cell_range, field_type, description)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(spreadsheet_id, sheet_id, name) DO UPDATE SET
		   aliases = excluded.aliases, cell_range = excluded.cell_range,
		   field_type = excluded.field_type, description = excluded.description,
		   updated_at = CURRENT_TIMESTAMP`,
		f.SpreadsheetID, f.SheetID, f.Name, f.Aliases, f.CellRange, f.FieldType, f.Description)
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
	row := s.db.QueryRowContext(ctx, `SELECT `+fieldColumns+` FROM fields WHERE name = ?`, name)
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
// Wildcard characters in the query are matched literally.
func (s *Store) SearchFields(ctx context.Context, query string) (out []Field, err error) {
	like := "%" + escapeLike(query) + "%"
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+fieldColumns+` FROM fields
		 WHERE name = ? OR name LIKE ? ESCAPE '\' OR aliases LIKE ? ESCAPE '\'
		 ORDER BY CASE
		   WHEN name = ? THEN 0
		   WHEN name LIKE ? ESCAPE '\' THEN 1
		   ELSE 2
		 END, name`,
		query, like, like, query, like)
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

// CacheCells upserts cell values into the snapshot cache.
func (s *Store) CacheCells(ctx context.Context, cells []CellValue) (err error) {
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
		`INSERT INTO snapshots (spreadsheet_id, sheet_id, cell, value, fetched_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(spreadsheet_id, sheet_id, cell) DO UPDATE SET
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
		if _, err := stmt.ExecContext(ctx, c.SpreadsheetID, c.SheetID, c.Cell, c.Value, formatTime(c.FetchedAt)); err != nil {
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
	cutoff := formatTime(time.Now().Add(-maxAge))
	rows, err := s.db.QueryContext(ctx,
		`SELECT spreadsheet_id, sheet_id, cell, value, fetched_at
		 FROM snapshots
		 WHERE spreadsheet_id = ? AND sheet_id = ? AND fetched_at >= ?
		 ORDER BY cell`,
		spreadsheetID, sheetID, cutoff)
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
// two-phase flow of workiva_update_field. Token is an opaque identifier
// (a UUID) presented by the caller on the second call.
type PendingWrite struct {
	Token         string
	FieldID       int64
	FieldName     string
	Value         string
	SpreadsheetID string
	SheetID       string
	CellRange     string
	CreatedAt     time.Time
}

// CreatePendingWrite stores one staged write. Reusing a token replaces
// the previous entry.
func (s *Store) CreatePendingWrite(ctx context.Context, w PendingWrite) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pending_writes
		 (token, field_id, field_name, value, spreadsheet_id, sheet_id, cell_range, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(token) DO UPDATE SET
		   field_id = excluded.field_id, field_name = excluded.field_name,
		   value = excluded.value, spreadsheet_id = excluded.spreadsheet_id,
		   sheet_id = excluded.sheet_id, cell_range = excluded.cell_range,
		   created_at = excluded.created_at`,
		w.Token, w.FieldID, w.FieldName, w.Value, w.SpreadsheetID, w.SheetID, w.CellRange, formatTime(w.CreatedAt))
	if err != nil {
		return fmt.Errorf("mapping: create pending write: %w", err)
	}
	return nil
}

// DeleteExpiredPendingWrites removes staged writes older than maxAge and
// returns how many rows were deleted. Called once at server startup so
// abandoned confirmations do not accumulate.
func (s *Store) DeleteExpiredPendingWrites(ctx context.Context, maxAge time.Duration) (int64, error) {
	cutoff := formatTime(time.Now().UTC().Add(-maxAge))
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM pending_writes WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("mapping: delete expired pending writes: %w", err)
	}
	return res.RowsAffected()
}

// ConsumePendingWrite returns the staged write for token and deletes it,
// so every token is single use. An unknown token yields (nil, nil). A
// token whose write is older than maxAge yields (nil,
// ErrPendingWriteExpired); the expired row is deleted as part of the
// consume.
func (s *Store) ConsumePendingWrite(ctx context.Context, token string, maxAge time.Duration) (write *PendingWrite, err error) {
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
	)
	err = tx.QueryRowContext(ctx,
		`SELECT field_id, field_name, value, spreadsheet_id, sheet_id, cell_range, created_at
		 FROM pending_writes WHERE token = ?`,
		token).Scan(&w.FieldID, &w.FieldName, &w.Value, &w.SpreadsheetID, &w.SheetID, &w.CellRange, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mapping: consume pending write: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM pending_writes WHERE token = ?`, token); err != nil {
		return nil, fmt.Errorf("mapping: consume pending write: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("mapping: consume pending write: %w", err)
	}
	committed = true

	w.Token = token
	w.CreatedAt = createdAt
	if time.Since(createdAt) > maxAge {
		return nil, ErrPendingWriteExpired
	}
	return &w, nil
}
