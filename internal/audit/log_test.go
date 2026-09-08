package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func openTestLog(t *testing.T) *Log {
	t.Helper()
	l, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:) returned error: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestAppendAndVerify(t *testing.T) {
	ctx := context.Background()
	l := openTestLog(t)

	entries := []Entry{
		{Actor: "alice@example.com", Tool: "workiva_get_field", Action: "read", Target: "ss-1/sh-1/B3", AfterJSON: `{"value":"42"}`},
		{Actor: "bob@example.com", Tool: "workiva_update_field", Action: "editCells", Target: "ss-1/sh-1/B3", BeforeJSON: `{"value":"42"}`, AfterJSON: `{"value":"43"}`, WorkivaOpURL: "https://api.eu.wdesk.com/operations/op-1"},
		{Actor: "copilot", Tool: "workiva_search_fields", Action: "search", Target: "energy"},
	}
	var seqs []int64
	var prevHash string
	for i, e := range entries {
		got, err := l.Append(ctx, e)
		if err != nil {
			t.Fatalf("Append(%d) returned error: %v", i, err)
		}
		if got.Seq != int64(i+1) {
			t.Errorf("entry %d seq = %d, want %d", i, got.Seq, i+1)
		}
		if got.Hash == "" {
			t.Errorf("entry %d has empty hash", i)
		}
		if got.Ts.IsZero() {
			t.Errorf("entry %d has zero ts", i)
		}
		if i == 0 && got.PrevHash != "GENESIS" {
			t.Errorf("genesis prev_hash = %q, want GENESIS", got.PrevHash)
		}
		if i > 0 && got.PrevHash != prevHash {
			t.Errorf("entry %d prev_hash = %q, want previous hash %q", i, got.PrevHash, prevHash)
		}
		prevHash = got.Hash
		seqs = append(seqs, got.Seq)
	}

	if err := l.Verify(ctx); err != nil {
		t.Fatalf("Verify returned error on intact chain: %v", err)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	ctx := context.Background()
	l := openTestLog(t)

	for i := 0; i < 3; i++ {
		if _, err := l.Append(ctx, Entry{Actor: "tester", Tool: "t", Action: "a", Target: "x", AfterJSON: `{"v":1}`}); err != nil {
			t.Fatalf("Append(%d) returned error: %v", i, err)
		}
	}

	// Tamper with the middle row directly, bypassing Append.
	if _, err := l.db.ExecContext(ctx, `UPDATE audit_log SET after_json = '{"v":999}' WHERE seq = 2`); err != nil {
		t.Fatalf("tamper UPDATE returned error: %v", err)
	}

	err := l.Verify(ctx)
	if err == nil {
		t.Fatal("Verify returned nil on tampered chain, want error")
	}
	if !strings.Contains(err.Error(), "seq 2") {
		t.Errorf("Verify error = %q, want it to identify seq 2", err)
	}
}

func TestVerifyDetectsBrokenPrevHashLink(t *testing.T) {
	ctx := context.Background()
	l := openTestLog(t)

	if _, err := l.Append(ctx, Entry{Actor: "a", Tool: "t", Action: "a1", Target: "x"}); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if _, err := l.Append(ctx, Entry{Actor: "a", Tool: "t", Action: "a2", Target: "x"}); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	// Rewrite the second row so its hash recomputes cleanly but no longer
	// links to row one.
	if _, err := l.db.ExecContext(ctx, `UPDATE audit_log SET prev_hash = 'GENESIS' WHERE seq = 2`); err != nil {
		t.Fatalf("tamper UPDATE returned error: %v", err)
	}

	err := l.Verify(ctx)
	if err == nil {
		t.Fatal("Verify returned nil on relinked chain, want error")
	}
	if !strings.Contains(err.Error(), "seq 2") {
		t.Errorf("Verify error = %q, want it to identify seq 2", err)
	}
}

func TestExportWritesJSONL(t *testing.T) {
	ctx := context.Background()
	l := openTestLog(t)

	want := 3
	for i := 0; i < want; i++ {
		if _, err := l.Append(ctx, Entry{Actor: "auditor", Tool: "export-test", Action: "a", Target: "t", BeforeJSON: `{"b":1}`, AfterJSON: `{"a":2}`}); err != nil {
			t.Fatalf("Append(%d) returned error: %v", i, err)
		}
	}

	var sb strings.Builder
	if err := l.Export(&sb); err != nil {
		t.Fatalf("Export returned error: %v", err)
	}

	scanner := bufio.NewScanner(strings.NewReader(sb.String()))
	var lines []Entry
	for scanner.Scan() {
		var e Entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			t.Fatalf("export line %d is not valid JSON: %v\nline: %s", len(lines)+1, err, scanner.Text())
		}
		lines = append(lines, e)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanning export output: %v", err)
	}
	if len(lines) != want {
		t.Fatalf("Export produced %d lines, want %d", len(lines), want)
	}
	for i, e := range lines {
		if e.Seq != int64(i+1) {
			t.Errorf("line %d seq = %d, want %d", i, e.Seq, i+1)
		}
		if e.Actor != "auditor" || e.BeforeJSON != `{"b":1}` || e.AfterJSON != `{"a":2}` {
			t.Errorf("line %d = %+v, want original entry content", i, e)
		}
		if e.Hash == "" || e.PrevHash == "" {
			t.Errorf("line %d missing hash chain fields: %+v", i, e)
		}
		if i == 0 && e.PrevHash != "GENESIS" {
			t.Errorf("line 0 prev_hash = %q, want GENESIS", e.PrevHash)
		}
	}
}

func TestVerifyEmptyLog(t *testing.T) {
	ctx := context.Background()
	l := openTestLog(t)

	if err := l.Verify(ctx); err != nil {
		t.Fatalf("Verify on empty log returned error: %v", err)
	}
}

func TestAppendFillsZeroTimestamp(t *testing.T) {
	ctx := context.Background()
	l := openTestLog(t)

	got, err := l.Append(ctx, Entry{Actor: "a", Tool: "t", Action: "a", Target: "x"})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if got.Ts.IsZero() {
		t.Error("Append left zero Ts, want current time")
	}
	if time.Since(got.Ts) > time.Minute {
		t.Errorf("Append Ts = %v, want recent time", got.Ts)
	}
}

var errTestSentinel = errors.New("sentinel")

func TestExportPropagatesWriterErrors(t *testing.T) {
	ctx := context.Background()
	l := openTestLog(t)

	if _, err := l.Append(ctx, Entry{Actor: "a", Tool: "t", Action: "a", Target: "x"}); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	if err := l.Export(failingWriter{}); err == nil {
		t.Fatal("Export with failing writer returned nil, want error")
	}
}

func TestWorkivaOpURLAffectsHash(t *testing.T) {
	ctx := context.Background()
	l := openTestLog(t)

	e1, err := l.Append(ctx, Entry{Actor: "a", Tool: "t", Action: "a", Target: "x", WorkivaOpURL: "https://api.eu.wdesk.com/operations/op-1"})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	e2, err := l.Append(ctx, Entry{Actor: "a", Tool: "t", Action: "a", Target: "x", WorkivaOpURL: "https://api.eu.wdesk.com/operations/op-2"})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if e1.Hash == e2.Hash {
		t.Errorf("hashes should differ when only WorkivaOpURL differs")
	}
}

type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) { return 0, errTestSentinel }
