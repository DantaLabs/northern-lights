// Package tools implements the builtin northern-lights MCP tools, one
// file per tool. Each tool owns its JSON schema, its Copilot-facing
// description, and its handler, and communicates with Workiva and the
// mapping store only through mcpserver.Deps.
package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

// toolError is the error envelope returned to MCP clients: a short error
// message plus an optional hint telling the caller (usually an LLM) how
// to recover. It renders as JSON, so {"error": "...", "hint": "..."}
// reaches the client.
type toolError struct {
	msg  string
	hint string
}

func (e toolError) Error() string {
	b, err := json.Marshal(e)
	if err != nil {
		return e.msg
	}
	return string(b)
}

// MarshalJSON renders the envelope shape expected by MCP clients.
func (e toolError) MarshalJSON() ([]byte, error) {
	out := map[string]string{"error": e.msg}
	if e.hint != "" {
		out["hint"] = e.hint
	}
	return json.Marshal(out)
}

// fail wraps an underlying error into the tool error envelope.
func fail(err error, hint string) toolError {
	return toolError{msg: err.Error(), hint: hint}
}

// failMsg builds an envelope from a plain message.
func failMsg(msg, hint string) toolError {
	return toolError{msg: msg, hint: hint}
}

// requireDeps validates the dependencies a tool cannot run without and
// reports a clear envelope when the server was wired incompletely.
func requireDeps(deps mcpserver.Deps, needStore, needAudit bool) error {
	if deps.Store == nil {
		return failMsg("mapping store is not available", "server misconfiguration: check the DB path in the config")
	}
	if needAudit && deps.Audit == nil {
		return failMsg("audit log is not available", "server misconfiguration: check the DB path in the config")
	}
	return nil
}

// cacheTTL returns the configured read cache TTL, or 0 when no config is
// wired (tests); a zero TTL disables cache reads.
func cacheTTL(deps mcpserver.Deps) time.Duration {
	if deps.Cfg == nil {
		return 0
	}
	return deps.Cfg.ReadCacheTTL
}

// cellText renders one cell for display. Formula detection applies only to
// string values beginning with "="; other scalar values pass through safely.
func cellText(c workiva.Cell) string {
	if value, ok := c.Value.(string); ok {
		if !strings.HasPrefix(value, "=") {
			return value
		}
		if c.CalculatedValue != nil {
			return scalarText(c.CalculatedValue)
		}
		return value
	}
	if c.Value != nil {
		return scalarText(c.Value)
	}
	return ""
}

func scalarText(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	if b, err := json.Marshal(value); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", value)
}

// gridToCachedCells converts every fetched SheetData page into snapshot cache
// entries keyed by A1 cell reference. Each page keeps its own official range
// origin so pagination cannot shift later cells onto earlier rows.
func gridToCachedCells(spreadsheetID, sheetID string, data *workiva.SheetData, fetchedAt time.Time) ([]mapping.CellValue, error) {
	if data == nil {
		return nil, fmt.Errorf("sheetdata is nil")
	}
	pages := data.Pages
	if len(pages) == 0 {
		if data.Range == nil {
			if len(data.Cells) == 0 {
				return nil, nil
			}
			return nil, fmt.Errorf("sheetdata has cells but no range metadata")
		}
		pages = []workiva.SheetData{{Range: data.Range, Cells: data.Cells}}
	}

	var out []mapping.CellValue
	for pageIdx, page := range pages {
		if len(page.Cells) == 0 {
			continue
		}
		if page.Range == nil {
			return nil, fmt.Errorf("sheetdata page %d has cells but no range metadata", pageIdx+1)
		}
		startRow := page.Range.StartRow
		if startRow < 0 {
			startRow = 0
		}
		startCol := page.Range.StartCol
		if startCol < 0 {
			startCol = 0
		}
		for rowIdx, row := range page.Cells {
			rowCoord, err := addCoordinate(startRow, rowIdx)
			if err != nil {
				return nil, fmt.Errorf("sheetdata page %d row %d: %w", pageIdx+1, rowIdx, err)
			}
			if page.Range.StopRow >= 0 && rowCoord > page.Range.StopRow {
				return nil, fmt.Errorf("sheetdata page %d row %d exceeds range stop row", pageIdx+1, rowIdx)
			}
			for colIdx := range row {
				colCoord, err := addCoordinate(startCol, colIdx)
				if err != nil {
					return nil, fmt.Errorf("sheetdata page %d column %d: %w", pageIdx+1, colIdx, err)
				}
				if page.Range.StopCol >= 0 && colCoord > page.Range.StopCol {
					return nil, fmt.Errorf("sheetdata page %d column %d exceeds range stop column", pageIdx+1, colIdx)
				}
				ref, err := workiva.RangeToA1(workiva.Range{
					StartRow: rowCoord,
					StartCol: colCoord,
					StopRow:  rowCoord,
					StopCol:  colCoord,
				})
				if err != nil {
					return nil, fmt.Errorf("sheetdata page %d cell %d,%d: %w", pageIdx+1, rowIdx, colIdx, err)
				}
				out = append(out, mapping.CellValue{
					SpreadsheetID: spreadsheetID,
					SheetID:       sheetID,
					Cell:          ref,
					Value:         cellText(row[colIdx]),
					FetchedAt:     fetchedAt,
				})
			}
		}
	}
	return out, nil
}

