package assurance

import (
	"context"
	"database/sql"
	"testing"

	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	_ "modernc.org/sqlite"
)

func TestV17StartupQuarantineUpgradePreservesRowsAndReopens(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if err := sqlitedb.Migrate(ctx, db, "assurance", migrations[:16]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO transfer_intents(tenant_id,transfer_id,actor_id,permission,intent_json,state,token_digest,idempotency_digest,request_digest,expires_at) VALUES('tenant','id','actor','permission','{}','claimed','token','idem','request','2026-12-31T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM transfer_intents WHERE tenant_id='tenant' AND transfer_id='id'`).Scan(&state); err != nil || state != "claimed" {
		t.Fatalf("preserved state=%q err=%v", state, err)
	}
	var version int
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations WHERE app='assurance'`).Scan(&version); err != nil || version != 17 {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

func TestV17StartupQuarantineCollisionRollsBackMarkerAndPreservesRows(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if err := sqlitedb.Migrate(ctx, db, "assurance", migrations[:16]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO transfer_intents(tenant_id,transfer_id,actor_id,permission,intent_json,state,token_digest,idempotency_digest,request_digest,expires_at) VALUES('tenant','id','actor','permission','{}','claimed','token','idem','request','2026-12-31T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE assurance_transfer_startup_quarantines (tenant_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err == nil {
		t.Fatal("v17 collision accepted")
	}
	var version, rows int
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations WHERE app='assurance'`).Scan(&version); err != nil || version != 16 {
		t.Fatalf("marker=%d err=%v", version, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM transfer_intents WHERE tenant_id='tenant' AND transfer_id='id'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows=%d err=%v", rows, err)
	}
}
