package recipe

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

func factStoreTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "facts.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// factStoreTestAdapters runs fn against the sqlite and memory adapters.
func factStoreTestAdapters(t *testing.T, fn func(t *testing.T, s factRecorder)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		s, err := newFactStore(factStoreTestDB(t))
		if err != nil {
			t.Fatalf("newFactStore: %v", err)
		}
		fn(t, s)
	})
	t.Run("memory", func(t *testing.T) {
		s, err := newFactStore(nil)
		if err != nil {
			t.Fatalf("newFactStore: %v", err)
		}
		fn(t, s)
	})
}

func factStoreTestFact(key string) Fact {
	return Fact{Key: key, Label: "Émissions", Unit: "m³", Current: "18400", Prior: "17000",
		Change: "1400", ChangePct: "8.2", Direction: "up", Material: true,
		Display: []string{"18 400 m³", "8%", "8.2%"}}
}

func TestScenario7LatestFactsPerActorAndSection(t *testing.T) {
	factStoreTestAdapters(t, func(t *testing.T, s factRecorder) {
		ctx := context.Background()
		facts := []Fact{factStoreTestFact("water")}
		blocks := []VerbatimBlock{{ID: "b1", Title: "Intro", Text: "Line one\r\nLigne deux: émissions — 18 400 m³\n"}}
		if err := s.replaceFacts(ctx, "analyst-a", "esg-environmental", facts, blocks); err != nil {
			t.Fatalf("replaceFacts: %v", err)
		}
		gotF, gotB, err := s.LatestFacts(ctx, "analyst-a", "esg-environmental")
		if err != nil {
			t.Fatalf("LatestFacts: %v", err)
		}
		if !reflect.DeepEqual(gotF, facts) || !reflect.DeepEqual(gotB, blocks) {
			t.Fatalf("round trip mismatch: %#v %#v", gotF, gotB)
		}
		if gotB[0].Text != blocks[0].Text {
			t.Fatalf("text not byte-equal: %q", gotB[0].Text)
		}
		for _, k := range [][2]string{{"analyst-b", "esg-environmental"}, {"analyst-a", "esg-social"}} {
			f, b, err := s.LatestFacts(ctx, k[0], k[1])
			if err != nil || f != nil || b != nil {
				t.Fatalf("%v: want nil,nil,nil got %#v %#v %v", k, f, b, err)
			}
		}
		other := identity.ContextWithPrincipal(ctx, identity.Principal{TenantID: "tenant-two"})
		f, b, err := s.LatestFacts(other, "analyst-a", "esg-environmental")
		if err != nil || f != nil || b != nil {
			t.Fatalf("other tenant saw facts: %#v %#v %v", f, b, err)
		}
	})
}

func TestFactStoreReplacesPreviousSet(t *testing.T) {
	factStoreTestAdapters(t, func(t *testing.T, s factRecorder) {
		ctx := context.Background()
		first := []Fact{factStoreTestFact("a"), factStoreTestFact("b")}
		second := []Fact{factStoreTestFact("c")}
		if err := s.replaceFacts(ctx, "x", "sec", first, []VerbatimBlock{{ID: "1"}}); err != nil {
			t.Fatal(err)
		}
		if err := s.replaceFacts(ctx, "x", "sec", second, nil); err != nil {
			t.Fatal(err)
		}
		f, b, err := s.LatestFacts(ctx, "x", "sec")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(f, second) || len(b) != 0 {
			t.Fatalf("not replaced: %#v %#v", f, b)
		}
	})
}

func TestFactStoreMemoryReturnsCopies(t *testing.T) {
	s, _ := newFactStore(nil)
	ctx := context.Background()
	facts := []Fact{factStoreTestFact("a")}
	if err := s.replaceFacts(ctx, "x", "sec", facts, nil); err != nil {
		t.Fatal(err)
	}
	facts[0].Key = "mutated"
	facts[0].Display[0] = "mutated"
	got, _, _ := s.LatestFacts(ctx, "x", "sec")
	got[0].Display[1] = "mutated"
	again, _, _ := s.LatestFacts(ctx, "x", "sec")
	if !reflect.DeepEqual(again, []Fact{factStoreTestFact("a")}) {
		t.Fatalf("stored set mutated: %#v", again)
	}
}

func TestFactStoreSQLiteSurvivesReopen(t *testing.T) {
	db := factStoreTestDB(t)
	first, err := newFactStore(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	facts := []Fact{factStoreTestFact("a")}
	blocks := []VerbatimBlock{{ID: "b", Title: "T", Text: "é"}}
	if err := first.replaceFacts(ctx, "x", "sec", facts, blocks); err != nil {
		t.Fatal(err)
	}
	second, err := newFactStore(db)
	if err != nil {
		t.Fatalf("second newFactStore: %v", err)
	}
	f, b, err := second.LatestFacts(ctx, "x", "sec")
	if err != nil || !reflect.DeepEqual(f, facts) || !reflect.DeepEqual(b, blocks) {
		t.Fatalf("reopen mismatch: %#v %#v %v", f, b, err)
	}
}
