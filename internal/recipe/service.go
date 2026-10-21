package recipe

import (
	"context"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/dantalabs/northern-lights/internal/changeset"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/demoenv"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

const defaultSectionsDir = "configs/demo/sections"

// service holds the loaded section recipes and the collaborators Prepare uses.
type service struct {
	recipes    map[string]recipeDef
	store      *mapping.Store
	client     workivaprovider.Backend // asserted to cellReader in Prepare
	facts      factRecorder
	changes    changeset.Service
	storageCtx context.Context
}

// New returns the section recipe Service and FactStore for the demo lane. main.go
// calls New once when NL_DEMO_PIPELINE is on.
//
// Contract: New must succeed in demo mode with an empty NL_DEMO_SECTIONS_DIR,
// so the L0 flag-on server tests stay green after the lanes land. A missing or
// empty sections directory loads zero recipes; an invalid recipe is an error.
//
// The sections directory is NL_DEMO_SECTIONS_DIR, default configs/demo/sections.
//
// Document ID override: in sandbox mode (env.Cfg != nil and !env.Cfg.DemoMode) a
// non-blank NL_DEMO_DOCUMENT_ID replaces the document_id of every recipe, so one
// build can target a real Workiva document. When the variable is unset or blank,
// or in demo mode, or with a nil Cfg, the YAML value is kept. The ID is never logged.
func New(env demoenv.Env, changes changeset.Service) (Service, FactStore, error) {
	dir := os.Getenv("NL_DEMO_SECTIONS_DIR")
	if dir == "" {
		dir = defaultSectionsDir
	}
	ctx := context.Background()
	if env.Cfg != nil && env.Cfg.AuthMode == config.AuthModeEntra {
		ctx = identity.ContextWithPrincipal(ctx, identity.Principal{TenantID: env.Cfg.EntraTenantID})
	}
	var lookup fieldLookup // nil until a store exists; a recipe then reports it
	if env.Store != nil {
		lookup = env.Store
	}
	defs, err := loadRecipes(ctx, dir, lookup)
	if err != nil {
		return nil, nil, err
	}
	override := ""
	if env.Cfg != nil && !env.Cfg.DemoMode {
		if v, ok := os.LookupEnv("NL_DEMO_DOCUMENT_ID"); ok && strings.TrimSpace(v) != "" {
			override = strings.TrimSpace(v)
		}
	}
	facts, err := newFactStore(env.DB)
	if err != nil {
		return nil, nil, err
	}
	svc := &service{
		recipes: make(map[string]recipeDef, len(defs)),
		store:   env.Store, client: env.Client, facts: facts, changes: changes, storageCtx: ctx,
	}
	for _, d := range defs {
		if override != "" {
			d.DocumentID = override
		}
		svc.recipes[d.ID] = d
	}
	log.Printf("section recipes: loaded %d from %s", len(defs), dir)
	return svc, facts, nil
}

// Sections lists every loaded recipe, sorted by ID.
func (s *service) Sections() []SectionInfo {
	out := make([]SectionInfo, 0, len(s.recipes))
	for _, r := range s.recipes {
		out = append(out, SectionInfo{ID: r.ID, Title: r.Title, Period: r.Period, PriorPeriod: r.PriorPeriod, DocumentID: r.DocumentID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
