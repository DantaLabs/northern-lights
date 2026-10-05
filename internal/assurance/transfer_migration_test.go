package assurance

import (
	"context"
	"database/sql"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	_ "modernc.org/sqlite"
	"testing"
)

func TestV14UpgradeFromV13PreservesRowsAndRollsBackFailure(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if err := sqlitedb.Migrate(ctx, db, "assurance", migrations[:13]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_resources(tenant_id,resource_id,provider,kind,external_id) VALUES('t','r','rest','sheet','external')`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_resources WHERE tenant_id='t' AND resource_id='r'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("preserved row count %d: %v", n, err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	broken, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broken.Close() }()
	broken.SetMaxOpenConns(1)
	if err := sqlitedb.Migrate(ctx, broken, "assurance", migrations[:13]); err != nil {
		t.Fatal(err)
	}
	if _, err := broken.Exec(`CREATE TABLE assurance_transfer_fences(tenant_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, broken); err == nil {
		t.Fatal("expected v14 object collision")
	}
	if err := broken.QueryRow(`SELECT max(version) FROM schema_migrations WHERE app='assurance'`).Scan(&n); err != nil || n != 13 {
		t.Fatalf("failed migration marker=%d err=%v", n, err)
	}
	var visual int
	if err := broken.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='assurance_transfer_visual_evidence'`).Scan(&visual); err != nil || visual != 0 {
		t.Fatalf("partial v14 table survived rollback: %d %v", visual, err)
	}
}
