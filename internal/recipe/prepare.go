package recipe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/changeset"
)

// cellReader is the uncached read seam; *workivaprovider.Router satisfies it.
type cellReader interface {
	ReadUncached(ctx context.Context, req assurance.SourceRequest) (assurance.ProviderRead, error)
}

// errClientUnavailable is returned by Prepare when no cell reader is configured.
var errClientUnavailable = errors.New("workiva client is not available")

type cellKind int

const (
	cellEmpty cellKind = iota
	cellNumeric
	cellText
)

// cellValue is a normalised cell. text holds the canonical decimal for numeric
// cells and the raw string for text cells.
type cellValue struct {
	kind cellKind
	text string
}

// prepareNormalise converts a provider scalar. Unsupported Go types are an error.
func prepareNormalise(v any) (cellValue, error) {
	switch x := v.(type) {
	case nil:
		return cellValue{}, nil
	case json.Number:
		canonical, err := assurance.AddDecimal(x.String(), "0")
		if err != nil {
			return cellValue{}, fmt.Errorf("unsupported number %q: %w", x.String(), err)
		}
		return cellValue{kind: cellNumeric, text: canonical}, nil
	case string:
		if strings.TrimSpace(x) == "" {
			return cellValue{}, nil
		}
		if _, _, err := assurance.ParseDecimal(x); err == nil {
			canonical, err := assurance.AddDecimal(x, "0")
			if err != nil {
				return cellValue{}, err
			}
			return cellValue{kind: cellNumeric, text: canonical}, nil
		}
		return cellValue{kind: cellText, text: x}, nil
	default:
		return cellValue{}, fmt.Errorf("unsupported cell type %T", v)
	}
}

// readCell reads one mapped field through the uncached provider path.
func (s *service) readCell(ctx context.Context, reader cellReader, mappedName string) (cellValue, error) {
	f, err := s.store.GetField(ctx, mappedName)
	if err != nil {
		return cellValue{}, fmt.Errorf("look up mapped field %s: %w", mappedName, err)
	}
	if f == nil {
		return cellValue{}, fmt.Errorf("mapped field %s is not in the mapping store", mappedName)
	}
	read, err := reader.ReadUncached(ctx, assurance.SourceRequest{
		ExternalResourceID: f.SpreadsheetID, SubresourceID: f.SheetID, Locator: f.CellRange,
		Consistency: assurance.ConsistencyNone,
	})
	if err != nil {
		return cellValue{}, fmt.Errorf("read %s: %w", mappedName, err)
	}
	raw := read.Value.Value
	if read.Value.Formula != "" {
		raw = read.Value.CalculatedValue
	}
	cell, err := prepareNormalise(raw)
	if err != nil {
		return cellValue{}, fmt.Errorf("%s: %w", mappedName, err)
	}
	return cell, nil
}

// Prepare reads the section's source and prior values, computes facts and
// findings, stages the change set and stores the facts for the actor.
func (s *service) Prepare(ctx context.Context, actor, sectionID string) (Prepared, error) {
	def, ok := s.recipes[sectionID]
	if !ok {
		return Prepared{}, s.unknownSection(sectionID)
	}
	reader, ok := s.client.(cellReader) // nil interface gives ok=false; main.go never passes a typed-nil router
	if !ok {
		return Prepared{}, errClientUnavailable
	}
	if s.store == nil {
		return Prepared{}, errors.New("mapping store is not available")
	}

	findings := []Finding{}
	facts := []Fact{}
	var items []changeset.Item
	for _, f := range def.Fields {
		current, err := s.readCell(ctx, reader, f.Source)
		if err != nil {
			return Prepared{}, fmt.Errorf("field %q: %w", f.Key, err)
		}
		prior := cellValue{}
		if f.Prior != "" {
			if prior, err = s.readCell(ctx, reader, f.Prior); err != nil {
				return Prepared{}, fmt.Errorf("field %q: %w", f.Key, err)
			}
		}
		if f.Required && current.kind == cellEmpty {
			findings = append(findings, Finding{Kind: "missing", Field: f.Label, Severity: "high",
				Message: f.Label + " is required but has no value."})
		}
		if current.kind == cellEmpty {
			continue
		}
		items = append(items, changeset.Item{FieldName: f.Target, Value: current.text, Source: f.Source})
		if current.kind != cellNumeric {
			continue
		}
		in := factInput{
			Key: f.Key, Label: f.Label, Unit: f.Unit, DisplayUnit: f.DisplayUnit, Decimals: f.Decimals,
			Current: current.text, MaterialityPct: def.MaterialityPct,
		}
		if prior.kind == cellNumeric {
			in.Prior = prior.text
		}
		fact, err := computeFact(in)
		if err != nil {
			return Prepared{}, err
		}
		facts = append(facts, fact)
		more, err := fieldFindings(f, fact)
		if err != nil {
			return Prepared{}, fmt.Errorf("field %q: %w", f.Key, err)
		}
		findings = append(findings, more...)
	}
	if len(items) == 0 {
		return Prepared{}, fmt.Errorf("section %q has no source values to stage", sectionID)
	}
	if s.changes == nil {
		return Prepared{}, errors.New("change set service is not available")
	}
	blocks, err := def.readVerbatim()
	if err != nil {
		return Prepared{}, err
	}
	preview, err := s.changes.Stage(ctx, actor, items)
	if err != nil {
		return Prepared{}, fmt.Errorf("stage change set: %w", err)
	}
	if err := s.facts.replaceFacts(ctx, actor, sectionID, facts, blocks); err != nil {
		return Prepared{}, fmt.Errorf("change set %s staged but facts not stored: %w", preview.ID, err)
	}
	return Prepared{SectionID: sectionID, Findings: findings, Facts: facts, Verbatim: blocks, ChangeSet: preview}, nil
}

