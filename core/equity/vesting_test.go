package equity

import (
	"math/rand"
	"testing"
	"time"
)

func q(n int64) Qty { return Qty(n) * QtyScale }

func periodic(months, cliff int, interval string) Schedule {
	return Schedule{Kind: "periodic", Months: months, CliffMonths: cliff, Interval: interval}
}

func vest(t *testing.T, in VestInput) Qty {
	t.Helper()
	v, err := Vested(in)
	if err != nil {
		t.Fatalf("Vested(%+v): %v", in, err)
	}
	return v
}

func TestStandardFourYearOneYearCliff(t *testing.T) {
	in := VestInput{Total: q(4800), Start: "2024-01-15", Sched: periodic(48, 12, "monthly")}
	cases := []struct {
		asof string
		want Qty
	}{
		{"2023-12-31", 0},       // before the start
		{"2024-01-15", 0},       // the start day
		{"2025-01-14", 0},       // the day before the cliff: nothing yet
		{"2025-01-15", q(1200)}, // the cliff date: 12/48
		{"2025-02-14", q(1200)}, // a month needs the full month
		{"2025-02-15", q(1300)}, // 13/48
		{"2027-12-31", q(4700)}, // 47 months
		{"2028-01-15", q(4800)}, // fully vested
		{"2090-01-01", q(4800)}, // and stays so
	}
	for _, c := range cases {
		in.AsOf = c.asof
		if got := vest(t, in); got != c.want {
			t.Errorf("as of %s: vested %s, want %s", c.asof, got, c.want)
		}
	}
}

func TestLeapYearAndMonthEnd(t *testing.T) {
	// Leap day start: the anniversary clamps to Feb 28 in a common year.
	in := VestInput{Total: q(4800), Start: "2024-02-29", Sched: periodic(48, 0, "monthly")}
	for asof, want := range map[string]Qty{
		"2025-02-27": q(1100), // 11 months
		"2025-02-28": q(1200), // clamped anniversary counts
		"2028-02-28": q(4700), // 47 months: the 48th is Feb 29 2028
		"2028-02-29": q(4800),
	} {
		in.AsOf = asof
		if got := vest(t, in); got != want {
			t.Errorf("leap start, as of %s: %s, want %s", asof, got, want)
		}
	}
	// Month-end start (31st): shorter months vest on their last day.
	in = VestInput{Total: q(1200), Start: "2023-01-31", Sched: periodic(12, 0, "monthly")}
	for asof, want := range map[string]Qty{
		"2023-02-27": 0,
		"2023-02-28": q(100),
		"2023-03-30": q(100),
		"2023-03-31": q(200),
		"2023-04-30": q(300),
		"2024-01-31": q(1200),
	} {
		in.AsOf = asof
		if got := vest(t, in); got != want {
			t.Errorf("month-end start, as of %s: %s, want %s", asof, got, want)
		}
	}
}

func TestQuarterlyVesting(t *testing.T) {
	in := VestInput{Total: q(1600), Start: "2024-01-01", Sched: periodic(48, 12, "quarterly")}
	for asof, want := range map[string]Qty{
		"2024-12-31": 0,
		"2025-01-01": q(400), // cliff 12 months = 4 quarters
		"2025-03-31": q(400),
		"2025-04-01": q(500),
		"2028-01-01": q(1600),
	} {
		in.AsOf = asof
		if got := vest(t, in); got != want {
			t.Errorf("quarterly as of %s: %s, want %s", asof, got, want)
		}
	}
}

func TestTerminationCutsOffVesting(t *testing.T) {
	in := VestInput{Total: q(4800), Start: "2024-01-15", Sched: periodic(48, 12, "monthly"), Termination: "2025-06-20"}
	in.AsOf = "2030-01-01"
	// Vesting stops at the termination date: 17 whole months (Jun 15 is the 17th).
	if got := vest(t, in); got != q(1700) {
		t.Errorf("after termination: %s, want 1700", got)
	}
	in.AsOf = "2025-06-14"
	if got := vest(t, in); got != q(1600) {
		t.Errorf("before the 17th month: %s, want 1600", got)
	}
	// Terminating before the cliff vests nothing, ever.
	in.Termination = "2024-12-31"
	in.AsOf = "2030-01-01"
	if got := vest(t, in); got != 0 {
		t.Errorf("terminated before cliff: %s, want 0", got)
	}
	// A vesting date that falls on the termination date counts.
	in.Termination = "2025-01-15"
	if got := vest(t, in); got != q(1200) {
		t.Errorf("terminated on the cliff date: %s, want 1200", got)
	}
}

