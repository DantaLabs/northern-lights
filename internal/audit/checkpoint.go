package audit

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

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
	var highWater, stored, signature, kind string
	err := l.db.QueryRowContext(ctx, `SELECT checkpoint_kind, high_water_mark, checkpoint_hash, signature_hex FROM assurance_checkpoints WHERE tenant_id=? AND checkpoint_id=?`, tenant, checkpointID).Scan(&kind, &highWater, &stored, &signature)
	if err == sql.ErrNoRows {
		return result, nil
	}
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return result, nil
		}
		return Checkpoint{}, err
	}
	storedSequence, parseErr := strconv.ParseInt(highWater, 10, 64)
	if parseErr == nil && storedSequence == sequence && stored == hash && (signature != "" || kind == "external") {
		result.Status = CheckpointVerified
	} else {
		result.Status = CheckpointMismatch
	}
	return result, nil
}
