package assurance

import (
	"fmt"
	"math/big"
	"strings"
)

// ParseDecimal parses a canonical base-10 decimal into an integer coefficient
// and decimal scale. It never converts through float64.
func ParseDecimal(value string) (*big.Int, int, error) {
	canonical, err := canonicalDecimal(value)
	if err != nil {
		return nil, 0, err
	}
	negative := strings.HasPrefix(canonical, "-")
	unsigned := strings.TrimPrefix(canonical, "-")
	parts := strings.SplitN(unsigned, ".", 2)
	scale := 0
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
		scale = len(fraction)
	}
	coefficient := new(big.Int)
	if _, ok := coefficient.SetString(parts[0]+fraction, 10); !ok {
		return nil, 0, fmt.Errorf("invalid decimal %q", value)
	}
	if negative {
		coefficient.Neg(coefficient)
	}
	return coefficient, scale, nil
}

func alignDecimals(a, b string) (*big.Int, *big.Int, int, error) {
	ac, as, err := ParseDecimal(a)
	if err != nil {
		return nil, nil, 0, err
	}
	bc, bs, err := ParseDecimal(b)
	if err != nil {
		return nil, nil, 0, err
	}
	scale := as
	if bs > scale {
		scale = bs
	}
	if as < scale {
		ac.Mul(ac, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale-as)), nil))
	}
	if bs < scale {
		bc.Mul(bc, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale-bs)), nil))
	}
	return ac, bc, scale, nil
}

func formatDecimal(coefficient *big.Int, scale int) (string, error) {
	if coefficient == nil || scale < 0 || scale > maxPrecision {
		return "", fmt.Errorf("invalid decimal result")
	}
	negative := coefficient.Sign() < 0
	digits := new(big.Int).Abs(coefficient).String()
	if scale == 0 {
		if negative && digits != "0" {
			return "-" + digits, nil
		}
		return digits, nil
	}
	if len(digits) <= scale {
		digits = strings.Repeat("0", scale-len(digits)+1) + digits
	}
	point := len(digits) - scale
	out := digits[:point] + "." + strings.TrimRight(digits[point:], "0")
	out = strings.TrimRight(out, ".")
	if out == "" {
		out = "0"
	}
	if negative && out != "0" {
		out = "-" + out
	}
	return out, nil
}

func AddDecimal(a, b string) (string, error) {
	ac, bc, scale, err := alignDecimals(a, b)
	if err != nil {
		return "", err
	}
	return formatDecimal(new(big.Int).Add(ac, bc), scale)
}

func SubtractDecimal(a, b string) (string, error) {
	ac, bc, scale, err := alignDecimals(a, b)
	if err != nil {
		return "", err
	}
	return formatDecimal(new(big.Int).Sub(ac, bc), scale)
}

func AbsoluteDecimal(value string) (string, error) {
	coefficient, scale, err := ParseDecimal(value)
	if err != nil {
		return "", err
	}
	return formatDecimal(new(big.Int).Abs(coefficient), scale)
}

func CompareDecimal(a, b string) (int, error) {
	ac, bc, _, err := alignDecimals(a, b)
	if err != nil {
		return 0, err
	}
	return ac.Cmp(bc), nil
}

type RoundingMode string

const (
	RoundHalfEven RoundingMode = "half_even"
	RoundHalfUp   RoundingMode = "half_up"
	RoundTruncate RoundingMode = "truncate"
)

func DivideDecimal(numerator, denominator string, rounding RoundingMode) (string, error) {
	n, ns, err := ParseDecimal(numerator)
	if err != nil {
		return "", err
	}
	d, ds, err := ParseDecimal(denominator)
	if err != nil {
		return "", err
	}
	if d.Sign() == 0 {
		return "", fmt.Errorf("decimal division by zero")
	}
	const outputScale = maxPrecision
	scaled := new(big.Int).Mul(n, new(big.Int).Exp(big.NewInt(10), big.NewInt(outputScale+int64(ds)), nil))
	divisor := new(big.Int).Mul(d, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(ns)), nil))
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(scaled, divisor, remainder)
	if remainder.Sign() != 0 && rounding != RoundTruncate {
		absR := new(big.Int).Abs(remainder)
		absD := new(big.Int).Abs(divisor)
		twiceR := new(big.Int).Lsh(absR, 1)
		adjust := twiceR.Cmp(absD) > 0 || (twiceR.Cmp(absD) == 0 && rounding == RoundHalfUp)
		if twiceR.Cmp(absD) == 0 && rounding == RoundHalfEven && quotient.Bit(0) == 1 {
			adjust = true
		}
		if adjust {
			if (scaled.Sign() < 0) != (divisor.Sign() < 0) {
				quotient.Sub(quotient, big.NewInt(1))
			} else {
				quotient.Add(quotient, big.NewInt(1))
			}
		}
	}
	return formatDecimal(quotient, outputScale)
}
