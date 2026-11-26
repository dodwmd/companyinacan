package reconcile

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// maxNamedHolders bounds how many holders of one role a held upgrade names.
const maxNamedHolders = 20

// holderNotes names, for every module role a held upgrade gives more to, who is assigned that role
// now: they get it the moment the upgrade is approved, so the operator should read the names first.
// The notes are information beside what is approved and not part of the approval code, which stands
// for what the module and its roles may newly do, whoever holds them.
func (r *Reconciler) holderNotes(ctx context.Context, module string, widens []string) ([]string, error) {
	var out, seen []string
	for _, w := range widens {
		rest, ok := strings.CutPrefix(w, "role ")
		role, _, gains := strings.Cut(rest, " gains ")
		if !ok || !gains || slices.Contains(seen, role) {
			continue
		}
		seen = append(seen, role)
		name := module + ":" + role
		holders, total, err := r.db.RoleHolders(ctx, name, maxNamedHolders)
		if err != nil {
			return nil, err
		}
		who := strings.Join(holders, ", ")
		switch {
		case total == 0:
			out = append(out, name+" is assigned to nobody now (owners hold every permission and get what is new)")
		case total > len(holders):
			who += fmt.Sprintf(" and %d more", total-len(holders))
			fallthrough
		default:
			out = append(out, name+" is assigned now to "+who+": they get what is new as soon as this is approved, as owners do")
		}
	}
	return out, nil
}
