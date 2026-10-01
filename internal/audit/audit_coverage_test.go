package audit

import (
	"context"
	"testing"
)

func TestAuditCoverageReportsV1TenantCaveat(t *testing.T) {
	log, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	entry, err := log.Append(context.Background(), Entry{Actor: "a", Tool: "t", Action: "a"})
	if err != nil {
		t.Fatal(err)
	}
	check, err := log.VerifyRange(context.Background(), entry.Seq, entry.Seq)
	if err != nil || len(check.Caveats) == 0 {
		t.Fatalf("check=%#v err=%v", check, err)
	}
}
