// Package schema validates documents against the embedded JSON Schemas.
package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	jsch "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/schemas"
)

// Kind selects which schema to validate against.
type Kind string

// Document kinds.
const (
	Company Kind = "company"
	Module  Kind = "module"
)

var (
	once     sync.Once
	compiled map[Kind]*jsch.Schema
	initErr  error
)

func compile() {
	c := jsch.NewCompiler()
	c.DefaultDraft(jsch.Draft2020)
	srcs := map[Kind][]byte{Company: schemas.CompanyV1, Module: schemas.ModuleV1}
	urls := map[Kind]string{Company: "company.v1.schema.json", Module: "module.v1.schema.json"}
	for k, b := range srcs {
		doc, err := jsch.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			initErr = fmt.Errorf("parse %s schema: %w", k, err)
			return
		}
		if err := c.AddResource(urls[k], doc); err != nil {
			initErr = err
			return
		}
	}
	compiled = map[Kind]*jsch.Schema{}
	for k, u := range urls {
		s, err := c.Compile(u)
		if err != nil {
			initErr = fmt.Errorf("compile %s schema: %w", k, err)
			return
		}
		compiled[k] = s
	}
}

// Validate checks doc against the schema for kind and returns all problems found.
func Validate(kindOf Kind, doc *config.Doc) []problem.Problem {
	once.Do(compile)
	if initErr != nil {
		return []problem.Problem{{File: doc.File, Rule: "schema.internal", Severity: problem.Error, Message: initErr.Error()}}
	}
	raw, err := json.Marshal(doc.Value)
	if err != nil {
		return []problem.Problem{{File: doc.File, Rule: "parse.yaml", Severity: problem.Error, Message: err.Error()}}
	}
	inst, err := jsch.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return []problem.Problem{{File: doc.File, Rule: "parse.yaml", Severity: problem.Error, Message: err.Error()}}
	}
	verr := compiled[kindOf].Validate(inst)
	if verr == nil {
		return nil
	}
	ve, ok := verr.(*jsch.ValidationError)
	if !ok {
		return []problem.Problem{{File: doc.File, Rule: "schema.internal", Severity: problem.Error, Message: verr.Error()}}
	}
	printer := message.NewPrinter(language.English)
	seen := map[string]bool{}
	var out []problem.Problem
	for _, leaf := range leaves(ve) {
		for _, p := range toProblems(doc, leaf, printer) {
			key := p.Path + "|" + p.Rule
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, p)
		}
	}
	problem.Sort(out)
	return out
}

func leaves(e *jsch.ValidationError) []*jsch.ValidationError {
	if len(e.Causes) == 0 {
		return []*jsch.ValidationError{e}
	}
	var out []*jsch.ValidationError
	for _, c := range e.Causes {
		out = append(out, leaves(c)...)
	}
	return out
}

func pointer(loc []string, extra ...string) string {
	parts := append(append([]string{}, loc...), extra...)
	if len(parts) == 0 {
		return ""
	}
	for i, p := range parts {
		p = strings.ReplaceAll(p, "~", "~0")
		parts[i] = strings.ReplaceAll(p, "/", "~1")
	}
	return "/" + strings.Join(parts, "/")
}

func toProblems(doc *config.Doc, e *jsch.ValidationError, pr *message.Printer) []problem.Problem {
	kw := e.ErrorKind.KeywordPath()
	last := ""
	if len(kw) > 0 {
		last = kw[len(kw)-1]
	}
	msg := e.ErrorKind.LocalizedString(pr)
	mk := func(rule, path, m string, useKey bool) problem.Problem {
		var l, c int
		if useKey {
			l, c = doc.KeyPos(path)
		} else {
			l, c = doc.Pos(path)
		}
		return problem.Problem{File: doc.File, Line: l, Column: c, Path: path, Rule: rule, Severity: problem.Error, Message: m}
	}
	switch k := e.ErrorKind.(type) {
	case *kind.Required:
		var out []problem.Problem
		for _, name := range k.Missing {
			p := pointer(e.InstanceLocation, name)
			pr := mk("schema.required", p, fmt.Sprintf("missing required field %q", name), false)
			pr.Line, pr.Column = doc.Pos(pointer(e.InstanceLocation)) // point at the parent object
			out = append(out, pr)
		}
		return out
	case *kind.AdditionalProperties:
		var out []problem.Problem
		for _, name := range k.Properties {
			out = append(out, mk("schema.unknown-field", pointer(e.InstanceLocation, name), fmt.Sprintf("unknown field %q (check spelling)", name), true))
		}
		return out
	case *kind.PropertyNames:
		return []problem.Problem{mk("schema.pattern", pointer(e.InstanceLocation, k.Property), msg, true)}
	case *kind.DependentRequired:
		return []problem.Problem{mk("schema.dependentRequired", pointer(e.InstanceLocation), msg, false)}
	}
	path := pointer(e.InstanceLocation)
	rule := "schema." + last
	switch last {
	case "const":
		if path == "/schema" {
			return []problem.Problem{mk("schema.version", path, "unsupported schema version: this tool supports schema 1", false)}
		}
	case "minLength", "maxLength":
		rule = "schema.length"
	case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum":
		rule = "schema.range"
	case "uniqueItems":
		rule = "schema.unique"
	}
	return []problem.Problem{mk(rule, path, msg, false)}
}
