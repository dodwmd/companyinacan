package finance

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Form parsing for the web UI. Money never passes through a float: a decimal string is split at
// the point and assembled into an integer count of the currency's minor unit, with the exponent of
// the currency (JPY 0, USD 2, KWD 3). Anything that is not plain digits with an optional single
// point is refused: no exponent, no grouping, no NaN.

var decimalRE = regexp.MustCompile(`^(-?)([0-9]{1,18})(?:\.([0-9]{1,9}))?$`)

// parseScaled reads a plain decimal with at most places fractional digits as an integer scaled by
// 10^places.
func parseScaled(s string, places int) (int64, error) {
	s = strings.TrimSpace(s)
	m := decimalRE.FindStringSubmatch(s)
	if m == nil {
		return 0, errors.New("use plain digits with an optional decimal point, such as 12.50")
	}
	if len(m[3]) > places {
		if places == 0 {
			return 0, errors.New("this value has no decimal places")
		}
		return 0, fmt.Errorf("at most %d decimal places", places)
	}
	frac := m[3] + strings.Repeat("0", places-len(m[3]))
	n, err := strconv.ParseInt(m[2]+frac, 10, 64)
	if err != nil {
		return 0, errors.New("the number is too large")
	}
	if m[1] == "-" {
		n = -n
	}
	return n, nil
}

// ParseMinor reads an amount in major units ("1234.56") as minor units for the currency.
func ParseMinor(s, cur string) (int64, error) {
	exp, ok := CurrencyExponent(cur)
	if !ok {
		return 0, errors.New("unknown currency")
	}
	n, err := parseScaled(s, exp)
	if err != nil {
		return 0, err
	}
	if n > maxMinor || n < -maxMinor {
		return 0, errors.New("the amount is too large")
	}
	return n, nil
}

// ParseMilli reads a quantity ("1.5") as milli-units.
func ParseMilli(s string) (int64, error) {
	n, err := parseScaled(s, 3)
	if err != nil {
		return 0, err
	}
	if n > maxQtyMilli || n < -maxQtyMilli {
		return 0, errors.New("the quantity is too large")
	}
	return n, nil
}

// ParseBP reads a percentage ("10.5") as basis points.
func ParseBP(s string) (int64, error) {
	n, err := parseScaled(s, 2)
	if err != nil {
		return 0, err
	}
	if n < 0 || n > 10000 {
		return 0, errors.New("between 0 and 100 percent")
	}
	return n, nil
}

// FormatMilli renders milli-units as a plain decimal without trailing zeros ("1.5", "2").
func FormatMilli(m int64) string {
	neg := m < 0
	if neg {
		m = -m
	}
	s := fmt.Sprintf("%d.%03d", m/1000, m%1000)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if neg {
		s = "-" + s
	}
	return s
}

// FormatBP renders basis points as a percentage ("10.5").
func FormatBP(b int64) string {
	s := fmt.Sprintf("%d.%02d", b/100, b%100)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}

// FormatPPM renders a tax rate in parts per million as a percentage ("20", "7.25").
func FormatPPM(p int64) string {
	s := fmt.Sprintf("%d.%04d", p/10000, p%10000)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}

// ParseHM reads a duration typed as "7:30" or "7" (hours). A decimal such as "7.5" is refused: it is
// ambiguous between hours and minutes.
func ParseHM(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	h, m, hasMin := strings.Cut(s, ":")
	hv, err := strconv.Atoi(h)
	if err != nil || hv < 0 || hv > 24 || strings.ContainsAny(h, "+- ") {
		return 0, errors.New("use hours and minutes such as 7:30")
	}
	mv := 0
	if hasMin {
		if len(m) != 2 {
			return 0, errors.New("use hours and minutes such as 7:30")
		}
		if mv, err = strconv.Atoi(m); err != nil || mv < 0 || mv > 59 || strings.ContainsAny(m, "+- ") {
			return 0, errors.New("use hours and minutes such as 7:30")
		}
	}
	return hv*60 + mv, nil
}

// FormatHM renders minutes as H:MM.
func FormatHM(minutes int) string { return fmt.Sprintf("%d:%02d", minutes/60, minutes%60) }

// formProblem is a form field the person must fix; it renders as the service's own validation
// error does.
func formProblem(field string, err error) error { return invalid(field, "%s", err.Error()) }

// formIdem returns the idempotency key a rendered form carried. A missing or malformed key is not
// replaced by a server-generated one: that would turn a double submit into two records.
func formIdem(r *http.Request) (string, error) {
	k := r.PostForm.Get("idem")
	if len(k) < 8 || len(k) > 128 || strings.ContainsAny(k, "\x00\r\n") {
		return "", invalid("idem", "the form is out of date; reload the page and try again")
	}
	return k, nil
}

// lineRows reads the repeated line rows of an invoice-like form. A row is the n-th value of each
// of desc, qty, price, disc, tax and sku; a row where every cell is empty is skipped, so the blank
// rows a form offers cost nothing. Prices use the document currency's exponent.
func lineRows(r *http.Request, cur string) ([]LineInput, error) {
	f := r.PostForm
	n := 0
	for _, k := range []string{"desc", "qty", "price", "disc", "tax", "sku"} {
		if len(f[k]) > n {
			n = len(f[k])
		}
	}
	if n > maxLines {
		return nil, invalid("lines", "at most %d lines", maxLines)
	}
	get := func(k string, i int) string {
		if i < len(f[k]) {
			return strings.TrimSpace(f[k][i])
		}
		return ""
	}
	var out []LineInput
	var p problems
	for i := 0; i < n; i++ {
		d, q, pr, di, tx, sk := get("desc", i), get("qty", i), get("price", i), get("disc", i), get("tax", i), get("sku", i)
		if d == "" && q == "" && pr == "" && di == "" && tx == "" && sk == "" {
			continue
		}
		fld := fmt.Sprintf("line %d", i+1)
		li := LineInput{Description: d, SKU: sk}
		if q == "" {
			q = "1"
		}
		qm, err := ParseMilli(q)
		if err != nil {
			p.add(fld+" quantity", "%s", err.Error())
			continue
		}
		li.QuantityMilli = qm
		if pr != "" {
			u, err := ParseMinor(pr, cur)
			if err != nil {
				p.add(fld+" price", "%s", err.Error())
				continue
			}
			li.UnitPriceMinor = &u
		}
		if di != "" {
			b, err := ParseBP(di)
			if err != nil {
				p.add(fld+" discount", "%s", err.Error())
				continue
			}
			li.DiscountBP = b
		}
		if tx != "" {
			t := tx
			li.TaxCode = &t
		}
		out = append(out, li)
	}
	if err := p.err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, invalid("lines", "add at least one line")
	}
	return out, nil
}

// ifMatchForm reads the record version a form was rendered from. It is optional only where the
// service allows no precondition; edits always carry it so a stale form is refused.
func ifMatchForm(r *http.Request) (*int64, error) {
	v := strings.TrimSpace(r.PostForm.Get("version"))
	if v == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 {
		return nil, invalid("version", "the form is out of date; reload the page and try again")
	}
	return &n, nil
}
