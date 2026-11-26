package modsettings

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Change is a mapped value that lets a module do, expose or destroy more than before.
type Change struct {
	Setting string // dotted settings path
	Value   any    // what the module would run with
	Risk    string
}

// String is what an operator approves: it names the value, so that the approval covers that one.
func (c Change) String() string {
	b, err := json.Marshal(c.Value)
	if err != nil || c.Value == nil {
		b = []byte("its default")
	}
	if r := []rune(string(b)); len(r) > 60 {
		b = []byte(string(r[:60]) + "...")
	}
	return fmt.Sprintf("setting %s becomes %s: %s", c.Setting, b, c.Risk)
}

// Widening lists which mapped values of now widen what a module running with was does. A value that
// is in neither is the module's default (Annotation.Unset), so removing a setting counts too.
func (s *Schema) Widening(was, now map[string]any) []Change {
	var out []Change
	for _, p := range s.Properties() {
		a := p.Annotation
		if a == nil || a.Widens == "" {
			continue
		}
		before, after := effective(a, was), effective(a, now)
		if widens(a.Widens, before, after) {
			out = append(out, Change{Setting: p.Name(), Value: after, Risk: a.Risk})
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
		return gains(was, now)
	case widensRemoved:
		return gains(now, was)
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
	v, ok := strings.CutPrefix(how, widensValue)
	return ok && fmt.Sprint(now) == v
}

// gains reports whether next has an entry that have does not.
func gains(have, next any) bool {
	h, _ := have.([]any)
	n, _ := next.([]any)
	return slices.ContainsFunc(n, func(x any) bool {
		return !slices.ContainsFunc(h, func(y any) bool { return same(x, y) })
	})
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
