package finance

import (
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// Money rules (the rounding policy is part of the contract, docs/finance.md):
//
//   - Every amount is an int64 count of the currency's minor unit (cents, pence, yen).
//     No floats anywhere. The minor-unit exponent comes from the table below (ISO 4217).
//   - Quantities are milli-units (1500 = 1.5), discounts are basis points (1000 = 10%),
//     tax rates are parts per million (200000 = 20%), FX rates are 1e-8 fixed point.
//   - Every division rounds half away from zero. Each line is rounded on its own and
//     the document totals are the sums of the rounded lines, so a printed invoice always
//     adds up to its total.
//   - Prices are tax-exclusive. Tax is computed on the line after discount.
//   - Amounts are capped so sums cannot overflow int64.

const (
	maxMinor    = int64(1) << 50 // about 1.1e15 minor units per line or document
	maxQtyMilli = int64(1) << 40
	fxScale     = int64(100_000_000)
	maxFXE8     = 1_000_000 * fxScale
)

var currencyExp = map[string]int{
	"USD": 2, "EUR": 2, "GBP": 2, "AUD": 2, "CAD": 2, "NZD": 2, "CHF": 2, "SEK": 2, "NOK": 2, "DKK": 2,
	"PLN": 2, "CZK": 2, "HUF": 2, "SGD": 2, "HKD": 2, "CNY": 2, "INR": 2, "MXN": 2, "BRL": 2, "ZAR": 2,
	"AED": 2, "ILS": 2, "TRY": 2, "THB": 2, "PHP": 2,
	"JPY": 0, "KRW": 0, "CLP": 0, "VND": 0, "ISK": 0,
	"BHD": 3, "KWD": 3, "OMR": 3, "JOD": 3, "TND": 3,
}

// CurrencyExponent returns the minor-unit exponent of an upper-case ISO 4217 code.
func CurrencyExponent(code string) (int, bool) {
	e, ok := currencyExp[code]
	return e, ok
}

// FormatMinor renders minor units as a plain decimal string ("1234.56"), the form used in
// CSV exports and HTML. No grouping and no symbol, so it is unambiguous.
func FormatMinor(minor int64, cur string) string {
	exp, _ := CurrencyExponent(cur)
	neg := minor < 0
	u := uint64(minor)
	if neg {
		u = -u
	}
	s := strconv.FormatUint(u, 10)
	if exp > 0 {
		for len(s) <= exp {
			s = "0" + s
		}
		s = s[:len(s)-exp] + "." + s[len(s)-exp:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

// AddMinor adds two amounts, failing on overflow past the per-document cap.
func AddMinor(a, b int64) (int64, error) {
	s := a + b
	if (a > 0 && b > 0 && s < a) || (a < 0 && b < 0 && s > a) || s > maxMinor || s < -maxMinor {
		return 0, errors.New("amount too large")
	}
	return s, nil
}

// mulDivRound computes round_half_away(a*b/d) for d > 0 using big integers.
func mulDivRound(a, b, d int64) (int64, bool) {
	n := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	return roundDiv(n, big.NewInt(d))
}

func roundDiv(n, d *big.Int) (int64, bool) {
	neg := n.Sign() < 0
	n = new(big.Int).Abs(n)
	q, r := new(big.Int).QuoRem(n, d, new(big.Int))
	if r.Lsh(r, 1).Cmp(d) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if neg {
		q.Neg(q)
	}
	if !q.IsInt64() {
		return 0, false
	}
	return q.Int64(), true
}

// LineResult is one line's computed amounts in minor units.
type LineResult struct {
	Gross    int64 // quantity x unit price
	Discount int64
	Taxable  int64 // Gross - Discount
	Tax      int64
	Total    int64 // Taxable + Tax
}

// CalcLine computes a line. qtyMilli > 0, unit >= 0, discountBP 0..10000, ratePPM 0..1e6.
func CalcLine(qtyMilli, unit, discountBP, ratePPM int64) (LineResult, error) {
	var r LineResult
	switch {
	case qtyMilli <= 0 || qtyMilli > maxQtyMilli:
		return r, errors.New("quantity must be greater than zero")
	case unit < 0 || unit > maxMinor:
		return r, errors.New("unit price out of range")
	case discountBP < 0 || discountBP > 10000:
		return r, errors.New("discount must be between 0 and 10000 basis points")
	case ratePPM < 0 || ratePPM > 1_000_000:
		return r, errors.New("tax rate must be between 0 and 1000000 ppm")
	}
	var ok bool
	if r.Gross, ok = mulDivRound(qtyMilli, unit, 1000); !ok || r.Gross > maxMinor {
		return r, errors.New("line amount too large")
	}
	if r.Discount, ok = mulDivRound(r.Gross, discountBP, 10000); !ok {
		return r, errors.New("line amount too large")
	}
	r.Taxable = r.Gross - r.Discount
	if r.Tax, ok = mulDivRound(r.Taxable, ratePPM, 1_000_000); !ok {
		return r, errors.New("line amount too large")
	}
	r.Total = r.Taxable + r.Tax
	return r, nil
}

// ParseFX reads a positive decimal such as "1.0850" (at most 8 decimals) into 1e-8 fixed point.
func ParseFX(s string) (int64, error) {
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || len(whole) > 7 || (hasDot && (frac == "" || len(frac) > 8)) {
		return 0, fmt.Errorf("%q is not a rate with at most 8 decimals", s)
	}
	for _, part := range []string{whole, frac} {
		for _, c := range part {
			if c < '0' || c > '9' {
				return 0, fmt.Errorf("%q is not a decimal number", s)
			}
		}
	}
	w, _ := strconv.ParseInt(whole, 10, 64)
	f := int64(0)
	if frac != "" {
		f, _ = strconv.ParseInt((frac + "00000000")[:8], 10, 64)
	}
	v := w*fxScale + f
	if v <= 0 || v > maxFXE8 {
		return 0, fmt.Errorf("rate %q must be greater than 0 and at most 1000000", s)
	}
	return v, nil
}

// FormatFX renders a fixed-point rate without trailing zeros.
func FormatFX(e8 int64) string {
	s := fmt.Sprintf("%d.%08d", e8/fxScale, e8%fxScale)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// ConvertMinor converts an amount between currencies: rate is the number of target major
// units per source major unit (1e-8 fixed point); the exponents bridge the minor units.
func ConvertMinor(amount int64, fromExp, toExp int, rateE8 int64) int64 {
	n := new(big.Int).Mul(big.NewInt(amount), big.NewInt(rateE8))
	d := big.NewInt(fxScale)
	switch {
	case toExp > fromExp:
		n.Mul(n, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(toExp-fromExp)), nil))
	case toExp < fromExp:
		d.Mul(d, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(fromExp-toExp)), nil))
	}
	v, _ := roundDiv(n, d)
	return v
}
