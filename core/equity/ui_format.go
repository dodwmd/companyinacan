package equity

// Presentation of dates, quantities, money and ownership. Everything is formatted from the exact
// integers the Service holds, textually: no floating point anywhere, and nothing is rounded.

import (
	"strconv"
	"strings"
	"time"
)

func fmtDate(s string) string {
	if t, err := time.Parse(dateLayout, s); err == nil {
		return t.Format("2 Jan 2006")
	}
	return s
}

// group puts thousands separators into the whole part of a plain decimal string.
func group(s string) string {
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	whole, frac, dot := strings.Cut(s, ".")
	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if dot {
		return sign + b.String() + "." + frac
	}
	return sign + b.String()
}

func fmtQty(q Qty) string { return group(q.String()) }

func fmtMoney(minor int64, cur string) string { return group(FormatMinor(minor, cur)) + " " + cur }

// fmtPPM renders parts per million as a percentage with four decimals, all of them exact.
func fmtPPM(ppm int64) string {
	frac := strconv.FormatInt(ppm%10000+10000, 10)[1:]
	return strconv.FormatInt(ppm/10000, 10) + "." + frac + "%"
}

// label looks up a value of a family in the message catalogue ("gstatus", "draft"); a value the
// catalogue does not know is shown as it is.
func (u *UI) label(family, value string) string {
	if v, ok := uiMessagesEN[family+"."+value]; ok {
		return v
	}
	return value
}
