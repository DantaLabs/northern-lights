package assurance

import (
	"database/sql"
	"testing"
)

func TestEvidenceStorageMustBeExplicit(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	store, err := NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if store.evidenceStorage != nil {
		t.Fatal("implicit memory storage configured")
	}
}
