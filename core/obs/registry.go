// Package obs is a minimal, dependency-free metrics registry with Prometheus text exposition
// (version 0.0.4): counters, gauges and fixed-bucket histograms, plus the HTTP middleware,
// health model and process metrics the in-house services share.
//
// It replaces prometheus/client_golang on purpose (Constitution II): the full client adds roughly
// 1.5 MiB to a binary and several MiB of resident memory from its default collectors; see
// specs/030-observability/research.md for the measurement.
//
// Safety properties, each covered by tests:
//   - updates are lock-free atomics (race-free);
//   - every metric family has at most MaxSeries label combinations; further combinations fold
//     into one series whose label values are all "overflow", so memory is bounded however
//     hostile the input;
//   - label values are truncated to MaxLabelLen bytes, made valid UTF-8 and escaped on output.
//
// Callers must pass only low-cardinality, non-personal label values (route patterns, status
// classes), never user ids, emails or raw paths.
package obs

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	// MaxSeries bounds the label combinations of one metric family.
	MaxSeries = 256
	// MaxLabelLen bounds one label value in bytes.
	MaxLabelLen = 128
	maxLabels   = 4
)

// DefBuckets are the default latency buckets in seconds.
var DefBuckets = []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// validName reports whether s matches [a-zA-Z_][a-zA-Z0-9_]* (plus ':' for metric names). Written
// by hand: regexp would add about 300 KB to every binary, can-static included.
func validName(s string, colon bool) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_', colon && c == ':':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

type labelKey [maxLabels]string

type series struct {
	vals    labelKey
	val     atomic.Uint64   // float64 bits: counter and gauge value
	sum     atomic.Uint64   // float64 bits: histogram sum
	buckets []atomic.Uint64 // non-cumulative per-bucket counts, last entry is +Inf
}

func (s *series) add(d float64) {
	for {
		old := s.val.Load()
		if s.val.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+d)) {
			return
		}
	}
}

type family struct {
	name, help, typ string
	labels          []string
	bounds          []float64
	fn              func() float64 // set for *Func families

	mu       sync.RWMutex
	series   map[labelKey]*series
	overflow *series
}

func (f *family) get(vals []string) *series {
	if len(vals) != len(f.labels) {
		panic(fmt.Sprintf("obs: %s takes %d label values, got %d", f.name, len(f.labels), len(vals)))
	}
	var k labelKey
	for i, v := range vals {
		k[i] = clean(v)
	}
	f.mu.RLock()
	s := f.series[k]
	f.mu.RUnlock()
	if s != nil {
		return s
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if s = f.series[k]; s != nil {
		return s
	}
	if len(f.series) >= MaxSeries {
		if f.overflow == nil {
			var ok labelKey
			for i := range f.labels {
				ok[i] = "overflow"
			}
			f.overflow = f.newSeries(ok)
		}
		return f.overflow
	}
	s = f.newSeries(k)
	f.series[k] = s
	return s
}

func (f *family) newSeries(k labelKey) *series {
	s := &series{vals: k}
	if f.typ == "histogram" {
		s.buckets = make([]atomic.Uint64, len(f.bounds)+1)
	}
	return s
}

// clean bounds a label value and makes it valid UTF-8.
func clean(v string) string {
	if len(v) > MaxLabelLen {
		v = v[:MaxLabelLen]
	}
	return strings.ToValidUTF8(v, "�")
}

// Registry holds metric families. The zero value is not usable; use NewRegistry.
type Registry struct {
	mu   sync.Mutex
	fams map[string]*family
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{fams: map[string]*family{}} }

// Default is the process-wide registry served by Handler.
var Default = NewRegistry()

func (r *Registry) register(name, help, typ string, labels []string, bounds []float64, fn func() float64) *family {
	if !validName(name, true) {
		panic("obs: invalid metric name " + strconv.Quote(name))
	}
	if len(labels) > maxLabels {
		panic("obs: too many labels on " + name)
	}
	for _, l := range labels {
		if !validName(l, false) || strings.HasPrefix(l, "__") || (typ == "histogram" && l == "le") {
			panic("obs: invalid label name " + strconv.Quote(l) + " on " + name)
		}
	}
	if typ == "histogram" {
		if !sort.Float64sAreSorted(bounds) {
			panic("obs: unsorted buckets on " + name)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.fams[name]; ok {
		if f.typ != typ || len(f.labels) != len(labels) || f.fn != nil || fn != nil {
			if fn != nil && f.fn != nil && f.typ == typ { // re-registered function: latest wins
				f.fn = fn
				return f
			}
			panic("obs: conflicting registration of " + name)
		}
		return f
	}
	f := &family{name: name, help: help, typ: typ, labels: append([]string(nil), labels...), bounds: append([]float64(nil), bounds...), fn: fn, series: map[labelKey]*series{}}
	r.fams[name] = f
	return f
}

// CounterVec is a family of monotonically increasing counters.
type CounterVec struct{ f *family }

// Counter is one counter series.
type Counter struct{ s *series }

// Counter registers (or returns the existing) counter family.
func (r *Registry) Counter(name, help string, labels ...string) *CounterVec {
	return &CounterVec{r.register(name, help, "counter", labels, nil, nil)}
}

// With returns the series for the label values.
func (c *CounterVec) With(vals ...string) *Counter { return &Counter{c.f.get(vals)} }

// Inc adds one.
func (c *Counter) Inc() { c.s.add(1) }

// Add adds d, which must not be negative (negative values are ignored).
func (c *Counter) Add(d float64) {
	if d > 0 {
		c.s.add(d)
	}
}

// GaugeVec is a family of gauges.
type GaugeVec struct{ f *family }

// Gauge is one gauge series.
type Gauge struct{ s *series }

// Gauge registers (or returns the existing) gauge family.
func (r *Registry) Gauge(name, help string, labels ...string) *GaugeVec {
	return &GaugeVec{r.register(name, help, "gauge", labels, nil, nil)}
}

// With returns the series for the label values.
func (g *GaugeVec) With(vals ...string) *Gauge { return &Gauge{g.f.get(vals)} }

// Set replaces the value.
func (g *Gauge) Set(v float64) { g.s.val.Store(math.Float64bits(v)) }

// Add adds d (negative allowed).
func (g *Gauge) Add(d float64) { g.s.add(d) }

// Inc adds one.
func (g *Gauge) Inc() { g.s.add(1) }

// Dec subtracts one.
func (g *Gauge) Dec() { g.s.add(-1) }

// GaugeFunc registers a gauge computed at scrape time. Registering the same name again replaces
// the function. The function must be fast and must not take locks held by request paths.
func (r *Registry) GaugeFunc(name, help string, fn func() float64) {
	r.register(name, help, "gauge", nil, nil, fn)
}

// CounterFunc is GaugeFunc for a monotonically increasing value.
func (r *Registry) CounterFunc(name, help string, fn func() float64) {
	r.register(name, help, "counter", nil, nil, fn)
}

// HistogramVec is a family of fixed-bucket histograms.
type HistogramVec struct{ f *family }

// Histogram is one histogram series.
type Histogram struct {
	s *series
	f *family
}

// Histogram registers (or returns the existing) histogram family with the given upper bounds.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *HistogramVec {
	return &HistogramVec{r.register(name, help, "histogram", labels, buckets, nil)}
}

// With returns the series for the label values.
func (h *HistogramVec) With(vals ...string) *Histogram { return &Histogram{h.f.get(vals), h.f} }

// Observe records one value.
func (h *Histogram) Observe(v float64) {
	b := h.f.bounds
	i := sort.SearchFloat64s(b, v) // first bound >= v
	h.s.buckets[i].Add(1)
	for {
		old := h.s.sum.Load()
		if h.s.sum.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+v)) {
			return
		}
	}
}

