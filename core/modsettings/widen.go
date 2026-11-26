package modsettings

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Change is a mapped value that lets a module do, expose or destroy more than before.
type Change struct {
	Setting string // dotted settings path
	// Verb is "becomes", or "adds" or "removes" for one entry of a list.
	Verb  string
	Shown any // the new value, or the entry added or removed
	Whole any // the whole value the module would run with
	Risk  string
}

// maxShown is how much of one value a line shows. The entries of lists and the addresses the
// schemas allow are shorter.
const maxShown = 300

// String is what an operator reads and approves. It ends in a digest of the whole value, so that an
// approval covers that value and no other, however long it is.
func (c Change) String() string {
	shown := "its default"
	switch v := c.Shown.(type) {
	case nil:
	case string:
		shown = strconv.QuoteToGraphic(v) // no control or direction characters on a terminal
	default:
		b, _ := json.Marshal(v)
		shown = string(b)
	}
	if r := []rune(shown); len(r) > maxShown {
		shown = fmt.Sprintf("%s... (%d characters)", string(r[:maxShown]), len(r))
	}
	b, _ := json.Marshal(c.Whole)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("setting %s %s %s: %s (sha256 %s)", c.Setting, c.Verb, shown, c.Risk, hex.EncodeToString(sum[:8]))
}

// Widening lists which mapped values of now widen what a module running with was does, one Change
// for each entry a list gains or loses. A value that is in neither is the module's default
// (Annotation.Unset), so removing a setting counts too.
func (s *Schema) Widening(was, now map[string]any) []Change {
	var out []Change
	for _, p := range s.Properties() {
		a := p.Annotation
		if a == nil || a.Widens == "" {
			continue
		}
		before, after := effective(a, was), effective(a, now)
		if !widens(a.Widens, before, after) {
			continue
		}
		c := Change{Setting: p.Name(), Verb: "becomes", Shown: after, Whole: after, Risk: a.Risk}
		var entries []any
		switch a.Widens {
		case widensAdded:
			c.Verb, entries = "adds", missing(before, after)
		case widensRemoved:
			c.Verb, entries = "removes", missing(after, before)
		default:
			out = append(out, c)
		}
		for _, e := range entries {
			c.Shown = e
			out = append(out, c)
		}
	}
	return out
}

// Within is next, except that every value that widens what was does stays as in was (absent there:
// the module's default). A restore installs this: nothing wider than what was installed arrives
// without an approval.
func (s *Schema) Within(was, next map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range next {
		out[k] = v
	}
	restore := func(target string) {
		if v, ok := was[target]; ok {
			out[target] = normal(v)
		} else {
			delete(out, target)
		}
	}
	for _, p := range s.Properties() {
		a := p.Annotation
		if a == nil || a.Widens == "" || !widens(a.Widens, effective(a, was), effective(a, next)) {
			continue
		}
		restore(a.Target())
		for k := range a.AlsoWhenTrue {
			restore("config." + k)
		}
	}
	return out
}

// PreviouslyApplied are those of values that releases before the mapping gave the module too.
func (s *Schema) PreviouslyApplied(values map[string]any) map[string]any {
	out := map[string]any{}
	for _, p := range s.Properties() {
		if a := p.Annotation; a != nil && a.PreviouslyApplied {
			if v, ok := values[a.Target()]; ok {
				out[a.Target()] = v
			}
		}
	}
	return out
}

func effective(a *Annotation, values map[string]any) any {
	if v, ok := values[a.Target()]; ok {
		return normal(v)
	}
	return a.Unset
}

func same(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func widens(how string, was, now any) bool {
	if same(was, now) {
		return false
	}
	switch how {
	case widensTrue:
		return now == true
	case widensFalse:
		return now == false
	case widensChanged:
		return true
	case widensAdded:
		return len(missing(was, now)) > 0
	case widensRemoved:
		return len(missing(now, was)) > 0
	case widensLower, widensHigher, widensKeepLess, widensLimit:
		w, wok := was.(int64)
		n, nok := now.(int64)
		if !wok || !nok {
			return true
		}
		switch how {
		case widensLower:
			return n < w
		case widensHigher:
			return n > w
		case widensKeepLess:
			return n != 0 && (w == 0 || n < w)
		default: // widensLimit
			return w != 0 && (n == 0 || n > w)
		}
	case widensRetention:
		w, wok := retentionDays(was)
		n, nok := retentionDays(now)
		return !wok || !nok || n < w
	}
	if order, ok := strings.CutPrefix(how, widensRises); ok {
		l := strings.Split(order, ",")
		return slices.Index(l, fmt.Sprint(now)) > slices.Index(l, fmt.Sprint(was))
	}
	v, ok := strings.CutPrefix(how, widensValue)
	return ok && fmt.Sprint(now) == v
}

// missing returns the entries of next that have does not have.
func missing(have, next any) []any {
	h, _ := have.([]any)
	n, _ := next.([]any)
	var out []any
	for _, x := range n {
		if !slices.ContainsFunc(h, func(y any) bool { return same(x, y) }) {
			out = append(out, x)
		}
	}
	return out
}

const forever = int(^uint(0) >> 1)

// retentionDays reads 30d, 12m, 7y, 0 or legal as the in-house modules do.
func retentionDays(v any) (int, bool) {
	s, _ := v.(string)
	switch s {
	case "legal":
		return forever, true
	case "0":
		return 0, true
	case "":
		return 0, false
	}
	mult, ok := map[byte]int{'d': 1, 'm': 30, 'y': 365}[s[len(s)-1]]
	n, err := strconv.Atoi(s[:len(s)-1])
	if !ok || err != nil {
		return 0, false
	}
	return n * mult, true
}
