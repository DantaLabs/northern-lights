package sqlitedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Migrate applies migrations for app in order, recording each applied
// version in a schema_migrations table. Migrations are applied inside a
// transaction. Migrations must be idempotent (IF NOT EXISTS) so databases
// created before migration tracking upgrade cleanly.
func Migrate(ctx context.Context, db *sql.DB, app string, migrations []string) error {
	if _, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
  app TEXT NOT NULL,
  version INTEGER NOT NULL,
  applied_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (app, version)
)`); err != nil {
		return fmt.Errorf("migrate %s: create schema_migrations: %w", app, err)
	}

	for i, m := range migrations {
		version := i + 1
		var exists int
		err := db.QueryRowContext(ctx,
			`SELECT 1 FROM schema_migrations WHERE app = ? AND version = ?`, app, version).Scan(&exists)
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("migrate %s: check version %d: %w", app, version, err)
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("migrate %s: begin version %d: %w", app, version, err)
		}
		if _, err := tx.ExecContext(ctx, m); err != nil {
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback: %w", rollbackErr))
			}
			return fmt.Errorf("migrate %s: apply version %d: %w", app, version, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (app, version) VALUES (?, ?)`, app, version); err != nil {
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback: %w", rollbackErr))
			}
			return fmt.Errorf("migrate %s: record version %d: %w", app, version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migrate %s: commit version %d: %w", app, version, err)
		}
	}
	return nil
}
