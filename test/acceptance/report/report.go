// Package report collects what the acceptance journey measured (step timings, `can up` phases,
// node memory), compares it with the budgets and renders it for a person (Markdown) and a program
// (JSON). specs/058-acceptance-journey.
package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Step is one step of the journey. Budget 0 means the step is measured but not budgeted.
type Step struct {
	Name    string  `json:"name"`
	Seconds float64 `json:"seconds"`
	Budget  float64 `json:"budget_seconds,omitempty"`
	Status  string  `json:"status"` // ok, over budget, failed, not run
	Note    string  `json:"note,omitempty"`
}

// Phase is one phase of a `can up` run, as `--output json` reports it.
type Phase struct {
	Name    string  `json:"name"`
	Seconds float64 `json:"seconds"`
	Skipped bool    `json:"skipped,omitempty"`
}

// Run is the phases of one `can up`.
type Run struct {
	Label  string  `json:"label"`
	Phases []Phase `json:"phases"`
}

// Memory is the node's memory at one moment, in MiB. K3s and Pods are cgroup working sets (current
// minus inactive file pages); Used is MemTotal minus MemAvailable of the whole machine, which also
// holds everything that is not the product (the CI runner, the stand-ins).
type Memory struct {
	Label  string `json:"label"`
	K3s    int    `json:"k3s_mib"`
	Pods   int    `json:"pods_mib"`
	Used   int    `json:"node_used_mib"`
	Total  int    `json:"node_total_mib"`
	Budget int    `json:"budget_mib,omitempty"` // for K3s+Pods
	Detail string `json:"detail,omitempty"`     // per container, as the runtime reports it
}

// Stack is what the product holds: k3s (with containerd) and the pods.
func (m Memory) Stack() int { return m.K3s + m.Pods }

// Report is the whole run.
type Report struct {
	Started  time.Time         `json:"started"`
	Facts    map[string]string `json:"facts,omitempty"`
	Steps    []Step            `json:"steps"`
	Runs     []Run             `json:"can_up_runs,omitempty"`
	Memory   []Memory          `json:"memory,omitempty"`
	Notes    []string          `json:"notes,omitempty"` // observations: gaps and warnings seen on the way
	StandIns []string          `json:"stand_ins,omitempty"`
}

// Add records a step and returns its status.
func (r *Report) Add(name string, took, budget time.Duration, failed bool, note string) string {
	s := Step{Name: name, Seconds: round(took.Seconds()), Budget: budget.Seconds(), Status: "ok", Note: note}
	switch {
	case failed:
		s.Status = "failed"
	case budget > 0 && took > budget:
		s.Status = "over budget"
	}
	r.Steps = append(r.Steps, s)
	return s.Status
}

// Note adds an observation once.
func (r *Report) Note(format string, a ...any) {
	n := fmt.Sprintf(format, a...)
	for _, have := range r.Notes {
		if have == n {
			return
		}
	}
	r.Notes = append(r.Notes, n)
}

// Violations lists every budget that was exceeded and every step that failed.
func (r *Report) Violations() []string {
	var out []string
	for _, s := range r.Steps {
		switch s.Status {
		case "failed":
			out = append(out, fmt.Sprintf("step %q failed", s.Name))
		case "over budget":
			out = append(out, fmt.Sprintf("step %q took %.0f s, budget %.0f s", s.Name, s.Seconds, s.Budget))
		}
	}
	for _, m := range r.Memory {
		if m.Budget > 0 && m.Stack() > m.Budget {
			out = append(out, fmt.Sprintf("memory %q: k3s and pods hold %d MiB, budget %d MiB", m.Label, m.Stack(), m.Budget))
		}
	}
	return out
}

// Total is the sum of all steps.
func (r *Report) Total() float64 {
	var t float64
	for _, s := range r.Steps {
		t += s.Seconds
	}
	return round(t)
}

func round(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

func cell(s string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "|", "\\|")
}

// Markdown renders the report.
func (r *Report) Markdown() string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	p("# Acceptance journey\n\n")
	if v := r.Violations(); len(v) == 0 && len(r.Steps) > 0 {
		p("Result: **passed**, every budget met. Sum of the steps: %.0f s.\n\n", r.Total())
	} else {
		p("Result: **failed**.\n\n")
		for _, x := range v {
			p("- %s\n", x)
		}
		p("\n")
	}
	if !r.Started.IsZero() {
		p("Started %s.\n\n", r.Started.UTC().Format(time.RFC3339))
	}
	for _, k := range sortedKeys(r.Facts) {
		p("- %s: %s\n", k, r.Facts[k])
	}
	p("\n## Steps\n\n| step | seconds | budget | status | note |\n|---|---|---|---|---|\n")
	for _, s := range r.Steps {
		budget := "-"
		if s.Budget > 0 {
			budget = fmt.Sprintf("%.0f", s.Budget)
		}
		p("| %s | %.1f | %s | %s | %s |\n", cell(s.Name), s.Seconds, budget, s.Status, cell(s.Note))
	}
	for _, run := range r.Runs {
		p("\n## `can up` phases: %s\n\n| phase | seconds |\n|---|---|\n", run.Label)
		for _, ph := range run.Phases {
			note := ""
			if ph.Skipped {
				note = " (skipped)"
			}
			p("| %s%s | %.1f |\n", ph.Name, note, ph.Seconds)
		}
	}
	if len(r.Memory) > 0 {
		p("\n## Node memory (MiB)\n\n| moment | k3s + containerd | pods | stack | stack budget | whole machine used / total |\n|---|---|---|---|---|---|\n")
		for _, m := range r.Memory {
			budget := "-"
			if m.Budget > 0 {
				budget = fmt.Sprint(m.Budget)
			}
			p("| %s | %d | %d | %d | %s | %d / %d |\n", cell(m.Label), m.K3s, m.Pods, m.Stack(), budget, m.Used, m.Total)
		}
		for _, m := range r.Memory {
			if m.Detail != "" {
				p("\n<details><summary>containers: %s</summary>\n\n```\n%s\n```\n\n</details>\n", m.Label, strings.TrimSpace(m.Detail))
			}
		}
	}
	list := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		p("\n## %s\n\n", title)
		for _, i := range items {
			p("- %s\n", i)
		}
	}
	list("Observations", r.Notes)
	list("Stand-ins (what this run did not prove)", r.StandIns)
	return b.String()
}

// JSON renders the report.
func (r *Report) JSON() ([]byte, error) { return json.MarshalIndent(r, "", "  ") }

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Redact replaces every secret in s.
func Redact(s string, secrets []string) string {
	for _, v := range secrets {
		if len(v) >= 6 {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	return s
}
