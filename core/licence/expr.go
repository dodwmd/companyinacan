package licence

import (
	"fmt"
	"strings"
)

// node is a parsed SPDX expression: a leaf id, or AND/OR over children.
type node struct {
	op       string // "", "AND", "OR"
	id       string
	children []*node
}

func tokenize(s string) []string {
	s = strings.NewReplacer("(", " ( ", ")", " ) ").Replace(s)
	return strings.Fields(s)
}

type parser struct {
	toks []string
	pos  int
}

func (p *parser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *parser) next() string { t := p.peek(); p.pos++; return t }

func parseExpr(s string) (*node, error) {
	p := &parser{toks: tokenize(s)}
	if len(p.toks) == 0 {
		return nil, fmt.Errorf("empty expression")
	}
	n, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("unexpected %q", p.peek())
	}
	return n, nil
}

func (p *parser) or() (*node, error) {
	return p.binary("OR", p.and)
}

func (p *parser) and() (*node, error) {
	return p.binary("AND", p.factor)
}

func (p *parser) binary(op string, sub func() (*node, error)) (*node, error) {
	first, err := sub()
	if err != nil {
		return nil, err
	}
	n := &node{op: op, children: []*node{first}}
	for p.peek() == op {
		p.next()
		c, err := sub()
		if err != nil {
			return nil, err
		}
		n.children = append(n.children, c)
	}
	if len(n.children) == 1 {
		return first, nil
	}
	return n, nil
}

func (p *parser) factor() (*node, error) {
	t := p.next()
	switch t {
	case "":
		return nil, fmt.Errorf("unexpected end of expression")
	case "(":
		n, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.next() != ")" {
			return nil, fmt.Errorf("missing closing parenthesis")
		}
		return n, nil
	case ")", "AND", "OR", "WITH":
		return nil, fmt.Errorf("unexpected %q", t)
	}
	n := &node{id: strings.TrimSuffix(t, "+")}
	if p.peek() == "WITH" { // licence exceptions do not change acceptance
		p.next()
		if e := p.next(); e == "" || e == "(" || e == ")" {
			return nil, fmt.Errorf("WITH needs an exception id")
		}
	}
	return n, nil
}

func (n *node) leaves() []string {
	if n.op == "" {
		return []string{n.id}
	}
	var out []string
	for _, c := range n.children {
		out = append(out, c.leaves()...)
	}
	return out
}

type state int

const (
	no state = iota
	choice
	yes
)

func (n *node) eval(a Accepted) state {
	switch n.op {
	case "":
		if a.Has(n.id) {
			return yes
		}
		return no
	case "AND":
		res := yes
		for _, c := range n.children {
			switch c.eval(a) {
			case no:
				return no
			case choice:
				res = choice
			}
		}
		return res
	default: // OR
		all, any := true, false
		for _, c := range n.children {
			switch c.eval(a) {
			case yes:
				any = true
			case choice:
				any, all = true, false
			default:
				all = false
			}
		}
		switch {
		case all:
			return yes
		case any:
			return choice
		}
		return no
	}
}
