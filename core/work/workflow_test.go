package work

import (
	"strings"
	"testing"
)

func TestDefaultWorkflowIsValid(t *testing.T) {
	w := DefaultWorkflow()
	if err := w.Validate(); err != nil {
		t.Fatal(err)
	}
	if w.Initial != "todo" {
		t.Fatalf("initial = %q", w.Initial)
	}
	for _, tc := range []struct {
		from, to string
		ok       bool
	}{
		{"todo", "in_progress", true},
		{"in_progress", "in_review", true},
		{"in_review", "done", true},
		{"todo", "done", false},
		{"done", "in_progress", true},
		{"done", "todo", false},
		{"todo", "todo", false},
	} {
		if got := w.Allows(tc.from, tc.to); got != tc.ok {
			t.Errorf("Allows(%s,%s) = %v, want %v", tc.from, tc.to, got, tc.ok)
		}
	}
	if c := w.Category("cancelled"); c != CategoryDone {
		t.Errorf("cancelled category = %q", c)
	}
}

func TestWorkflowValidateRejects(t *testing.T) {
	good := func() Workflow { return DefaultWorkflow() }
	cases := map[string]func(*Workflow){
		"no statuses":      func(w *Workflow) { w.Statuses = nil },
		"duplicate status": func(w *Workflow) { w.Statuses = append(w.Statuses, w.Statuses[0]) },
		"bad name":         func(w *Workflow) { w.Statuses[0].Name = "To Do!" },
		"bad category":     func(w *Workflow) { w.Statuses[0].Category = "weird" },
		"unknown initial":  func(w *Workflow) { w.Initial = "nope" },
		"done initial":     func(w *Workflow) { w.Initial = "done" },
		"unknown from":     func(w *Workflow) { w.Transitions["ghost"] = []string{"todo"} },
		"unknown to":       func(w *Workflow) { w.Transitions["todo"] = []string{"ghost"} },
		"no done status": func(w *Workflow) {
			w.Statuses = w.Statuses[:3]
			w.Transitions = map[string][]string{"todo": {"in_progress"}}
		},
		"too many statuses":    func(w *Workflow) { w.Statuses = manyStatuses(40) },
		"unreachable done":     func(w *Workflow) { w.Transitions = map[string][]string{"todo": {"in_progress"}} },
		"transition to itself": func(w *Workflow) { w.Transitions["todo"] = []string{"todo"} },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			w := good()
			mut(&w)
			if err := w.Validate(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func manyStatuses(n int) []Status {
	var out []Status
	for i := 0; i < n; i++ {
		out = append(out, Status{Name: "s" + strings.Repeat("a", i), Category: CategoryActive})
	}
	return out
}

func TestSLAHints(t *testing.T) {
	s := DefaultSLA()
	if s["P0"].RespondHours >= s["P1"].RespondHours || s["P1"].ResolveHours >= s["P3"].ResolveHours {
		t.Fatalf("SLA hints must tighten with priority: %+v", s)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	delete(s, "P2")
	if err := s.Validate(); err == nil {
		t.Fatal("missing priority must be rejected")
	}
}
