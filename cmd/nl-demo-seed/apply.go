package main

import (
	"context"
	"errors"
	"io"

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

// Apply carries out a plan. Not implemented yet.
func Apply(ctx context.Context, p Plan, wk Workiva, out io.Writer) error {
	return errors.New("apply is not implemented yet")
}
