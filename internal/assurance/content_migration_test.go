package assurance

import (
	"context"
	"database/sql"
	"testing"

	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	_ "modernc.org/sqlite"
)

func TestV18ContentStorageUpgradePreservesRowsAndReopens(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if err := sqlitedb.Migrate(ctx, db, "assurance", migrations[:17]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_resources(tenant_id,resource_id,provider,kind,external_id) VALUES('tenant','resource','rest','sheet','external')`); err != nil {
		t.Fatal(err)
	}
	priorBundleJSON := `{"bundle_id":"active-before-v18","signed_bytes":"preserve exactly"}`
	if _, err := db.Exec(`INSERT INTO assurance_active_bundles
	 (tenant_id,singleton,bundle_id,bundle_version,schema_version,bundle_json,signature_hex,content_hash,activated_at)
	 VALUES('tenant',1,'active-before-v18',7,1,?,?,?,?)`, priorBundleJSON, "a1b2c3", "content-hash-before-v18", "2026-10-09T12:34:56Z"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("upgrade v17 to v18: %v", err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("reopen at v18: %v", err)
	}
	var version, rows int
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations WHERE app='assurance'`).Scan(&version); err != nil || version != 22 {
		t.Fatalf("assurance migration marker=%d err=%v, want 22", version, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM assurance_resources WHERE tenant_id='tenant' AND resource_id='resource'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("pre-v18 row count=%d err=%v, want preserved row", rows, err)
	}
	var bundleJSON, signatureHex, contentHash, activatedAt string
	if err := db.QueryRow(`SELECT bundle_json,signature_hex,content_hash,activated_at FROM assurance_active_bundles WHERE tenant_id='tenant' AND singleton=1`).Scan(&bundleJSON, &signatureHex, &contentHash, &activatedAt); err != nil {
		t.Fatalf("read preserved active signed bundle: %v", err)
	}
	if bundleJSON != priorBundleJSON || signatureHex != "a1b2c3" || contentHash != "content-hash-before-v18" || activatedAt != "2026-10-09T12:34:56Z" {
		t.Fatalf("active signed bundle changed across v18: json=%q signature=%q hash=%q activated=%q", bundleJSON, signatureHex, contentHash, activatedAt)
	}
	for _, table := range []string{"assurance_content_sources", "assurance_content_items", "assurance_content_drafts"} {
		var exists int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&exists); err != nil || exists != 1 {
			t.Fatalf("v18 table %s exists=%d err=%v", table, exists, err)
		}
		columns, err := tableColumns(db, table)
		if err != nil {
			t.Fatalf("%s columns: %v", table, err)
		}
		if len(columns) == 0 || columns[0] != "tenant_id" {
			t.Fatalf("%s first column=%v, want tenant_id", table, columns)
		}
	}
	var sourceBytesType string
	if err := db.QueryRow(`SELECT type FROM pragma_table_info('assurance_content_sources') WHERE name='source_bytes'`).Scan(&sourceBytesType); err != nil || sourceBytesType != "BLOB" {
		t.Fatalf("source_bytes declared type=%q err=%v, want BLOB", sourceBytesType, err)
	}
}
