package provision

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// planInput is everything the planner needs; it performs no I/O, so a plan is reproducible and a
// dry run is exactly what a real run would do.
type planInput struct {
	Kind       Kind
	Person     Person
	Urgent     bool
	Privileged bool
	// NotBefore is when access removal may start (a leaver's last day, start of day); zero = now.
	NotBefore time.Time
	// Accounts maps connector -> external id for the person's active accounts.
	Accounts map[string]string
	// Applied maps connector -> groups the person was added to by this module.
	Applied map[string][]string
	// Groups are the person's SCIM group names.
	Groups []string
}

// privilegedGroup reports whether a group name is on the configured privileged list.
func (p Policy) privilegedGroup(name string) bool {
	for _, g := range p.PrivilegedGroups {
		if strings.EqualFold(g, name) {
			return true
		}
	}
	return false
}

func (p Policy) anyPrivileged(groups []string) bool {
	return slices.ContainsFunc(groups, p.privilegedGroup)
}

// desiredGroups is what a connector should hold for the person: its default groups plus, when it
// syncs groups, the person's SCIM groups.
func desiredGroups(cc ConnConfig, groups []string) []string {
	set := map[string]bool{}
	for _, g := range cc.DefaultGroups {
		set[g] = true
	}
	if cc.syncGroups() {
		for _, g := range groups {
			set[g] = true
		}
	}
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// approvalType is the can-core approval type for a sensitive step of a run kind.
func approvalType(k Kind) string {
	if k == KindLeaver {
		return "privileged-leaver"
	}
	return "privileged-access"
}

// plan lists the ordered steps of a workflow. Order matters: a leaver loses can-core access first,
// then group memberships, then the account; the optional task step is last so that a failing task
// can never hold up access removal.
func (p Policy) plan(conns *ConnectorSet, in planInput) ([]Step, error) {
	var out []Step
	add := func(s Step) {
		s.Ord = len(out) + 1
		s.State = StepPending
		out = append(out, s)
	}
	var deletes []Step
	nb := int64(0)
	if !in.NotBefore.IsZero() && !in.Urgent {
		nb = in.NotBefore.Unix()
	}
	for _, name := range conns.Names() {
		cc := p.Conn[name]
		desired := desiredGroups(cc, in.Groups)
		ext, hasAcct := in.Accounts[name]
		switch in.Kind {
		case KindJoiner:
			comp := opDisableAccount
			if p.CompensateWithDelete {
				comp = opDeleteAccount
			}
			add(Step{Op: opCreateAccount, Connector: name, Sensitive: cc.Privileged, CompOp: comp})
			for _, g := range desired {
				add(Step{Op: opAddGroup, Connector: name, Arg: g, Sensitive: p.privilegedGroup(g), CompOp: opRemoveGroup})
			}
		case KindMover:
			if !hasAcct {
				continue // the module manages only accounts it created or was told about
			}
			add(Step{Op: opUpdateAccount, Connector: name})
			applied := in.Applied[name]
			for _, g := range desired {
				if !slices.Contains(applied, g) {
					add(Step{Op: opAddGroup, Connector: name, Arg: g, Sensitive: p.privilegedGroup(g), CompOp: opRemoveGroup})
				}
			}
			for _, g := range applied {
				if !slices.Contains(desired, g) {
					add(Step{Op: opRemoveGroup, Connector: name, Arg: g, CompOp: opAddGroup})
				}
			}
		case KindLeaver:
			if !hasAcct {
				continue
			}
			_ = ext
			for _, g := range in.Applied[name] {
				add(Step{Op: opRemoveGroup, Connector: name, Arg: g, NotBefore: ts(nb), notBefore: nb})
			}
			// Access REMOVAL is never gated: a pending or refused approval must never leave a
			// leaver's access enabled. Only the irreversible step (deleting the account) waits
			// for a second person, and only for privileged accounts; it is planned last so it
			// can never hold up a disable or the can-core kill switch.
			add(Step{Op: opDisableAccount, Connector: name, NotBefore: ts(nb), notBefore: nb})
			if p.DeleteAfterDays > 0 {
				base := in.NotBefore
				if base.IsZero() {
					base = time.Now()
				}
				at := base.AddDate(0, 0, p.DeleteAfterDays).Unix()
				deletes = append(deletes, Step{Op: opDeleteAccount, Connector: name, Sensitive: cc.Privileged || in.Privileged, NotBefore: ts(at), notBefore: at})
			}
		}
	}
	if in.Kind == KindLeaver {
		// The can-core kill switch comes after the account steps: can-core refuses it until the
		// people module has reported the person offboarded, which can be a minute after the last
		// day starts, and a step that has to wait must never hold up the other accounts.
		add(Step{Op: opRevokeCore, Connector: connCore, NotBefore: ts(nb), notBefore: nb})
	}
	tpl := p.JoinerTemplate
	if in.Kind == KindLeaver {
		tpl = p.LeaverTemplate
	}
	if tpl != "" && in.Kind != KindMover {
		add(Step{Op: opCreateTasks, Connector: connTasks, Arg: tpl, Optional: true})
	}
	for _, d := range deletes {
		add(d)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: nothing to do for this person (no connectors with an account)", ErrConflict)
	}
	if len(out) > p.MaxStepsPerRun {
		return nil, fmt.Errorf("%w: the plan has %d steps, more than the limit of %d", ErrInvalid, len(out), p.MaxStepsPerRun)
	}
	return out, nil
}
