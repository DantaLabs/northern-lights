package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

func auditTenantContext(tenant, object string) context.Context {
	return identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: tenant, ObjectID: object})
}

func TestAuditRecentAndPageAreTenantScopedAcrossInterleavedChains(t *testing.T) {
	l := openTestLog(t)
	a := auditTenantContext("11111111-1111-1111-1111-111111111111", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	b := auditTenantContext("22222222-2222-2222-2222-222222222222", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	for _, item := range []struct {
		ctx    context.Context
		target string
	}{{a, "A-secret-1"}, {b, "B-secret-1"}, {a, "A-secret-2"}, {b, "B-secret-2"}} {
		if _, err := l.Append(item.ctx, Entry{Actor: "trusted", Tool: "test", Action: "read", Target: item.target, AfterJSON: `{"semantic":"private"}`}); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		ctx         context.Context
		wantPrefix  string
		otherPrefix string
	}{{a, "A-", "B-"}, {b, "B-", "A-"}} {
		entries, err := l.Recent(tc.ctx, 10, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 {
			t.Fatalf("tenant recent entries = %+v, want 2", entries)
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Target, tc.wantPrefix) || strings.Contains(entry.Target, tc.otherPrefix) {
				t.Fatalf("cross-tenant audit exposure: %+v", entries)
			}
		}
		page, err := l.RecentPage(tc.ctx, 1, "", 0)
		if err != nil || len(page) != 1 || !strings.HasPrefix(page[0].Target, tc.wantPrefix) {
			t.Fatalf("tenant page = %+v, err=%v", page, err)
		}
	}
	if err := l.Verify(context.Background()); err != nil {
		t.Fatalf("interleaved tenant chains did not verify: %v", err)
	}
}

func TestAuditHashBindsTenantIdentity(t *testing.T) {
	l := openTestLog(t)
	a := auditTenantContext("11111111-1111-1111-1111-111111111111", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if _, err := l.Append(a, Entry{Actor: "trusted", Tool: "test", Action: "read", Target: "private"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.db.Exec(`UPDATE audit_log SET tenant_id='22222222-2222-2222-2222-222222222222' WHERE seq=1`); err != nil {
		t.Fatalf("tamper tenant_id: %v", err)
	}
	if err := l.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "seq 1") {
		t.Fatalf("Verify after tenant tamper = %v, want seq 1 failure", err)
	}
}

func TestLegacyAuditRowsRemainVerifiableAndLegacyVisibleOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-audit.db")
	db, err := sql.Open("sqlite", sqlitedb.SharedFileDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := sqlitedb.Migrate(ctx, db, "audit", []string{migration0001, migration0002}); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC().Format(timeFormat)
	hash := entryHash(genesisHash, ts, "legacy-actor", "legacy-tool", "read", "legacy-secret", "", `{"value":"legacy"}`, "", "legacy-audit-id")
	if _, err := db.Exec(`INSERT INTO audit_log(ts,actor,tool,action,target,before_json,after_json,workiva_op_url,audit_id,prev_hash,hash)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, ts, "legacy-actor", "legacy-tool", "read", "legacy-secret", "", `{"value":"legacy"}`, "", "legacy-audit-id", genesisHash, hash); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if err := l.Verify(ctx); err != nil {
		t.Fatalf("legacy chain no longer verifies: %v", err)
	}
	if entries, err := l.Recent(ctx, 10, ""); err != nil || len(entries) != 1 || entries[0].Target != "legacy-secret" {
		t.Fatalf("legacy recent = %+v, err=%v", entries, err)
	}
	entra := auditTenantContext("11111111-1111-1111-1111-111111111111", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if entries, err := l.Recent(entra, 10, ""); err != nil || len(entries) != 0 {
		t.Fatalf("Entra saw legacy audit semantics: %+v, err=%v", entries, err)
	}
}

func TestAuditExportIncludesHashVersionAcrossMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mixed-version-audit.db")
	db, err := sql.Open("sqlite", sqlitedb.SharedFileDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := sqlitedb.Migrate(ctx, db, "audit", []string{migration0001, migration0002}); err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, time.September, 24, 4, 5, 6, 0, time.UTC).Format(timeFormat)
	hash := entryHash(genesisHash, ts, "legacy-actor", "legacy-tool", "read", "legacy-target", "", `{"value":"legacy"}`, "", "legacy-audit-id")
	if _, err := db.Exec(`INSERT INTO audit_log(ts,actor,tool,action,target,before_json,after_json,workiva_op_url,audit_id,prev_hash,hash)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, ts, "legacy-actor", "legacy-tool", "read", "legacy-target", "", `{"value":"legacy"}`, "", "legacy-audit-id", genesisHash, hash); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	appended, err := l.Append(ctx, Entry{Actor: "new-actor", Tool: "new-tool", Action: "write", Target: "new-target"})
	if err != nil {
		t.Fatal(err)
	}
	if appended.PrevHash != hash {
		t.Fatalf("v2 row prev_hash=%q, want legacy v1 hash %q", appended.PrevHash, hash)
	}
	var out strings.Builder
	if err := l.Export(&out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("export lines=%d, want 2: %s", len(lines), out.String())
	}
	for i, want := range []float64{1, 2} {
		var object map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &object); err != nil {
			t.Fatalf("decode export line %d: %v", i+1, err)
		}
		if got := object["hash_version"]; got != want {
			t.Fatalf("export line %d hash_version=%v, want %.0f; object=%v", i+1, got, want, object)
		}
	}
	if err := l.Verify(ctx); err != nil {
		t.Fatalf("mixed v1/v2 chain does not verify: %v", err)
	}
}

func TestAuditTenantMigrationFailureRollsBackTableAndMarker(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rollback-audit.db")
	db, err := sql.Open("sqlite", sqlitedb.SharedFileDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := sqlitedb.Migrate(ctx, db, "audit", []string{migration0001, migration0002}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO audit_log(actor,target) VALUES('preserved','Original'); CREATE TABLE audit_log_v3(sentinel TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if log, err := Open(path); err == nil {
		_ = log.Close()
		t.Fatal("Open succeeded despite injected audit migration collision")
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
	var target string
	if err := db.QueryRow(`SELECT target FROM audit_log WHERE actor='preserved'`).Scan(&target); err != nil || target != "Original" {
		t.Fatalf("legacy audit table not preserved: target=%q err=%v", target, err)
	}
	var versionCount int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE app='audit' AND version=3`).Scan(&versionCount); err != nil || versionCount != 0 {
		t.Fatalf("failed audit migration marker count=%d err=%v", versionCount, err)
	}
}
