package obs

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// BuildInfo identifies the running binary.
type BuildInfo struct{ Version, Commit string }

var processStart = time.Now()

// DeclaredRAMBytes reads the RAM the component declared in its manifest (Constitution II) from
// CAN_RAM_DECLARED_BYTES (charts set it from the Downward API, resourceFieldRef requests.memory)
// or CAN_RAM_DECLARED_MB. It returns 0 when nothing valid is declared.
func DeclaredRAMBytes(getenv func(string) string) int64 {
	if v := getenv("CAN_RAM_DECLARED_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
		return 0
	}
	if v := getenv("CAN_RAM_DECLARED_MB"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && n < 1<<30 {
			return n << 20
		}
	}
	return 0
}

// rssFromStatm parses /proc/self/statm ("size resident shared ...", in pages).
func rssFromStatm(s string, pageSize int) (int64, error) {
	f := strings.Fields(s)
	if len(f) < 2 {
		return 0, fmt.Errorf("statm: too few fields")
	}
	n, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("statm: bad resident field")
	}
	return n * int64(pageSize), nil
}

func rssBytes() float64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	n, err := rssFromStatm(string(b), os.Getpagesize())
	if err != nil {
		return 0
	}
	return float64(n)
}

func openFDs() float64 {
	d, err := os.Open("/proc/self/fd")
	if err != nil {
		return 0
	}
	defer func() { _ = d.Close() }()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return 0
	}
	return float64(len(names))
}

func gcPauseSeconds() float64 {
	var st debug.GCStats
	st.Pause = nil // only the totals are needed
	debug.ReadGCStats(&st)
	return st.PauseTotal.Seconds()
}

// RegisterProcess registers the process and footprint metrics on r. Everything is computed at
// scrape time, so an idle service pays nothing between scrapes.
//
// can_ram_rss_bytes against can_ram_declared_bytes is the footprint self-check: Constitution II
// fails a component above 120% of its declared RAM, and deploy/monitoring alerts on that ratio.
func RegisterProcess(r *Registry, bi BuildInfo, getenv func(string) string) {
	if bi.Version == "" {
		bi.Version = "dev"
	}
	if bi.Commit == "" {
		bi.Commit = "none"
	}
	r.Gauge("can_build_info", "Build information; the value is always 1.", "version", "commit", "go").
		With(bi.Version, bi.Commit, runtime.Version()).Set(1)
	r.GaugeFunc("can_process_start_time_seconds", "Start time of the process, Unix seconds.", func() float64 { return float64(processStart.Unix()) })
	r.GaugeFunc("can_uptime_seconds", "Seconds since the process started.", func() float64 { return time.Since(processStart).Seconds() })
	r.GaugeFunc("can_goroutines", "Number of goroutines.", func() float64 { return float64(runtime.NumGoroutine()) })
	r.CounterFunc("can_gc_pause_seconds_total", "Total garbage collector pause time.", gcPauseSeconds)
	r.GaugeFunc("can_open_fds", "Open file descriptors.", openFDs)
	r.GaugeFunc("can_ram_rss_bytes", "Resident set size of the process (/proc/self/statm).", rssBytes)
	if d := DeclaredRAMBytes(getenv); d > 0 {
		r.GaugeFunc("can_ram_declared_bytes", "RAM the component declares in its manifest (Constitution II); compare with can_ram_rss_bytes.", func() float64 { return float64(d) })
	}
}
