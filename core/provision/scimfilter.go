package provision

import (
	"fmt"
	"strings"
)

// SCIM filter subset (RFC 7644 section 3.4.2.2): `attr eq value` terms joined by `and`. That is
// what identity providers send to find a user ("userName eq ...") and it is all this server
// accepts; every other operator, `or`, `not` and grouping answer 400 invalidFilter. The parser is a
// single left-to-right pass with hard bounds (length, terms, value size), takes only attribute
// names from the caller's allow-list, and never builds a query out of the text: a filter is
// matched in memory against resources, so there is nothing to inject into.
const (
	maxFilterLen   = 512
	maxFilterTerms = 8
	maxFilterValue = 256
)

// scimBoolAttrs are the boolean attributes: they take true or false, not a string.
var scimBoolAttrs = map[string]bool{"active": true}

// SCIMError is an RFC 7644 section 3.12 error.
type SCIMError struct {
	Status int
	Type   string // scimType, empty when none applies
	Detail string
}

func (e *SCIMError) Error() string { return fmt.Sprintf("scim %d %s: %s", e.Status, e.Type, e.Detail) }

func invalidFilter(format string, a ...any) *SCIMError {
	return &SCIMError{Status: 400, Type: "invalidFilter", Detail: fmt.Sprintf(format, a...)}
}

// Term is one `attr eq value` comparison; Attr is lower-case.
type Term struct {
	Attr  string
	Value string
}

// Filter is a conjunction of terms.
type Filter []Term

// ParseFilter parses s. allowed holds the lower-case attribute names the resource supports.
func ParseFilter(s string, allowed map[string]bool) (Filter, error) {
	if len(s) > maxFilterLen {
		return nil, invalidFilter("the filter is longer than %d characters", maxFilterLen)
	}
	p := &fparser{s: s, allowed: allowed}
	var out Filter
	for {
		p.ws()
		t, err := p.term()
		if err != nil {
			return nil, err
		}
		out = append(out, t)
		if len(out) > maxFilterTerms {
			return nil, invalidFilter("at most %d terms are supported", maxFilterTerms)
		}
		p.ws()
		if p.i >= len(p.s) {
			return out, nil
		}
		if !p.keyword("and") {
			return nil, invalidFilter("only the \"and\" operator is supported between terms")
		}
		if !p.ws() {
			return nil, invalidFilter("expected a space after \"and\"")
		}
	}
}

type fparser struct {
	s       string
	i       int
	allowed map[string]bool
}

// ws skips spaces and reports whether any were skipped.
func (p *fparser) ws() bool {
	start := p.i
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
	return p.i > start
}

func (p *fparser) keyword(kw string) bool {
	if p.i+len(kw) > len(p.s) || !strings.EqualFold(p.s[p.i:p.i+len(kw)], kw) {
		return false
	}
	if end := p.i + len(kw); end < len(p.s) && isWordByte(p.s[end]) {
		return false
	}
	p.i += len(kw)
	return true
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
}

func (p *fparser) term() (Term, error) {
	start := p.i
	for p.i < len(p.s) && isWordByte(p.s[p.i]) {
		p.i++
	}
	attr := strings.ToLower(p.s[start:p.i])
	if attr == "" {
		return Term{}, invalidFilter("expected an attribute name")
	}
	if !p.allowed[attr] {
		return Term{}, invalidFilter("filtering on %q is not supported", truncate(attr, 40))
	}
	if !p.ws() || !p.keyword("eq") {
		return Term{}, invalidFilter("only the \"eq\" operator is supported")
	}
	if !p.ws() {
		return Term{}, invalidFilter("expected a value after \"eq\"")
	}
	if scimBoolAttrs[attr] {
		switch {
		case p.keyword("true"):
			return Term{attr, "true"}, nil
		case p.keyword("false"):
			return Term{attr, "false"}, nil
		}
		return Term{}, invalidFilter("%q takes true or false", attr)
	}
	v, err := p.quoted()
	if err != nil {
		return Term{}, err
	}
	return Term{attr, v}, nil
}

func (p *fparser) quoted() (string, error) {
	if p.i >= len(p.s) || p.s[p.i] != '"' {
		return "", invalidFilter("a string value must be in double quotes")
	}
	p.i++
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return b.String(), nil
		case c < 0x20 || c == 0x7f:
			return "", invalidFilter("control characters are not allowed in a value")
		case c == '\\':
			if p.i+1 >= len(p.s) || (p.s[p.i+1] != '"' && p.s[p.i+1] != '\\') {
				return "", invalidFilter("only \\\" and \\\\ escapes are supported")
			}
			b.WriteByte(p.s[p.i+1])
			p.i += 2
		default:
			b.WriteByte(c)
			p.i++
		}
		if b.Len() > maxFilterValue {
			return "", invalidFilter("a value is longer than %d characters", maxFilterValue)
		}
	}
	return "", invalidFilter("unterminated string value")
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// caseExactAttrs compare exactly; everything else compares case-insensitively (RFC 7643).
var caseExactAttrs = map[string]bool{"id": true}

// Match reports whether a resource satisfies every term. get returns the resource's value for a
// lower-case attribute name; a missing attribute never matches.
func (f Filter) Match(get func(attr string) (string, bool)) bool {
	for _, t := range f {
		v, ok := get(t.Attr)
		if !ok {
			return false
		}
		if caseExactAttrs[t.Attr] {
			if v != t.Value {
				return false
			}
		} else if !strings.EqualFold(v, t.Value) {
			return false
		}
	}
	return true
}