// fieldFindings returns the out_of_range, material_change and sign_change
// findings for one numeric field. For "%" fields min and max are in percent, so
// the percent-scaled fact.Current is the compared value.
func fieldFindings(f fieldDef, fact Fact) ([]Finding, error) {
	var out []Finding
	suffix := ""
	if f.Unit == percentUnit {
		suffix = "%"
	}
	for _, b := range []struct {
		limit string
		want  func(cmp int) bool
		word  string
	}{
		{f.Min, func(c int) bool { return c < 0 }, "below the minimum of"},
		{f.Max, func(c int) bool { return c > 0 }, "above the maximum of"},
	} {
		if b.limit == "" {
			continue
		}
		cmp, err := assurance.CompareDecimal(fact.Current, b.limit)
		if err != nil {
			return nil, err
		}
		if b.want(cmp) {
			out = append(out, Finding{Kind: "out_of_range", Field: f.Label, Severity: "high",
				Message: fmt.Sprintf("%s is %s%s, %s %s%s.", f.Label, fact.Current, suffix, b.word, b.limit, suffix)})
		}
	}
	if fact.Material {
		var amount string
		var err error
		if f.Unit == percentUnit {
			if amount, err = roundHalfAway(fact.Change, 1); err == nil {
				amount += " percentage points"
			}
		} else if amount, err = roundHalfAway(fact.ChangePct, 1); err == nil {
			amount += "%"
		}
		if err != nil {
			return nil, err
		}
		out = append(out, Finding{Kind: "material_change", Field: f.Label, Severity: "medium",
			Message: fmt.Sprintf("%s changed by %s from the prior period.", f.Label, amount)})
	}
	if fact.Prior != "" && fact.Prior != "0" && fact.Current != "0" && isNegative(fact.Prior) != isNegative(fact.Current) {
		out = append(out, Finding{Kind: "sign_change", Field: f.Label, Severity: "medium",
			Message: fmt.Sprintf("%s changed sign from %s%s to %s%s.", f.Label, fact.Prior, suffix, fact.Current, suffix)})
	}
	return out, nil
}

// unknownSection builds the R11 error listing the available sections.
func (s *service) unknownSection(id string) error {
	if len(s.recipes) == 0 {
		return fmt.Errorf("unknown section ID %q; no section recipes are loaded", id)
	}
	infos := s.Sections()
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	parts := make([]string, len(infos))
	for i, info := range infos {
		parts[i] = fmt.Sprintf("%s (%s)", info.ID, info.Title)
	}
	return fmt.Errorf("unknown section ID %q; available sections: %s", id, strings.Join(parts, ", "))
}

// readVerbatim returns each approved text file byte for byte. Files are read
// through the sections directory root, which refuses paths that escape it.
func (r recipeDef) readVerbatim() ([]VerbatimBlock, error) {
	blocks := []VerbatimBlock{}
	for _, v := range r.Verbatim {
		if r.root == nil {
			return nil, fmt.Errorf("verbatim file %q: sections directory is not available", v.File)
		}
		data, err := r.root.ReadFile(v.File)
		if err != nil {
			return nil, fmt.Errorf("verbatim file %q: %w", v.File, err)
		}
		if !utf8.Valid(data) {
			return nil, fmt.Errorf("verbatim file %q is not valid UTF-8", v.File)
		}
		blocks = append(blocks, VerbatimBlock{ID: v.ID, Title: v.Title, Text: string(data)})
	}
	return blocks, nil
}
