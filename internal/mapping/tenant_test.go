package mapping

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

func tenantContext(tenant, object string) context.Context {
	return identity.ContextWithPrincipal(context.Background(), identity.Principal{
		TenantID: tenant,
		ObjectID: object,
	})
}

func TestTenantScopedMappingsPermitIdenticalIdentifiers(t *testing.T) {
	s := openTestStore(t)
	a := tenantContext("11111111-1111-1111-1111-111111111111", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	b := tenantContext("22222222-2222-2222-2222-222222222222", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	for _, tc := range []struct {
		ctx         context.Context
		book, sheet string
		rangeA1     string
	}{
		{a, "Tenant A book", "Tenant A sheet", "A1"},
		{b, "Tenant B book", "Tenant B sheet", "B2"},
	} {
		if err := s.UpsertSpreadsheet(tc.ctx, Spreadsheet{ID: "same-sp", Name: tc.book, Region: "eu"}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertSheet(tc.ctx, Sheet{ID: "same-sh", SpreadsheetID: "same-sp", Name: tc.sheet}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpsertField(tc.ctx, Field{SpreadsheetID: "same-sp", SheetID: "same-sh", Name: "same-field", CellRange: tc.rangeA1}); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		ctx         context.Context
		book, sheet string
		rangeA1     string
	}{
		{a, "Tenant A book", "Tenant A sheet", "A1"},
		{b, "Tenant B book", "Tenant B sheet", "B2"},
	} {
		books, err := s.ListSpreadsheets(tc.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(books) != 1 || books[0].Name != tc.book || len(books[0].Sheets) != 1 || books[0].Sheets[0].Name != tc.sheet {
			t.Fatalf("tenant list = %+v, want only %q/%q", books, tc.book, tc.sheet)
		}
		field, err := s.GetField(tc.ctx, "same-field")
		if err != nil {
			t.Fatal(err)
		}
		if field == nil || field.CellRange != tc.rangeA1 {
			t.Fatalf("tenant field = %+v, want range %s", field, tc.rangeA1)
		}
		matches, err := s.SearchFields(tc.ctx, "same")
		if err != nil || len(matches) != 1 || matches[0].CellRange != tc.rangeA1 {
			t.Fatalf("tenant search = %+v, err=%v", matches, err)
		}
	}
}

func TestTenantScopedSnapshotsDoNotCrossCacheBoundary(t *testing.T) {
	s := openTestStore(t)
	a := tenantContext("11111111-1111-1111-1111-111111111111", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	b := tenantContext("22222222-2222-2222-2222-222222222222", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	now := time.Now().UTC()
	for _, tc := range []struct {
		ctx   context.Context
		value string
	}{{a, "A-private"}, {b, "B-private"}} {
		if err := s.CacheCells(tc.ctx, []CellValue{{SpreadsheetID: "same-sp", SheetID: "same-sh", Cell: "A1", Value: tc.value, FetchedAt: now}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		ctx   context.Context
		value string
	}{{a, "A-private"}, {b, "B-private"}} {
		cells, err := s.GetCachedCells(tc.ctx, "same-sp", "same-sh", time.Minute)
		if err != nil || len(cells) != 1 || cells[0].Value != tc.value {
			t.Fatalf("tenant cache = %+v, err=%v, want %q", cells, err, tc.value)
		}
	}
}

func TestResourceOwnershipSpreadsheetRowAloneAssertsOwnership(t *testing.T) {
	s := openTestStore(t)
	a := tenantContext("11111111-1111-1111-1111-111111111111", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	b := tenantContext("22222222-2222-2222-2222-222222222222", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	// Tenant A holds only a spreadsheets row for sp-1: no sheet or field rows.
	if err := s.UpsertSpreadsheet(a, Spreadsheet{ID: "sp-1", Name: "A book"}); err != nil {
		t.Fatal(err)
	}

	owned, foreign, err := s.ResourceOwnership(a, "sp-1", "any-sheet")
	if err != nil || !owned || foreign {
		t.Fatalf("owner with only a spreadsheet row = owned %v foreign %v err %v, want owned", owned, foreign, err)
	}
	owned, foreign, err = s.ResourceOwnership(b, "sp-1", "any-sheet")
	if err != nil || owned || !foreign {
		t.Fatalf("other tenant with owner spreadsheet row = owned %v foreign %v err %v, want foreign", owned, foreign, err)
	}
}

func TestResourceOwnershipDistinguishesCurrentForeignAndUnowned(t *testing.T) {
	s := openTestStore(t)
	a := tenantContext("11111111-1111-1111-1111-111111111111", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	b := tenantContext("22222222-2222-2222-2222-222222222222", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	if err := s.UpsertSpreadsheet(b, Spreadsheet{ID: "sp", Name: "B"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertSheet(b, Sheet{ID: "sh", SpreadsheetID: "sp", Name: "B"}); err != nil {
		t.Fatal(err)
	}

	owned, foreign, err := s.ResourceOwnership(a, "sp", "sh")
	if err != nil || owned || !foreign {
		t.Fatalf("A ownership of B resource = owned %v foreign %v err %v", owned, foreign, err)
	}
	owned, foreign, err = s.ResourceOwnership(a, "unowned", "unowned")
	if err != nil || owned || foreign {
		t.Fatalf("unowned resource = owned %v foreign %v err %v", owned, foreign, err)
	}
	if err := s.UpsertSpreadsheet(a, Spreadsheet{ID: "sp", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertSheet(a, Sheet{ID: "sh", SpreadsheetID: "sp", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	owned, foreign, err = s.ResourceOwnership(a, "sp", "sh")
	if err != nil || !owned || !foreign {
		t.Fatalf("shared identifiers = owned %v foreign %v err %v", owned, foreign, err)
	}
}

func TestLegacyRowsMigrateToLegacyTenantOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", sqlitedb.SharedFileDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := sqlitedb.Migrate(ctx, db, "mapping", []string{migration0001, migration0002, migration0003}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO spreadsheets(id,name,region,synced_at) VALUES('sp','Legacy','eu',CURRENT_TIMESTAMP);
		INSERT INTO sheets(id,spreadsheet_id,name) VALUES('sh','sp','Legacy sheet');
		INSERT INTO fields(spreadsheet_id,sheet_id,name,cell_range) VALUES('sp','sh','legacy_field','A1');
		INSERT INTO snapshots(spreadsheet_id,sheet_id,cell,value,fetched_at) VALUES('sp','sh','A1','legacy-value',CURRENT_TIMESTAMP);
		INSERT INTO pending_writes(token,field_id,field_name,value,spreadsheet_id,sheet_id,cell_range,created_at)
		VALUES('legacy-token',1,'legacy_field','2','sp','sh','A1',CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	legacyBooks, err := s.ListSpreadsheets(ctx)
	if err != nil || len(legacyBooks) != 1 || legacyBooks[0].Name != "Legacy" {
		t.Fatalf("legacy books = %+v, err=%v", legacyBooks, err)
	}
	if field, err := s.GetField(ctx, "legacy_field"); err != nil || field == nil {
		t.Fatalf("legacy field = %+v, err=%v", field, err)
	}
	if cells, err := s.GetCachedCells(ctx, "sp", "sh", time.Hour); err != nil || len(cells) != 1 || cells[0].Value != "legacy-value" {
		t.Fatalf("legacy cache = %+v, err=%v", cells, err)
	}

	entra := tenantContext("11111111-1111-1111-1111-111111111111", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if books, err := s.ListSpreadsheets(entra); err != nil || len(books) != 0 {
		t.Fatalf("Entra saw legacy books: %+v, err=%v", books, err)
	}
	if field, err := s.GetField(entra, "legacy_field"); err != nil || field != nil {
		t.Fatalf("Entra saw legacy field: %+v, err=%v", field, err)
	}
	if cells, err := s.GetCachedCells(entra, "sp", "sh", time.Hour); err != nil || len(cells) != 0 {
		t.Fatalf("Entra saw legacy cache: %+v, err=%v", cells, err)
	}
}

func TestExpiredPendingWriteCleanupIsTenantScoped(t *testing.T) {
	s := openTestStore(t)
	a := tenantContext("11111111-1111-1111-1111-111111111111", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	b := tenantContext("22222222-2222-2222-2222-222222222222", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	old := time.Now().Add(-time.Hour)
	for _, ctx := range []context.Context{a, b} {
		if err := s.CreatePendingWrite(ctx, PendingWrite{Token: "same-token", FieldID: 1, FieldName: "f", Value: "v", CreatedAt: old}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.DeleteExpiredPendingWrites(a, 5*time.Minute); err != nil || n != 1 {
		t.Fatalf("tenant A cleanup deleted %d, err=%v", n, err)
	}
	var remaining int
	if err := s.db.QueryRow(`SELECT count(*) FROM pending_writes WHERE tenant_id=?`, identity.StorageTenant(b)).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("tenant B token was crossed by cleanup: remaining=%d err=%v", remaining, err)
	}
}

// The startup janitor runs before any tenant context exists and must clear
// expired staged writes for every tenant; unexpired rows survive.
func TestDeleteExpiredPendingWritesGlobalSpansTenants(t *testing.T) {
	s := openTestStore(t)
	a := tenantContext("11111111-1111-1111-1111-111111111111", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	b := tenantContext("22222222-2222-2222-2222-222222222222", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	old := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		ctx   context.Context
		token string
		at    time.Time
	}{{a, "expired-a", old}, {b, "expired-b", old}, {a, "fresh-a", time.Now()}} {
		if err := s.CreatePendingWrite(tc.ctx, PendingWrite{Token: tc.token, FieldID: 1, FieldName: "f", Value: "v", CreatedAt: tc.at}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.DeleteExpiredPendingWritesGlobal(context.Background(), 5*time.Minute)
	if err != nil {
		t.Fatalf("DeleteExpiredPendingWritesGlobal: %v", err)
	}
	if n != 2 {
		t.Fatalf("global cleanup deleted %d rows, want 2 (one expired row per tenant)", n)
	}
	var remaining int
	if err := s.db.QueryRow(`SELECT count(*) FROM pending_writes`).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("rows after global cleanup = %d, err=%v, want only the unexpired row", remaining, err)
	}
	var digest string
	if err := s.db.QueryRow(`SELECT token_digest FROM pending_writes`).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if digest != tokenDigest("fresh-a") {
		t.Fatalf("surviving row digest = %q, want the unexpired fresh-a row", digest)
	}
}

func TestTenantMigrationFailureRollsBackReplacementTablesAndMarker(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rollback.db")
	db, err := sql.Open("sqlite", sqlitedb.SharedFileDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := sqlitedb.Migrate(ctx, db, "mapping", []string{migration0001, migration0002, migration0003}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO spreadsheets(id,name) VALUES('preserved','Original');
		CREATE TABLE fields_v4 (sentinel TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if store, err := Open(path); err == nil {
		_ = store.Close()
		t.Fatal("Open succeeded despite injected tenant-migration table collision")
	}
	db, err = sql.Open("sqlite", sqlitedb.SharedFileDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close rollback database: %v", err)
		}
	})
	var name string
	if err := db.QueryRow(`SELECT name FROM spreadsheets WHERE id='preserved'`).Scan(&name); err != nil || name != "Original" {
		t.Fatalf("legacy table not preserved: name=%q err=%v", name, err)
	}
	var versionCount int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE app='mapping' AND version=4`).Scan(&versionCount); err != nil || versionCount != 0 {
		t.Fatalf("failed migration marker count=%d err=%v", versionCount, err)
	}
	for _, table := range []string{"spreadsheets_v4", "sheets_v4", "snapshots_v4", "pending_writes_v4"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial replacement table %s count=%d err=%v", table, count, err)
		}
	}
}
