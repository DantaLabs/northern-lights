package workiva

import (
	"fmt"
	"strconv"
	"strings"
)

// unbounded marks a dimension of a Range that is left open, for example
// the rows of a whole-column range such as "A:A".
const unbounded = -1

// Range identifies a rectangular block of cells using zero-based,
// inclusive indexes, matching the Workiva Range object. A dimension set
// to -1 (unbounded) covers all rows or columns, as in the A1 forms
// "A:C" or "3:10".
type Range struct {
	StartRow int `json:"startRow"`
	StartCol int `json:"startColumn"`
	StopRow  int `json:"stopRow"`
	StopCol  int `json:"stopColumn"`
}

// A1ToRange converts an A1 notation string into a zero-based inclusive
// Range. Supported forms: a single cell ("B3"), a rectangle ("B3:D10"),
// a column span ("A:C"), a row span ("3:10"), and a whole column
// ("A:A"). Columns are letters with A = 1 and rows are 1-indexed, so
// "B3:D10" becomes startRow 2, startCol 1, stopRow 9, stopCol 3.
// Unbounded dimensions become -1. Input is case-insensitive and
// surrounding whitespace is trimmed; anything else is an error.
func A1ToRange(a1 string) (Range, error) {
	trimmed := strings.TrimSpace(a1)
	if trimmed == "" {
		return Range{}, fmt.Errorf("a1: empty range")
	}
	parts := strings.Split(strings.ToUpper(trimmed), ":")
	if len(parts) > 2 {
		return Range{}, fmt.Errorf("a1: too many parts in %q", a1)
	}

	if len(parts) == 1 {
		col, row, err := parseCellRef(parts[0])
		if err != nil {
			return Range{}, fmt.Errorf("a1: %q: %w", a1, err)
		}
		return Range{StartRow: row, StartCol: col, StopRow: row, StopCol: col}, nil
	}

	leftLetters, leftDigits, err := splitRef(parts[0])
	if err != nil {
		return Range{}, fmt.Errorf("a1: %q: %w", a1, err)
	}
	rightLetters, rightDigits, err := splitRef(parts[1])
	if err != nil {
		return Range{}, fmt.Errorf("a1: %q: %w", a1, err)
	}
	leftCell := leftLetters != "" && leftDigits != ""
	rightCell := rightLetters != "" && rightDigits != ""

	switch {
	case leftCell && rightCell:
		startCol, startRow, err := parseCellRef(parts[0])
		if err != nil {
			return Range{}, fmt.Errorf("a1: %q: %w", a1, err)
		}
		stopCol, stopRow, err := parseCellRef(parts[1])
		if err != nil {
			return Range{}, fmt.Errorf("a1: %q: %w", a1, err)
		}
		if startRow > stopRow || startCol > stopCol {
			return Range{}, fmt.Errorf("a1: %q: start is after stop", a1)
		}
		return Range{StartRow: startRow, StartCol: startCol, StopRow: stopRow, StopCol: stopCol}, nil

	case !leftCell && !rightCell && leftLetters != "" && rightLetters != "":
		startCol := lettersToCol(leftLetters) - 1
		stopCol := lettersToCol(rightLetters) - 1
		if startCol > stopCol {
			return Range{}, fmt.Errorf("a1: %q: start column is after stop column", a1)
		}
		return Range{StartRow: unbounded, StartCol: startCol, StopRow: unbounded, StopCol: stopCol}, nil

	case !leftCell && !rightCell && leftDigits != "" && rightDigits != "":
		startRow, err := parseRow(leftDigits)
		if err != nil {
			return Range{}, fmt.Errorf("a1: %q: %w", a1, err)
		}
		stopRow, err := parseRow(rightDigits)
		if err != nil {
			return Range{}, fmt.Errorf("a1: %q: %w", a1, err)
		}
		if startRow > stopRow {
			return Range{}, fmt.Errorf("a1: %q: start row is after stop row", a1)
		}
		return Range{StartRow: startRow, StartCol: unbounded, StopRow: stopRow, StopCol: unbounded}, nil

	default:
		return Range{}, fmt.Errorf("a1: %q mixes a cell with an open-ended row or column", a1)
	}
}

