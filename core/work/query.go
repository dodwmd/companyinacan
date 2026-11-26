package work

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The search language is a space-separated list of terms, all of which must match:
//
//	status:todo,in_progress   priority:P0,P1   type:bug   assignee:me   assignee:none
//	reporter:x   label:backend   project:ACME   key:ACME-7   parent:ACME-1   sprint:active
//	is:open|resolved|overdue|blocked   due<2026-12-01   created>=-7d   updated>today
//	-status:done   (negation)   login "two words"   (free text in title or description)
//
// Parsing builds SQL from a fixed set of fragments. User input is only ever passed as bound
// arguments, never spliced into the statement, so there is nothing to inject into.
const (
	maxQueryLen    = 500
	maxQueryTerms  = 20
	maxQueryValues = 20
)

// Filter is a parsed query, ready to render as a WHERE clause over `issues i JOIN projects p`.
type Filter struct {
	clauses []string
	args    []any
	scope   []string // project keys the caller may see; empty means all
	text    int      // free-text terms seen
}

const maxTextTerms = 3

var (
	issueKeyRE = regexp.MustCompile(`^([A-Z][A-Z0-9]{1,9})-([1-9][0-9]{0,9})$`)
	relDateRE  = regexp.MustCompile(`^-([0-9]{1,4})([hdw])$`)
)

var plainFields = map[string]string{
	"status":   "i.status",
	"priority": "i.priority",
	"type":     "i.type",
	"project":  "p.key",
}

