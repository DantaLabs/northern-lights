package sqlitedb

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigrateAppliesAndRecords(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	ctx := context.Background()

	migrations := []string{
		`CREATE TABLE IF NOT EXISTS t1 (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE IF NOT EXISTS t2 (id INTEGER PRIMARY KEY)`,
	}
	if err := Migrate(ctx, db, "app", migrations); err != nil {
		t.Fatalf("first migrate: %v", err)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE app = 'app'`).Scan(&n); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if n != 2 {
		t.Errorf("recorded %d migrations, want 2", n)
	}

	// Second run applies nothing new.
	if err := Migrate(ctx, db, "app", migrations); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE app = 'app'`).Scan(&n); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if n != 2 {
		t.Errorf("after second run recorded %d migrations, want 2", n)
	}

	// Apps are tracked independently.
	if err := Migrate(ctx, db, "other", migrations[:1]); err != nil {
		t.Fatalf("other app migrate: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count all: %v", err)
	}
	if n != 3 {
		t.Errorf("recorded %d total migrations, want 3", n)
	}
}

func TestMigrateRollbackOnError(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	ctx := context.Background()

	err = Migrate(ctx, db, "bad", []string{`CREATE TABLE ok_table (id INTEGER)`, `THIS IS NOT SQL`})
	if err == nil {
		t.Fatal("expected error for bad migration")
	}
	// The first migration committed, the bad one did not record a version.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE app = 'bad'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("recorded %d migrations, want 1 (only the good one)", n)
	}
}
