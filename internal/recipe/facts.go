package recipe

// Narrative fact arithmetic and display forms.
//
// All arithmetic is exact decimal arithmetic on math/big and the helpers in
// internal/assurance/decimal.go; no binary floating point is involved.
//
// Rounding: every display value is rounded half away from zero. The codebase
// has no display-rounding convention; assurance validation uses half-even for
// ratio comparisons, which is a comparison rule and not a display rule. A
// value that rounds to zero is shown unsigned ("-0" never appears).
//
// Percent fields (Unit "%"): the stored cell value is a fraction
// (configs/demo/README.md percent-cells-store-fractions: yes), so Current and
// Prior are scaled x100 before anything else and Fact.Current/Prior are
// percent. The change is shown both as an absolute change in % and in
// percentage points. Material is decided in percentage points, not in
// relative percent. Recipe min/max for % fields are written in percent
// (applied when the recipe is prepared, not here).
//
// Display forms (order is part of the contract, the number guard matches
// them as exact tokens): current, prior (when present), change, [percentage
// points for % fields], percent at 0 dp, percent at 1 dp. When the change is
// negative the unsigned magnitudes of the change and percent forms follow,
// because narrative text writes "fell 3%". A positive change carries no "+".

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/dantalabs/northern-lights/internal/assurance"
)

// factInput is one field's values in its stored unit, as canonical decimal text.
type factInput struct {
	Key, Label, Unit string
	DisplayUnit      string // "" means Unit
	Decimals         int    // 0..6
	Current          string // decimal text as read from the cell, required
	Prior            string // decimal text, "" when the prior value is missing
	MaterialityPct   string // decimal text, e.g. "10"
}

const percentUnit = "%"

var unitFamilies = [][]string{
	{"Wh", "kWh", "MWh", "GWh"},
	{"t", "kt"},
}

// unitShift returns n such that a value in `from` equals value * 1000^n in `to`.
// Families: Wh, kWh, MWh, GWh and t, kt. to=="" or to==from gives 0. Any other
// pair is an error.
func unitShift(from, to string) (int, error) {
	if to == "" || to == from {
		return 0, nil
	}
	for _, family := range unitFamilies {
		fi, ti := -1, -1
		for i, name := range family {
			if name == from {
				fi = i
			}
			if name == to {
				ti = i
			}
		}
		if fi >= 0 && ti >= 0 {
			return fi - ti, nil
		}
	}
	return 0, fmt.Errorf("cannot convert unit %q to %q", from, to)
}

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// canonicalText returns the canonical form of decimal text.
func canonicalText(value string) (string, error) {
	return assurance.AddDecimal(value, "0")
}

// shiftTen returns value * 10^exp as canonical decimal text.
func shiftTen(value string, exp int) (string, error) {
	coefficient, scale, err := assurance.ParseDecimal(value)
	if err != nil {
		return "", err
	}
	scale -= exp
	if scale < 0 {
		coefficient.Mul(coefficient, pow10(-scale))
		scale = 0
	}
	return canonicalText(fixedText(coefficient, scale))
}

// fixedText renders coefficient / 10^scale with exactly scale decimals.
func fixedText(coefficient *big.Int, scale int) string {
	digits := new(big.Int).Abs(coefficient).String()
	if scale > 0 {
		if len(digits) <= scale {
			digits = strings.Repeat("0", scale-len(digits)+1) + digits
		}
		digits = digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
	}
	if coefficient.Sign() < 0 && strings.Trim(digits, "0.") != "" {
		return "-" + digits
	}
	return digits
}

// roundHalfAway rounds decimal text to places decimals, half away from zero, and
// returns fixed-point text with exactly places decimals ("-0" never appears: a
// result that rounds to zero is unsigned).
func roundHalfAway(value string, places int) (string, error) {
	if places < 0 {
		return "", fmt.Errorf("negative decimal places %d", places)
	}
	coefficient, scale, err := assurance.ParseDecimal(value)
	if err != nil {
		return "", err
	}
	if scale <= places {
		coefficient.Mul(coefficient, pow10(places-scale))
		return fixedText(coefficient, places), nil
	}
	divisor := pow10(scale - places)
	quotient, remainder := new(big.Int).QuoRem(new(big.Int).Abs(coefficient), divisor, new(big.Int))
	if new(big.Int).Lsh(remainder, 1).Cmp(divisor) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if coefficient.Sign() < 0 {
		quotient.Neg(quotient)
	}
	return fixedText(quotient, places), nil
}

// groupThousands inserts commas in the integer part: "18400.5" -> "18,400.5",
// "-1240000" -> "-1,240,000".
func groupThousands(fixed string) string {
	sign := ""
	if strings.HasPrefix(fixed, "-") {
		sign, fixed = "-", fixed[1:]
	}
	whole, fraction, hasFraction := strings.Cut(fixed, ".")
	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	out := sign + b.String()
	if hasFraction {
		out += "." + fraction
	}
	return out
}

