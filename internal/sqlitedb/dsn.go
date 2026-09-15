// Package sqlitedb holds shared helpers for opening SQLite databases in
// this project.
package sqlitedb

import (
	"net/url"
	"strings"
)

// SharedFileDSN returns the DSN for opening path. File-backed databases
// get WAL journaling and a 5 second busy timeout so that several handles
// on the same file (mapping store plus audit log) can write concurrently
// without immediate SQLITE_BUSY errors. ":memory:" is returned unchanged.
func SharedFileDSN(path string) string {
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		return path
	}
	// Escape the entire filename so SQLite decodes it literally, including
	// percent escapes and platform-specific path separators.
	return "file:" + url.PathEscape(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
}
