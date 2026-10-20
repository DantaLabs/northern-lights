package main

import (
	"context"
	"fmt"
	"io"
	"math/big"
	"time"

	"github.com/dantalabs/northern-lights/internal/workiva"
)

// Section is a section of the report document.
type Section struct{ ID, Name string }

// Workiva is the part of the Workiva API the seed uses. *workiva.Client has
// the first two methods.
type Workiva interface {
	WriteCells(ctx context.Context, spreadsheetID, sheetID string, edits []workiva.CellEdit) error
	GetSheetDataTyped(ctx context.Context, spreadsheetID, sheetID, cellRange string, fields []string) (*workiva.SheetData, error)
	ListSections(ctx context.Context, documentID string) ([]Section, error)
	DeleteSection(ctx context.Context, documentID, sectionID string) error
}

// clearValue is what a cleared cell is sent as: the README line
// "clear-value: empty-string" in configs/demo/README.md (Live write
// semantics). Workiva rejects JSON null with HTTP 400.
const clearValue = ""

type sheetGroup struct {
	spreadsheetID, sheetID string
	cells                  []Write
}

// Apply carries out a plan: one WriteCells per sheet for the writes and
// clears, a read-back of every written cell, then the section deletes. A
// Workiva error stops it; read-back mismatches are reported and make it
// return an error at the end. It touches nothing that is not in the plan.
func Apply(ctx context.Context, p Plan, wk Workiva, out io.Writer) error {
	start := time.Now()
	groups, err := groupBySheet(p)
	if err != nil {
		return err
	}
	for _, g := range groups {
		edits := make([]workiva.CellEdit, len(g.cells))
		for i, c := range g.cells {
			r, _ := workiva.A1ToRange(c.Ref.Range)
			v := c.Value
			if v == "" {
				v = clearValue
			}
			edits[i] = workiva.CellEdit{Row: r.StartRow, Column: r.StartCol, Value: v}
		}
		if err := wk.WriteCells(ctx, g.spreadsheetID, g.sheetID, edits); err != nil {
			return fmt.Errorf("write %s / %s (from %s): %w", g.cells[0].Ref.SpreadsheetName, g.cells[0].Ref.SheetName, g.cells[0].Field, err)
		}
	}
	ok, bad := 0, 0
	for _, g := range groups {
		for _, c := range g.cells {
			read, err := readCell(ctx, wk, c)
			if err != nil {
				return fmt.Errorf("read back %s: %w", c.Field, err)
			}
			if sameDecimal(c.Value, read) {
				ok++
				continue
			}
			bad++
			if _, err := fmt.Fprintf(out, "mismatch %s: wrote %s read %s\n", c.Field, shown(c.Value), shown(read)); err != nil {
				return err
			}
		}
	}
	if len(groups) > 0 {
		if _, err := fmt.Fprintf(out, "read-back: %d ok, %d mismatch\n", ok, bad); err != nil {
			return err
		}
	}
	for _, d := range p.SectionDeletes {
		secs, err := wk.ListSections(ctx, d.DocumentID)
		if err != nil {
			return fmt.Errorf("list sections for %q: %w", d.Title, err)
		}
		n := 0
		for _, s := range secs {
			if s.Name != d.Title {
				continue
			}
			if err := wk.DeleteSection(ctx, d.DocumentID, s.ID); err != nil {
				return fmt.Errorf("delete section %q: %w", d.Title, err)
			}
			n++
		}
		if _, err := fmt.Fprintf(out, "deleted-section %q (%d)\n", d.Title, n); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(out, "done: elapsed %.1fs\n", time.Since(start).Seconds()); err != nil {
		return err
	}
	if bad > 0 {
		return fmt.Errorf("read-back: %d of %d cells do not hold the written value", bad, ok+bad)
	}
	return nil
}

// groupBySheet orders writes then clears by first appearance of each sheet.
func groupBySheet(p Plan) ([]*sheetGroup, error) {
	var groups []*sheetGroup
	idx := map[[2]string]*sheetGroup{}
	for _, c := range append(append([]Write(nil), p.Writes...), p.Clears...) {
		if _, err := workiva.A1ToRange(c.Ref.Range); err != nil {
			return nil, fmt.Errorf("field %s: %w", c.Field, err)
		}
		k := [2]string{c.Ref.SpreadsheetID, c.Ref.SheetID}
		g, ok := idx[k]
		if !ok {
			g = &sheetGroup{spreadsheetID: c.Ref.SpreadsheetID, sheetID: c.Ref.SheetID}
			idx[k] = g
			groups = append(groups, g)
		}
		g.cells = append(g.cells, c)
	}
	return groups, nil
}

// readCell returns the cell's exact decimal text, "" for an empty cell, or
// the marker "(not a number)". Values are read typed so numbers keep their
// text.
func readCell(ctx context.Context, wk Workiva, c Write) (string, error) {
	d, err := wk.GetSheetDataTyped(ctx, c.Ref.SpreadsheetID, c.Ref.SheetID, c.Ref.Range, []string{"cells.value"})
	if err != nil {
		return "", err
	}
	if d == nil || len(d.Cells) == 0 || len(d.Cells[0]) == 0 {
		return "", nil
	}
	text, ok := DecimalText(d.Cells[0][0].Value)
	if !ok {
		return notANumber, nil
	}
	return text, nil
}

const notANumber = "(not a number)"

// sameDecimal compares two decimal texts exactly. "" equals only "".
func sameDecimal(wrote, read string) bool {
	if wrote == "" || read == "" {
		return wrote == read
	}
	a, ok1 := new(big.Rat).SetString(wrote)
	b, ok2 := new(big.Rat).SetString(read)
	return ok1 && ok2 && a.Cmp(b) == 0
}

func shown(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}