func addCoordinate(base, offset int) (int, error) {
	maxInt := int(^uint(0) >> 1)
	if base < 0 {
		base = 0
	}
	if offset < 0 || offset > maxInt-base {
		return 0, fmt.Errorf("coordinate overflow")
	}
	return base + offset, nil
}

// maxCachedCells bounds the work needed to prove a bounded cache hit. Larger
// ranges fall back to a live read rather than allocating unbounded state.
const maxCachedCells uint64 = 100_000

// valueFromCells joins a complete bounded cached range into one display
// value. Cells are emitted in row-major order independent of storage order.
// Unbounded ranges always fall back to a live read because completeness cannot
// be proven from a finite snapshot.
func valueFromCells(cells []mapping.CellValue, cellRange string) (string, bool) {
	outer, err := workiva.A1ToRange(cellRange)
	if err != nil {
		return "", false
	}
	if outer.StartRow < 0 || outer.StartCol < 0 || outer.StopRow < 0 || outer.StopCol < 0 {
		return "", false
	}
	rows := uint64(outer.StopRow-outer.StartRow) + 1
	columns := uint64(outer.StopCol-outer.StartCol) + 1
	if rows > maxCachedCells || columns > maxCachedCells || rows > maxCachedCells/columns {
		return "", false
	}

	capacity := len(cells)
	if max := int(rows * columns); capacity > max {
		capacity = max
	}
	byCoordinate := make(map[[2]int]string, capacity)
	for _, c := range cells {
		inner, err := workiva.A1ToRange(c.Cell)
		if err != nil || inner.StartRow != inner.StopRow || inner.StartCol != inner.StopCol ||
			!containsCell(outer, inner) {
			continue
		}
		byCoordinate[[2]int{inner.StartRow, inner.StartCol}] = c.Value
	}

	parts := make([]string, 0, int(rows*columns))
	for rowOffset := uint64(0); rowOffset < rows; rowOffset++ {
		row := outer.StartRow + int(rowOffset)
		for columnOffset := uint64(0); columnOffset < columns; columnOffset++ {
			column := outer.StartCol + int(columnOffset)
			value, ok := byCoordinate[[2]int{row, column}]
			if !ok {
				return "", false
			}
			parts = append(parts, value)
		}
	}
	return joinNonEmpty(parts), true
}

// containsCell reports whether the single-cell range inner lies inside
// outer. Unbounded dimensions of outer always match.
func containsCell(outer, inner workiva.Range) bool {
	colOK := outer.StartCol < 0 || (inner.StartCol >= outer.StartCol && inner.StopCol <= outer.StopCol)
	rowOK := outer.StartRow < 0 || (inner.StartRow >= outer.StartRow && inner.StopRow <= outer.StopRow)
	return colOK && rowOK
}