func isNegative(value string) bool { return strings.HasPrefix(value, "-") }

func computeFact(in factInput) (Fact, error) {
	fact, err := computeFactValues(in)
	if err != nil {
		return Fact{}, fmt.Errorf("field %q: %w", in.Key, err)
	}
	return fact, nil
}

func computeFactValues(in factInput) (Fact, error) {
	if in.Decimals < 0 || in.Decimals > 6 {
		return Fact{}, fmt.Errorf("decimals %d outside 0..6", in.Decimals)
	}
	shift, err := unitShift(in.Unit, in.DisplayUnit)
	if err != nil {
		return Fact{}, err
	}
	percent := in.Unit == percentUnit
	materiality, err := canonicalText(in.MaterialityPct)
	if err != nil {
		return Fact{}, fmt.Errorf("materiality: %w", err)
	}
	current, err := canonicalText(in.Current)
	if err != nil {
		return Fact{}, fmt.Errorf("current value: %w", err)
	}
	prior := ""
	if in.Prior != "" {
		if prior, err = canonicalText(in.Prior); err != nil {
			return Fact{}, fmt.Errorf("prior value: %w", err)
		}
	}
	if percent {
		if current, err = shiftTen(current, 2); err != nil {
			return Fact{}, err
		}
		if prior != "" {
			if prior, err = shiftTen(prior, 2); err != nil {
				return Fact{}, err
			}
		}
	}

	fact := Fact{Key: in.Key, Label: in.Label, Unit: in.Unit, Current: current, Prior: prior, Direction: "new"}
	hasPrior := prior != "" && prior != "0"
	change := ""
	if hasPrior {
		if change, err = assurance.SubtractDecimal(current, prior); err != nil {
			return Fact{}, err
		}
		fact.Change = change
		fact.Direction = "flat"
		if change != "0" {
			fact.Direction = "up"
			if isNegative(change) {
				fact.Direction = "down"
			}
		}
		scaled, err := shiftTen(change, 2)
		if err != nil {
			return Fact{}, err
		}
		absPrior, err := assurance.AbsoluteDecimal(prior)
		if err != nil {
			return Fact{}, err
		}
		if fact.ChangePct, err = assurance.DivideDecimal(scaled, absPrior, assurance.RoundHalfUp); err != nil {
			return Fact{}, err
		}
		measure := fact.ChangePct
		if percent {
			measure = change
		}
		if measure, err = assurance.AbsoluteDecimal(measure); err != nil {
			return Fact{}, err
		}
		cmp, err := assurance.CompareDecimal(measure, materiality)
		if err != nil {
			return Fact{}, err
		}
		fact.Material = cmp >= 0
	}

	suffix := ""
	switch {
	case percent:
		suffix = "%"
	case in.Unit == "number":
	case in.DisplayUnit != "":
		suffix = " " + in.DisplayUnit
	default:
		suffix = " " + in.Unit
	}
	show := func(value string) (string, error) {
		shifted, err := shiftTen(value, 3*shift)
		if err != nil {
			return "", err
		}
		rounded, err := roundHalfAway(shifted, in.Decimals)
		if err != nil {
			return "", err
		}
		return groupThousands(rounded) + suffix, nil
	}

	display := []string{}
	cur, err := show(current)
	if err != nil {
		return Fact{}, err
	}
	display = append(display, cur)
	if prior != "" {
		pri, err := show(prior)
		if err != nil {
			return Fact{}, err
		}
		display = append(display, pri)
	}
	if fact.Direction == "new" {
		fact.Display = display
		return fact, nil
	}
	chg, err := show(change)
	if err != nil {
		return Fact{}, err
	}
	display = append(display, chg)
	signed := []string{chg}
	if percent {
		pp, err := percentagePoints(change, in.Decimals)
		if err != nil {
			return Fact{}, err
		}
		if !containsString(display, pp) {
			display = append(display, pp)
			signed = append(signed, pp)
		}
	}
	for _, places := range []int{0, 1} {
		rounded, err := roundHalfAway(fact.ChangePct, places)
		if err != nil {
			return Fact{}, err
		}
		display = append(display, rounded+"%")
		signed = append(signed, rounded+"%")
	}
	if isNegative(change) {
		for _, form := range signed {
			if magnitude, ok := strings.CutPrefix(form, "-"); ok && !containsString(display, magnitude) {
				display = append(display, magnitude)
			}
		}
	}
	fact.Display = display
	return fact, nil
}

// percentagePoints renders a percent-field change as "<pp> percentage points".
func percentagePoints(change string, decimals int) (string, error) {
	rounded, err := roundHalfAway(change, decimals)
	if err != nil {
		return "", err
	}
	magnitude, err := assurance.AbsoluteDecimal(rounded)
	if err != nil {
		return "", err
	}
	noun := "percentage points"
	if magnitude == "1" {
		noun = "percentage point"
	}
	return groupThousands(rounded) + " " + noun, nil
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
