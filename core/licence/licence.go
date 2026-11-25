package licence

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/problem"
)

// Result is the licence verdict for one module.
type Result struct {
	Problems        []problem.Problem
	SourceAvailable bool
	Skipped         bool // in-house modules are covered by the repository licence
}

// Evaluate applies the accepted-licence rules (SPEC.md §5) to one manifest.
func Evaluate(m *config.Module, acc Accepted, file string) Result {
	var res Result
	fail := func(rule, msg string) Result {
		res.Problems = append(res.Problems, problem.Problem{
			File: file, Path: "/provider/licence", Rule: rule, Severity: problem.Error,
			Message: fmt.Sprintf("module %q: %s", m.ID, msg),
		})
		return res
	}
	if m.Provider.Type == "inhouse" {
		res.Skipped = true
		return res
	}
	if strings.TrimSpace(m.Provider.Licence) == "" {
		return fail("licence.missing", "the upstream licence is missing (set provider.licence to an SPDX expression)")
	}
	if m.Provider.LicenceClass == "source-available" {
		res.SourceAvailable = true
		return res
	}
	expr, err := parseExpr(m.Provider.Licence)
	if err != nil {
		return fail("licence.invalid-expression", fmt.Sprintf("cannot parse licence %q: %v", m.Provider.Licence, err))
	}
	switch expr.eval(acc) {
	case yes:
		return res
	case no:
		return fail("licence.not-accepted", fmt.Sprintf("licence %q is not on the accepted list; mark the module licence_class: source-available to opt in", m.Provider.Licence))
	}
	// choice: some alternatives are accepted, the operator must elect one
	sel := m.Provider.LicenceSelected
	inExpr := false
	for _, l := range expr.leaves() {
		if strings.EqualFold(l, sel) {
			inExpr = true
		}
	}
	switch {
	case sel == "" || !inExpr:
		return fail("licence.expression-ambiguous", fmt.Sprintf("licence %q offers alternatives; set provider.licence_selected to one accepted alternative", m.Provider.Licence))
	case !acc.Has(sel):
		return fail("licence.not-accepted", fmt.Sprintf("selected licence %q is not on the accepted list", sel))
	}
	return res
}

// Entry is one third-party module with its verdict.
type Entry struct {
	Module *config.Module
	Result Result
}

// Collect evaluates every module under dir (skipping "_" example dirs and in-house modules).
func Collect(dir string, acc Accepted) ([]Entry, []problem.Problem) {
	dirs, err := os.ReadDir(dir)
	if err != nil {
		return nil, []problem.Problem{{File: dir, Rule: "licence.read", Severity: problem.Error, Message: err.Error()}}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name() < dirs[j].Name() })
	var entries []Entry
	var ps []problem.Problem
	for _, d := range dirs {
		if !d.IsDir() || strings.HasPrefix(d.Name(), "_") || strings.HasPrefix(d.Name(), ".") {
			continue
		}
		file := filepath.Join(dir, d.Name(), "module.yaml")
		if _, err := os.Stat(file); err != nil {
			continue
		}
		doc, lp := config.Load(file)
		if doc == nil {
			ps = append(ps, lp...)
			continue
		}
		m, err := config.DecodeModule(doc)
		if err != nil {
			ps = append(ps, problem.Problem{File: file, Rule: "parse.yaml", Severity: problem.Error, Message: err.Error()})
			continue
		}
		res := Evaluate(m, acc, file)
		for i := range res.Problems {
			res.Problems[i].Line, res.Problems[i].Column = doc.Pos(res.Problems[i].Path)
		}
		ps = append(ps, res.Problems...)
		if !res.Skipped {
			entries = append(entries, Entry{Module: m, Result: res})
		}
	}
	return entries, ps
}