// WriteTo writes the text exposition. Families and series are sorted so output is stable.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	fams := make([]*family, 0, len(r.fams))
	for _, f := range r.fams {
		fams = append(fams, f)
	}
	r.mu.Unlock()
	sort.Slice(fams, func(i, j int) bool { return fams[i].name < fams[j].name })
	var b bytes.Buffer
	for _, f := range fams {
		f.write(&b)
	}
	n, err := w.Write(b.Bytes())
	return int64(n), err
}

func (f *family) write(b *bytes.Buffer) {
	b.WriteString("# HELP " + f.name + " " + escapeHelp(f.help) + "\n# TYPE " + f.name + " " + f.typ + "\n")
	if f.fn != nil {
		b.WriteString(f.name + " " + formatFloat(f.fn()) + "\n")
		return
	}
	f.mu.RLock()
	all := make([]*series, 0, len(f.series)+1)
	for _, s := range f.series {
		all = append(all, s)
	}
	ov := f.overflow
	f.mu.RUnlock()
	sort.Slice(all, func(i, j int) bool {
		for k := 0; k < maxLabels; k++ {
			if all[i].vals[k] != all[j].vals[k] {
				return all[i].vals[k] < all[j].vals[k]
			}
		}
		return false
	})
	if ov != nil {
		all = append(all, ov)
	}
	for _, s := range all {
		if f.typ != "histogram" {
			b.WriteString(f.name)
			f.writeLabels(b, s, "")
			b.WriteString(" " + formatFloat(math.Float64frombits(s.val.Load())) + "\n")
			continue
		}
		var cum uint64
		for i := range s.buckets {
			cum += s.buckets[i].Load()
			le := "+Inf"
			if i < len(f.bounds) {
				le = formatFloat(f.bounds[i])
			}
			b.WriteString(f.name + "_bucket")
			f.writeLabels(b, s, le)
			b.WriteString(" " + strconv.FormatUint(cum, 10) + "\n")
		}
		b.WriteString(f.name + "_sum")
		f.writeLabels(b, s, "")
		b.WriteString(" " + formatFloat(math.Float64frombits(s.sum.Load())) + "\n")
		b.WriteString(f.name + "_count")
		f.writeLabels(b, s, "")
		b.WriteString(" " + strconv.FormatUint(cum, 10) + "\n")
	}
}

func (f *family) writeLabels(b *bytes.Buffer, s *series, le string) {
	if len(f.labels) == 0 && le == "" {
		return
	}
	b.WriteByte('{')
	for i, l := range f.labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l + `="` + escapeLabel(s.vals[i]) + `"`)
	}
	if le != "" {
		if len(f.labels) > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`le="` + le + `"`)
	}
	b.WriteByte('}')
}

var (
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

func escapeLabel(v string) string { return labelEscaper.Replace(v) }
func escapeHelp(v string) string  { return helpEscaper.Replace(v) }

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
