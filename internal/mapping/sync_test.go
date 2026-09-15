package mapping

import (
	"context"
	"testing"
)

func TestSyncMappingRollsBackWholeBatchOnConstraintFailure(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.db.Exec(`CREATE TRIGGER fail_second_field BEFORE INSERT ON fields WHEN NEW.name = 'second' BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	err = s.SyncMapping(context.Background(), Spreadsheet{ID: "sp", Region: "eu"}, Sheet{ID: "sh", SpreadsheetID: "sp"}, []Field{
		{SpreadsheetID: "sp", SheetID: "sh", Name: "first", CellRange: "B2"},
		{SpreadsheetID: "sp", SheetID: "sh", Name: "second", CellRange: "B3"},
	})
	if err == nil {
		t.Fatal("SyncMapping succeeded; want injected failure")
	}
	for _, table := range []string{"spreadsheets", "sheets", "fields"} {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s has %d partial rows after rollback", table, count)
		}
	}
}
