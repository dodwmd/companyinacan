package equity

import (
	"math/big"
	"regexp"
	"sort"
	"time"
)

// The vesting calculator is a pure function: the same input always gives the same answer, it
// reads no clock and touches no database. It models the common shapes (a cliff, then monthly or
// quarterly instalments; dated or milestone tranches; single- and double-trigger acceleration
// as data). It does not model any particular plan's legal text, any tax rule, or any
// jurisdiction's treatment: the operator and counsel decide what the plan says, this computes
// what the recorded data implies. See docs/equity.md.

// Maximum sizes keep every loop and every product bounded.
const (
	maxVestMonths  = 600
	maxTranches    = 50
	maxAccelClause = 10
)

// Schedule is the vesting terms of one grant. It is frozen when the grant is submitted for
// approval.
type Schedule struct {
	Kind string `json:"kind"` // periodic | tranches
	// periodic: Months in total, nothing before CliffMonths, then every Interval.
	Months      int    `json:"months,omitempty"`
	CliffMonths int    `json:"cliff_months,omitempty"`
	Interval    string `json:"interval,omitempty"` // monthly | quarterly
	// tranches: dated or milestone tranches whose basis points sum to 10000.
	Tranches []Tranche `json:"tranches,omitempty"`
	// Rounding says how a fractional share is treated: floor (whole shares, rounded down,
	// the last instalment takes the remainder), nearest (whole shares, half up) or
	// fractional (four decimal places, rounded down). The cumulative amount is rounded, never
	// each instalment, so the total is always exact.
	Rounding     string         `json:"rounding,omitempty"`
	Acceleration []Acceleration `json:"acceleration,omitempty"`
}

// Tranche is one slice of a tranche schedule: vests on Date, or when its milestone is
// recorded as achieved.
type Tranche struct {
	Label       string `json:"label"`
	BasisPoints int    `json:"basis_points"`
	Date        string `json:"date,omitempty"`
	Milestone   bool   `json:"milestone,omitempty"`
}

// Acceleration is an acceleration clause held as data. Type single needs one trigger event,
// double needs two (any order). When the last trigger has happened, PercentBP of what was
// still unvested vests at that date. The service records trigger events; it does not decide
// whether a transaction is a change of control.
type Acceleration struct {
	Name                    string   `json:"name"`
	Type                    string   `json:"type"` // single | double
	Triggers                []string `json:"triggers"`
	PercentBP               int      `json:"percent_bp"`
	AppliesAfterTermination bool     `json:"applies_after_termination,omitempty"`
}

// AccelEvent is a recorded trigger event.
type AccelEvent struct {
	Trigger string `json:"trigger"`
	Date    string `json:"date"`
}

var triggerRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// Validate checks the schedule's shape.
func (s Schedule) Validate() error {
	switch s.Rounding {
	case "", "floor", "nearest", "fractional":
	default:
		return invalid("rounding must be floor, nearest or fractional")
	}
	switch s.Kind {
	case "periodic":
		step := 1
		switch s.Interval {
		case "monthly":
		case "quarterly":
			step = 3
		default:
			return invalid("interval must be monthly or quarterly")
		}
		if s.Months < 1 || s.Months > maxVestMonths {
			return invalid("months must be between 1 and %d", maxVestMonths)
		}
		if s.CliffMonths < 0 || s.CliffMonths > s.Months {
			return invalid("cliff_months must be between 0 and months")
		}
		if s.Months%step != 0 || s.CliffMonths%step != 0 {
			return invalid("months and cliff_months must be multiples of %d for a %s schedule", step, s.Interval)
		}
		if len(s.Tranches) != 0 {
			return invalid("a periodic schedule has no tranches")
		}
	case "tranches":
		if len(s.Tranches) < 1 || len(s.Tranches) > maxTranches {
			return invalid("tranches must have between 1 and %d entries", maxTranches)
		}
		sum := 0
		for i, t := range s.Tranches {
			if err := validLine("tranche label", t.Label, 100, true); err != nil {
				return err
			}
			if t.BasisPoints < 1 || t.BasisPoints > 10000 {
				return invalid("tranche %d: basis_points must be between 1 and 10000", i)
			}
			sum += t.BasisPoints
			if t.Milestone == (t.Date != "") {
				return invalid("tranche %d: give either a date or milestone=true", i)
			}
			if t.Date != "" {
				if _, err := parseDate(t.Date); err != nil {
					return err
				}
			}
		}
		if sum != 10000 {
			return invalid("tranche basis points must add up to 10000 (got %d)", sum)
		}
		if s.Months != 0 || s.CliffMonths != 0 || s.Interval != "" {
			return invalid("a tranche schedule has no months, cliff or interval")
		}
	default:
		return invalid("kind must be periodic or tranches")
	}
	if len(s.Acceleration) > maxAccelClause {
		return invalid("at most %d acceleration clauses", maxAccelClause)
	}
	names := map[string]bool{}
	for _, a := range s.Acceleration {
		if err := validLine("acceleration name", a.Name, 64, true); err != nil {
			return err
		}
		if names[a.Name] {
			return invalid("duplicate acceleration clause %q", a.Name)
		}
		names[a.Name] = true
		want := map[string]int{"single": 1, "double": 2}[a.Type]
		if want == 0 || len(a.Triggers) != want {
			return invalid("acceleration %q: type single needs 1 trigger and double needs 2", a.Name)
		}
		seen := map[string]bool{}
		for _, tr := range a.Triggers {
			if !triggerRe.MatchString(tr) || seen[tr] {
				return invalid("acceleration %q: triggers must be distinct names like change_of_control", a.Name)
			}
			seen[tr] = true
		}
		if a.PercentBP < 1 || a.PercentBP > 10000 {
			return invalid("acceleration %q: percent_bp must be between 1 and 10000", a.Name)
		}
	}
	return nil
}

