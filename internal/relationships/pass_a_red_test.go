package relationships

import (
	"context"
	"strings"
	"testing"
)

func TestPassARefreshDoesNotInactivateDifferentExactScope(t *testing.T) {
	db := testDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Refresh(context.Background(), "tenant", Scope{ResourceIDs: []string{"a", "b"}}, Discovery{
		Complete: true, EndOfScope: true,
		Nodes: []Node{{ResourceID: "a", Kind: "spreadsheet", ExternalID: "a"}, {ResourceID: "x", Kind: "spreadsheet", ExternalID: "x"}},
		Edges: []Edge{{ID: "shared", From: "a", To: "x", Relation: "derived_from", Provenance: "observed", Confidence: "high"}},
	}, "actor", "audit"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Refresh(context.Background(), "tenant", Scope{ResourceIDs: []string{"a"}}, Discovery{Complete: true, EndOfScope: true}, "actor", "audit-2"); err != nil {
		t.Fatal(err)
	}
	var active int
	if err = db.QueryRow(`SELECT active FROM assurance_relationship_edges WHERE tenant_id='tenant' AND edge_id='shared'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("refresh of a different exact scope inactivated prior edge: active=%d", active)
	}
}

func TestPassARefreshRejectsPersistedValuesBeyondOutputSchema(t *testing.T) {
	db := testDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Refresh(context.Background(), "tenant", Scope{ResourceIDs: []string{"a"}}, Discovery{
		Complete: true, EndOfScope: true,
		Nodes: []Node{{ResourceID: "a", Kind: strings.Repeat("k", 65), ExternalID: strings.Repeat("e", 513)}},
	}, "actor", "audit")
	if err == nil {
		t.Fatal("refresh persisted node values beyond advertised runtime schema caps")
	}
}

func TestPassALocatorCannotBroadenBlankPersistedLocator(t *testing.T) {
	db := testDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Refresh(context.Background(), "tenant", Scope{ResourceIDs: []string{"root"}}, Discovery{
		Complete: true, EndOfScope: true,
		Nodes: []Node{{ResourceID: "root", Kind: "spreadsheet", ExternalID: "root"}},
	}, "operator", "audit"); err != nil {
		t.Fatal(err)
	}
	activateRelationshipAllowlist(t, db, "tenant", "actor", "root")
	if _, err = store.Traverse(context.Background(), "tenant", "actor", "root", "caller-selected-locator", "both", 1, 10, 10); err == nil {
		t.Fatal("caller locator broadened a persisted resource whose locator is blank")
	}
}
