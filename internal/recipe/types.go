// Package recipe prepares a section recipe: findings, facts and verbatim blocks
// for one section of a report document.
package recipe

import (
	"context"

	"github.com/dantalabs/northern-lights/internal/changeset"
)

// Finding is a check result raised while preparing a section.
// Kind: missing | out_of_range | material_change | sign_change
type Finding struct{ Kind, Field, Severity, Message string }

// Fact is a narrative fact derived from a field and its prior period.
type Fact struct {
	Key, Label, Unit  string
	Current, Prior    string // canonical decimal strings
	Change, ChangePct string // canonical decimal strings, empty when prior is missing or zero
	Direction         string // up | down | flat | new
	Material          bool
	Display           []string // every approved display form, e.g. "1.24 GWh", "8%", "8.2%"
}

// VerbatimBlock is approved text that must be reproduced exactly.
type VerbatimBlock struct{ ID, Title, Text string }

// Prepared is the result of preparing a section recipe.
type Prepared struct {
	SectionID string
	Findings  []Finding
	Facts     []Fact
	Verbatim  []VerbatimBlock
	ChangeSet changeset.Preview
}

// SectionInfo describes a recipe section. Parent names the existing report
// chapter the section is created under; empty means top level.
type SectionInfo struct{ ID, Title, Period, PriorPeriod, DocumentID, Parent string }

// Service prepares section recipes.
type Service interface {
	Prepare(ctx context.Context, actor, sectionID string) (Prepared, error)
	Sections() []SectionInfo
}

// FactStore reads the latest facts and verbatim blocks for a section.
type FactStore interface {
	LatestFacts(ctx context.Context, actor, sectionID string) ([]Fact, []VerbatimBlock, error)
}
