// Package config loads YAML files while remembering source positions.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/problem"
)

// Doc is a parsed YAML document with position lookup.
type Doc struct {
	File  string
	Root  *yaml.Node
	Value any // decoded generic value (maps use string keys)
}

var lineRe = regexp.MustCompile(`line (\d+)`)

// Load reads and parses a file.
func Load(file string) (*Doc, []problem.Problem) {
	data, err := os.ReadFile(file) //nolint:gosec // path comes from the user's own command line
	if err != nil {
		return nil, []problem.Problem{{File: file, Rule: "parse.read", Severity: problem.Error, Message: err.Error()}}
	}
	return LoadBytes(file, data)
}

// LoadBytes parses YAML bytes. It returns exactly one parse.yaml problem on failure.
func LoadBytes(file string, data []byte) (*Doc, []problem.Problem) {
	fail := func(line int, msg string) (*Doc, []problem.Problem) {
		col := 0
		if line > 0 {
			col = 1
		}
		return nil, []problem.Problem{{File: file, Line: line, Column: col, Rule: "parse.yaml", Severity: problem.Error, Message: msg}}
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var node yaml.Node
	if err := dec.Decode(&node); err != nil {
		if errors.Is(err, io.EOF) {
			return fail(1, "file is empty")
		}
		line := 0
		if m := lineRe.FindStringSubmatch(err.Error()); m != nil {
			line, _ = strconv.Atoi(m[1])
		}
		return fail(line, strings.TrimPrefix(err.Error(), "yaml: "))
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return fail(extra.Line, "multiple YAML documents in one file are not supported")
	}
	if len(node.Content) == 0 {
		return fail(1, "file is empty")
	}
	root := node.Content[0]
	if root.Kind != yaml.MappingNode {
		return fail(root.Line, "top level must be a mapping")
	}
	if p := checkKeys(root); p != "" {
		return fail(0, p)
	}
	var v any
	if err := root.Decode(&v); err != nil {
		return fail(root.Line, strings.TrimPrefix(err.Error(), "yaml: "))
	}
	return &Doc{File: file, Root: root, Value: v}, nil
}

// checkKeys rejects non-string mapping keys anywhere in the tree.
func checkKeys(n *yaml.Node) string {
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode || k.Tag != "!!str" {
				return fmt.Sprintf("line %d: mapping keys must be strings", k.Line)
			}
			if s := checkKeys(n.Content[i+1]); s != "" {
				return s
			}
		}
		return ""
	}
	for _, c := range n.Content {
		if s := checkKeys(c); s != "" {
			return s
		}
	}
	return ""
}

// Pos returns the line and column of the value at a JSON-pointer-like path.
// If the path does not exist, the nearest existing ancestor is used.
func (d *Doc) Pos(path string) (line, col int) {
	n := d.lookup(path, false)
	return n.Line, n.Column
}

// KeyPos is like Pos but points at the key of the last path segment when it
// is a mapping entry (useful for unknown-field errors).
func (d *Doc) KeyPos(path string) (line, col int) {
	n := d.lookup(path, true)
	return n.Line, n.Column
}

func (d *Doc) lookup(path string, wantKey bool) *yaml.Node {
	cur := d.Root
	segs := splitPath(path)
	for i, seg := range segs {
		var next, key *yaml.Node
		switch cur.Kind {
		case yaml.MappingNode:
			for j := 0; j+1 < len(cur.Content); j += 2 {
				if cur.Content[j].Value == seg {
					key, next = cur.Content[j], cur.Content[j+1]
					break
				}
			}
		case yaml.SequenceNode:
			if idx, err := strconv.Atoi(seg); err == nil && idx >= 0 && idx < len(cur.Content) {
				next = cur.Content[idx]
			}
		}
		if next == nil {
			return cur
		}
		if wantKey && key != nil && i == len(segs)-1 {
			return key
		}
		cur = next
	}
	return cur
}

func splitPath(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		s = strings.ReplaceAll(s, "~1", "/")
		segs[i] = strings.ReplaceAll(s, "~0", "~")
	}
	return segs
}
