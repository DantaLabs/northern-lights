package sqlitedb_test

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/transfer"
)

// TestReleasedWave2BinaryReopensCurrentMigratedDatabase is opt-in because it
// requires a binary built from the exact deployed source archive. Set
// NL_WAVE2_BINARY to that binary's path when running the compatibility gate.
func TestReleasedWave2BinaryReopensCurrentMigratedDatabase(t *testing.T) {
	binary := strings.TrimSpace(os.Getenv("NL_WAVE2_BINARY"))
	if binary == "" {
		t.Skip("set NL_WAVE2_BINARY to the binary built from released commit 03a9ba88df6de671d398ef804887a7d7dc872861")
	}
	if info, err := os.Stat(binary); err != nil || info.IsDir() {
		t.Fatalf("NL_WAVE2_BINARY must name the released executable: stat error=%v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "phase2.db")
	first := startReleasedWave2(t, binary, dbPath)
	stopReleasedWave2(t, first)

	baseline, err := inspectReleasedPhase2DB(dbPath)
	if err != nil {
		t.Fatalf("read genuine old-binary initialization baseline: %v", err)
	}
	if baseline.fieldCount == 0 || baseline.auditCount == 0 || baseline.setupEntryHash == "" {
		t.Fatalf("old binary did not initialize durable Phase 2 mapping and audit rows: %+v", baseline)
	}

	// Exercise the current shared database open path and all current schema
	// owners, then close every handle before launching the old process again.
	db, err := sqlitedb.Open(dbPath)
	if err != nil {
		t.Fatalf("current SQLite opener: %v", err)
	}
	if _, err := mapping.NewWithDB(db); err != nil {
		_ = db.Close()
		t.Fatalf("current mapping migrations: %v", err)
	}
	currentAudit, err := audit.NewWithDB(db)
	if err != nil {
		_ = db.Close()
		t.Fatalf("current audit migrations: %v", err)
	}
	if _, err := assurance.NewWithDB(db); err != nil {
		_ = db.Close()
		t.Fatalf("current assurance migrations: %v", err)
	}
	if _, err := transfer.NewWithDB(db); err != nil {
		_ = db.Close()
		t.Fatalf("current transfer migrations: %v", err)
	}
	if err := currentAudit.Verify(context.Background()); err != nil {
		_ = db.Close()
		t.Fatalf("Phase 2 audit chain after current migration: %v", err)
	}
	upgraded, err := inspectPhase2Rows(db)
	if err != nil {
		_ = db.Close()
		t.Fatalf("read Phase 2 rows after current migration: %v", err)
	}
	if !reflect.DeepEqual(upgraded.mappingRows, baseline.mappingRows) || !reflect.DeepEqual(upgraded.auditRows, baseline.auditRows) {
		_ = db.Close()
		t.Fatalf("current migration changed Phase 2 durable row content: before=%+v after=%+v", baseline, upgraded)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close upgraded DB: %v", err)
	}

	second := startReleasedWave2(t, binary, dbPath)
	stopReleasedWave2(t, second)
	finalDB, err := sqlitedb.Open(dbPath)
	if err != nil {
		t.Fatalf("open DB after old binary restart: %v", err)
	}
	defer func() { _ = finalDB.Close() }()
	finalAudit, err := audit.NewWithDB(finalDB)
	if err != nil {
		t.Fatalf("current audit opener after old restart: %v", err)
	}
	if err := finalAudit.Verify(context.Background()); err != nil {
		t.Fatalf("audit chain after old binary restart: %v", err)
	}
	final, err := inspectPhase2Rows(finalDB)
	if err != nil {
		t.Fatalf("read Phase 2 rows after old binary restart: %v", err)
	}
	if !reflect.DeepEqual(final.mappingRows, baseline.mappingRows) || len(final.auditRows) != len(baseline.auditRows)+1 || !reflect.DeepEqual(final.auditRows[:len(baseline.auditRows)], baseline.auditRows) {
		t.Fatalf("old binary restart did not preserve original Phase 2 rows and append its startup audit: before=%+v after=%+v", baseline, final)
	}
	appended := final.auditRows[len(final.auditRows)-1]
	if appended.Actor != "setup" || appended.Action != "init" || appended.Target != "demo" || appended.Seq <= baseline.auditRows[len(baseline.auditRows)-1].Seq {
		t.Fatalf("old binary startup audit was not append-only: prior=%+v appended=%+v", baseline.auditRows[len(baseline.auditRows)-1], appended)
	}
}

type releasedPhase2Rows struct {
	mappingRows    []releasedMappingRow
	auditRows      []releasedAuditRow
	fieldCount     int
	auditCount     int
	setupEntryHash string
}

type releasedMappingRow struct {
	ID                                                                       int64
	SpreadsheetID, SheetID, Name, Aliases, CellRange, FieldType, Description string
}

type releasedAuditRow struct {
	Seq                                                                                  int64
	Timestamp, Actor, Tool, Action, Target, BeforeJSON, AfterJSON, OperationURL, AuditID string
	PreviousHash, Hash                                                                   string
}

func inspectReleasedPhase2DB(path string) (releasedPhase2Rows, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return releasedPhase2Rows{}, err
	}
	defer func() { _ = db.Close() }()
	return inspectPhase2Rows(db)
}

func inspectPhase2Rows(db *sql.DB) (releasedPhase2Rows, error) {
	var result releasedPhase2Rows
	// updated_at is intentionally excluded: the released demo bootstrap
	// refreshes it on every launch. All persistent mapping identity and content
	// columns are compared byte-for-byte across each migration/reopen boundary.
	fieldRows, err := db.Query(`SELECT id, COALESCE(spreadsheet_id,''), COALESCE(sheet_id,''), COALESCE(name,''), COALESCE(aliases,''), COALESCE(cell_range,''), COALESCE(field_type,''), COALESCE(description,'') FROM fields ORDER BY id`)
	if err != nil {
		return result, err
	}
	for fieldRows.Next() {
		var row releasedMappingRow
		if err := fieldRows.Scan(&row.ID, &row.SpreadsheetID, &row.SheetID, &row.Name, &row.Aliases, &row.CellRange, &row.FieldType, &row.Description); err != nil {
			_ = fieldRows.Close()
			return result, err
		}
		result.mappingRows = append(result.mappingRows, row)
	}
	if err := fieldRows.Err(); err != nil {
		_ = fieldRows.Close()
		return result, err
	}
	if err := fieldRows.Close(); err != nil {
		return result, err
	}
	result.fieldCount = len(result.mappingRows)
	auditRows, err := db.Query(`SELECT seq, COALESCE(ts,''), COALESCE(actor,''), COALESCE(tool,''), COALESCE(action,''), COALESCE(target,''), COALESCE(before_json,''), COALESCE(after_json,''), COALESCE(workiva_op_url,''), COALESCE(audit_id,''), COALESCE(prev_hash,''), COALESCE(hash,'') FROM audit_log ORDER BY seq`)
	if err != nil {
		return result, err
	}
	for auditRows.Next() {
		var row releasedAuditRow
		if err := auditRows.Scan(&row.Seq, &row.Timestamp, &row.Actor, &row.Tool, &row.Action, &row.Target, &row.BeforeJSON, &row.AfterJSON, &row.OperationURL, &row.AuditID, &row.PreviousHash, &row.Hash); err != nil {
			_ = auditRows.Close()
			return result, err
		}
		result.auditRows = append(result.auditRows, row)
	}
	if err := auditRows.Err(); err != nil {
		_ = auditRows.Close()
		return result, err
	}
	if err := auditRows.Close(); err != nil {
		return result, err
	}
	result.auditCount = len(result.auditRows)
	for _, row := range result.auditRows {
		if row.Actor == "setup" && row.Action == "init" && row.Target == "demo" {
			result.setupEntryHash = row.Hash
			break
		}
	}
	if result.setupEntryHash == "" {
		return result, fmt.Errorf("Phase 2 demo startup audit row not found")
	}
	return result, nil
}

type releasedWave2Process struct {
	cmd  *exec.Cmd
	wait chan error
	addr string
}

func startReleasedWave2(t *testing.T, binary, dbPath string) releasedWave2Process {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary)
	cmd.Env = releasedWave2Environment(os.Environ(), dbPath, addr)
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("start released binary: %v", err)
	}
	process := releasedWave2Process{cmd: cmd, wait: make(chan error, 1), addr: addr}
	go func() { process.wait <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.ProcessState != nil {
			return
		}
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-process.wait:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-process.wait
		}
	})
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-process.wait:
			t.Fatalf("released binary exited before HTTP health: %v", err)
		default:
		}
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return process
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	stopReleasedWave2(t, process)
	t.Fatalf("released binary did not become healthy at 127.0.0.1 (addr %s)", addr)
	return releasedWave2Process{}
}

func stopReleasedWave2(t *testing.T, process releasedWave2Process) {
	t.Helper()
	if process.cmd == nil || process.cmd.Process == nil {
		t.Fatal("cannot stop an unstarted released process")
	}
	if err := process.cmd.Process.Signal(os.Interrupt); err != nil && !strings.Contains(err.Error(), "process already finished") {
		t.Fatalf("signal released process: %v", err)
	}
	select {
	case err := <-process.wait:
		if err != nil {
			t.Fatalf("released process shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = process.cmd.Process.Kill()
		t.Fatal("released process did not stop after interrupt")
	}
}

func releasedWave2Environment(existing []string, dbPath, addr string) []string {
	env := make([]string, 0, len(existing)+3)
	for _, item := range existing {
		if !strings.HasPrefix(item, "NL_") {
			env = append(env, item)
		}
	}
	return append(env, "NL_DEMO_MODE=true", "NL_DB_PATH="+dbPath, "NL_LISTEN_ADDR="+addr)
}
