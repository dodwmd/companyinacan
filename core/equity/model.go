package equity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Errors mapped to HTTP statuses by the API layer.
var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
	ErrConflict  = errors.New("conflict")
)

// InvalidError is a client mistake (HTTP 400).
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &InvalidError{Msg: fmt.Sprintf(format, a...)} }

// ConflictError carries the reason for a 409.
type ConflictError struct{ Msg string }

func (e *ConflictError) Error() string { return e.Msg }
func (e *ConflictError) Unwrap() error { return ErrConflict }

func conflict(format string, a ...any) error { return &ConflictError{Msg: fmt.Sprintf(format, a...)} }

// ErrSeparation is returned when the same person would act twice in a chain that must have
// two. It maps to 403 separation_of_duties.
var ErrSeparation = errors.New("separation of duties: the person who created, edited, submitted or holds a record cannot approve it")

// ---- quantities ---------------------------------------------------------------------------

// Qty is a share or option quantity: an exact decimal held as an int64 count of 1/10000 of a
// share. Whole numbers are the normal case; four decimal places are supported for fractional
// issuance. No floats anywhere. The cap keeps every product used by the calculators in int64
// range (MaxQty * 10000 basis points, MaxQty * maxVestMonths).
type Qty int64

const (
	// QtyScale is the number of Qty units in one share.
	QtyScale Qty = 10000
	// MaxQty is 100 billion shares: far above any real company, small enough that no
	// intermediate product overflows.
	MaxQty Qty = 100_000_000_000 * QtyScale
)

var qtyRe = regexp.MustCompile(`^(0|[1-9][0-9]{0,11})(\.[0-9]{1,4})?$`)

// ParseQty parses a plain decimal ("1000", "1000.25"); no sign, exponent or grouping.
func ParseQty(s string) (Qty, error) {
	if !qtyRe.MatchString(s) {
		return 0, invalid("quantity %q must be a plain decimal with at most 4 decimal places", s)
	}
	whole, frac, _ := strings.Cut(s, ".")
	frac += strings.Repeat("0", 4-len(frac))
	var w, f int64
	for _, c := range whole {
		w = w*10 + int64(c-'0')
	}
	for _, c := range frac {
		f = f*10 + int64(c-'0')
	}
	if w > int64(MaxQty/QtyScale) {
		return 0, invalid("quantity is above the maximum")
	}
	q := Qty(w)*QtyScale + Qty(f)
	if q > MaxQty {
		return 0, invalid("quantity is above the maximum")
	}
	return q, nil
}

// String renders the shortest exact decimal.
func (q Qty) String() string {
	neg := q < 0
	u := uint64(q)
	if neg {
		u = -u
	}
	s := fmt.Sprintf("%d", u/uint64(QtyScale))
	if f := u % uint64(QtyScale); f != 0 {
		s += strings.TrimRight(fmt.Sprintf(".%04d", f), "0")
	}
	if neg {
		return "-" + s
	}
	return s
}

// MarshalJSON writes a decimal string so no client rounds it.
func (q Qty) MarshalJSON() ([]byte, error) { return json.Marshal(q.String()) }

// UnmarshalJSON accepts a decimal string or a JSON number, both parsed textually (never as a
// float).
func (q *Qty) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v, err := ParseQty(s)
		*q = v
		return err
	}
	v, err := ParseQty(string(b))
	*q = v
	return err
}

// ---- money --------------------------------------------------------------------------------

// maxMinor bounds one price in minor units.
const maxMinor = int64(1) << 50

var currencyExp = map[string]int{
	"USD": 2, "EUR": 2, "GBP": 2, "AUD": 2, "CAD": 2, "NZD": 2, "CHF": 2, "SEK": 2, "NOK": 2, "DKK": 2,
	"PLN": 2, "CZK": 2, "HUF": 2, "SGD": 2, "HKD": 2, "CNY": 2, "INR": 2, "MXN": 2, "BRL": 2, "ZAR": 2,
	"AED": 2, "ILS": 2, "TRY": 2, "THB": 2, "PHP": 2,
	"JPY": 0, "KRW": 0, "CLP": 0, "VND": 0, "ISK": 0,
	"BHD": 3, "KWD": 3, "OMR": 3, "JOD": 3, "TND": 3,
}

func validCurrency(c string) error {
	if _, ok := currencyExp[c]; !ok {
		return invalid("currency %q is not a supported ISO 4217 code", c)
	}
	return nil
}