// VestInput is everything the calculator needs.
type VestInput struct {
	Total       Qty
	Start       string // vesting commencement date
	Sched       Schedule
	AsOf        string
	Termination string         // optional: vesting stops on this date (inclusive)
	Achieved    map[int]string // tranche index -> date the milestone was achieved
	Events      []AccelEvent
}

// addMonthsClamped adds n months, clamping the day to the last day of the target month, so a
// grant that starts on the 31st vests on the last day of shorter months and a leap-day start
// vests on Feb 28 in common years.
func addMonthsClamped(t time.Time, n int) time.Time {
	y, m, d := t.Date()
	total := int(m) - 1 + n
	y += total / 12
	mo := total % 12
	if mo < 0 {
		mo += 12
		y--
	}
	last := time.Date(y, time.Month(mo+1)+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if d > last {
		d = last
	}
	return time.Date(y, time.Month(mo+1), d, 0, 0, 0, 0, time.UTC)
}

// monthsElapsed is the number of whole months m (>= 0) such that start+m months <= asof.
func monthsElapsed(start, asof time.Time) int {
	if asof.Before(start) {
		return 0
	}
	m := (asof.Year()-start.Year())*12 + int(asof.Month()-start.Month())
	for m > 0 && addMonthsClamped(start, m).After(asof) {
		m--
	}
	return m
}

// share returns total*num/den under the rounding policy, never above total.
func share(total Qty, num, den int64, rounding string) Qty {
	if num >= den {
		return total
	}
	if num <= 0 {
		return 0
	}
	unit := int64(QtyScale)
	if rounding == "fractional" {
		unit = 1
	}
	n := new(big.Int).Mul(big.NewInt(int64(total)), big.NewInt(num))
	d := new(big.Int).Mul(big.NewInt(den), big.NewInt(unit))
	if rounding == "nearest" {
		n.Mul(n, big.NewInt(2))
		n.Add(n, d)
		d.Mul(d, big.NewInt(2))
	}
	n.Quo(n, d)
	n.Mul(n, big.NewInt(unit))
	v := Qty(n.Int64())
	if v > total {
		v = total
	}
	return v
}

func (in VestInput) baseAt(start, cut time.Time) Qty {
	s := in.Sched
	switch s.Kind {
	case "periodic":
		m := monthsElapsed(start, cut)
		if m > s.Months {
			m = s.Months
		}
		if m < s.CliffMonths {
			return 0
		}
		if s.Interval == "quarterly" {
			m -= m % 3
		}
		return share(in.Total, int64(m), int64(s.Months), s.Rounding)
	case "tranches":
		bp := 0
		for i, t := range s.Tranches {
			ds := t.Date
			if t.Milestone {
				ds = in.Achieved[i]
			}
			if ds == "" {
				continue
			}
			if d, err := time.Parse(dateLayout, ds); err == nil && !d.After(cut) {
				bp += t.BasisPoints
			}
		}
		return share(in.Total, int64(bp), 10000, s.Rounding)
	}
	return 0
}

// accelDate is the date the clause became effective: the latest of its triggers' (earliest)
// event dates. ok is false while any trigger is missing.
func accelDate(a Acceleration, events []AccelEvent) (time.Time, bool) {
	var eff time.Time
	for _, tr := range a.Triggers {
		var first time.Time
		found := false
		for _, e := range events {
			if e.Trigger != tr {
				continue
			}
			d, err := time.Parse(dateLayout, e.Date)
			if err != nil {
				continue
			}
			if !found || d.Before(first) {
				first, found = d, true
			}
		}
		if !found {
			return time.Time{}, false
		}
		if first.After(eff) {
			eff = first
		}
	}
	return eff, true
}

// Vested returns how many units of Total have vested as of AsOf. The result is never negative,
// never above Total, and never decreases as AsOf moves forward.
func Vested(in VestInput) (Qty, error) {
	if in.Total < 0 || in.Total > MaxQty {
		return 0, invalid("quantity out of range")
	}
	if err := in.Sched.Validate(); err != nil {
		return 0, err
	}
	start, err := parseDate(in.Start)
	if err != nil {
		return 0, err
	}
	asof, err := parseDate(in.AsOf)
	if err != nil {
		return 0, err
	}
	cut := asof
	var term time.Time
	if in.Termination != "" {
		if term, err = parseDate(in.Termination); err != nil {
			return 0, err
		}
		if term.Before(cut) {
			cut = term
		}
	}
	best := in.baseAt(start, cut)
	for _, a := range in.Sched.Acceleration {
		d, ok := accelDate(a, in.Events)
		if !ok || d.After(asof) {
			continue
		}
		at := d
		if !term.IsZero() && term.Before(d) {
			if !a.AppliesAfterTermination {
				continue
			}
			at = term
		}
		b := in.baseAt(start, at)
		extra := share(in.Total-b, int64(a.PercentBP), 10000, in.Sched.Rounding)
		if v := b + extra; v > best {
			best = v
		}
	}
	return best, nil
}

// VestPoint is one step of a vesting timeline.
type VestPoint struct {
	Date       string `json:"date"`
	Cumulative Qty    `json:"cumulative"`
	Reason     string `json:"reason"` // cliff | instalment | tranche | milestone | acceleration
}

// Timeline lists every date on which vesting increases, up to full vesting (or the termination
// date). Dates are computed by the same code path as Vested, so the two always agree.
func Timeline(in VestInput) ([]VestPoint, error) {
	if err := in.Sched.Validate(); err != nil {
		return nil, err
	}
	start, err := parseDate(in.Start)
	if err != nil {
		return nil, err
	}
	type cand struct {
		d      time.Time
		reason string
	}
	var cs []cand
	s := in.Sched
	switch s.Kind {
	case "periodic":
		step := 1
		if s.Interval == "quarterly" {
			step = 3
		}
		first := s.CliffMonths
		if first == 0 {
			first = step
		}
		for m := first; m <= s.Months; m += step {
			r := "instalment"
			if m == s.CliffMonths && s.CliffMonths > 0 {
				r = "cliff"
			}
			cs = append(cs, cand{addMonthsClamped(start, m), r})
		}
	case "tranches":
		for i, t := range s.Tranches {
			ds, r := t.Date, "tranche"
			if t.Milestone {
				ds, r = in.Achieved[i], "milestone"
			}
			if d, err := time.Parse(dateLayout, ds); err == nil && ds != "" {
				cs = append(cs, cand{d, r})
			}
		}
	}
	for _, a := range s.Acceleration {
		if d, ok := accelDate(a, in.Events); ok {
			cs = append(cs, cand{d, "acceleration"})
		}
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].d.Before(cs[j].d) })
	var out []VestPoint
	var prev Qty
	for _, c := range cs {
		x := in
		x.AsOf = c.d.Format(dateLayout)
		v, err := Vested(x)
		if err != nil {
			return nil, err
		}
		if v > prev {
			out = append(out, VestPoint{Date: x.AsOf, Cumulative: v, Reason: c.reason})
			prev = v
		}
	}
	return out, nil
}
