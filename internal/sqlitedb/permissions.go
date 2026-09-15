package sqlitedb

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// PreparePrivateDatabase creates a file-backed SQLite database with owner-only
// permissions, or tightens an existing database before it is opened. In-memory
// DSNs are left to the SQLite driver; persistent URIs are rejected.
func PreparePrivateDatabase(path string) error {
	if path == ":memory:" {
		return nil
	}
	if strings.HasPrefix(path, "file:") {
		u, err := url.Parse(path)
		if err == nil {
			q, queryErr := url.ParseQuery(u.RawQuery)
			modes := q["mode"]
			if queryErr == nil && len(modes) == 1 && modes[0] == "memory" {
				return nil
			}
			if queryErr == nil && len(modes) == 0 && (u.Opaque == ":memory:" || u.Path == ":memory:") {
				return nil
			}
		}
		return fmt.Errorf("persistent or unsupported SQLite file URI: use a plain filesystem path for owner-only permissions")
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("prepare database %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("prepare database %q: close: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("prepare database %q: chmod: %w", path, err)
	}
	return nil
}