func validMinor(name string, v int64) error {
	if v < 0 || v > maxMinor {
		return invalid("%s must be between 0 and %d minor units", name, maxMinor)
	}
	return nil
}

var minorRe = regexp.MustCompile(`^(0|[1-9][0-9]{0,15})(\.[0-9]+)?$`)

// ParseMinor reads a plain decimal amount ("1.25") as minor units of a currency, textually. More
// decimals than the currency has are refused, never rounded.
func ParseMinor(s, cur string) (int64, error) {
	if err := validCurrency(cur); err != nil {
		return 0, err
	}
	exp := currencyExp[cur]
	whole, frac, _ := strings.Cut(s, ".")
	if !minorRe.MatchString(s) || len(frac) > exp {
		return 0, invalid("amount %q must be a plain decimal with at most %d decimal places (%s)", s, exp, cur)
	}
	var v int64
	for _, c := range whole + frac + strings.Repeat("0", exp-len(frac)) {
		v = v*10 + int64(c-'0')
		if v > maxMinor {
			return 0, invalid("amount %q is too large", s)
		}
	}
	return v, nil
}

// FormatMinor renders minor units as a plain decimal string ("1234.56").
func FormatMinor(minor int64, cur string) string {
	exp := currencyExp[cur]
	neg := minor < 0
	u := uint64(minor)
	if neg {
		u = -u
	}
	s := fmt.Sprintf("%d", u)
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

// exerciseCost is qty x strike in minor units, rounded half away from zero (the rounding
// policy of the module, documented in docs/equity.md). A 4-decimal quantity times an integer
// minor-unit strike is exact in big arithmetic; only the final division rounds.
func exerciseCost(q Qty, strikeMinor int64) (int64, error) {
	n := new(big.Int).Mul(big.NewInt(int64(q)), big.NewInt(strikeMinor))
	d := big.NewInt(int64(QtyScale))
	quo, rem := new(big.Int).QuoRem(n, d, new(big.Int))
	if new(big.Int).Mul(rem, big.NewInt(2)).Cmp(d) >= 0 {
		quo.Add(quo, big.NewInt(1))
	}
	if !quo.IsInt64() || quo.Int64() > maxMinor*1024 {
		return 0, invalid("quantity x strike is too large to represent")
	}
	return quo.Int64(), nil
}

// ---- dates and text -----------------------------------------------------------------------

const dateLayout = "2006-01-02"

// minYear and maxYear bound every date so calendar loops stay bounded.
const (
	minYear = 1970
	maxYear = 2200
)

func parseDate(s string) (time.Time, error) {
	t, err := time.Parse(dateLayout, s)
	if err != nil || t.Year() < minYear || t.Year() > maxYear {
		return time.Time{}, invalid("date %q must be YYYY-MM-DD between %d and %d", s, minYear, maxYear)
	}
	return t, nil
}

func validText(name, v string, max int, required bool) error {
	if required && strings.TrimSpace(v) == "" {
		return invalid("%s is required", name)
	}
	if utf8.RuneCountInString(v) > max || !utf8.ValidString(v) {
		return invalid("%s is too long or not valid UTF-8 (max %d characters)", name, max)
	}
	for _, r := range v {
		if r < 0x20 && r != '\n' && r != '\t' {
			return invalid("%s contains control characters", name)
		}
	}
	return nil
}

func validLine(name, v string, max int, required bool) error {
	if strings.ContainsAny(v, "\n\r\t") {
		return invalid("%s must be a single line", name)
	}
	return validText(name, v, max, required)
}

// canonSubject folds a subject for comparison: Unicode compatibility normalisation, invisible
// format characters removed, trimmed, case-folded. "Alice", "alice " and a zero-width-space
// variant are one person for separation of duties.
func canonSubject(s string) string {
	s = norm.NFKC.String(s)
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	return cases.Fold().String(strings.TrimSpace(s))
}

var (
	idRe       = regexp.MustCompile(`^(ent|cls|stk|pln|grt|exr|led|val|cvt)_[0-9a-f]{24}$`)
	personIDRe = regexp.MustCompile(`^pers_[0-9a-f]{24}$`)
	subjectRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+-]{0,127}$`)
)

// validID returns ErrNotFound for a malformed id: it is indistinguishable from an unknown one.
func validID(prefix, id string) error {
	if !idRe.MatchString(id) || !strings.HasPrefix(id, prefix+"_") {
		return ErrNotFound
	}
	return nil
}
