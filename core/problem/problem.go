// Package problem defines the validation result type shared by all checks.
package problem

import (
	"fmt"
	"sort"
)

// Severity of a problem.
type Severity string

// Severities.
const (
	Error   Severity = "error"
	Warning Severity = "warning"
)

// Problem is one validation finding. Line and Column are 1-based; 0 means unknown.
type Problem struct {
	File     string   `json:"file"`
	Line     int      `json:"line"`
	Column   int      `json:"column"`
	Path     string   `json:"path"`
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
}

// String renders "file:line:col: path: message [rule]" (position and path omitted when unknown).
func (p Problem) String() string {
	loc := p.File
	if p.Line > 0 {
		loc = fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Column)
	}
	msg := p.Message
	if p.Severity == Warning {
		msg = "warning: " + msg
	}
	if p.Path != "" {
		return fmt.Sprintf("%s: %s: %s [%s]", loc, p.Path, msg, p.Rule)
	}
	return fmt.Sprintf("%s: %s [%s]", loc, msg, p.Rule)
}

// HasErrors reports whether any problem has error severity.
func HasErrors(ps []Problem) bool {
	for _, p := range ps {
		if p.Severity == Error {
			return true
		}
	}
	return false
}

// Sort orders problems by file, line, column, path, rule for deterministic output.
func Sort(ps []Problem) {
	sort.SliceStable(ps, func(i, j int) bool {
		a, b := ps[i], ps[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Column != b.Column {
			return a.Column < b.Column
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Rule < b.Rule
	})
}
