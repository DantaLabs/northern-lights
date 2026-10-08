package assurance

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestProductionGateSnapshotRaceRawSQLiteAndJanitor(t *testing.T) {
	db, pub, _ := recoveryDB(t)
	gate := NewDrainGate()
	manager := NewBackupManager(db, gate)
	// A deliberately stale reservation is a janitor write target.
	_, err := db.Exec(`INSERT INTO assurance_idempotency_records
 (tenant_id,record_id,actor_id,tool,action,idempotency_digest,request_digest,state,owner_nonce,lease_expires_at,last_heartbeat_at,correlation_id,created_at,expires_at,retention_class)
 VALUES (?,'stale','actor','snapshot','capture','digest','request','reserved','owner','2000-01-01 00:00:00','2000-01-01 00:00:00','corr','2000-01-01 00:00:00','2100-01-01 00:00:00','default')`, testTenant)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	release, err := gate.EnterWrite(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A real materialization can span provider work between local transactions.
	providerDone := make(chan struct{})
	providerResult := make(chan error, 1)
	go func() {
		<-providerDone
		tx, err := db.Begin()
		if err == nil {
			_, err = tx.Exec(`INSERT INTO transfer_intents (tenant_id,transfer_id,actor_id,permission,intent_json,state,token_digest,idempotency_digest,request_digest,expires_at) VALUES (?,'transfer-complete','actor','write','{}','staged','token','intent-key','request','2100-01-01')`, testTenant)
			if err == nil {
				_, err = tx.Exec(`INSERT INTO transfer_audit_events (tenant_id,transfer_id,event_id,disposition,details,created_at) VALUES (?,'transfer-complete','event-complete','staged','{}','2026-01-01')`, testTenant)
			}
			if err == nil {
				err = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
		}
		providerResult <- err
		release()
	}()
	enteredFault := make(chan struct{})
	continueBackup := make(chan struct{})
	result := make(chan struct {
		envelope BackupEnvelope
		err      error
	}, 1)
	backupPath := filepath.Join(t.TempDir(), "snapshot")
	opts := BackupOptions{TenantID: testTenant, SigningKey: SigningKey{HMAC: []byte("gate-race-key")}, CheckpointPublicKey: pub,
		Fault: func(phase BackupPhase) error {
			if phase == BackupPhaseDatabaseComplete {
				close(enteredFault)
				<-continueBackup
			}
			return nil
		}}
	go func() {
		envelope, err := manager.Create(context.Background(), backupPath, opts)
		result <- struct {
			envelope BackupEnvelope
			err      error
		}{envelope, err}
	}()
	deadline := time.After(3 * time.Second)
	for !gate.Draining() {
		select {
		case <-deadline:
			close(providerDone)
			t.Fatal("backup never closed admission")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if _, err := gate.EnterWrite(context.Background()); !errors.Is(err, ErrBackupDraining) {
		t.Fatalf("new write admitted: %v", err)
	}
	if _, err := manager.Create(context.Background(), filepath.Join(t.TempDir(), "second"), opts); !errors.Is(err, ErrBackupDraining) {
		t.Fatalf("concurrent backup: %v", err)
	}
	select {
	case <-enteredFault:
		t.Fatal("snapshot started before provider finished")
	default:
	}
	close(providerDone)
	select {
	case <-enteredFault:
	case <-time.After(3 * time.Second):
		t.Fatal("backup did not start")
	}
	janitorCtx, cancel := context.WithCancel(context.Background())
	janitorDone := make(chan struct{})
	go func() { defer close(janitorDone); store.RunJanitorGated(janitorCtx, time.Millisecond, gate) }()
	time.Sleep(25 * time.Millisecond)
	var state string
	if err := db.QueryRow(`SELECT state FROM assurance_idempotency_records WHERE tenant_id=? AND record_id='stale'`, testTenant).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "reserved" {
		t.Fatalf("janitor wrote during drain: %s", state)
	}
	if _, err := db.Exec(`INSERT INTO assurance_resources (tenant_id,resource_id,provider,kind,external_id) VALUES (?,'bypass','test','sheet','external')`, testTenant); err == nil {
		t.Fatal("raw SQLite write bypassed query_only")
	}
	close(continueBackup)
	outcome := <-result
	if err := <-providerResult; err != nil {
		t.Fatalf("transfer transaction failed: %v", err)
	}
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if outcome.envelope.Manifest.TableRowCounts["transfer_intents"] != 1 || outcome.envelope.Manifest.TableRowCounts["transfer_audit_events"] != 1 {
		t.Fatalf("half-written transfer in snapshot: intent=%d audit=%d", outcome.envelope.Manifest.TableRowCounts["transfer_intents"], outcome.envelope.Manifest.TableRowCounts["transfer_audit_events"])
	}
	cancel()
	<-janitorDone
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM transfer_intents WHERE tenant_id=? AND transfer_id='transfer-complete'`, testTenant).Scan(&count); err != nil || count != 1 {
		t.Fatalf("completed transfer absent: count %d err %v", count, err)
	}
	if err := manager.WithWrite(context.Background(), func(db *sql.DB) error {
		_, err := db.Exec(`INSERT INTO assurance_resources (tenant_id,resource_id,provider,kind,external_id) VALUES (?,'reopened','test','sheet','external-reopened')`, testTenant)
		return err
	}); err != nil {
		t.Fatalf("gate/query_only did not reopen: %v", err)
	}
}

func TestBackupFailureRestoresReadinessAndAdmission(t *testing.T) {
	db, pub, _ := recoveryDB(t)
	gate := NewDrainGate()
	manager := NewBackupManager(db, gate)
	_, err := manager.Create(context.Background(), filepath.Join(t.TempDir(), "failed"), BackupOptions{TenantID: testTenant, SigningKey: SigningKey{HMAC: []byte("failure-key")}, CheckpointPublicKey: pub, Fault: func(phase BackupPhase) error {
		if phase == BackupPhaseDatabaseComplete {
			return errors.New("injected failure")
		}
		return nil
	}})
	if err == nil {
		t.Fatal("backup unexpectedly succeeded")
	}
	if gate.Draining() {
		t.Fatal("failed backup left gate closed")
	}
	if _, err := db.Exec(`INSERT INTO assurance_resources (tenant_id,resource_id,provider,kind,external_id) VALUES (?,'after-failure','test','sheet','external')`, testTenant); err != nil {
		t.Fatalf("query_only not reset: %v", err)
	}
}
