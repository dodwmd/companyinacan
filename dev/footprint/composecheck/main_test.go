package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBudgetFileCoversTheStack(t *testing.T) {
	b, err := load("../budgets.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"core", "site", "forms", "work", "people", "jobs", "finance", "equity", "files"} {
		v, ok := b.Services[s]
		if !ok || v.IdleAnon <= 0 || v.LoadedAnon < v.IdleAnon {
			t.Errorf("%s: idle and loaded budgets are required, got %+v", s, v)
		}
		bin := "can-" + s
		if s == "site" {
			bin = "can-static"
		}
		if b.BinaryMiB[bin] <= 0 {
			t.Errorf("%s: no binary size budget", bin)
		}
	}
}

func budgets() *Budgets {
	return &Budgets{
		Services:   map[string]Budget{"core": {IdleAnon: 20, LoadedAnon: 30, PeakRSS: 60}, "work": {IdleAnon: 10, LoadedAnon: 12}},
		Unbudgeted: []string{"edge"}, IdleTotal: 28, LoadTotal: 40,
	}
}

func TestCheckMemory(t *testing.T) {
	ok := []Sample{{Service: "core", Anon: 15 * mib, HWM: 90 * mib}, {Service: "work", Anon: 9 * mib}, {Service: "edge", Anon: 100 * mib}}
	var out bytes.Buffer
	if over := checkMemory(&out, budgets(), ok, "idle"); len(over) != 0 {
		t.Fatalf("within budget (peak is not checked when idle, edge not counted): %v", over)
	}
	for name, c := range map[string]struct {
		samples []Sample
		phase   string
		want    string
	}{
		"service over":  {[]Sample{{Service: "core", Anon: 21 * mib}, {Service: "work", Anon: mib}}, "idle", "core idle anonymous memory is 21.0 MiB, budget 20.0"},
		"total over":    {[]Sample{{Service: "core", Anon: 20 * mib}, {Service: "work", Anon: 10 * mib}}, "idle", "stack's idle"},
		"peak over":     {[]Sample{{Service: "core", Anon: mib, HWM: 61 * mib}, {Service: "work", Anon: mib}}, "loaded", "core peak RSS is 61.0"},
		"missing":       {[]Sample{{Service: "core", Anon: mib}}, "idle", "work is not running"},
		"unknown extra": {[]Sample{{Service: "core"}, {Service: "work"}, {Service: "new"}}, "idle", "new has no budget"},
	} {
		over := checkMemory(&bytes.Buffer{}, budgets(), c.samples, c.phase)
		if !strings.Contains(strings.Join(over, "\n"), c.want) {
			t.Errorf("%s: want %q in %v", name, c.want, over)
		}
	}
}

func TestLoadedNeedsTheSameContainers(t *testing.T) {
	run := Instance{ID: "a1", PID: 10, Started: "t0"}
	before := map[string]Instance{"core": run, "work": run}
	same := []Sample{{Service: "core", Run: run}, {Service: "work", Run: run}}
	if got := replaced(before, same); len(got) != 0 {
		t.Fatalf("unchanged: %v", got)
	}
	for name, c := range map[string]struct {
		now  Instance
		want string
	}{
		"recreated":     {Instance{ID: "b2", PID: 11, Started: "t1"}, "recreated"},
		"restarted":     {Instance{ID: "a1", PID: 11, Started: "t1"}, "restarted"},
		"restart count": {Instance{ID: "a1", PID: 10, Restarts: 1, Started: "t0"}, "restarted"},
		"pid reused":    {Instance{ID: "a1", PID: 10, Started: "t1"}, "restarted"},
	} {
		got := replaced(before, []Sample{{Service: "core", Run: c.now}, {Service: "work", Run: run}})
		if len(got) != 1 || !strings.Contains(got[0], "core was "+c.want) {
			t.Errorf("%s: %v", name, got)
		}
	}
	if got := replaced(before, same[:1]); len(got) != 1 || !strings.Contains(got[0], "work is no longer running") {
		t.Errorf("gone: %v", got)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if _, err := loadState(path); err == nil {
		t.Error("a missing state file must be an error")
	}
	if err := saveState(path, same); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(path)
	if err != nil || len(replaced(got, same)) != 0 {
		t.Fatalf("round trip: %v %v", got, err)
	}
}

func TestParseInspect(t *testing.T) {
	name, run, err := parseInspect("core\tabc\t42\t0\t2026-01-01 10:00:00.5 +0000 UTC")
	if err != nil || name != "core" || run != (Instance{ID: "abc", PID: 42, Started: "2026-01-01 10:00:00.5 +0000 UTC"}) {
		t.Fatalf("%s %+v %v", name, run, err)
	}
	for _, bad := range []string{"core abc", "core\tabc\t0\t0\tt", "core\tabc\t1\tx\tt"} {
		if _, _, err := parseInspect(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSampleWalksUpToTheMemoryCgroup(t *testing.T) {
	proc, cg := t.TempDir(), t.TempDir()
	write := func(path, body string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(proc, "7", "status"), "Name:\tx\nVmHWM:\t  2048 kB\nVmRSS:\t  1024 kB\nRssAnon:\t  512 kB\n")
	write(filepath.Join(proc, "7", "cgroup"), "0::/a/b/container\n")
	write(filepath.Join(cg, "a", "b", "memory.current"), "5000\n")
	write(filepath.Join(cg, "a", "b", "memory.stat"), "anon 9999\nfile 100\n")
	if err := os.MkdirAll(filepath.Join(cg, "a", "b", "container"), 0o750); err != nil {
		t.Fatal(err)
	}
	s, err := sample(proc, cg, "x", 7)
	if err != nil {
		t.Fatal(err)
	}
	if s != (Sample{Service: "x", Current: 5000, Anon: 512 << 10, File: 100, RSS: 1 << 20, HWM: 2 << 20}) {
		t.Fatalf("sample = %+v", s)
	}
}

func TestBinaries(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), make([]byte, 2*mib), 0o600); err != nil {
		t.Fatal(err)
	}
	over := checkBinaries(&bytes.Buffer{}, &Budgets{BinaryMiB: map[string]float64{"a": 1, "gone": 1}}, dir)
	if len(over) != 2 || !strings.Contains(over[0], "a is 2.0 MiB") || !strings.Contains(over[1], "not measured") {
		t.Fatalf("%v", over)
	}
}