// RangeToA1 converts a Range back into A1 notation. It is the inverse of
// A1ToRange: unbounded dimensions render as column or row spans, and a
// single cell renders without a colon. A Range with inconsistent bounds
// (one side of a dimension unbounded, start after stop, or values below
// -1) is an error, as is a fully unbounded Range.
func RangeToA1(r Range) (string, error) {
	if err := validateDim("row", r.StartRow, r.StopRow); err != nil {
		return "", err
	}
	if err := validateDim("column", r.StartCol, r.StopCol); err != nil {
		return "", err
	}

	rowsOpen := r.StartRow == unbounded
	colsOpen := r.StartCol == unbounded

	switch {
	case rowsOpen && colsOpen:
		return "", fmt.Errorf("a1: fully unbounded range has no A1 representation")
	case rowsOpen:
		return colToLetters(r.StartCol+1) + ":" + colToLetters(r.StopCol+1), nil
	case colsOpen:
		return strconv.Itoa(r.StartRow+1) + ":" + strconv.Itoa(r.StopRow+1), nil
	default:
		start := colToLetters(r.StartCol+1) + strconv.Itoa(r.StartRow+1)
		stop := colToLetters(r.StopCol+1) + strconv.Itoa(r.StopRow+1)
		if start == stop {
			return start, nil
		}
		return start + ":" + stop, nil
	}
}

// validateDim checks that both bounds of one dimension are -1 together
// or set together, and that start does not exceed stop.
func validateDim(name string, start, stop int) error {
	if start < unbounded || stop < unbounded {
		return fmt.Errorf("a1: %s bound below -1", name)
	}
	if (start == unbounded) != (stop == unbounded) {
		return fmt.Errorf("a1: %s bounds must both be set or both be -1", name)
	}
	if start != unbounded && start > stop {
		return fmt.Errorf("a1: %s start is after stop", name)
	}
	return nil
}

// splitRef splits a cell reference part into its leading letters and
// trailing digits. At least one of the two must be present; letters must
// be A-Z and digits 0-9 (input is expected already uppercased).
func splitRef(s string) (letters, digits string, err error) {
	i := 0
	for i < len(s) && s[i] >= 'A' && s[i] <= 'Z' {
		i++
	}
	letters = s[:i]
	digits = s[i:]
	for j := 0; j < len(digits); j++ {
		if digits[j] < '0' || digits[j] > '9' {
			return "", "", fmt.Errorf("%q contains invalid characters", s)
		}
	}
	if letters == "" && digits == "" {
		return "", "", fmt.Errorf("empty range part")
	}
	return letters, digits, nil
}

// parseCellRef parses a full cell reference such as "B3" into zero-based
// column and row indexes.
func parseCellRef(s string) (col, row int, err error) {
	letters, digits, err := splitRef(s)
	if err != nil {
		return 0, 0, err
	}
	if letters == "" || digits == "" {
		return 0, 0, fmt.Errorf("%q is not a cell reference", s)
	}
	parsedRow, err := parseRow(digits)
	if err != nil {
		return 0, 0, fmt.Errorf("%q: %w", s, err)
	}
	return lettersToCol(letters) - 1, parsedRow, nil
}

// parseRow converts a 1-indexed A1 row number into a zero-based index.
func parseRow(digits string) (int, error) {
	n, err := strconv.Atoi(digits)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("row %q must be a positive integer", digits)
	}
	return n - 1, nil
}

// lettersToCol converts 1-based column letters ("A" = 1, "AA" = 27) to
// a number.
func lettersToCol(letters string) int {
	n := 0
	for i := 0; i < len(letters); i++ {
		n = n*26 + int(letters[i]-'A'+1)
	}
	return n
}

// colToLetters is the inverse of lettersToCol: 1 becomes "A", 27
// becomes "AA".
func colToLetters(col int) string {
	var b []byte
	for col > 0 {
		col--
		b = append([]byte{byte('A' + col%26)}, b...)
		col /= 26
	}
	return string(b)
}
