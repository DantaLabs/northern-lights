package recipe

import (
	"context"
	"errors"

	"github.com/dantalabs/northern-lights/internal/changeset"
	"github.com/dantalabs/northern-lights/internal/demoenv"
)

var errNotImplemented = errors.New("section recipe service is not implemented yet")

type stubService struct{}

type stubFactStore struct{}

// New returns the section recipe Service and FactStore for the demo lane. main.go
// calls New once when NL_DEMO_PIPELINE is on. L2 replaces the stub bodies.
//
// Contract: New must succeed in demo mode with an empty NL_DEMO_SECTIONS_DIR,
// so the L0 flag-on server tests stay green after the lanes land.
func New(env demoenv.Env, changes changeset.Service) (Service, FactStore, error) {
	_, _ = env, changes
	return stubService{}, stubFactStore{}, nil
}

// Prepare returns a zero Prepared and an error until L2 lands.
func (stubService) Prepare(ctx context.Context, actor, sectionID string) (Prepared, error) {
	_, _, _ = ctx, actor, sectionID
	return Prepared{}, errNotImplemented
}

// Sections returns nil until L2 lands.
func (stubService) Sections() []SectionInfo {
	return nil
}

// LatestFacts returns an error until L2 lands.
func (stubFactStore) LatestFacts(ctx context.Context, actor, sectionID string) ([]Fact, []VerbatimBlock, error) {
	_, _, _ = ctx, actor, sectionID
	return nil, nil, errNotImplemented
}
