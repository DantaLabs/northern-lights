package audit

import (
	"context"
	"testing"
)

func TestAuditEvidenceCountsTenantRowsNotGlobalSequence(t *testing.T) {
	log, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	ctx := context.Background()
	first, _ := log.Append(ctx, Entry{Actor: "a", Tool: "t", Action: "a"})
	last, _ := log.Append(ctx, Entry{Actor: "a", Tool: "t", Action: "b"})
	check, err := log.VerifyRange(ctx, first.Seq, last.Seq)
	if err != nil || check.Completeness.OmittedCount != 0 {
		t.Fatalf("check=%#v err=%v", check, err)
	}
}
