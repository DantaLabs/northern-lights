package assurance

import (
	"github.com/dantalabs/northern-lights/internal/identity"
	"testing"
	"time"
)

func TestExpiredContentSourceCannotBeReadOrReingested(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := contentPrincipalContext(testTenant, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", identity.TokenTypeDelegated, true, false)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(time.Minute)
	input := sampleContentIngest()
	reservation := reserveContentIngest(t, store, ctx, "expiry-source-initial-key", input, now)
	created, err := store.FinalizeContentIngest(ctx, reservation, input, expiresAt, "audit-expiry-initial", now)
	if err != nil {
		t.Fatalf("finalize source: %v", err)
	}
	if _, err := store.getContentSourceAt(ctx, created.SourceArtifactID, expiresAt); err == nil {
		t.Fatal("source read succeeded at its expiry time")
	}
	reingest := sampleContentIngest()
	reingest.ExistingSourceArtifactID = created.SourceArtifactID
	reingest.ExpectedExistingSourceSHA256 = created.SourceSHA256
	reservation = reserveContentIngest(t, store, ctx, "expiry-source-reingest-key", reingest, expiresAt)
	if _, err := store.FinalizeContentIngest(ctx, reservation, reingest, expiresAt.Add(time.Hour), "audit-expiry-reingest", expiresAt); err == nil {
		t.Fatal("re-ingest succeeded after the owned source expired")
	}
}
