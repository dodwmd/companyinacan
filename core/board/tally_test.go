package board

import (
	"math/rand"
	"testing"
)

func TestQuorumRequiredExactIntegers(t *testing.T) {
	cases := []struct {
		eligible, num, den, min, want int
	}{
		{5, 1, 2, 0, 3},  // half of 5 rounded up
		{4, 1, 2, 0, 2},  // exactly half of 4 meets a "half" rule
		{4, 1, 2, 3, 3},  // an absolute minimum wins when larger
		{9, 2, 3, 0, 6},  // two thirds of 9 is exactly 6
		{10, 2, 3, 0, 7}, // 6.67 rounds up to 7
		{1, 1, 2, 0, 1},
		{0, 1, 2, 0, 1}, // never zero: an empty body cannot reach quorum by default
		{7, 1, 1, 0, 7},
		{3, 0, 1, 2, 2},
	}
	for _, c := range cases {
		if got := QuorumRequired(c.eligible, c.num, c.den, c.min); got != c.want {
			t.Errorf("QuorumRequired(%d, %d/%d, min %d) = %d, want %d", c.eligible, c.num, c.den, c.min, got, c.want)
		}
	}
}

func TestTally(t *testing.T) {
	cases := []struct {
		f, a, ab int
		num, den int
		incl     bool
		want     bool
	}{
		{3, 2, 0, 1, 2, false, true},  // simple majority
		{2, 2, 0, 1, 2, false, false}, // a tie fails
		{1, 0, 4, 1, 2, false, true},  // abstentions are not votes cast
		{0, 0, 5, 1, 2, false, false}, // nothing for: never passes
		{2, 1, 0, 2, 3, true, true},   // exactly two thirds, inclusive
		{2, 1, 0, 2, 3, false, false}, // exactly two thirds, strict
		{4, 0, 0, 1, 1, true, true},   // unanimity
		{4, 1, 0, 1, 1, true, false},
		{6, 3, 0, 2, 3, true, true}, // 6 of 9 cast is exactly two thirds
	}
	for _, c := range cases {
		if got := TallyPasses(c.f, c.a, c.ab, c.num, c.den, c.incl); got != c.want {
			t.Errorf("TallyPasses(for %d, against %d, abstain %d, %d/%d incl=%v) = %v, want %v", c.f, c.a, c.ab, c.num, c.den, c.incl, got, c.want)
		}
	}
}

// Property: the integer rule agrees with exact arithmetic for large counts, and an abstention never
// changes the outcome.
func TestTallyProperties(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 20000; i++ {
		f, a, ab := r.Intn(1_000_000), r.Intn(1_000_000), r.Intn(1_000_000)
		den := 1 + r.Intn(9)
		num := r.Intn(den + 1)
		incl := r.Intn(2) == 0
		got := TallyPasses(f, a, ab, num, den, incl)
		if got != TallyPasses(f, a, ab+1, num, den, incl) {
			t.Fatalf("an abstention changed the result: %d %d %d %d/%d", f, a, ab, num, den)
		}
		lhs, rhs := int64(f)*int64(den), int64(num)*(int64(f)+int64(a))
		want := f > 0 && ((incl && lhs >= rhs) || (!incl && lhs > rhs))
		if got != want {
			t.Fatalf("TallyPasses(%d,%d,%d,%d/%d,%v) = %v, want %v", f, a, ab, num, den, incl, got, want)
		}
	}
}

func FuzzQuorumRequired(f *testing.F) {
	f.Add(5, 1, 2, 0)
	f.Fuzz(func(t *testing.T, eligible, num, den, min int) {
		if eligible < 0 || eligible > 1_000_000 || num < 0 || num > den || den < 1 || den > 100 || min < 0 || min > 1_000_000 {
			return
		}
		got := QuorumRequired(eligible, num, den, min)
		if got < 1 || got < min || got*den < eligible*num {
			t.Fatalf("QuorumRequired(%d,%d/%d,%d) = %d violates the rule", eligible, num, den, min, got)
		}
		if got > 1 && got > min && (got-1)*den >= eligible*num {
			t.Fatalf("QuorumRequired(%d,%d/%d,%d) = %d is not the smallest count", eligible, num, den, min, got)
		}
	})
}
