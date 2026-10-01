package audit

import (
	"context"
	"testing"
)

func TestExportLinkedSelectsOnlySubjectAuditIDs(t *testing.T) {
	log, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	ctx := context.Background()
	first, err := log.Append(ctx, Entry{AuditID: "subject-a", Actor: "actor", Tool: "tool", Action: "capture", Target: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(ctx, Entry{AuditID: "unrelated", Actor: "actor", Tool: "tool", Action: "other", Target: "b"}); err != nil {
		t.Fatal(err)
	}
	last, err := log.Append(ctx, Entry{AuditID: "subject-a", Actor: "actor", Tool: "tool", Action: "validate", Target: "a"})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := log.ExportLinked(ctx, []string{"subject-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Entries) != 2 || selected.Entries[0].Seq != first.Seq || selected.Entries[1].Seq != last.Seq {
		t.Fatalf("selected=%#v", selected)
	}
	for _, entry := range selected.Entries {
		if entry.AuditID == "unrelated" {
			t.Fatal("unrelated audit event leaked into subject selection")
		}
	}
}
