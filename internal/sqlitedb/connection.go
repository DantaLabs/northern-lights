package sqlitedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// Open opens the application's one SQLite handle and owns all connection
// pragmas. Package stores must wrap this handle with NewWithDB rather than open
// independent connections.
func Open(path string) (*sql.DB, error) {
	if err := PreparePrivateDatabase(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", SharedFileDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open shared SQLite database %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	closeOnError := func(cause error) (*sql.DB, error) {
		if closeErr := db.Close(); closeErr != nil {
			cause = errors.Join(cause, fmt.Errorf("close shared SQLite database: %w", closeErr))
		}
		return nil, cause
	}
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		return closeOnError(fmt.Errorf("enable SQLite foreign keys: %w", err))
	}
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout=5000`); err != nil {
		return closeOnError(fmt.Errorf("set SQLite busy timeout: %w", err))
	}
	if path != ":memory:" && !strings.Contains(path, "mode=memory") {
		var mode string
		if err := db.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&mode); err != nil {
			return closeOnError(fmt.Errorf("enable SQLite WAL: %w", err))
		}
		if !strings.EqualFold(mode, "wal") {
			return closeOnError(fmt.Errorf("enable SQLite WAL: journal mode is %q", mode))
		}
	}
	if err := db.PingContext(ctx); err != nil {
		return closeOnError(fmt.Errorf("ping shared SQLite database: %w", err))
	}
	return db, nil
}
