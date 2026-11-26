// Command composecheck compares the memory of a running Docker Compose stack, and the sizes of the
// service binaries, with the budgets of dev/footprint/budgets.json (docs/measurements/footprint.md).
//
//	composecheck -project can-e2e -phase idle|loaded   # memory of the running containers
//	composecheck -bin-dir DIR                          # sizes of DIR/<binary>
//
// It exits 1 when something is over budget or missing, naming each offender. Linux with cgroup v2
// only; it reads /proc/<pid> and the container's cgroup, which need no root.
//
// The budgeted figure is anonymous memory (memory.stat "anon": heap, stacks, runtime), not
// memory.current: page cache depends on the host and on what ran before, and is reclaimable.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
)

const mib = 1 << 20

// Budget is the limit of one service, in MiB. A zero field is not checked.
type Budget struct {
	IdleAnon   float64 `json:"idle_anon_mib"`
	LoadedAnon float64 `json:"loaded_anon_mib"`
	PeakRSS    float64 `json:"peak_rss_mib"` // VmHWM of the main process, checked when loaded
}

// Budgets is dev/footprint/budgets.json.
type Budgets struct {
	Services   map[string]Budget  `json:"services"`
	IdleTotal  float64            `json:"idle_total_anon_mib"`
	LoadTotal  float64            `json:"loaded_total_anon_mib"`
	BinaryMiB  map[string]float64 `json:"binary_mib"`
	Comment    string             `json:"comment"`
	Unbudgeted []string           `json:"unbudgeted"` // services reported but not counted (not ours)
}

// Sample is one container's memory, in bytes.
type Sample struct {
	Service                       string
	Current, Anon, File, RSS, HWM int64
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("composecheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("budgets", "dev/footprint/budgets.json", "budget file")
	project := fs.String("project", "", "compose project whose containers are measured")
	phase := fs.String("phase", "idle", "idle or loaded")
	binDir := fs.String("bin-dir", "", "check the sizes of the binaries in this directory instead")
	engine := fs.String("engine", "docker", "container CLI (docker or podman)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	b, err := load(*file)
	if err != nil {
		fmt.Fprintln(stderr, "composecheck:", err)
		return 2
	}
	var over []string
	switch {
	case *binDir != "":
		over = checkBinaries(stdout, b, *binDir)
	case *project != "" && (*phase == "idle" || *phase == "loaded"):
		samples, err := collect(*engine, *project)
		if err != nil {
			fmt.Fprintln(stderr, "composecheck:", err)
			return 2
		}
		over = checkMemory(stdout, b, samples, *phase)
	default:
		fmt.Fprintln(stderr, "composecheck: need -bin-dir, or -project with -phase idle|loaded")
		return 2
	}
	if len(over) > 0 {
		fmt.Fprintf(stderr, "FOOTPRINT BUDGET EXCEEDED (%d):\n", len(over))
		for _, o := range over {
			fmt.Fprintln(stderr, "  - "+o)
			if os.Getenv("GITHUB_ACTIONS") == "true" { // shows on the pull request although the job is advisory
				fmt.Fprintln(stdout, "::error title=Footprint budget::"+o)
			}
		}
		fmt.Fprintln(stderr, "Reduce the memory, or change "+*file+" and say why in docs/measurements/footprint.md.")
		return 1
	}
	fmt.Fprintln(stdout, "footprint: within budget")
	return 0
}

func load(path string) (*Budgets, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // developer tool, path from the command line
	if err != nil {
		return nil, err
	}
	var b Budgets
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(b.Services) == 0 || len(b.BinaryMiB) == 0 {
		return nil, fmt.Errorf("%s: no budgets", path)
	}
	return &b, nil
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for s := range m {
		k = append(k, s)
	}
	slices.Sort(k)
	return k
}

func checkBinaries(w io.Writer, b *Budgets, dir string) (over []string) {
	tw := tabwriter.NewWriter(w, 2, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "binary\tMiB\tbudget\t")
	for _, name := range sortedKeys(b.BinaryMiB) {
		limit := b.BinaryMiB[name]
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			over = append(over, fmt.Sprintf("binary %s: not measured: %v", name, err))
			continue
		}
		got := float64(st.Size()) / mib
		fmt.Fprintf(tw, "%s\t%.1f\t%.1f\t%s\n", name, got, limit, verdict(got, limit))
		if got > limit {
			over = append(over, fmt.Sprintf("binary %s is %.1f MiB, budget %.1f", name, got, limit))
		}
	}
	_ = tw.Flush()
	return over
}

func verdict(got, limit float64) string {
	if limit > 0 && got > limit {
		return "OVER"
	}
	return "ok"
}

func checkMemory(w io.Writer, b *Budgets, samples []Sample, phase string) (over []string) {
	byName := map[string]Sample{}
	for _, s := range samples {
		byName[s.Service] = s
	}
	tw := tabwriter.NewWriter(w, 2, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "service (%s)\tanon\tbudget\tcgroup\tfile\tRSS\tpeak RSS\t\n", phase)
	var total, current float64
	for _, name := range sortedKeys(byName) {
		s := byName[name]
		bud, budgeted := b.Services[name]
		limit := bud.IdleAnon
		if phase == "loaded" {
			limit = bud.LoadedAnon
		}
		anon, hwm := float64(s.Anon)/mib, float64(s.HWM)/mib
		v := verdict(anon, limit)
		switch {
		case slices.Contains(b.Unbudgeted, name):
			v = "not budgeted"
		case !budgeted:
			v = "NO BUDGET"
			over = append(over, fmt.Sprintf("service %s has no budget (add it to the budget file)", name))
		case anon > limit:
			over = append(over, fmt.Sprintf("%s %s anonymous memory is %.1f MiB, budget %.1f", name, phase, anon, limit))
		}
		if budgeted && phase == "loaded" && bud.PeakRSS > 0 && hwm > bud.PeakRSS {
			v = "OVER (peak)"
			over = append(over, fmt.Sprintf("%s peak RSS is %.1f MiB, budget %.1f", name, hwm, bud.PeakRSS))
		}
		if !slices.Contains(b.Unbudgeted, name) {
			total += anon
		}
		current += float64(s.Current) / mib
		fmt.Fprintf(tw, "%s\t%.1f\t%.1f\t%.1f\t%.1f\t%.1f\t%.1f\t%s\n", name, anon, limit,
			float64(s.Current)/mib, float64(s.File)/mib, float64(s.RSS)/mib, hwm, v)
	}
	limit := b.IdleTotal
	if phase == "loaded" {
		limit = b.LoadTotal
	}
	fmt.Fprintf(tw, "total budgeted anon\t%.1f\t%.1f\t\t\t\t\t%s\n", total, limit, verdict(total, limit))
	fmt.Fprintf(tw, "total cgroup, all containers\t\t\t%.1f\t\t\t\t\n", current)
	_ = tw.Flush()
	if limit > 0 && total > limit {
		over = append(over, fmt.Sprintf("the stack's %s anonymous memory is %.1f MiB, budget %.1f", phase, total, limit))
	}
	for _, name := range sortedKeys(b.Services) {
		if _, ok := byName[name]; !ok {
			over = append(over, fmt.Sprintf("service %s is not running: nothing was measured", name))
		}
	}
	return over
}

// collect measures every running container of the compose project.
func collect(engine, project string) ([]Sample, error) {
	ids, err := exec.Command(engine, "ps", "-q", "--filter", "label=com.docker.compose.project="+project).Output() //nolint:gosec // developer tool
	if err != nil {
		return nil, fmt.Errorf("%s ps: %w", engine, err)
	}
	if len(strings.Fields(string(ids))) == 0 {
		return nil, errors.New("no running containers in project " + project)
	}
	args := append([]string{"inspect", "--format", `{{index .Config.Labels "com.docker.compose.service"}} {{.State.Pid}}`}, strings.Fields(string(ids))...)
	out, err := exec.Command(engine, args...).Output() //nolint:gosec // developer tool
	if err != nil {
		return nil, fmt.Errorf("%s inspect: %w", engine, err)
	}
	var samples []Sample
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			return nil, fmt.Errorf("unexpected inspect line %q", line)
		}
		pid, err := strconv.Atoi(f[1])
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("%s is not running", f[0])
		}
		s, err := sample("/proc", "/sys/fs/cgroup", f[0], pid)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f[0], err)
		}
		samples = append(samples, s)
	}
	return samples, nil
}

