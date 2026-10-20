package recipe

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
)

// factRecorder is the FactStore plus the write side Prepare uses.
type factRecorder interface {
	FactStore
	replaceFacts(ctx context.Context, actor, sectionID string, facts []Fact, blocks []VerbatimBlock) error
}

// newFactStore returns a SQLite-backed store when db is non-nil and an
// in-memory store when db is nil.
func newFactStore(db *sql.DB) (factRecorder, error) {
	if db == nil {
		return &factStoreMemory{sets: map[[3]string]factStoreMemorySet{}}, nil
	}
	const ddl = `CREATE TABLE IF NOT EXISTS recipe_prepared (
tenant_id TEXT NOT NULL, actor TEXT NOT NULL, section_id TEXT NOT NULL,
facts_json TEXT NOT NULL, verbatim_json TEXT NOT NULL, prepared_at TEXT NOT NULL,
PRIMARY KEY (tenant_id, actor, section_id))`
	if _, err := db.Exec(ddl); err != nil {
		return nil, fmt.Errorf("create recipe_prepared: %w", err)
	}
	return &factStoreSQLite{db: db}, nil
}

type factStoreSQLite struct{ db *sql.DB }

func (s *factStoreSQLite) replaceFacts(ctx context.Context, actor, sectionID string, facts []Fact, blocks []VerbatimBlock) error {
	factsJSON, err := json.Marshal(facts)
	if err != nil {
		return fmt.Errorf("encode facts: %w", err)
	}
	blocksJSON, err := json.Marshal(blocks)
	if err != nil {
		return fmt.Errorf("encode verbatim blocks: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO recipe_prepared
(tenant_id, actor, section_id, facts_json, verbatim_json, prepared_at) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (tenant_id, actor, section_id) DO UPDATE SET
facts_json = excluded.facts_json, verbatim_json = excluded.verbatim_json, prepared_at = excluded.prepared_at`,
		identity.StorageTenant(ctx), actor, sectionID, string(factsJSON), string(blocksJSON),
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("store prepared facts: %w", err)
	}
	return nil
}

func (s *factStoreSQLite) LatestFacts(ctx context.Context, actor, sectionID string) ([]Fact, []VerbatimBlock, error) {
	var factsJSON, blocksJSON string
	err := s.db.QueryRowContext(ctx, `SELECT facts_json, verbatim_json FROM recipe_prepared
WHERE tenant_id = ? AND actor = ? AND section_id = ?`,
		identity.StorageTenant(ctx), actor, sectionID).Scan(&factsJSON, &blocksJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read prepared facts: %w", err)
	}
	var facts []Fact
	if err := json.Unmarshal([]byte(factsJSON), &facts); err != nil {
		return nil, nil, fmt.Errorf("decode facts: %w", err)
	}
	var blocks []VerbatimBlock
	if err := json.Unmarshal([]byte(blocksJSON), &blocks); err != nil {
		return nil, nil, fmt.Errorf("decode verbatim blocks: %w", err)
	}
	return facts, blocks, nil
}

type factStoreMemorySet struct {
	facts  []Fact
	blocks []VerbatimBlock
}

type factStoreMemory struct {
	mu   sync.Mutex
	sets map[[3]string]factStoreMemorySet
}

func factStoreCloneFacts(in []Fact) []Fact {
	if in == nil {
		return nil
	}
	out := make([]Fact, len(in))
	for i, f := range in {
		out[i] = f
		if f.Display != nil {
			out[i].Display = append([]string(nil), f.Display...)
		}
	}
	return out
}

func factStoreCloneBlocks(in []VerbatimBlock) []VerbatimBlock {
	if in == nil {
		return nil
	}
	return append([]VerbatimBlock(nil), in...)
}

func (m *factStoreMemory) replaceFacts(ctx context.Context, actor, sectionID string, facts []Fact, blocks []VerbatimBlock) error {
	key := [3]string{identity.StorageTenant(ctx), actor, sectionID}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sets[key] = factStoreMemorySet{facts: factStoreCloneFacts(facts), blocks: factStoreCloneBlocks(blocks)}
	return nil
}

func (m *factStoreMemory) LatestFacts(ctx context.Context, actor, sectionID string) ([]Fact, []VerbatimBlock, error) {
	key := [3]string{identity.StorageTenant(ctx), actor, sectionID}
	m.mu.Lock()
	defer m.mu.Unlock()
	set, ok := m.sets[key]
	if !ok {
		return nil, nil, nil
	}
	return factStoreCloneFacts(set.facts), factStoreCloneBlocks(set.blocks), nil
}
