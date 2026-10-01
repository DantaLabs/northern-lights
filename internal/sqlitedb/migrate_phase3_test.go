package sqlitedb_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

func TestC050C069SharedHandleBootstrapsMappingBeforeAuditAndAssuranceWithoutRowLoss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "phase3.db")
	phase2, err := mapping.Open(path)
	if err != nil {
		t.Fatalf("open Phase 2 store: %v", err)
	}
	ctx := context.Background()
	if _, err := phase2.UpsertField(ctx, mapping.Field{
		SpreadsheetID: "sp-1", SheetID: "sh-1", Name: "preserved", CellRange: "B3", FieldType: "number",
	}); err != nil {
		t.Fatalf("seed Phase 2 row: %v", err)
	}
	if err := phase2.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatalf("open shared database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mappingStore, err := mapping.NewWithDB(db)
	if err != nil {
		t.Fatalf("mapping bootstrap: %v", err)
	}
	auditLog, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatalf("audit bootstrap: %v", err)
	}
	assuranceStore, err := assurance.NewWithDB(db)
	if err != nil {
		t.Fatalf("assurance bootstrap: %v", err)
	}

	field, err := mappingStore.GetField(ctx, "preserved")
	if err != nil || field == nil || field.CellRange != "B3" {
		t.Fatalf("preserved Phase 2 row = %#v err=%v", field, err)
	}
	for app, want := range map[string]int{"mapping": 6, "audit": 3, "assurance": 11} {
		var got int
		if err := db.QueryRow(`SELECT max(version) FROM schema_migrations WHERE app=?`, app).Scan(&got); err != nil {
			t.Fatalf("%s migration marker: %v", app, err)
		}
		if got != want {
			t.Fatalf("%s schema version = %d, want %d", app, got, want)
		}
	}
	var foreignKeys int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign_keys=%d err=%v", foreignKeys, err)
	}
	if stats := db.Stats(); stats.MaxOpenConnections != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1", stats.MaxOpenConnections)
	}
	if err := mappingStore.Close(); err != nil {
		t.Fatal(err)
	}
	if err := auditLog.Close(); err != nil {
		t.Fatal(err)
	}
	if err := assuranceStore.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("shared handle closed by package wrapper: %v", err)
	}
}

func TestC043AuditAppendTxCommitsAtomicallyOnSharedHandle(t *testing.T) {
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := mapping.NewWithDB(db); err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := assurance.NewWithDB(db); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO assurance_retention_policies
 (tenant_id, retention_class, duration_seconds, policy_json, content_hash) VALUES ('legacy-api-key','standard',3600,'{}','hash')`); err != nil {
		t.Fatal(err)
	}
	if _, err := log.AppendTx(context.Background(), tx, audit.Entry{Actor: "actor", Tool: "test", Action: "commit", Ts: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	for table := range map[string]bool{"assurance_retention_policies": true, "audit_log": true} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("rollback left %d rows in %s", count, table)
		}
	}
}