func tokenize(q string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inQuote, has := false, false
	for _, r := range q {
		switch {
		case r == '"':
			inQuote = !inQuote
			has = true
		case (r == ' ' || r == '\t' || r == '\n') && !inQuote:
			if has {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	if inQuote {
		return nil, errors.New("unterminated quote")
	}
	if has {
		out = append(out, cur.String())
	}
	return out, nil
}

// ParseQuery parses q. now anchors relative dates; me is what `me` means (the caller).
func ParseQuery(q string, now time.Time, me string) (*Filter, error) {
	return ParseQueryScoped(q, now, me, nil)
}

// ParseQueryScoped is ParseQuery for a caller limited to some projects: terms that look at
// other issues (is:blocked) only consider issues in those projects, so they cannot reveal
// what happens outside the scope. The caller still ANDs the scope onto the result rows.
func ParseQueryScoped(q string, now time.Time, me string, scope []string) (*Filter, error) {
	if len(q) > maxQueryLen {
		return nil, fmt.Errorf("query longer than %d characters", maxQueryLen)
	}
	toks, err := tokenize(q)
	if err != nil {
		return nil, err
	}
	if len(toks) > maxQueryTerms {
		return nil, fmt.Errorf("query has more than %d terms", maxQueryTerms)
	}
	f := &Filter{scope: scope}
	for _, tok := range toks {
		if err := f.addTerm(tok, now, me); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// SQL renders the WHERE clause (never empty) and its arguments.
func (f *Filter) SQL() (string, []any) {
	if len(f.clauses) == 0 {
		return "1=1", nil
	}
	return strings.Join(f.clauses, " AND "), f.args
}

// add appends one term. Every clause is wrapped in parentheses, so a clause containing OR can
// never change how the surrounding ANDs (including the caller's project scope) bind.
func (f *Filter) add(neg bool, clause string, args ...any) {
	clause = "(" + clause + ")"
	if neg {
		clause = "NOT " + clause
	}
	f.clauses = append(f.clauses, clause)
	f.args = append(f.args, args...)
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// splitTerm takes one token of tokenize apart. ok is false for free text, and then rest is the text.
func splitTerm(tok string) (neg bool, field, op, rest string, ok bool) {
	if strings.HasPrefix(tok, "-") && len(tok) > 1 && !relDateRE.MatchString(tok) {
		neg, tok = true, tok[1:]
	}
	idx := strings.IndexAny(tok, ":<>=")
	if idx <= 0 {
		return neg, "", "", tok, false
	}
	field, op, rest = strings.ToLower(tok[:idx]), string(tok[idx]), tok[idx+1:]
	if (op == "<" || op == ">") && strings.HasPrefix(rest, "=") {
		op += "="
		rest = rest[1:]
	}
	return neg, field, op, rest, true
}

func (f *Filter) addTerm(tok string, now time.Time, me string) error {
	neg, field, op, rest, ok := splitTerm(tok)
	if !ok {
		tok = rest
		// Free text. A leading "-" on plain text negates it too.
		if f.text++; f.text > maxTextTerms {
			return fmt.Errorf("at most %d free-text terms (use field:value terms to narrow)", maxTextTerms)
		}
		f.add(neg, "(i.title LIKE ? ESCAPE '\\' OR i.description LIKE ? ESCAPE '\\')", "%"+escapeLike(tok)+"%", "%"+escapeLike(tok)+"%")
		return nil
	}
	if rest == "" {
		return fmt.Errorf("%s: missing value", field)
	}
	switch field {
	case "due", "created", "updated", "resolved":
		return f.addDate(neg, field, op, rest, now)
	}
	if op != ":" && op != "=" {
		return fmt.Errorf("%s does not support %q (use %s:value)", field, op, field)
	}
	vals := strings.Split(rest, ",")
	if len(vals) > maxQueryValues {
		return fmt.Errorf("%s: more than %d values", field, maxQueryValues)
	}
	for _, v := range vals {
		if v == "" {
			return fmt.Errorf("%s: empty value", field)
		}
	}
	args := make([]any, len(vals))
	for i, v := range vals {
		args[i] = v
	}
	switch field {
	case "status", "type", "project":
		f.add(neg, plainFields[field]+" IN ("+placeholders(len(vals))+")", args...)
	case "priority":
		for _, v := range vals {
			if !ValidPriority(v) {
				return fmt.Errorf("priority: %q is not P0, P1, P2 or P3", v)
			}
		}
		f.add(neg, "i.priority IN ("+placeholders(len(vals))+")", args...)
	case "assignee", "reporter":
		col := "i." + field
		if len(vals) == 1 && vals[0] == "none" && field == "assignee" {
			f.add(neg, "i.assignee IS NULL")
			return nil
		}
		for i, v := range vals {
			if v == "me" {
				args[i] = me
			}
		}
		f.add(neg, col+" IN ("+placeholders(len(vals))+")", args...)
	case "label":
		if len(vals) == 1 && vals[0] == "none" {
			f.add(neg, "NOT EXISTS (SELECT 1 FROM issue_labels l WHERE l.issue_id = i.id)")
			return nil
		}
		f.add(neg, "EXISTS (SELECT 1 FROM issue_labels l WHERE l.issue_id = i.id AND l.label IN ("+placeholders(len(vals))+"))", args...)
	case "sprint":
		return f.addSprint(neg, vals)
	case "parent":
		if len(vals) == 1 && vals[0] == "none" {
			f.add(neg, "i.parent_id IS NULL")
			return nil
		}
		var parts []string
		var pargs []any
		for _, v := range vals {
			m := issueKeyRE.FindStringSubmatch(v)
			if m == nil {
				return fmt.Errorf("parent: %q is not an issue key like ACME-12", v)
			}
			n, _ := strconv.ParseInt(m[2], 10, 64)
			parts = append(parts, "(SELECT i2.id FROM issues i2 JOIN projects p2 ON p2.id = i2.project_id WHERE p2.key = ? AND i2.number = ?)")
			pargs = append(pargs, m[1], n)
		}
		f.add(neg, "i.parent_id IN ("+strings.Join(parts, ",")+")", pargs...)
	case "key":
		var parts []string
		var pargs []any
		for _, v := range vals {
			m := issueKeyRE.FindStringSubmatch(v)
			if m == nil {
				return fmt.Errorf("key: %q is not an issue key like ACME-12", v)
			}
			n, _ := strconv.ParseInt(m[2], 10, 64)
			parts = append(parts, "(p.key = ? AND i.number = ?)")
			pargs = append(pargs, m[1], n)
		}
		f.add(neg, strings.Join(parts, " OR "), pargs...)
	case "is":
		return f.addIs(neg, vals, now)
	case "text":
		for _, v := range vals {
			if f.text++; f.text > maxTextTerms {
				return fmt.Errorf("at most %d free-text terms (use field:value terms to narrow)", maxTextTerms)
			}
			f.add(neg, "(i.title LIKE ? ESCAPE '\\' OR i.description LIKE ? ESCAPE '\\')", "%"+escapeLike(v)+"%", "%"+escapeLike(v)+"%")
		}
	default:
		return fmt.Errorf("unknown search field %q", field)
	}
	return nil
}

func (f *Filter) addSprint(neg bool, vals []string) error {
	if len(vals) != 1 {
		return errors.New("sprint: give one value (an id, active or none)")
	}
	switch v := vals[0]; v {
	case "none":
		f.add(neg, "i.sprint_id IS NULL")
	case "active":
		f.add(neg, "i.sprint_id IN (SELECT id FROM sprints WHERE state = 'active')")
	default:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			return fmt.Errorf("sprint: %q is not a sprint id, active or none", v)
		}
		f.add(neg, "i.sprint_id = ?", n)
	}
	return nil
}

func (f *Filter) addIs(neg bool, vals []string, now time.Time) error {
	for _, v := range vals {
		switch v {
		case "open":
			f.add(neg, "i.resolved_at IS NULL")
		case "resolved":
			f.add(neg, "i.resolved_at IS NOT NULL")
		case "overdue":
			f.add(neg, "i.resolved_at IS NULL AND i.due_date IS NOT NULL AND i.due_date < ?", now.UTC().Format(time.DateOnly))
		case "blocked":
			clause := "EXISTS (SELECT 1 FROM issue_links k JOIN issues b ON b.id = k.source_id JOIN projects bp ON bp.id = b.project_id WHERE k.type = 'blocks' AND k.target_id = i.id AND b.resolved_at IS NULL"
			var args []any
			if len(f.scope) > 0 {
				clause += " AND bp.key IN (" + placeholders(len(f.scope)) + ")"
				for _, k := range f.scope {
					args = append(args, k)
				}
			}
			f.add(neg, clause+")", args...)
		default:
			return fmt.Errorf("is: %q is not open, resolved, overdue or blocked", v)
		}
	}
	return nil
}

func (f *Filter) addDate(neg bool, field, op, v string, now time.Time) error {
	if op == ":" {
		op = "="
	}
	if op != "=" && op != "<" && op != "<=" && op != ">" && op != ">=" {
		return fmt.Errorf("%s: unsupported operator %q", field, op)
	}
	t, err := parseWhen(v, now)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	if field == "due" {
		col := "i.due_date"
		f.add(neg, col+" "+op+" ?", t.UTC().Format(time.DateOnly))
		return nil
	}
	col := map[string]string{"created": "i.created_at", "updated": "i.updated_at", "resolved": "i.resolved_at"}[field]
	if op == "=" { // a whole day
		f.add(neg, col+" >= ? AND "+col+" < ?", t.Unix(), t.Add(24*time.Hour).Unix())
		return nil
	}
	f.add(neg, col+" "+op+" ?", t.Unix())
	return nil
}

func parseWhen(v string, now time.Time) (time.Time, error) {
	now = now.UTC()
	if v == "today" {
		return now.Truncate(24 * time.Hour), nil
	}
	if m := relDateRE.FindStringSubmatch(v); m != nil {
		n, _ := strconv.Atoi(m[1])
		d := map[string]time.Duration{"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
		return now.Add(-time.Duration(n) * d), nil
	}
	if t, err := time.Parse(time.DateOnly, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%q is not a date (YYYY-MM-DD, RFC 3339, today or -7d)", v)
}