func TestRoundingPolicies(t *testing.T) {
	// 1000 shares over 48 months: 20.8333 per month.
	for _, c := range []struct {
		rounding string
		months   string
		want     Qty
	}{
		{"floor", "2024-02-01", q(20)},
		{"nearest", "2024-02-01", q(21)},
		{"fractional", "2024-02-01", 208333},
		{"floor", "2024-03-01", q(41)},
		{"nearest", "2024-03-01", q(42)},
		{"floor", "2028-01-01", q(1000)}, // the last tranche takes the remainder
		{"nearest", "2028-01-01", q(1000)},
		{"fractional", "2028-01-01", q(1000)},
	} {
		s := periodic(48, 0, "monthly")
		s.Rounding = c.rounding
		got := vest(t, VestInput{Total: q(1000), Start: "2024-01-01", Sched: s, AsOf: c.months})
		if got != c.want {
			t.Errorf("%s as of %s: %s, want %s", c.rounding, c.months, got, c.want)
		}
	}
	// A fractional total under the floor policy still ends at exactly the total.
	s := periodic(3, 0, "monthly")
	total := Qty(10005) // 1.0005
	if got := vest(t, VestInput{Total: total, Start: "2024-01-01", Sched: s, AsOf: "2024-04-01"}); got != total {
		t.Errorf("end of schedule %s, want %s", got, total)
	}
}

func TestMilestoneTranches(t *testing.T) {
	s := Schedule{Kind: "tranches", Tranches: []Tranche{
		{Label: "beta", BasisPoints: 2500, Milestone: true},
		{Label: "launch", BasisPoints: 2500, Milestone: true},
		{Label: "y1", BasisPoints: 5000, Date: "2025-06-01"},
	}}
	in := VestInput{Total: q(1000), Start: "2024-01-01", Sched: s, AsOf: "2024-12-31"}
	if got := vest(t, in); got != 0 {
		t.Errorf("nothing achieved: %s", got)
	}
	in.Achieved = map[int]string{1: "2024-09-01"}
	if got := vest(t, in); got != q(250) {
		t.Errorf("one milestone: %s", got)
	}
	in.AsOf = "2024-08-31"
	if got := vest(t, in); got != 0 {
		t.Errorf("milestone achieved after the as-of date counts: %s", got)
	}
	in.AsOf = "2025-06-01"
	in.Achieved = map[int]string{0: "2024-03-01", 1: "2024-09-01"}
	if got := vest(t, in); got != q(1000) {
		t.Errorf("everything: %s", got)
	}
	if err := (Schedule{Kind: "tranches", Tranches: []Tranche{{Label: "a", BasisPoints: 9000, Date: "2025-01-01"}}}).Validate(); err == nil {
		t.Error("tranches that do not sum to 100% were accepted")
	}
}

func TestAcceleration(t *testing.T) {
	single := Acceleration{Name: "coc", Type: "single", Triggers: []string{"change_of_control"}, PercentBP: 10000}
	double := Acceleration{Name: "dt", Type: "double", Triggers: []string{"change_of_control", "termination_without_cause"}, PercentBP: 5000}
	in := VestInput{Total: q(4800), Start: "2024-01-01", Sched: periodic(48, 12, "monthly"), AsOf: "2026-01-01"}
	base := vest(t, in) // 24 months
	if base != q(2400) {
		t.Fatalf("base = %s", base)
	}
	in.Sched.Acceleration = []Acceleration{single}
	if got := vest(t, in); got != base {
		t.Errorf("clause without a trigger event changed vesting: %s", got)
	}
	in.Events = []AccelEvent{{Trigger: "change_of_control", Date: "2026-06-01"}}
	if got := vest(t, in); got != base {
		t.Errorf("trigger in the future changed vesting: %s", got)
	}
	in.AsOf = "2026-06-01"
	if got := vest(t, in); got != q(4800) {
		t.Errorf("single trigger fully accelerates: %s", got)
	}
	in.Sched.Acceleration = []Acceleration{double}
	if got := vest(t, in); got != q(2900) { // 29 months at 2026-06-01; one trigger of two is not enough
		t.Errorf("double trigger with one event: %s", got)
	}
	in.Events = append(in.Events, AccelEvent{Trigger: "termination_without_cause", Date: "2026-09-10"})
	in.AsOf = "2026-09-09"
	if got := vest(t, in); got != q(3200) { // 32 months; second trigger not yet
		t.Errorf("double trigger before the second event: %s", got)
	}
	in.AsOf = "2026-09-10"
	// base at the effective date (the later trigger): 32 months = 3200; unvested 1600; 50% = 800.
	if got := vest(t, in); got != q(4000) {
		t.Errorf("double trigger complete: %s, want 4000", got)
	}
	// Acceleration after the grantee left does not apply unless the clause says it survives.
	in.Termination = "2026-08-01"
	in.AsOf = "2027-01-01"
	if got := vest(t, in); got != q(3100) { // vesting stopped at 31 months
		t.Errorf("acceleration after termination without the survival flag: %s", got)
	}
	in.Sched.Acceleration[0].AppliesAfterTermination = true
	if got := vest(t, in); got != q(3100)+(q(4800)-q(3100))/2 {
		t.Errorf("surviving acceleration: %s", got)
	}
}

