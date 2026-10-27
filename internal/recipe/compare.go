package recipe

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Comparer compares a section's fields with the prior period without staging
// a change set or storing facts. It is separate from Service so existing
// Service fakes keep compiling; the value New returns satisfies both.
type Comparer interface {
	Compare(ctx context.Context, sectionID string, fields []string) (Comparison, error)
}

var _ Comparer = (*service)(nil)

// Comparison is a read-only period comparison for one section. Facts use the
// same arithmetic and display forms as Prepare, so a percentage quoted in an
// answer matches the one the narrative will use.
type Comparison struct {
	SectionID, SectionTitle, Period, PriorPeriod string
	MaterialityPct                               string
	Items                                        []ComparedField
}

// Cell locates a mapped field for a citation.
type Cell struct{ SpreadsheetID, SheetID, Range string }

// ComparedField is one field of a comparison. Fact is empty when Missing is
// true or the current value is text.
type ComparedField struct {
	Fact
	Missing          bool
	CurrentCell      Cell
	PriorCell        *Cell // nil when the recipe has no prior field
	CurrentFieldName string
	PriorFieldName   string
}

// Compare reads the current and prior values of the selected fields. An empty
// fields list selects every field; each entry matches a field key exactly or a
// label case-insensitively as a substring.
func (s *service) Compare(ctx context.Context, sectionID string, fields []string) (Comparison, error) {
	def, ok := s.recipes[sectionID]
	if !ok {
		return Comparison{}, s.unknownSection(sectionID)
	}
	reader, ok := s.client.(cellReader)
	if !ok {
		return Comparison{}, errClientUnavailable
	}
	if s.store == nil {
		return Comparison{}, errors.New("mapping store is not available")
	}
	selected, err := selectFields(def, fields)
	if err != nil {
		return Comparison{}, err
	}
	out := Comparison{SectionID: def.ID, SectionTitle: def.Title, Period: def.Period, PriorPeriod: def.PriorPeriod,
		MaterialityPct: def.MaterialityPct, Items: make([]ComparedField, 0, len(selected))}
	for _, f := range selected {
		item := ComparedField{Fact: Fact{Key: f.Key, Label: f.Label, Unit: f.Unit}, CurrentFieldName: f.Source, PriorFieldName: f.Prior}
		if item.CurrentCell, err = s.cellOf(ctx, f.Source); err != nil {
			return Comparison{}, fmt.Errorf("field %q: %w", f.Key, err)
		}
		current, err := s.readCell(ctx, reader, f.Source)
		if err != nil {
			return Comparison{}, fmt.Errorf("field %q: %w", f.Key, err)
		}
		prior := cellValue{}
		if f.Prior != "" {
			cell, err := s.cellOf(ctx, f.Prior)
			if err != nil {
				return Comparison{}, fmt.Errorf("field %q: %w", f.Key, err)
			}
			item.PriorCell = &cell
			if prior, err = s.readCell(ctx, reader, f.Prior); err != nil {
				return Comparison{}, fmt.Errorf("field %q: %w", f.Key, err)
			}
		}
		switch current.kind {
		case cellEmpty:
			item.Missing = true
		case cellNumeric:
			in := factInput{Key: f.Key, Label: f.Label, Unit: f.Unit, DisplayUnit: f.DisplayUnit, Decimals: f.Decimals,
				Current: current.text, MaterialityPct: def.MaterialityPct}
			if prior.kind == cellNumeric {
				in.Prior = prior.text
			}
			if item.Fact, err = computeFact(in); err != nil {
				return Comparison{}, err
			}
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

func (s *service) cellOf(ctx context.Context, mappedName string) (Cell, error) {
	f, err := s.store.GetField(ctx, mappedName)
	if err != nil {
		return Cell{}, fmt.Errorf("look up mapped field %s: %w", mappedName, err)
	}
	if f == nil {
		return Cell{}, fmt.Errorf("mapped field %s is not in the mapping store", mappedName)
	}
	return Cell{SpreadsheetID: f.SpreadsheetID, SheetID: f.SheetID, Range: f.CellRange}, nil
}

// selectFields keeps recipe order. An entry that matches nothing is an error
// listing the field keys, so the caller can retry with a valid one.
func selectFields(def recipeDef, wanted []string) ([]fieldDef, error) {
	if len(wanted) == 0 {
		return def.Fields, nil
	}
	keep := make([]bool, len(def.Fields))
	for _, w := range wanted {
		needle := strings.ToLower(strings.TrimSpace(w))
		matched := false
		for i, f := range def.Fields {
			if needle != "" && (f.Key == w || strings.Contains(strings.ToLower(f.Label), needle)) {
				keep[i], matched = true, true
			}
		}
		if !matched {
			keys := make([]string, len(def.Fields))
			for i, f := range def.Fields {
				keys[i] = f.Key
			}
			return nil, fmt.Errorf("no field in section %q matches %q; field keys: %s", def.ID, w, strings.Join(keys, ", "))
		}
	}
	out := []fieldDef{}
	for i, f := range def.Fields {
		if keep[i] {
			out = append(out, f)
		}
	}
	return out, nil
}
