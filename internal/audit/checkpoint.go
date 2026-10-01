package audit

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/dantalabs/northern-lights/internal/identity"
)

type CheckpointStatus string

const (
	CheckpointVerified     CheckpointStatus = "verified"
	CheckpointMissing      CheckpointStatus = "missing"
	CheckpointMismatch     CheckpointStatus = "mismatch"
	CheckpointUnverified   CheckpointStatus = "unverified"
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
	if parseErr != nil || storedSequence != sequence || stored != hash {
		result.Status = CheckpointMismatch
		return result, nil
	}
	if len(l.checkpointPublicKey) != ed25519.PublicKeySize || signature == "" {
		result.Status = CheckpointUnverified
		return result, nil
	}
	signatureBytes, decodeErr := hex.DecodeString(signature)
	if decodeErr != nil || !ed25519.Verify(l.checkpointPublicKey, checkpointSigningBytes(tenant, sequence, hash, checkpointID), signatureBytes) {
		result.Status = CheckpointMismatch
		return result, nil
	}
	result.Status = CheckpointVerified
	return result, nil
}

func checkpointSigningBytes(tenant string, sequence int64, hash, checkpointID string) []byte {
	return []byte(strings.Join([]string{tenant, strconv.FormatInt(sequence, 10), hash, checkpointID}, "\x00"))
}
