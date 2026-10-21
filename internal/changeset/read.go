package changeset

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

func boundedRange(ref string) (workiva.Range, error) {
	r, err := workiva.A1ToRange(ref)
	if err != nil {
		return r, err
	}
	if r.StartRow < 0 || r.StartCol < 0 || r.StopRow < r.StartRow || r.StopCol < r.StartCol {
		return r, fmt.Errorf("field requires a bounded A1 range: %s", ref)
	}
	return r, nil
}

// read bypasses the snapshot cache and preserves individual cells for both
// verification and cache refresh. Empty cells remain part of verification.
func (s *service) read(ctx context.Context, item stagedItem) (string, []mapping.CellValue, error) {
	data, err := s.env.Client.GetSheetData(ctx, item.SpreadsheetID, item.SheetID, item.Range, []string{"cells.value", "cells.calculatedValue"})
	if err != nil {
		return "", nil, err
	}
	if data == nil {
		return "", nil, fmt.Errorf("no sheet data for %s", item.FieldName)
	}
	r, err := boundedRange(item.Range)
	if err != nil {
		return "", nil, err
	}
	pages := data.Pages
	if len(pages) == 0 {
		pages = []workiva.SheetData{*data}
	}
	values := map[[2]int]string{}
	for _, page := range pages {
		origin := r
		if page.Range != nil {
			origin = *page.Range
		}
		for row, cells := range page.Cells {
			for col, cell := range cells {
				y, x := origin.StartRow+row, origin.StartCol+col
				if y < r.StartRow || y > r.StopRow || x < r.StartCol || x > r.StopCol {
					return "", nil, fmt.Errorf("read outside mapped range for %s", item.FieldName)
				}
				values[[2]int{y, x}] = cellText(cell)
			}
		}
	}
	var cells []mapping.CellValue
	var parts []string
	now := time.Now().UTC()
	for row := r.StartRow; row <= r.StopRow; row++ {
		for col := r.StartCol; col <= r.StopCol; col++ {
			ref, err := workiva.RangeToA1(workiva.Range{StartRow: row, StopRow: row, StartCol: col, StopCol: col})
			if err != nil {
				return "", nil, err
			}
			value := values[[2]int{row, col}]
			cells = append(cells, mapping.CellValue{SpreadsheetID: item.SpreadsheetID, SheetID: item.SheetID, Cell: ref, Value: value, FetchedAt: now})
			if value != "" {
				parts = append(parts, value)
			}
		}
	}
	return strings.Join(parts, ", "), cells, nil
}

// Copied from the existing tool display helpers so update_field stays intact.
func cellText(cell workiva.Cell) string {
	value := cell.Value
	if text, ok := value.(string); ok && strings.HasPrefix(text, "=") && cell.CalculatedValue != nil {
		value = cell.CalculatedValue
	}
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(raw)
}
