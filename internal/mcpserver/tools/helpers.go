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

// cellText renders one cell for display: literal values pass through,
// formulas resolve to their calculated value, empty cells render as "".
func cellText(c workiva.Cell) string {
	if c.Value != nil && !strings.HasPrefix(*c.Value, "=") {
		return *c.Value
	}
	if c.CalculatedValue != nil {
		if b, err := json.Marshal(c.CalculatedValue); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", c.CalculatedValue)
	}
	if c.Value != nil {
		return *c.Value
	}
	return ""
}

// gridToCachedCells converts a fetched SheetData grid into snapshot cache
// entries keyed by A1 cell reference. It returns nil when the response
// carries no range metadata, since cell addresses cannot be derived.
func gridToCachedCells(spreadsheetID, sheetID string, data *workiva.SheetData, fetchedAt time.Time) []mapping.CellValue {
	if data.Range == nil {
		return nil
	}
	var out []mapping.CellValue
	for rowIdx, row := range data.Cells {
		for colIdx := range row {
			ref, err := workiva.RangeToA1(workiva.Range{
				StartRow: data.Range.StartRow + rowIdx,
				StartCol: data.Range.StartCol + colIdx,
				StopRow:  data.Range.StartRow + rowIdx,
				StopCol:  data.Range.StartCol + colIdx,
			})
			if err != nil {
				continue
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
	return out
}

// valueFromCells joins the cached cells falling inside cellRange into a
// single display value. The boolean is false when no cached cell covers
// the range.
func valueFromCells(cells []mapping.CellValue, cellRange string) (string, bool) {
	outer, err := workiva.A1ToRange(cellRange)
	if err != nil {
		return "", false
	}
	var parts []string
	for _, c := range cells {
		inner, err := workiva.A1ToRange(c.Cell)
		if err != nil || !containsCell(outer, inner) {
			continue
		}
		parts = append(parts, c.Value)
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, ", "), true
}

// containsCell reports whether the single-cell range inner lies inside
// outer. Unbounded dimensions of outer always match.
func containsCell(outer, inner workiva.Range) bool {
	colOK := outer.StartCol < 0 || (inner.StartCol >= outer.StartCol && inner.StopCol <= outer.StopCol)
	rowOK := outer.StartRow < 0 || (inner.StartRow >= outer.StartRow && inner.StopRow <= outer.StopRow)
	return colOK && rowOK
}
