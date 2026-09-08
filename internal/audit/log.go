// Package audit provides the append-only, hash-chained audit log required
// for EU AI Act Art. 12 record keeping. Every tool call and Workiva mutation
// is one row; each row hashes the previous row's hash, so tampering with any
// single row breaks the chain at that sequence number. The log lives in the
// same SQLite database as the mapping store, but owns its handle so it can
// be opened, verified, and exported standalone (e.g. by an auditor CLI).
package audit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

// genesisHash is the prev_hash of the first row in the chain.
const genesisHash = "GENESIS"

// timeFormat is the storage format for DATETIME values; it also feeds the
// hash chain, so it must stay stable. Lexicographic order equals
// chronological order.
const timeFormat = "2006-01-02 15:04:05"

// migration0001 creates the audit table. The exact column layout is part of
// the public contract; newer migrations must be additive.
const migration0001 = `
CREATE TABLE IF NOT EXISTS audit_log (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  ts DATETIME DEFAULT CURRENT_TIMESTAMP,
  actor TEXT,
  tool TEXT, action TEXT,
  target TEXT,
  before_json TEXT, after_json TEXT,
  workiva_op_url TEXT,
  prev_hash TEXT, hash TEXT
);
`

// Entry is one audit log record. Seq, Ts, PrevHash, and Hash are assigned by
// Append; the caller supplies the semantic fields.
type Entry struct {
	Seq          int64     `json:"seq"`
	Ts           time.Time `json:"ts"`
	Actor        string    `json:"actor"`
	Tool         string    `json:"tool"`
	Action       string    `json:"action"`
	Target       string    `json:"target"`
	BeforeJSON   string    `json:"before_json,omitempty"`
	AfterJSON    string    `json:"after_json,omitempty"`
	WorkivaOpURL string    `json:"workiva_op_url,omitempty"`
	PrevHash     string    `json:"prev_hash"`
	Hash         string    `json:"hash"`
}

// Log wraps the SQLite handle holding the audit chain.
type Log struct {
	db     *sql.DB
	ownsDB bool
}

// Open opens (creating if needed) the SQLite database at path and applies
// the schema migration. Use ":memory:" for an ephemeral log. The Log owns
// the handle; Close releases it. File-backed databases enable WAL and a
// busy timeout so this log can share one file with the mapping store.
func Open(path string) (*Log, error) {
	db, err := sql.Open("sqlite", sqlitedb.SharedFileDSN(path))
	if err != nil {
		return nil, fmt.Errorf("audit: open %q: %w", path, err)
	}
	// Single connection: appends are serialized and SQLite never sees
	// concurrent writers.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(migration0001); err != nil {
		db.Close()
		return nil, fmt.Errorf("audit: migrate: %w", err)
	}
	return &Log{db: db, ownsDB: true}, nil
}

// NewWithDB wraps an existing database handle, ensuring the audit table
// exists. The caller retains ownership of the handle; Close on the returned
// Log is a no-op. Useful when the audit log shares one database (and one
// transaction) with the mapping store.
func NewWithDB(db *sql.DB) (*Log, error) {
	if _, err := db.Exec(migration0001); err != nil {
		return nil, fmt.Errorf("audit: migrate: %w", err)
	}
	return &Log{db: db}, nil
}

// Close releases the underlying database handle when the Log owns it (that
// is, it was created with Open). It is a no-op for logs created with
// NewWithDB.
func (l *Log) Close() error {
	if !l.ownsDB {
		return nil
	}
	return l.db.Close()
}

func entryHash(prevHash, ts, actor, tool, action, target, before, after, opURL string) string {
	sum := sha256.Sum256([]byte(prevHash + ts + actor + tool + action + target + before + after + opURL))
	return hex.EncodeToString(sum[:])
}

