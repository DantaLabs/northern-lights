package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/dantalabs/northern-lights/internal/audit"
)

func TestAuditVerifyCleanChain(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "audit.db")
	seedAuditDB(t, dbPath, false)

	var out bytes.Buffer
	if err := runAudit([]string{"verify", "-db", dbPath}, &out); err != nil {
		t.Fatalf("verify clean chain: %v", err)
	}
	if !strings.Contains(out.String(), "OK") {
		t.Errorf("expected OK in output, got %q", out.String())
	}
}

func TestAuditVerifyTamperedChain(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "audit.db")
	seedAuditDB(t, dbPath, true)

	var out bytes.Buffer
	err := runAudit([]string{"verify", "-db", dbPath}, &out)
	if err == nil {
		t.Fatal("expected verify to fail on tampered chain")
	}
}

func TestAuditExportJSONL(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "audit.db")
	seedAuditDB(t, dbPath, false)

	var out bytes.Buffer
	if err := runAudit([]string{"export", "-db", dbPath, "-format", "jsonl"}, &out); err != nil {
		t.Fatalf("export: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 JSONL lines, got %d", len(lines))
	}
	for _, line := range lines {
		var entry audit.Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Errorf("invalid JSONL line: %v", err)
		}
	}
}

func TestAuditUsageError(t *testing.T) {
	var out bytes.Buffer
	if err := runAudit([]string{"bogus"}, &out); err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
}

func seedAuditDB(t *testing.T, dbPath string, tamper bool) {
	t.Helper()
	log, err := audit.Open(dbPath)
	if err != nil {
		t.Fatalf("open audit db: %v", err)
	}
	defer log.Close()
	ctx := context.Background()
	for _, tool := range []string{"tool_a", "tool_b", "tool_c"} {
		if _, err := log.Append(ctx, audit.Entry{Actor: "test", Tool: tool, Action: "call"}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if tamper {
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("open raw db: %v", err)
		}
		defer db.Close()
		if _, err := db.Exec(`UPDATE audit_log SET actor = 'mallory' WHERE seq = 2`); err != nil {
			t.Fatalf("tamper: %v", err)
		}
	}
}
