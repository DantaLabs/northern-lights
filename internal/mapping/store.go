// Package mapping provides the persistent semantic mapping layer: it binds
// natural-language field names (spreadsheet, sheet, name, A1 range) and
// caches fetched cell values so repeat reads stay fast. Storage is SQLite
// via the pure-Go modernc.org/sqlite driver, so a ":memory:" database works
// for tests and a file path works in production.
package mapping

import (
	"context"
	"database/sql"
	"fmt"
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
	if _, err := db.Exec(migration0001); err != nil {
		db.Close()
		return nil, fmt.Errorf("mapping: migrate: %w", err)
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
func (s *Store) ListSpreadsheets(ctx context.Context) ([]Spreadsheet, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id, s.name, s.region, s.synced_at, sh.id, sh.name
		 FROM spreadsheets s
		 LEFT JOIN sheets sh ON sh.spreadsheet_id = s.id
		 ORDER BY s.id, sh.id`)
	if err != nil {
		return nil, fmt.Errorf("mapping: list spreadsheets: %w", err)
	}
	defer rows.Close()

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

	out := make([]Spreadsheet, 0, len(order))
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

// SearchFields returns fields matching query, ranked: exact name match
// first, then names containing the query, then alias matches last.
func (s *Store) SearchFields(ctx context.Context, query string) ([]Field, error) {
	like := "%" + query + "%"
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+fieldColumns+` FROM fields
		 WHERE name = ? OR name LIKE ? OR aliases LIKE ?
		 ORDER BY CASE
		   WHEN name = ? THEN 0
		   WHEN name LIKE ? THEN 1
		   ELSE 2
		 END, name`,
		query, like, like, query, like)
	if err != nil {
		return nil, fmt.Errorf("mapping: search fields %q: %w", query, err)
	}
	defer rows.Close()

	var out []Field
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
func (s *Store) CacheCells(ctx context.Context, cells []CellValue) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mapping: cache cells: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO snapshots (spreadsheet_id, sheet_id, cell, value, fetched_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(spreadsheet_id, sheet_id, cell) DO UPDATE SET
		   value = excluded.value, fetched_at = excluded.fetched_at`)
	if err != nil {
		return fmt.Errorf("mapping: cache cells: %w", err)
	}
	defer stmt.Close()
	for _, c := range cells {
		if _, err := stmt.ExecContext(ctx, c.SpreadsheetID, c.SheetID, c.Cell, c.Value, formatTime(c.FetchedAt)); err != nil {
			return fmt.Errorf("mapping: cache cell %q: %w", c.Cell, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mapping: cache cells: %w", err)
	}
	return nil
}

// GetCachedCells returns cached cells for a sheet whose fetched_at is
// within maxAge of now. Stale rows are filtered out.
func (s *Store) GetCachedCells(ctx context.Context, spreadsheetID, sheetID string, maxAge time.Duration) ([]CellValue, error) {
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
	defer rows.Close()

	var out []CellValue
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
