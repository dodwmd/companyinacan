package work

import (
	"math/rand"
	"sort"
	"strings"
	"testing"
)

func TestRankBetweenBasics(t *testing.T) {
	first := rankBetween("", "")
	if first == "" {
		t.Fatal("empty rank")
	}
	after := rankBetween(first, "")
	before := rankBetween("", first)
	if before >= first || first >= after {
		t.Fatalf("order broken: %q %q %q", before, first, after)
	}
	mid := rankBetween(first, after)
	if first >= mid || mid >= after {
		t.Fatalf("mid %q not between %q and %q", mid, first, after)
	}
}

func TestRankNeverEndsInZeroAndStaysInAlphabet(t *testing.T) {
	r := rankBetween("", "")
	for i := 0; i < 200; i++ {
		r = rankBetween("", r) // keep inserting at the very front
		if strings.HasSuffix(r, "0") || strings.Trim(r, rankAlphabet) != "" {
			t.Fatalf("bad rank %q", r)
		}
	}
}

// Property: inserting at random positions keeps the list sorted and ranks unique.
func TestRankRandomInsertsKeepOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var list []string
	for i := 0; i < 2000; i++ {
		pos := 0
		if len(list) > 0 {
			pos = rng.Intn(len(list) + 1)
		}
		var lo, hi string
		if pos > 0 {
			lo = list[pos-1]
		}
		if pos < len(list) {
			hi = list[pos]
		}
		r := rankBetween(lo, hi)
		if (lo != "" && r <= lo) || (hi != "" && r >= hi) {
			t.Fatalf("rank %q not strictly between %q and %q", r, lo, hi)
		}
		list = append(list[:pos], append([]string{r}, list[pos:]...)...)
	}
	if !sort.StringsAreSorted(list) {
		t.Fatal("list not sorted")
	}
}

func TestRankAppendsGrowSlowly(t *testing.T) {
	r := rankBetween("", "")
	for i := 0; i < 1000; i++ {
		n := rankBetween(r, "")
		if n <= r {
			t.Fatalf("%q is not after %q", n, r)
		}
		r = n
	}
	if len(r) > 60 {
		t.Fatalf("1000 appends produced a %d-character rank", len(r))
	}
}
