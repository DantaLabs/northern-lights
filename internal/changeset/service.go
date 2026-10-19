package changeset

import (
	"context"
	"errors"

	"github.com/dantalabs/northern-lights/internal/demoenv"
)

var errNotImplemented = errors.New("change set service is not implemented yet")

type stubService struct{}

// New returns the change set Service for the demo lane. main.go calls New once
// when NL_DEMO_PIPELINE is on. L1 replaces this stub body.
//
// Contract: New must succeed in demo mode with an empty sections directory
// (NL_DEMO_SECTIONS_DIR pointing at an empty directory), so the L0 flag-on
// server tests stay green after the lanes land.
func New(env demoenv.Env) Service {
	_ = env
	return stubService{}
}

// Stage returns a zero Preview and an error until L1 lands.
func (stubService) Stage(ctx context.Context, actor string, items []Item) (Preview, error) {
	_, _, _ = ctx, actor, items
	return Preview{}, errNotImplemented
}

// Confirm returns a zero Result and an error until L1 lands.
func (stubService) Confirm(ctx context.Context, actor, id, token string) (Result, error) {
	_, _, _, _ = ctx, actor, id, token
	return Result{}, errNotImplemented
}