// Append records an entry and extends the hash chain. It fills in Ts (when
// zero), Seq, PrevHash, and Hash on the returned copy.
func (l *Log) Append(ctx context.Context, e Entry) (Entry, error) {
	ts := e.Ts
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	tsStr := ts.UTC().Format(timeFormat)

	prev, err := l.latestHash(ctx)
	if err != nil {
		return Entry{}, err
	}
	if prev == "" {
		prev = genesisHash
	}

	hash := entryHash(prev, tsStr, e.Actor, e.Tool, e.Action, e.Target, e.BeforeJSON, e.AfterJSON, e.WorkivaOpURL)
	res, err := l.db.ExecContext(ctx,
		`INSERT INTO audit_log (ts, actor, tool, action, target, before_json, after_json, workiva_op_url, prev_hash, hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tsStr, e.Actor, e.Tool, e.Action, e.Target, e.BeforeJSON, e.AfterJSON, e.WorkivaOpURL, prev, hash)
	if err != nil {
		return Entry{}, fmt.Errorf("audit: append: %w", err)
	}

	e.Ts = ts.UTC()
	e.PrevHash = prev
	e.Hash = hash
	if id, err := res.LastInsertId(); err == nil {
		e.Seq = id
	}
	return e, nil
}

// latestHash returns the hash of the most recent row, or "" for an empty
// log.
func (l *Log) latestHash(ctx context.Context) (string, error) {
	var h sql.NullString
	err := l.db.QueryRowContext(ctx, `SELECT hash FROM audit_log ORDER BY seq DESC LIMIT 1`).Scan(&h)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("audit: read latest hash: %w", err)
	}
	if !h.Valid {
		return "", nil
	}
	return h.String, nil
}

// normalizeTs renders a scanned ts value in the storage format, regardless
// of whether the driver hands back a string or a time.Time.
func normalizeTs(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case time.Time:
		return t.UTC().Format(timeFormat), nil
	case []byte:
		return string(t), nil
	case nil:
		return "", fmt.Errorf("missing ts")
	default:
		return "", fmt.Errorf("unexpected ts type %T", v)
	}
}

const auditColumns = `seq, ts, actor, tool, action, target, before_json, after_json, workiva_op_url, prev_hash, hash`

// scanEntry reads one audit row from an active cursor.
func scanEntry(rows interface {
	Scan(dest ...any) error
}) (Entry, error) {
	var (
		e      Entry
		tsRaw  any
		before sql.NullString
		after  sql.NullString
		opURL  sql.NullString
		actor  sql.NullString
		tool   sql.NullString
		action sql.NullString
		target sql.NullString
		prev   sql.NullString
		hash   sql.NullString
	)
	err := rows.Scan(&e.Seq, &tsRaw, &actor, &tool, &action, &target, &before, &after, &opURL, &prev, &hash)
	if err != nil {
		return Entry{}, err
	}
	ts, err := normalizeTs(tsRaw)
	if err != nil {
		return Entry{}, err
	}
	if t, err := time.ParseInLocation(timeFormat, ts, time.UTC); err == nil {
		e.Ts = t
	}
	if actor.Valid {
		e.Actor = actor.String
	}
	if tool.Valid {
		e.Tool = tool.String
	}
	if action.Valid {
		e.Action = action.String
	}
	if target.Valid {
		e.Target = target.String
	}
	if before.Valid {
		e.BeforeJSON = before.String
	}
	if after.Valid {
		e.AfterJSON = after.String
	}
	if opURL.Valid {
		e.WorkivaOpURL = opURL.String
	}
	if prev.Valid {
		e.PrevHash = prev.String
	}
	if hash.Valid {
		e.Hash = hash.String
	}
	return e, nil
}

// Recent returns up to limit entries, newest first. When target is not
// empty only entries with that exact target (for example a spreadsheet,
// sheet, and range such as "ss-1/sh-1/B3") are returned. A non-positive
// limit behaves like 20.
func (l *Log) Recent(ctx context.Context, limit int, target string) ([]Entry, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := l.db.QueryContext(ctx,
		`SELECT `+auditColumns+` FROM audit_log
		 WHERE ? = '' OR target = ?
		 ORDER BY seq DESC LIMIT ?`,
		target, target, limit)
	if err != nil {
		return nil, fmt.Errorf("audit: recent: %w", err)
	}
	defer rows.Close()

	out := []Entry{}
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("audit: recent: scan row: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: recent: %w", err)
	}
	return out, nil
}

// Verify walks the chain in seq order and recomputes every hash. It returns
// an error identifying the first broken seq, or nil when the chain is
// intact.
func (l *Log) Verify(ctx context.Context) error {
	rows, err := l.db.QueryContext(ctx, `SELECT `+auditColumns+` FROM audit_log ORDER BY seq`)
	if err != nil {
		return fmt.Errorf("audit: verify: %w", err)
	}
	defer rows.Close()

	expectedPrev := genesisHash
	for rows.Next() {
		var (
			e      Entry
			tsRaw  any
			before sql.NullString
			after  sql.NullString
			opURL  sql.NullString
			actor  sql.NullString
			tool   sql.NullString
			action sql.NullString
			target sql.NullString
			prev   sql.NullString
			hash   sql.NullString
		)
		if err := rows.Scan(&e.Seq, &tsRaw, &actor, &tool, &action, &target, &before, &after, &opURL, &prev, &hash); err != nil {
			return fmt.Errorf("audit: verify: scan row: %w", err)
		}
		ts, err := normalizeTs(tsRaw)
		if err != nil {
			return fmt.Errorf("audit: verify seq %d: %w", e.Seq, err)
		}
		if actor.Valid {
			e.Actor = actor.String
		}
		if tool.Valid {
			e.Tool = tool.String
		}
		if action.Valid {
			e.Action = action.String
		}
		if target.Valid {
			e.Target = target.String
		}
		if before.Valid {
			e.BeforeJSON = before.String
		}
		if after.Valid {
			e.AfterJSON = after.String
		}

		if !prev.Valid || prev.String != expectedPrev {
			return fmt.Errorf("audit: chain broken at seq %d: prev_hash = %q, want %q", e.Seq, prev.String, expectedPrev)
		}
		want := entryHash(prev.String, ts, e.Actor, e.Tool, e.Action, e.Target, e.BeforeJSON, e.AfterJSON, e.WorkivaOpURL)
		if !hash.Valid || hash.String != want {
			return fmt.Errorf("audit: chain broken at seq %d: stored hash %q, recomputed %q", e.Seq, hash.String, want)
		}
		expectedPrev = hash.String
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("audit: verify: %w", err)
	}
	return nil
}

// Export writes every entry as one JSON object per line (JSONL), in seq
// order. JSONL is the auditor-facing format required for EU AI Act Art. 12
// review.
func (l *Log) Export(w io.Writer) error {
	rows, err := l.db.Query(`SELECT ` + auditColumns + ` FROM audit_log ORDER BY seq`)
	if err != nil {
		return fmt.Errorf("audit: export: %w", err)
	}
	defer rows.Close()

	enc := json.NewEncoder(w)
	for rows.Next() {
		var (
			e     Entry
			tsRaw any
			before,
			after,
			opURL,
			actor,
			tool,
			action,
			target,
			prev,
			hash sql.NullString
		)
		if err := rows.Scan(&e.Seq, &tsRaw, &actor, &tool, &action, &target, &before, &after, &opURL, &prev, &hash); err != nil {
			return fmt.Errorf("audit: export: scan row: %w", err)
		}
		ts, err := normalizeTs(tsRaw)
		if err != nil {
			return fmt.Errorf("audit: export seq %d: %w", e.Seq, err)
		}
		if t, err := time.ParseInLocation(timeFormat, ts, time.UTC); err == nil {
			e.Ts = t
		}
		if actor.Valid {
			e.Actor = actor.String
		}
		if tool.Valid {
			e.Tool = tool.String
		}
		if action.Valid {
			e.Action = action.String
		}
		if target.Valid {
			e.Target = target.String
		}
		if before.Valid {
			e.BeforeJSON = before.String
		}
		if after.Valid {
			e.AfterJSON = after.String
		}
		if opURL.Valid {
			e.WorkivaOpURL = opURL.String
		}
		if prev.Valid {
			e.PrevHash = prev.String
		}
		if hash.Valid {
			e.Hash = hash.String
		}
		if err := enc.Encode(e); err != nil {
			return fmt.Errorf("audit: export seq %d: %w", e.Seq, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("audit: export: %w", err)
	}
	return nil
}
