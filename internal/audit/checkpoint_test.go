package audit

import (
	"context"
	"testing"
)

func TestCheckpointVerificationMissingIsReadOnly(t *testing.T) {
	log, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	result, err := log.VerifyCheckpoint(context.Background(), 1, "hash", "anchor")
	if err != nil || result.Status != CheckpointMissing {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}
