package obs

import (
	"bytes"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func expose(t *testing.T, r *Registry) string {
	t.Helper()
	var b bytes.Buffer
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestExpositionGolden(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("can_requests_total", "Requests.", "method", "class")
	c.With("GET", "2xx").Add(3)
	c.With("POST", "5xx").Inc()
	g := r.Gauge("can_inflight", "In flight.")
	g.With().Set(2)
	g.With().Add(-0.5)
	h := r.Histogram("can_latency_seconds", "Latency.", []float64{0.1, 1}, "route")
	h.With("/x").Observe(0.05)
	h.With("/x").Observe(0.5)
	h.With("/x").Observe(7)
	r.GaugeFunc("can_up_fn", "Fn.", func() float64 { return 1.5 })

	want := `# HELP can_inflight In flight.
# TYPE can_inflight gauge
can_inflight 1.5
# HELP can_latency_seconds Latency.
# TYPE can_latency_seconds histogram
can_latency_seconds_bucket{route="/x",le="0.1"} 1
can_latency_seconds_bucket{route="/x",le="1"} 2
can_latency_seconds_bucket{route="/x",le="+Inf"} 3
can_latency_seconds_sum{route="/x"} 7.55
can_latency_seconds_count{route="/x"} 3
# HELP can_requests_total Requests.
# TYPE can_requests_total counter
can_requests_total{method="GET",class="2xx"} 3
can_requests_total{method="POST",class="5xx"} 1
# HELP can_up_fn Fn.
# TYPE can_up_fn gauge
can_up_fn 1.5
`
	if got := expose(t, r); got != want {
		t.Fatalf("exposition mismatch\n--- got\n%s--- want\n%s", got, want)
	}
}

func TestLabelEscaping(t *testing.T) {
	r := NewRegistry()
	r.Counter("can_e_total", "Help with \\ and\nnewline.", "v").With("a\"b\\c\nd\xff").Inc()
	got := expose(t, r)
	if !strings.Contains(got, "v=\"a\\\"b\\\\c\\nd�\"") {
		t.Fatalf("label not escaped: %q", got)
	}
	if !strings.Contains(got, `# HELP can_e_total Help with \\ and\nnewline.`) {
		t.Fatalf("help not escaped: %q", got)
	}
	if strings.Count(got, "\n") != 3 {
		t.Fatalf("raw newline leaked: %q", got)
	}
}

func TestCardinalityCapOverflow(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("can_card_total", "x", "k")
	for i := 0; i < MaxSeries+500; i++ {
		c.With("v" + strconv.Itoa(i)).Inc()
	}
	got := expose(t, r)
	if n := strings.Count(got, "can_card_total{"); n != MaxSeries+1 {
		t.Fatalf("series = %d, want %d (cap + overflow)", n, MaxSeries+1)
	}
	if !strings.Contains(got, `can_card_total{k="overflow"} 500`) {
		t.Fatalf("overflow bucket missing or wrong")
	}
}

func TestLongLabelValueTruncated(t *testing.T) {
	r := NewRegistry()
	r.Counter("can_long_total", "x", "k").With(strings.Repeat("z", 5000)).Inc()
	if len(expose(t, r)) > 1000 {
		t.Fatal("label value not bounded")
	}
}

func TestInvalidNamesPanic(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	NewRegistry().Counter("bad name", "x")
}

func TestRaceFreeUpdates(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("can_race_total", "x", "k")
	h := r.Histogram("can_race_seconds", "x", DefBuckets)
	g := r.Gauge("can_race_g", "x")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				c.With("a").Inc()
				h.With().Observe(0.01)
				g.With().Add(1)
				if j%100 == 0 {
					expose(t, r)
				}
			}
		}()
	}
	wg.Wait()
	out := expose(t, r)
	if !strings.Contains(out, `can_race_total{k="a"} 8000`) {
		t.Fatal("lost counter updates")
	}
	if !strings.Contains(out, "can_race_g 8000") {
		t.Fatal("lost gauge updates")
	}
}

func TestDuplicateRegistrationReturnsSame(t *testing.T) {
	r := NewRegistry()
	a := r.Counter("can_dup_total", "x", "k")
	b := r.Counter("can_dup_total", "x", "k")
	a.With("1").Inc()
	b.With("1").Inc()
	if !strings.Contains(expose(t, r), `can_dup_total{k="1"} 2`) {
		t.Fatal("duplicate registration made a second series")
	}
}

func BenchmarkCounterInc(b *testing.B) {
	c := NewRegistry().Counter("can_b_total", "x", "method", "route", "class")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c.With("GET", "GET /v1/leads/{id}", "2xx").Inc()
	}
}

func BenchmarkHistogramObserve(b *testing.B) {
	h := NewRegistry().Histogram("can_b_seconds", "x", DefBuckets, "method", "route")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.With("GET", "GET /v1/leads/{id}").Observe(0.012)
	}
}
