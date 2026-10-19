package sectiontext

import (
	"context"
	"errors"

	"github.com/dantalabs/northern-lights/internal/demoenv"
	"github.com/dantalabs/northern-lights/internal/recipe"
)

var errNotImplemented = errors.New("section text service is not implemented yet")

type stubService struct{}

// New returns the section text Service for the demo lane. main.go calls New once
// when NL_DEMO_PIPELINE is on. L3 replaces the stub body.
//
// Contract: New must succeed in demo mode with an empty NL_DEMO_SECTIONS_DIR,
// so the L0 flag-on server tests stay green after the lanes land.
func New(env demoenv.Env, recipes recipe.Service, facts recipe.FactStore) Service {
	_, _, _ = env, recipes, facts
	return stubService{}
}

// Stage returns a zero Preview and an error until L3 lands.
func (stubService) Stage(ctx context.Context, actor, sectionID string, blocks []Block) (Preview, error) {
	_, _, _, _ = ctx, actor, sectionID, blocks
	return Preview{}, errNotImplemented
}

// Confirm returns a zero Result and an error until L3 lands.
func (stubService) Confirm(ctx context.Context, actor, id, token string) (Result, error) {
	_, _, _, _ = ctx, actor, id, token
	return Result{}, errNotImplemented
}
