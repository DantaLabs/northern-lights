package main

import (
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"
)

// percentCellsStoreFractions mirrors the README line
// "percent-cells-store-fractions: yes" in configs/demo/README.md (Live write
// semantics): a cell with a percent number format holds 0.465 for 46.5%.
const percentCellsStoreFractions = true

// Write is one cell the seed writes. An empty Value clears the cell.
type Write struct {
	Year  string
	Field string
	Ref   FieldRef
	Value string
}

// SectionDelete names a report document section that reset removes.
type SectionDelete struct{ DocumentID, Title string }

// Plan is everything one seed run will change.
type Plan struct {
	Writes         []Write
	Clears         []Write
	SectionDeletes []SectionDelete
}

// BuildPlan plans the 2025 and 2024 writes of every recipe field, and with
// reset also the clear of every target and the delete of every recipe
// section. Every cell comes from Mappings.Field, so nothing outside the
// mapping file can be planned. On error the Plan is empty.
func BuildPlan(m Mappings, recipes []Recipe, current, prior []Row, reset bool) (Plan, error) {
	if reset && m.DocumentID == "" {
		return Plan{}, fmt.Errorf("reset needs document_id in the mapping file")
	}
	recipes = append([]Recipe(nil), recipes...)
	sort.Slice(recipes, func(i, j int) bool { return recipes[i].ID < recipes[j].ID })
	cur, pri := rowIndex(current), rowIndex(prior)
	var cw, pw []Write
	var p Plan
	for _, rc := range recipes {
		for _, f := range rc.Fields {
			c, ok := cur[f.Key]
			if !ok {
				return Plan{}, fmt.Errorf("recipe %s: key %s has no row in the 2025 CSV", rc.ID, f.Key)
			}
			pr, ok := pri[f.Key]
			if !ok {
				return Plan{}, fmt.Errorf("recipe %s: key %s has no row in the 2024 CSV", rc.ID, f.Key)
			}
			w, err := planWrite(m, "2025", f.Source, c.Value)
			if err != nil {
				return Plan{}, fmt.Errorf("recipe %s: %w", rc.ID, err)
			}
			cw = append(cw, w)
			w, err = planWrite(m, "2024", f.Prior, pr.Value)
			if err != nil {
				return Plan{}, fmt.Errorf("recipe %s: %w", rc.ID, err)
			}
			pw = append(pw, w)
			if reset {
				t, err := planWrite(m, "2025", f.Target, "")
				if err != nil {
					return Plan{}, fmt.Errorf("recipe %s: %w", rc.ID, err)
				}
				p.Clears = append(p.Clears, t)
			}
		}
		if reset {
			p.SectionDeletes = append(p.SectionDeletes, SectionDelete{DocumentID: m.DocumentID, Title: rc.Title})
		}
	}
	p.Writes = append(cw, pw...)
	return p, nil
}

func rowIndex(rows []Row) map[string]Row {
	idx := make(map[string]Row, len(rows))
	for _, r := range rows {
		idx[r.Key] = r
	}
	return idx
}

func planWrite(m Mappings, year, field, value string) (Write, error) {
	ref, ok := m.Field(field)
	if !ok {
		return Write{}, fmt.Errorf("field %s is not in the mapping file", field)
	}
	if ref.FieldType == "percent" && percentCellsStoreFractions && value != "" {
		var err error
		if value, err = percentToFraction(value); err != nil {
			return Write{}, fmt.Errorf("field %s: %w", field, err)
		}
	}
	return Write{Year: year, Field: field, Ref: ref, Value: value}, nil
}

// percentToFraction divides a plain decimal by 100 and returns the shortest
// exact decimal text (46.5 -> 0.465). Text that is not a number is an error.
func percentToFraction(s string) (string, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return "", fmt.Errorf("percent value %q is not a decimal number", s)
	}
	r.Quo(r, big.NewRat(100, 1))
	ten := big.NewRat(10, 1)
	scaled := new(big.Rat).Set(r)
	for n := 0; n < 64; n++ {
		if scaled.IsInt() {
			return r.FloatString(n), nil
		}
		scaled.Mul(scaled, ten)
	}
	return "", fmt.Errorf("percent value %q has no exact decimal fraction", s)
}

// Print writes one line per planned item and a summary. It prints spreadsheet
// and sheet names, never IDs.
func (p Plan) Print(w io.Writer) error {
	for _, x := range p.Writes {
		v := x.Value
		if v == "" {
			v = "(empty)"
		}
		if _, err := fmt.Fprintf(w, "write %s %s -> %s = %s\n", x.Year, x.Field, location(x.Ref), v); err != nil {
			return err
		}
	}
	for _, x := range p.Clears {
		if _, err := fmt.Fprintf(w, "clear %s %s -> %s\n", x.Year, x.Field, location(x.Ref)); err != nil {
			return err
		}
	}
	for _, d := range p.SectionDeletes {
		if _, err := fmt.Fprintf(w, "delete-section %q\n", d.Title); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "planned: %d writes, %d clears, %d section deletes\n", len(p.Writes), len(p.Clears), len(p.SectionDeletes))
	return err
}

func location(r FieldRef) string {
	return strings.Join([]string{r.SpreadsheetName, r.SheetName}, " / ") + " ! " + r.Range
}
