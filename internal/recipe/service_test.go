package recipe

import (
	"context"
	"testing"

	"github.com/dantalabs/northern-lights/internal/changeset"
	"github.com/dantalabs/northern-lights/internal/demoenv"
)

func TestNewStubSucceedsAndReturnsNotImplemented(t *testing.T) {
	svc, facts, err := New(demoenv.Env{}, changeset.New(demoenv.Env{}))
	if err != nil {
		t.Fatalf("New: unexpected error %v", err)
	}
	if _, err := svc.Prepare(context.Background(), "actor", "sec"); err == nil {
		t.Fatal("Prepare: want not-implemented error")
	}
	if got := svc.Sections(); got != nil {
		t.Fatalf("Sections: want nil, got %v", got)
	}
	if _, _, err := facts.LatestFacts(context.Background(), "actor", "sec"); err == nil {
		t.Fatal("LatestFacts: want not-implemented error")
	}
}
