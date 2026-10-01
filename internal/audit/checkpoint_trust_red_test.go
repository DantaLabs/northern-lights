package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"testing"
)

func TestVerifyRangeDoesNotClaimCompleteWithoutTerminalAnchor(t *testing.T) {
	log, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	entry, err := log.Append(context.Background(), Entry{Actor: "actor", Tool: "tool", Action: "action"})
	if err != nil {
		t.Fatal(err)
	}
	verification, err := log.VerifyRange(context.Background(), entry.Seq, entry.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if verification.Completeness.Status == "complete" || verification.Completeness.TerminalAnchorVerified || verification.Completeness.FinalRowDeletionDetectable {
		t.Fatalf("unanchored completeness overclaimed: %#v", verification.Completeness)
	}
}

func TestVerifyCheckpointRequiresValidSignature(t *testing.T) {
	log, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	createCheckpointTable(t, log)
	ctx := context.Background()
	entry, err := log.Append(ctx, Entry{Actor: "actor", Tool: "tool", Action: "action"})
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	log.SetCheckpointPublicKey(publicKey)
	message := checkpointSigningBytes("legacy-api-key", entry.Seq, entry.Hash, "checkpoint-valid")
	signature := ed25519.Sign(privateKey, message)
	if _, err := log.db.Exec(`INSERT INTO assurance_checkpoints (tenant_id, checkpoint_id, checkpoint_kind, high_water_mark, checkpoint_hash, signature_hex, metadata_json, created_at) VALUES (?, ?, 'external', ?, ?, ?, '{}', CURRENT_TIMESTAMP)`, "legacy-api-key", "checkpoint-valid", entry.Seq, entry.Hash, hex.EncodeToString(signature)); err != nil {
		t.Fatal(err)
	}
	valid, err := log.VerifyCheckpoint(ctx, entry.Seq, entry.Hash, "checkpoint-valid")
	if err != nil || valid.Status != CheckpointVerified {
		t.Fatalf("valid checkpoint=%#v err=%v", valid, err)
	}
	if _, err := log.db.Exec(`UPDATE assurance_checkpoints SET signature_hex='not-a-valid-signature' WHERE tenant_id=? AND checkpoint_id=?`, "legacy-api-key", "checkpoint-valid"); err != nil {
		t.Fatal(err)
	}
	invalid, err := log.VerifyCheckpoint(ctx, entry.Seq, entry.Hash, "checkpoint-valid")
	if err != nil || invalid.Status != CheckpointMismatch {
		t.Fatalf("invalid checkpoint=%#v err=%v", invalid, err)
	}
}

func TestVerifyCheckpointReturnsUnverifiedWithoutTrustBoundary(t *testing.T) {
	log, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	createCheckpointTable(t, log)
	entry, err := log.Append(context.Background(), Entry{Actor: "actor", Tool: "tool", Action: "action"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.db.Exec(`INSERT INTO assurance_checkpoints (tenant_id, checkpoint_id, checkpoint_kind, high_water_mark, checkpoint_hash, signature_hex, metadata_json, created_at) VALUES (?, ?, 'external', ?, ?, 'anything', '{}', CURRENT_TIMESTAMP)`, "legacy-api-key", "checkpoint-untrusted", entry.Seq, entry.Hash); err != nil {
		t.Fatal(err)
	}
	got, err := log.VerifyCheckpoint(context.Background(), entry.Seq, entry.Hash, "checkpoint-untrusted")
	if err != nil || got.Status != CheckpointUnverified {
		t.Fatalf("checkpoint=%#v err=%v", got, err)
	}
}

func createCheckpointTable(t *testing.T, log *Log) {
	t.Helper()
	if _, err := log.db.Exec(`CREATE TABLE assurance_checkpoints (tenant_id TEXT NOT NULL, checkpoint_id TEXT NOT NULL, checkpoint_kind TEXT NOT NULL, high_water_mark TEXT NOT NULL, checkpoint_hash TEXT NOT NULL, signature_hex TEXT NOT NULL, metadata_json TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (tenant_id, checkpoint_id))`); err != nil {
		t.Fatal(err)
	}
}

func TestAllV1AndDeletedFinalRowRemainUncomplete(t *testing.T) {
	log, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	ctx := context.Background()
	first, err := log.Append(ctx, Entry{Actor: "actor", Tool: "tool", Action: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.db.Exec(`DELETE FROM audit_log WHERE seq=?`, first.Seq); err != nil {
		t.Fatal(err)
	}
	// A remaining all-v1 segment and a missing final row cannot establish a
	// signed terminal anchor merely from mutable chain rows.
	if _, err := log.db.Exec(`INSERT INTO audit_log (seq, ts, tenant_id, hash_version, actor, tool, action, target, before_json, after_json, workiva_op_url, audit_id, prev_hash, hash) VALUES (1, CURRENT_TIMESTAMP, 'legacy-api-key', 1, 'actor', 'tool', 'action', '', '', '', '', '', 'GENESIS', 'bad')`); err != nil {
		t.Fatal(err)
	}
	verification, err := log.VerifyRange(ctx, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if verification.Completeness.Status == "complete" || verification.Completeness.FinalRowDeletionDetectable {
		t.Fatalf("all-v1/deleted-final-row completeness overclaimed: %#v", verification)
	}
}
