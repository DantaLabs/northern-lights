package sqlitedb_test

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

func TestSharedFileDSNLiteralPath(t *testing.T) {
	for i, name := range []string{"state.db", "state?test.db", "state#test.db", "state%41.db", "state%.db"} {
		for _, relative := range []bool{false, true} {
			t.Run(fmt.Sprintf("case%d/relative=%t", i, relative), func(t *testing.T) {
				if runtime.GOOS == "windows" && strings.Contains(name, "?") {
					t.Skip("Windows filenames cannot contain ?")
				}
				dir := t.TempDir()
				t.Chdir(dir)
				path := filepath.Join(dir, name)
				configured := path
				if relative {
					configured = name
				}
				store, err := mapping.Open(configured)
				if err != nil {
					t.Fatal(err)
				}
				log, err := audit.Open(configured)
				if err != nil {
					_ = store.Close()
					t.Fatal(err)
				}
				if err := log.Close(); err != nil {
					t.Fatal(err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if entry.Name() != name {
						t.Errorf("unexpected alternate file %q", entry.Name())
					}
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(string(data), "SQLite format 3\x00") {
					t.Fatalf("literal path is not the written SQLite database (%d bytes)", len(data))
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
					t.Errorf("database mode = %o, want 600", info.Mode().Perm())
				}
				// Independently encode a read-only URI to verify both Open migrations
				// wrote the literal file, without using the DSN helper under test.
				uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=ro"}
				db, err := sql.Open("sqlite", uri.String())
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := db.Close(); err != nil {
						t.Error(err)
					}
				}()
				for _, table := range []string{"fields", "audit_log"} {
					var count int
					if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
						t.Errorf("literal database missing %s: %v", table, err)
					}
				}
			})
		}
	}
}

func TestSharedFileDSNPathForms(t *testing.T) {
	for _, path := range []string{"state.db", "/tmp/state?#%41.db", `C:\data\state#%41.db`, `C:state%41.db`, `\\server\share\state#%.db`, "C:/data/state#%41.db"} {
		dsn := sqlitedb.SharedFileDSN(path)
		encoded, query, ok := strings.Cut(strings.TrimPrefix(dsn, "file:"), "?")
		decoded, err := url.PathUnescape(encoded)
		if err != nil || decoded != path || strings.Contains(encoded, "#") || !ok || query != "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)" {
			t.Errorf("path %q: unsafe DSN %q (decoded %q, error %v)", path, dsn, decoded, err)
		}
	}
	for _, path := range []string{":memory:", "file::memory:", "file::memory:?cache=shared", "file:shared?mode=memory&cache=shared"} {
		if got := sqlitedb.SharedFileDSN(path); got != path {
			t.Errorf("memory DSN changed: %q => %q", path, got)
		}
	}
}
