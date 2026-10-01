package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/dantalabs/northern-lights/internal/identity"
)

type CheckpointStatus string

const (
	CheckpointVerified     CheckpointStatus = "verified"
	CheckpointMissing      CheckpointStatus = "missing"
	CheckpointMismatch     CheckpointStatus = "mismatch"
	CheckpointNotRequested CheckpointStatus = "not_requested"
)

type Checkpoint struct {
	CheckpointID   string           `json:"checkpoint_id"`
	Sequence       int64            `json:"sequence"`
	Hash           string           `json:"hash"`
	ExternalAnchor string           `json:"external_anchor"`
	Status         CheckpointStatus `json:"status"`
}

func (l *Log) VerifyCheckpoint(ctx context.Context, sequence int64, hash, checkpointID string) (Checkpoint, error) {
	result := Checkpoint{CheckpointID: checkpointID, Sequence: sequence, Hash: hash, ExternalAnchor: checkpointID, Status: CheckpointMissing}
	if sequence <= 0 || checkpointID == "" {
		return result, nil
	}
	tenant := identity.StorageTenant(ctx)
	if _, err := l.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS assurance_checkpoints (tenant_id TEXT NOT NULL, checkpoint_id TEXT NOT NULL, checkpoint_kind TEXT NOT NULL, high_water_mark TEXT NOT NULL, checkpoint_hash TEXT NOT NULL, signature_hex TEXT NOT NULL DEFAULT '', metadata_json TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (tenant_id, checkpoint_id))`); err != nil {
		return Checkpoint{}, err
	}
	var stored string
	err := l.db.QueryRowContext(ctx, `SELECT hash FROM audit_log WHERE tenant_id=? AND seq=?`, tenant, sequence).Scan(&stored)
	if err == sql.ErrNoRows {
		return result, nil
	}
	if err != nil {
		return Checkpoint{}, err
	}
	if stored == hash {
		result.Status = CheckpointVerified
	} else {
		result.Status = CheckpointMismatch
	}
	metadata, _ := json.Marshal(result)
	_, insertErr := l.db.ExecContext(ctx, `INSERT INTO assurance_checkpoints (tenant_id, checkpoint_id, checkpoint_kind, high_water_mark, checkpoint_hash, metadata_json, created_at) VALUES (?, ?, 'audit', ?, ?, ?, CURRENT_TIMESTAMP) ON CONFLICT(tenant_id, checkpoint_id) DO UPDATE SET high_water_mark=excluded.high_water_mark, checkpoint_hash=excluded.checkpoint_hash, metadata_json=excluded.metadata_json`, tenant, checkpointID, fmt.Sprint(sequence), hash, string(metadata))
	if insertErr != nil {
		return Checkpoint{}, insertErr
	}
	return result, nil
}
