// Package footprint turns raw idle-memory samples taken from a k3d/k3s node into a JSON report
// and checks a workload against a module's declared ram_mb (Constitution II, SPEC 3.2).
//
// The shell side (dev/footprint) only collects text; everything that interprets it lives here so
// it can be unit-tested with canned input and no cluster.
//
// Sample format, one line each (cgroup v2, bytes; PROC in kB):
//
//	NODE <memory.current> <inactive_file>           the node container's root cgroup
//	CG <path> <memory.current> <inactive_file>      one pod-level cgroup under kubepods
//	PROC <name> <VmRSS kB>                          a process inside the node (the k3s server)
//
// The figure used everywhere is the working set: memory.current minus inactive_file (the same
// definition kubelet and metrics-server use), so reclaimable page cache is not counted.
package footprint

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// MiB is the unit behind every "MB" figure in this package (SPEC and ram_mb use binary MB).
const MiB = 1024 * 1024

// DefaultTolerance is Constitution II: measured idle RAM may exceed declared ram_mb by 20%.
const DefaultTolerance = 0.20

// PodInfo identifies a pod in a `kubectl get pods -o json` answer.
type PodInfo struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
	Phase     string `json:"phase"`
	Ready     bool   `json:"ready"`
}

// ParsePods reads the output of `kubectl get pods -A -o json`.
func ParsePods(data []byte) ([]PodInfo, error) {
	var doc struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
				UID       string `json:"uid"`
			} `json:"metadata"`
			Status struct {
				Phase      string `json:"phase"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("pods json: %w", err)
	}
	out := make([]PodInfo, 0, len(doc.Items))
	for _, it := range doc.Items {
		p := PodInfo{Namespace: it.Metadata.Namespace, Name: it.Metadata.Name, UID: it.Metadata.UID, Phase: it.Status.Phase}
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				p.Ready = true
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// Cgroup is one memory reading.
type Cgroup struct {
	Path         string
	Current      int64
	InactiveFile int64
}

// WorkingSet is memory.current minus inactive_file, never negative.
func (c Cgroup) WorkingSet() int64 {
	if v := c.Current - c.InactiveFile; v > 0 {
		return v
	}
	return 0
}

// Raw is one parsed sample.
type Raw struct {
	Node     Cgroup
	HasNode  bool
	Pods     []Cgroup
	ProcRSSK map[string]int64
}

func parseNums(s ...string) ([]int64, bool) {
	out := make([]int64, len(s))
	for i, x := range s {
		v, err := strconv.ParseInt(x, 10, 64)
		if err != nil || v < 0 {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}

// ParseRaw parses one sample file. Unknown or malformed lines are errors: a half-parsed sample
// must not silently produce a flattering number.
func ParseRaw(text string) (Raw, error) {
	r := Raw{ProcRSSK: map[string]int64{}}
	for i, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		bad := fmt.Errorf("sample line %d: malformed %q", i+1, line)
		switch f[0] {
		case "NODE":
			n, ok := parseNums(f[1:]...)
			if len(f) != 3 || !ok {
				return Raw{}, bad
			}
			r.Node = Cgroup{Path: "/", Current: n[0], InactiveFile: n[1]}
			r.HasNode = true
		case "CG":
			if len(f) != 4 {
				return Raw{}, bad
			}
			n, ok := parseNums(f[2:]...)
			if !ok {
				return Raw{}, bad
			}
			r.Pods = append(r.Pods, Cgroup{Path: f[1], Current: n[0], InactiveFile: n[1]})
		case "PROC":
			if len(f) != 3 {
				return Raw{}, bad
			}
			n, ok := parseNums(f[2])
			if !ok {
				return Raw{}, bad
			}
			r.ProcRSSK[f[1]] = n[0]
		default:
			return Raw{}, bad
		}
	}
	if !r.HasNode {
		return Raw{}, fmt.Errorf("sample has no NODE line")
	}
	return r, nil
}

var podUIDRE = regexp.MustCompile(`pod([0-9a-f]{8}[-_][0-9a-f]{4}[-_][0-9a-f]{4}[-_][0-9a-f]{4}[-_][0-9a-f]{12})(\.slice)?$`)

// PodUID extracts the pod UID from a pod-level cgroup path, accepting both the cgroupfs layout
// (.../pod<uid>) and the systemd layout (...-pod<uid_with_underscores>.slice).
func PodUID(path string) (string, bool) {
	m := podUIDRE.FindStringSubmatch(path)
	if m == nil {
		return "", false
	}
	return strings.ReplaceAll(m[1], "_", "-"), true
}

// Stat summarises a series in MiB.
type Stat struct {
	MedianMB float64 `json:"median_mb"`
	MaxMB    float64 `json:"max_mb"`
}

// PodStat is one pod's working set over the window.
type PodStat struct {
	Namespace string  `json:"namespace"`
	Name      string  `json:"name"`
	MedianMB  float64 `json:"median_mb"`
	MaxMB     float64 `json:"max_mb"`
	Samples   int     `json:"samples"`
}

// NSStat is the sum of pod medians in a namespace.
type NSStat struct {
	Namespace string  `json:"namespace"`
	MedianMB  float64 `json:"median_mb"`
}

// Report is the JSON the harness emits.
//
// Node is the whole k3d node container (k3s, containerd, every pod). OnPods is the sum of pod
// working sets; System is Node minus OnPods (k3s server, containerd, shims and kernel memory of
// the container). K3sRSS is the k3s server process alone.
type Report struct {
	Label    string    `json:"label"`
	Source   string    `json:"source"` // "cgroup" or "kubectl-top"
	Samples  int       `json:"samples"`
	Node     *Stat     `json:"node,omitempty"`
	OnPods   *Stat     `json:"pods_total,omitempty"`
	System   *Stat     `json:"system,omitempty"`
	K3sRSS   *Stat     `json:"k3s_server_rss,omitempty"`
	Pods     []PodStat `json:"pods"`
	ByNS     []NSStat  `json:"namespaces"`
	Unmapped int       `json:"unmapped_cgroups"`
}

func mb(bytes float64) float64 { return math.Round(bytes/MiB*100) / 100 }

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func maxOf(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		m = math.Max(m, x)
	}
	return m
}

func stat(v []float64) *Stat {
	if len(v) == 0 {
		return nil
	}
	return &Stat{MedianMB: mb(median(v)), MaxMB: mb(maxOf(v))}
}

// Build aggregates samples taken during the idle window into a Report. Pods are matched to
// cgroups by UID; cgroups with no matching pod (pods that vanished) are counted in Unmapped and
// still included in the pod total, so the total is honest.
func Build(label string, pods []PodInfo, samples []Raw) (Report, error) {
	if len(samples) == 0 {
		return Report{}, fmt.Errorf("no samples")
	}
	byUID := map[string]PodInfo{}
	for _, p := range pods {
		byUID[p.UID] = p
	}
	type key struct{ ns, name string }
	series := map[key][]float64{}
	var node, podsTotal, system, k3s []float64
	unmapped := 0
	for _, s := range samples {
		var total float64
		for _, c := range s.Pods {
			ws := float64(c.WorkingSet())
			total += ws
			uid, ok := PodUID(c.Path)
			p, found := byUID[uid]
			if !ok || !found {
				unmapped++
				continue
			}
			k := key{p.Namespace, p.Name}
			series[k] = append(series[k], ws)
		}
		n := float64(s.Node.WorkingSet())
		node = append(node, n)
		podsTotal = append(podsTotal, total)
		system = append(system, math.Max(0, n-total))
		if v, ok := s.ProcRSSK["k3s-server"]; ok {
			k3s = append(k3s, float64(v)*1024)
		}
	}
	r := Report{Label: label, Source: "cgroup", Samples: len(samples), Pods: []PodStat{}, ByNS: []NSStat{}, Unmapped: unmapped}
	r.Node, r.OnPods, r.System, r.K3sRSS = stat(node), stat(podsTotal), stat(system), stat(k3s)
	nsSum := map[string]float64{}
	for k, v := range series {
		med := median(v)
		r.Pods = append(r.Pods, PodStat{Namespace: k.ns, Name: k.name, MedianMB: mb(med), MaxMB: mb(maxOf(v)), Samples: len(v)})
		nsSum[k.ns] += med
	}
	sort.Slice(r.Pods, func(i, j int) bool {
		if r.Pods[i].Namespace != r.Pods[j].Namespace {
			return r.Pods[i].Namespace < r.Pods[j].Namespace
		}
		return r.Pods[i].Name < r.Pods[j].Name
	})
	for ns, v := range nsSum {
		r.ByNS = append(r.ByNS, NSStat{ns, mb(v)})
	}
	sort.Slice(r.ByNS, func(i, j int) bool { return r.ByNS[i].Namespace < r.ByNS[j].Namespace })
	return r, nil
}

// TopRow is one line of `kubectl top pods -A --no-headers`.
type TopRow struct {
	Namespace string
	Name      string
	MB        float64
}

// ParseTop parses `kubectl top pods -A --no-headers` (columns: NAMESPACE NAME CPU MEMORY).
// Memory units Ki, Mi and Gi are accepted. It needs metrics-server, which dev/k3d.yaml
// disables, so the cgroup path is the default; this serves clusters that have it.
func ParseTop(text string) ([]TopRow, error) {
	var rows []TopRow
	for i, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 4 {
			return nil, fmt.Errorf("top line %d: want 4 columns, got %q", i+1, line)
		}
		v, err := parseQuantityMB(f[3])
		if err != nil {
			return nil, fmt.Errorf("top line %d: %w", i+1, err)
		}
		rows = append(rows, TopRow{f[0], f[1], v})
	}
	return rows, nil
}

func parseQuantityMB(s string) (float64, error) {
	for _, u := range []struct {
		suffix string
		toMiB  float64
	}{{"Ki", 1.0 / 1024}, {"Mi", 1}, {"Gi", 1024}} {
		if strings.HasSuffix(s, u.suffix) {
			v, err := strconv.ParseFloat(strings.TrimSuffix(s, u.suffix), 64)
			if err != nil || v < 0 {
				return 0, fmt.Errorf("bad memory %q", s)
			}
			return v * u.toMiB, nil
		}
	}
	return 0, fmt.Errorf("bad memory %q (want Ki, Mi or Gi)", s)
}

// FromTop builds a one-shot Report from kubectl top rows.
func FromTop(label string, rows []TopRow) Report {
	r := Report{Label: label, Source: "kubectl-top", Samples: 1, Pods: []PodStat{}, ByNS: []NSStat{}}
	ns := map[string]float64{}
	for _, x := range rows {
		v := math.Round(x.MB*100) / 100
		r.Pods = append(r.Pods, PodStat{Namespace: x.Namespace, Name: x.Name, MedianMB: v, MaxMB: v, Samples: 1})
		ns[x.Namespace] += x.MB
	}
	for k, v := range ns {
		r.ByNS = append(r.ByNS, NSStat{k, math.Round(v*100) / 100})
	}
	sort.Slice(r.ByNS, func(i, j int) bool { return r.ByNS[i].Namespace < r.ByNS[j].Namespace })
	return r
}

// Selector picks the pods that make up the workload under test.
type Selector struct {
	Namespace  string // required
	NamePrefix string // optional
}

// Result is the outcome of comparing a workload with a declared ram_mb.
type Result struct {
	Pass       bool    `json:"pass"`
	DeclaredMB int     `json:"declared_ram_mb"`
	Tolerance  float64 `json:"tolerance"`
	LimitMB    float64 `json:"limit_mb"`
	ActualMB   float64 `json:"actual_mb"`
	Pods       int     `json:"pods"`
	Message    string  `json:"message"`
}

// Check compares the summed median working set of the selected pods with ram_mb*(1+tolerance).
// Selecting no pod is an error, not a pass: an empty selection measures nothing.
func Check(r Report, sel Selector, declaredMB int, tolerance float64) (Result, error) {
	if declaredMB <= 0 {
		return Result{}, fmt.Errorf("declared ram_mb must be positive, got %d", declaredMB)
	}
	if tolerance < 0 {
		return Result{}, fmt.Errorf("tolerance must not be negative")
	}
	if sel.Namespace == "" {
		return Result{}, fmt.Errorf("a namespace is required")
	}
	res := Result{DeclaredMB: declaredMB, Tolerance: tolerance, LimitMB: math.Round(float64(declaredMB)*(1+tolerance)*100) / 100}
	for _, p := range r.Pods {
		if p.Namespace == sel.Namespace && strings.HasPrefix(p.Name, sel.NamePrefix) {
			res.ActualMB += p.MedianMB
			res.Pods++
		}
	}
	if res.Pods == 0 {
		return res, fmt.Errorf("no pod in namespace %q with prefix %q in the report", sel.Namespace, sel.NamePrefix)
	}
	res.ActualMB = math.Round(res.ActualMB*100) / 100
	res.Pass = res.ActualMB <= res.LimitMB
	verdict := "within"
	if !res.Pass {
		verdict = "OVER"
	}
	res.Message = fmt.Sprintf("%s: idle %.2f MB across %d pod(s) vs declared %d MB (+%.0f%% = %.2f MB): %s",
		sel.Namespace, res.ActualMB, res.Pods, declaredMB, tolerance*100, res.LimitMB, verdict)
	return res, nil
}
