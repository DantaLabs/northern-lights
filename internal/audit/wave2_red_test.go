package audit

import (
	"context"
	"testing"
)

func TestWave2BoundedAuditRangeSeparatesChainFromCompleteness(t *testing.T) {
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
	last, err := log.Append(ctx, Entry{Actor: "actor", Tool: "tool", Action: "two"})
	if err != nil {
		t.Fatal(err)
	}
	rangeResult, err := log.ExportRange(ctx, first.Seq, last.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(rangeResult.Entries) != 2 || rangeResult.FirstSeq != first.Seq || rangeResult.LastHash != last.Hash {
		t.Fatalf("range = %#v", rangeResult)
	}
	verified, err := log.VerifyRange(ctx, first.Seq, last.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if !verified.ChainVerified || verified.Completeness.Status != "complete" {
		t.Fatalf("verification = %#v", verified)
	}
}

func TestWave2CheckpointReportsVerifiedMissingAndMismatch(t *testing.T) {
	log, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	ctx := context.Background()
	entry, err := log.Append(ctx, Entry{Actor: "actor", Tool: "tool", Action: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.db.Exec(`CREATE TABLE assurance_checkpoints (tenant_id TEXT NOT NULL, checkpoint_id TEXT NOT NULL, checkpoint_kind TEXT NOT NULL, high_water_mark TEXT NOT NULL, checkpoint_hash TEXT NOT NULL, signature_hex TEXT NOT NULL, metadata_json TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (tenant_id, checkpoint_id))`); err != nil {
		t.Fatal(err)
	}
	if _, err := log.db.Exec(`INSERT INTO assurance_checkpoints (tenant_id, checkpoint_id, checkpoint_kind, high_water_mark, checkpoint_hash, signature_hex, metadata_json, created_at) VALUES (?, 'checkpoint-1', 'external', ?, ?, 'signed', '{}', CURRENT_TIMESTAMP)`, "legacy-api-key", entry.Seq, entry.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err := log.db.Exec(`INSERT INTO assurance_checkpoints (tenant_id, checkpoint_id, checkpoint_kind, high_water_mark, checkpoint_hash, signature_hex, metadata_json, created_at) VALUES (?, 'checkpoint-2', 'external', ?, 'wrong', 'signed', '{}', CURRENT_TIMESTAMP)`, "legacy-api-key", entry.Seq); err != nil {
		t.Fatal(err)
	}
	verified, err := log.VerifyCheckpoint(ctx, entry.Seq, entry.Hash, "checkpoint-1")
	if err != nil || verified.Status != CheckpointVerified {
		t.Fatalf("verified checkpoint = %#v, %v", verified, err)
	}
	mismatch, err := log.VerifyCheckpoint(ctx, entry.Seq, entry.Hash, "checkpoint-2")
	if err != nil || mismatch.Status != CheckpointMismatch {
		t.Fatalf("mismatch checkpoint = %#v, %v", mismatch, err)
	}
	missing, err := log.VerifyCheckpoint(ctx, entry.Seq+1, entry.Hash, "checkpoint-3")
	if err != nil || missing.Status != CheckpointMissing {
		t.Fatalf("missing checkpoint = %#v, %v", missing, err)
	}
}
