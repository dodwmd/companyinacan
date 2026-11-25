package work

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
)

// Status categories group workflow statuses for boards, search and "resolved" tracking.
const (
	CategoryTodo   = "todo"
	CategoryActive = "active"
	CategoryDone   = "done"
)

// Status is one node of a project workflow.
type Status struct {
	Name     string `json:"name"`
	Category string `json:"category"`
}

// Workflow is a per-project status graph. Transitions lists, for every status, the
// statuses an issue may move to next; a status that is absent from the map is terminal.
type Workflow struct {
	Statuses    []Status            `json:"statuses"`
	Initial     string              `json:"initial"`
	Transitions map[string][]string `json:"transitions"`
}

var statusNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,29}$`)

const maxStatuses = 30

// DefaultWorkflow is todo, in_progress, in_review, done and cancelled with the usual moves.
func DefaultWorkflow() Workflow {
	return Workflow{
		Statuses: []Status{
			{"todo", CategoryTodo},
			{"in_progress", CategoryActive},
			{"in_review", CategoryActive},
			{"done", CategoryDone},
			{"cancelled", CategoryDone},
		},
		Initial: "todo",
		Transitions: map[string][]string{
			"todo":        {"in_progress", "cancelled"},
			"in_progress": {"todo", "in_review", "done", "cancelled"},
			"in_review":   {"in_progress", "done", "cancelled"},
			"done":        {"in_progress"},
			"cancelled":   {"todo"},
		},
	}
}

// Validate checks that the workflow is internally consistent and can reach a done status.
func (w *Workflow) Validate() error {
	if len(w.Statuses) == 0 || len(w.Statuses) > maxStatuses {
		return fmt.Errorf("workflow needs between 1 and %d statuses", maxStatuses)
	}
	cat := map[string]string{}
	for _, s := range w.Statuses {
		if !statusNameRE.MatchString(s.Name) {
			return fmt.Errorf("status name %q must match %s", s.Name, statusNameRE)
		}
		if s.Category != CategoryTodo && s.Category != CategoryActive && s.Category != CategoryDone {
			return fmt.Errorf("status %q: category must be todo, active or done", s.Name)
		}
		if _, dup := cat[s.Name]; dup {
			return fmt.Errorf("duplicate status %q", s.Name)
		}
		cat[s.Name] = s.Category
	}
	c, ok := cat[w.Initial]
	if !ok {
		return fmt.Errorf("initial status %q is not a status", w.Initial)
	}
	if c == CategoryDone {
		return errors.New("initial status must not be in the done category")
	}
	for from, tos := range w.Transitions {
		if _, ok := cat[from]; !ok {
			return fmt.Errorf("transitions: unknown status %q", from)
		}
		for _, to := range tos {
			if _, ok := cat[to]; !ok {
				return fmt.Errorf("transitions: %q -> unknown status %q", from, to)
			}
			if to == from {
				return fmt.Errorf("transitions: %q cannot move to itself", from)
			}
		}
	}
	// Every status must be able to reach a done status; otherwise issues get stuck.
	reach := map[string]bool{w.Initial: true}
	queue := []string{w.Initial}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, n := range w.Transitions[cur] {
			if !reach[n] {
				reach[n] = true
				queue = append(queue, n)
			}
		}
	}
	for n, ok := range reach {
		if ok && cat[n] == CategoryDone {
			return nil
		}
	}
	return errors.New("no done status is reachable from the initial status")
}

// Allows reports whether an issue may move from one status to another.
func (w *Workflow) Allows(from, to string) bool {
	return slices.Contains(w.Transitions[from], to)
}

// Category returns the category of a status, or "" when unknown.
func (w *Workflow) Category(status string) string {
	for _, s := range w.Statuses {
		if s.Name == status {
			return s.Category
		}
	}
	return ""
}

// Next lists the statuses reachable from one status, in workflow order.
func (w *Workflow) Next(from string) []string {
	return slices.Clone(w.Transitions[from])
}

// Priorities, highest first. P0 drop everything, P1 this sprint, P2 planned, P3 backlog.
var Priorities = []string{"P0", "P1", "P2", "P3"}

// ValidPriority reports whether p is P0..P3.
func ValidPriority(p string) bool { return slices.Contains(Priorities, p) }

// SLATarget is a response and resolution hint in hours. Hints are advice shown to people
// and automation; the tracker does not escalate on them yet.
type SLATarget struct {
	RespondHours int `json:"respond_hours"`
	ResolveHours int `json:"resolve_hours"`
}

// SLA maps each priority to its hint.
type SLA map[string]SLATarget

// DefaultSLA is the starting point; a project can override it.
func DefaultSLA() SLA {
	return SLA{
		"P0": {RespondHours: 1, ResolveHours: 24},
		"P1": {RespondHours: 8, ResolveHours: 24 * 7},
		"P2": {RespondHours: 24 * 3, ResolveHours: 24 * 30},
		"P3": {RespondHours: 24 * 7, ResolveHours: 24 * 90},
	}
}

// Validate requires a sane hint for every priority and no others.
func (s SLA) Validate() error {
	if len(s) != len(Priorities) {
		return errors.New("sla must define exactly P0, P1, P2 and P3")
	}
	for _, p := range Priorities {
		t, ok := s[p]
		if !ok {
			return fmt.Errorf("sla: %s is missing", p)
		}
		if t.RespondHours < 1 || t.ResolveHours < t.RespondHours || t.ResolveHours > 24*365 {
			return fmt.Errorf("sla: %s needs 1 <= respond_hours <= resolve_hours <= 8760", p)
		}
	}
	return nil
}
