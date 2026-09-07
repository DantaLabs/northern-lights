package audit_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/mapping"
)

// TestConcurrentHandlesSameFile reproduces the production wiring of
// cmd/workiva-mcp: the mapping store and the audit log open the same
// SQLite file through separate handles. Concurrent writes from both
// handles must not fail with SQLITE_BUSY.
func TestConcurrentHandlesSameFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")

	store, err := mapping.Open(path)
	if err != nil {
		t.Fatalf("mapping.Open: %v", err)
	}
	defer store.Close()

	log, err := audit.Open(path)
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	defer log.Close()

	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 64)

	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if err := store.UpsertSpreadsheet(ctx, mapping.Spreadsheet{
				ID: fmt.Sprintf("sp-%d", i), Name: "s", Region: "eu",
			}); err != nil {
				errs <- fmt.Errorf("upsert: %w", err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			if _, err := log.Append(ctx, audit.Entry{
				Actor: "test", Tool: "tool", Action: "call", Target: fmt.Sprintf("t-%d", i),
			}); err != nil {
				errs <- fmt.Errorf("append: %w", err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write: %v", err)
	}
}
