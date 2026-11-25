package finance

import "strings"

// Scope clauses. Every query that returns records for a caller builds its WHERE from these
// helpers. Each returns a fully parenthesised fragment and is joined to the rest of the
// query with AND, so an OR inside a filter can never widen the scope: the scope applies
// to the whole query, not to one branch of it.

// partyClause limits col to the principal's parties.
func partyClause(a *Principal, col string) (string, []any) {
	if len(a.Parties) == 0 {
		return "(1 = 1)", nil
	}
	args := make([]any, len(a.Parties))
	for i, id := range a.Parties {
		args[i] = id
	}
	return "(" + col + " IN (" + placeholders(len(args)) + "))", args
}

// projectClause limits col to the principal's projects. A record with no project is
// invisible to a project-scoped principal.
func projectClause(a *Principal, col string) (string, []any) {
	if len(a.Projects) == 0 {
		return "(1 = 1)", nil
	}
	args := make([]any, len(a.Projects))
	for i, k := range a.Projects {
		args[i] = k
	}
	return "(" + col + " IN (" + placeholders(len(args)) + "))", args
}

// ownerClause confines own-only principals to records they own.
func ownerClause(a *Principal, col string) (string, []any) {
	if !a.OwnOnly {
		return "(1 = 1)", nil
	}
	return "(" + col + " = ?)", []any{a.Subject}
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// where joins parenthesised clauses with AND.
type where struct {
	parts []string
	args  []any
}

func (w *where) and(clause string, args ...any) {
	w.parts = append(w.parts, clause)
	w.args = append(w.args, args...)
}

func (w *where) scope(c string, a []any) { w.and(c, a...) }

func (w *where) sql() string {
	if len(w.parts) == 0 {
		return "(1 = 1)"
	}
	return strings.Join(w.parts, " AND ")
}
