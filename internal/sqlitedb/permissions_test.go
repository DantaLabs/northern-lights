package sqlitedb

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPreparePrivateDatabaseCreatesPrivateFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "state.db")
	if err := PreparePrivateDatabase(path); err != nil {
		t.Fatalf("PreparePrivateDatabase: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
}

func TestPreparePrivateDatabaseTightensExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(path, []byte("existing"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if err := PreparePrivateDatabase(path); err != nil {
		t.Fatalf("PreparePrivateDatabase: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
}

func TestPreparePrivateDatabaseAllowsMemoryDSNs(t *testing.T) {
	for _, path := range []string{":memory:", "file::memory:", "file::memory:?cache=shared", "file:shared?mode=memory&cache=shared"} {
		if err := PreparePrivateDatabase(path); err != nil {
			t.Fatalf("PreparePrivateDatabase(%q): %v", path, err)
		}
	}
}

func TestPreparePrivateDatabaseRejectsPersistentURI(t *testing.T) {
	for _, path := range []string{"file:state.db", "file:/tmp/state.db?mode=rwc", "file:state.db?mode=memory&mode=rwc", "file:state.db?mode=memory&bad=%xx"} {
		if err := PreparePrivateDatabase(path); err == nil || !strings.Contains(err.Error(), "plain filesystem path") {
			t.Errorf("%q: got %v, want plain filesystem path instruction", path, err)
		}
	}
}
