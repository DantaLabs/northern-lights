package assurance

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

func TestBackupRestoreRoundTripVerifiesRowsAuditCheckpointAndEvidence(t *testing.T) {
	db, auditPublic, auditPrivate := recoveryDB(t)
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: testTenant, ObjectID: "actor"})
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := log.Append(ctx, audit.Entry{Actor: "actor", Tool: "test", Action: "write", Target: "resource"})
	if err != nil {
		t.Fatal(err)
	}
	checkpointSignature := ed25519.Sign(auditPrivate, []byte(strings.Join([]string{testTenant, "1", entry.Hash, "audit-terminal"}, "\x00")))
	if _, err := db.Exec(`INSERT INTO assurance_checkpoints (tenant_id, checkpoint_id, checkpoint_kind, high_water_mark, checkpoint_hash, signature_hex, metadata_json, created_at) VALUES (?, 'audit-terminal', 'terminal', ?, ?, ?, '{}', CURRENT_TIMESTAMP)`, testTenant, entry.Seq, entry.Hash, hex.EncodeToString(checkpointSignature)); err != nil {
		t.Fatal(err)
	}
	rawManifest := `{"status":"complete"}`
	hash := sha256.Sum256([]byte(rawManifest))
	if _, err := db.Exec(`INSERT INTO assurance_evidence_manifests (tenant_id, manifest_id, manifest_version, subject_kind, subject_id, manifest_hash, manifest_json, audit_integrity, audit_completeness, expiry_at, legal_hold, idempotency_digest) VALUES (?, 'manifest-1', 2, 'snapshot', 's-1', ?, ?, '{}', 'complete', ?, 0, 'digest')`, testTenant, hex.EncodeToString(hash[:]), rawManifest, time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_resources (tenant_id, resource_id, provider, kind, external_id) VALUES (?, 'resource-1', 'test', 'sheet', 'external-1')`, testTenant); err != nil {
		t.Fatal(err)
	}

	manifestKey := []byte("local-backup-test-key")
	backupDir := filepath.Join(t.TempDir(), "backup")
	var drainedWriteErr error
	envelope, err := CreateBackup(ctx, db, backupDir, BackupOptions{TenantID: testTenant, SigningKey: SigningKey{HMAC: manifestKey}, CheckpointPublicKey: auditPublic, Fault: func(phase BackupPhase) error {
		if phase == BackupPhaseDatabaseComplete {
			_, drainedWriteErr = db.Exec(`INSERT INTO assurance_resources (tenant_id, resource_id, provider, kind, external_id) VALUES ('drain-test', 'r', 'p', 'k', 'e')`)
		}
		return nil
	}})
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if drainedWriteErr == nil {
		t.Fatal("raw SQLite write entered during the coordinated drain")
	}
	if envelope.Manifest.DatabaseSHA256 == "" || envelope.Manifest.Audit.Checkpoint == nil {
		t.Fatalf("manifest lacks exact database/checkpoint evidence: %#v", envelope.Manifest)
	}

	restored := filepath.Join(t.TempDir(), "restored.db")
	result, err := RestoreBackup(ctx, backupDir, restored, RestoreOptions{ExpectedTenantID: testTenant, SigningKey: SigningKey{HMAC: manifestKey}, CheckpointPublicKey: auditPublic})
	if err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	if !result.Ready {
		t.Fatal("restore did not become ready")
	}
	reopened, err := sqlitedb.Open(restored)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	var resources, manifests int
	if err := reopened.QueryRow(`SELECT count(*) FROM assurance_resources WHERE tenant_id=?`, testTenant).Scan(&resources); err != nil {
		t.Fatal(err)
	}
	if err := reopened.QueryRow(`SELECT count(*) FROM assurance_evidence_manifests WHERE tenant_id=?`, testTenant).Scan(&manifests); err != nil {
		t.Fatal(err)
	}
	if resources != 1 || manifests != 1 {
		t.Fatalf("restored rows resources=%d manifests=%d", resources, manifests)
	}
}

func TestBackupRestoreFailsClosedForTamperWrongKeyRollbackAndIncomplete(t *testing.T) {
	db, auditPublic, _ := recoveryDB(t)
	key := []byte("tamper-key")
	backupDir := filepath.Join(t.TempDir(), "backup")
	if _, err := CreateBackup(context.Background(), db, backupDir, BackupOptions{TenantID: testTenant, SigningKey: SigningKey{HMAC: key}, CheckpointPublicKey: auditPublic}); err != nil {
		t.Fatal(err)
	}

	wrong := filepath.Join(t.TempDir(), "wrong.db")
	if _, err := RestoreBackup(context.Background(), backupDir, wrong, RestoreOptions{ExpectedTenantID: testTenant, SigningKey: SigningKey{HMAC: []byte("wrong")}, CheckpointPublicKey: auditPublic}); !errors.Is(err, ErrBackupWrongKey) {
		t.Fatalf("wrong key error=%v", err)
	}
	truncatedDir := filepath.Join(t.TempDir(), "truncated")
	if _, err := CreateBackup(context.Background(), db, truncatedDir, BackupOptions{TenantID: testTenant, SigningKey: SigningKey{HMAC: key}, CheckpointPublicKey: auditPublic}); err != nil {
		t.Fatal(err)
	}
	truncatedDatabase := filepath.Join(truncatedDir, backupDatabaseName)
	if err := os.Chmod(truncatedDatabase, 0o600); err != nil {
		t.Fatal(err)
	}
	truncatedFile, err := os.OpenFile(truncatedDatabase, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := truncatedFile.Truncate(1); err != nil {
		_ = truncatedFile.Close()
		t.Fatal(err)
	}
	if err := truncatedFile.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreBackup(context.Background(), truncatedDir, filepath.Join(t.TempDir(), "truncated.db"), RestoreOptions{ExpectedTenantID: testTenant, SigningKey: SigningKey{HMAC: key}, CheckpointPublicKey: auditPublic}); !errors.Is(err, ErrBackupTampered) {
		t.Fatalf("truncation error=%v", err)
	}
	if _, err := RestoreBackup(context.Background(), filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "missing.db"), RestoreOptions{ExpectedTenantID: testTenant, SigningKey: SigningKey{HMAC: key}}); !errors.Is(err, ErrBackupIncomplete) {
		t.Fatalf("incomplete error=%v", err)
	}
	if _, err := RestoreBackup(context.Background(), backupDir, filepath.Join(t.TempDir(), "tenant.db"), RestoreOptions{ExpectedTenantID: "other-tenant", SigningKey: SigningKey{HMAC: key}, CheckpointPublicKey: auditPublic}); !errors.Is(err, ErrBackupWrongTenant) {
		t.Fatalf("wrong tenant error=%v", err)
	}
	if _, err := RestoreBackup(context.Background(), backupDir, filepath.Join(t.TempDir(), "rollback.db"), RestoreOptions{ExpectedTenantID: testTenant, SigningKey: SigningKey{HMAC: key}, CheckpointPublicKey: auditPublic, MinimumCreatedAt: time.Now().UTC().Add(time.Minute)}); !errors.Is(err, ErrBackupRollback) {
		t.Fatalf("rollback error=%v", err)
	}

	manifestPath := filepath.Join(backupDir, backupManifestName)
	if err := os.Chmod(backupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(manifestPath, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = []byte(strings.Replace(string(manifest), `"signature_hex":"`, `"signature_hex":"0`, 1))
	if err := os.WriteFile(manifestPath, manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreBackup(context.Background(), backupDir, filepath.Join(t.TempDir(), "tampered.db"), RestoreOptions{ExpectedTenantID: testTenant, SigningKey: SigningKey{HMAC: key}, CheckpointPublicKey: auditPublic}); err == nil {
		t.Fatal("tampered manifest restored")
	}

	faultDir := filepath.Join(t.TempDir(), "fault")
	_, err = CreateBackup(context.Background(), db, faultDir, BackupOptions{TenantID: testTenant, SigningKey: SigningKey{HMAC: key}, CheckpointPublicKey: auditPublic, Fault: func(phase BackupPhase) error {
		if phase == BackupPhaseDatabaseComplete {
			return errors.New("simulated crash after database seal")
		}
		return nil
	}})
	if err == nil {
		t.Fatal("fault injection unexpectedly succeeded")
	}
	if _, err := RestoreBackup(context.Background(), faultDir, filepath.Join(t.TempDir(), "fault.db"), RestoreOptions{ExpectedTenantID: testTenant, SigningKey: SigningKey{HMAC: key}, CheckpointPublicKey: auditPublic}); !errors.Is(err, ErrBackupIncomplete) {
		t.Fatalf("fault window error=%v", err)
	}
}

func TestEd25519SignedBackupReopensWithPublicVerificationKey(t *testing.T) {
	db, auditPublic, _ := recoveryDB(t)
	manifestPublic, manifestPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "ed25519")
	if _, err := CreateBackup(context.Background(), db, directory, BackupOptions{TenantID: testTenant, SigningKey: SigningKey{Ed25519Private: manifestPrivate}, CheckpointPublicKey: auditPublic}); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreBackup(context.Background(), directory, filepath.Join(t.TempDir(), "restored.db"), RestoreOptions{ExpectedTenantID: testTenant, SigningKey: SigningKey{Ed25519Public: manifestPublic}, CheckpointPublicKey: auditPublic}); err != nil {
		t.Fatal(err)
	}
}

func TestDrainGateRejectsNewWritersWhileExistingWriterDrains(t *testing.T) {
	gate := NewDrainGate()
	release, err := gate.EnterWrite(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan error, 1)
	go func() {
		releaseDrain, err := gate.BeginDrain(context.Background())
		if err == nil {
			releaseDrain()
		}
		drained <- err
	}()
	deadline := time.After(2 * time.Second)
	for !gate.Draining() {
		select {
		case <-deadline:
			t.Fatal("drain did not close writer admission")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if _, err := gate.EnterWrite(context.Background()); !errors.Is(err, ErrBackupDraining) {
		t.Fatalf("new writer error=%v, want ErrBackupDraining", err)
	}
	release()
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if release, err := gate.EnterWrite(context.Background()); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
}

func recoveryDB(t *testing.T) (*sql.DB, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.db")
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mapping.NewWithDB(db); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := audit.NewWithDB(db); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := NewWithDB(db); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, public, private
}