// sample reads one process and its cgroup (v2).
func sample(procRoot, cgRoot, service string, pid int) (Sample, error) {
	s := Sample{Service: service}
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	status, err := os.ReadFile(filepath.Join(dir, "status")) //nolint:gosec // /proc
	if err != nil {
		return s, err
	}
	kb := fields(string(status), ":")
	s.RSS, s.HWM = kb["VmRSS"]*1024, kb["VmHWM"]*1024
	cg, err := os.ReadFile(filepath.Join(dir, "cgroup")) //nolint:gosec // /proc
	if err != nil {
		return s, err
	}
	_, rel, ok := strings.Cut(strings.TrimSpace(string(cg)), "0::")
	if !ok {
		return s, errors.New("cgroup v2 is required")
	}
	// Some engines put the process in a child cgroup without the memory controller: walk up.
	path := filepath.Join(cgRoot, rel)
	for {
		if _, err := os.Stat(filepath.Join(path, "memory.current")); err == nil {
			break
		}
		if path = filepath.Dir(path); len(path) <= len(cgRoot) {
			return s, errors.New("no memory cgroup found")
		}
	}
	cur, err := os.ReadFile(filepath.Join(path, "memory.current")) //nolint:gosec // cgroupfs
	if err != nil {
		return s, err
	}
	if s.Current, err = strconv.ParseInt(strings.TrimSpace(string(cur)), 10, 64); err != nil {
		return s, err
	}
	stat, err := os.ReadFile(filepath.Join(path, "memory.stat")) //nolint:gosec // cgroupfs
	if err != nil {
		return s, err
	}
	m := fields(string(stat), " ")
	if _, ok := m["anon"]; !ok {
		return s, errors.New("memory.stat has no anon line")
	}
	s.Anon, s.File = m["anon"], m["file"]
	return s, nil
}

// fields parses "key<sep> value [unit]" lines with numeric values; others are skipped.
func fields(text, sep string) map[string]int64 {
	out := map[string]int64{}
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, sep)
		if !ok {
			continue
		}
		if f := strings.Fields(v); len(f) > 0 {
			if n, err := strconv.ParseInt(f[0], 10, 64); err == nil {
				out[k] = n
			}
		}
	}
	return out
}