func TestScheduleValidation(t *testing.T) {
	bad := []Schedule{
		{Kind: "nope"},
		periodic(0, 0, "monthly"),
		periodic(601, 0, "monthly"),
		periodic(48, 49, "monthly"),
		periodic(48, -1, "monthly"),
		periodic(10, 0, "quarterly"), // not a multiple of 3
		periodic(48, 5, "quarterly"), // cliff not a multiple of the step
		periodic(48, 0, "weekly"),
		{Kind: "periodic", Months: 12, Interval: "monthly", Rounding: "banker"},
		{Kind: "tranches"},
		{Kind: "periodic", Months: 12, Interval: "monthly", Acceleration: []Acceleration{{Name: "x", Type: "double", Triggers: []string{"a"}, PercentBP: 100}}},
		{Kind: "periodic", Months: 12, Interval: "monthly", Acceleration: []Acceleration{{Name: "x", Type: "single", Triggers: []string{"a"}, PercentBP: 10001}}},
	}
	for i, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("schedule %d accepted: %+v", i, s)
		}
	}
	if err := periodic(48, 12, "monthly").Validate(); err != nil {
		t.Errorf("standard schedule rejected: %v", err)
	}
	if _, err := Vested(VestInput{Total: q(1), Start: "2024-02-30", Sched: periodic(12, 0, "monthly"), AsOf: "2024-03-01"}); err == nil {
		t.Error("an impossible date was accepted")
	}
}

// ---- properties ---------------------------------------------------------------------------

func randomSchedule(r *rand.Rand) (VestInput, bool) {
	interval := []string{"monthly", "quarterly"}[r.Intn(2)]
	step := 1
	if interval == "quarterly" {
		step = 3
	}
	months := step * (1 + r.Intn(40))
	cliff := step * r.Intn(months/step+1)
	s := periodic(months, cliff, interval)
	s.Rounding = []string{"", "floor", "nearest", "fractional"}[r.Intn(4)]
	start := time.Date(2000+r.Intn(40), time.Month(1+r.Intn(12)), 1+r.Intn(31), 0, 0, 0, 0, time.UTC)
	if start.Day() < 1 || start.Month() == 0 {
		return VestInput{}, false
	}
	total := Qty(1 + r.Int63n(int64(MaxQty)))
	if r.Intn(2) == 0 {
		total = Qty(1+r.Intn(100000)) * QtyScale
	}
	return VestInput{Total: total, Start: start.Format(dateLayout), Sched: s}, true
}

func TestVestingProperties(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 3000; i++ {
		in, ok := randomSchedule(r)
		if !ok {
			continue
		}
		if err := in.Sched.Validate(); err != nil {
			t.Fatalf("generated schedule invalid: %v", err)
		}
		start, _ := time.Parse(dateLayout, in.Start)
		var prev Qty
		for d := -40; d < in.Sched.Months*31+80; d += 1 + r.Intn(40) {
			in.AsOf = start.AddDate(0, 0, d).Format(dateLayout)
			v := vest(t, in)
			if v < 0 || v > in.Total {
				t.Fatalf("vested %d outside [0,%d] for %+v", v, in.Total, in)
			}
			if v < prev {
				t.Fatalf("vesting went backwards (%d -> %d) at %s for %+v", prev, v, in.AsOf, in)
			}
			prev = v
			if d < 0 && v != 0 {
				t.Fatalf("vested before the start: %+v", in)
			}
		}
		in.AsOf = start.AddDate(0, in.Sched.Months+1, 0).Format(dateLayout)
		if v := vest(t, in); v != in.Total {
			t.Fatalf("not fully vested after the term: %d of %d for %+v", v, in.Total, in)
		}
		// Terminating can only reduce what vests.
		in.AsOf = start.AddDate(0, in.Sched.Months+1, 0).Format(dateLayout)
		in.Termination = start.AddDate(0, 0, r.Intn(in.Sched.Months*30)).Format(dateLayout)
		if v := vest(t, in); v > in.Total {
			t.Fatalf("terminated vesting exceeds total")
		}
	}
}

func FuzzVested(f *testing.F) {
	f.Add(int64(4800)*10000, "2024-01-31", "2025-02-28", 48, 12, 0, "")
	f.Add(int64(1), "2024-02-29", "2028-02-29", 12, 0, 1, "2024-06-01")
	f.Fuzz(func(t *testing.T, total int64, start, asof string, months, cliff, mode int, term string) {
		in := VestInput{Total: Qty(total), Start: start, AsOf: asof, Termination: term, Sched: Schedule{
			Kind: "periodic", Months: months, CliffMonths: cliff,
			Interval: []string{"monthly", "quarterly"}[(mode&1+2)%2], Rounding: []string{"floor", "nearest", "fractional"}[(mode>>1&3)%3],
		}}
		if total < 0 || total > int64(MaxQty) || in.Sched.Validate() != nil {
			return
		}
		v, err := Vested(in)
		if err != nil {
			return
		}
		if v < 0 || v > in.Total {
			t.Fatalf("vested %d outside [0,%d] for %+v", v, in.Total, in)
		}
	})
}
