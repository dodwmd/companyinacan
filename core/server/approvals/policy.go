package approvals

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/rbac"
)

var (
	nameRe     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	roleNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}(:[a-z][a-z0-9-]{0,39})?$`)
	durationRe = regexp.MustCompile(`^([1-9][0-9]{0,3})([hd])$`)
)

// ParseDuration reads the policy duration syntax: a number of hours (72h) or days (7d).
func ParseDuration(s string) (time.Duration, error) {
	m := durationRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("duration %q: want <n>h or <n>d", s)
	}
	n, _ := strconv.Atoi(m[1])
	if m[2] == "d" {
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.Duration(n) * time.Hour, nil
}

func validApprover(a config.Approver) error {
	set := 0
	if a.Role != "" {
		set++
		if !roleNameRe.MatchString(a.Role) {
			return fmt.Errorf("role %q is not a valid role name", a.Role)
		}
	}
	if a.User != "" {
		set++
		if !rbac.ValidSubject(a.User) {
			return fmt.Errorf("user %q is not a subject (user:<name> or party:<id>)", a.User)
		}
	}
	if a.ManagerOfRequester {
		set++
	}
	if set != 1 {
		return errors.New("an approver sets exactly one of role, user, manager_of_requester")
	}
	return nil
}

func validApprovers(as []config.Approver, min int) error {
	if len(as) < min || len(as) > 10 {
		return fmt.Errorf("want %d to 10 approvers, have %d", min, len(as))
	}
	for _, a := range as {
		if err := validApprover(a); err != nil {
			return err
		}
	}
	return nil
}

// ValidatePolicy checks one policy.
func ValidatePolicy(p config.ApprovalPolicy) error {
	if len(p.Steps) < 1 || len(p.Steps) > 12 {
		return errors.New("a policy has 1 to 12 steps")
	}
	seen := map[string]bool{}
	for i, s := range p.Steps {
		if !nameRe.MatchString(s.Name) || seen[s.Name] {
			return fmt.Errorf("step %d: name %q is invalid or repeated", i, s.Name)
		}
		seen[s.Name] = true
		if i == 0 && s.Parallel {
			return errors.New("the first step cannot be parallel")
		}
		if err := validApprovers(s.Approvers, 1); err != nil {
			return fmt.Errorf("step %q: %w", s.Name, err)
		}
		if err := validApprovers(s.Fallback, 0); err != nil {
			return fmt.Errorf("step %q fallback: %w", s.Name, err)
		}
		if s.Quorum < 0 || s.Quorum > 10 {
			return fmt.Errorf("step %q: quorum must be 1 to 10", s.Name)
		}
		if w := s.When; w != nil {
			if (w.AmountGTE != nil && *w.AmountGTE < 0) || (w.AmountLT != nil && *w.AmountLT < 1) || (w.AmountGTE != nil && w.AmountLT != nil && *w.AmountGTE >= *w.AmountLT) {
				return fmt.Errorf("step %q: empty or negative amount range", s.Name)
			}
		}
	}
	var exp, esc time.Duration
	var err error
	if p.ExpiresAfter != "" {
		if exp, err = ParseDuration(p.ExpiresAfter); err != nil {
			return err
		}
	}
	if p.EscalateAfter != "" {
		if esc, err = ParseDuration(p.EscalateAfter); err != nil {
			return err
		}
		if len(p.EscalateTo) == 0 {
			return errors.New("escalate_after needs escalate_to")
		}
		if exp > 0 && esc >= exp {
			return errors.New("escalate_after must be shorter than expires_after")
		}
	}
	return validApprovers(p.EscalateTo, 0)
}

// SyncPolicies replaces every stored policy with the company.yaml ones (`can apply`). In-flight
// requests keep the policy they were created under.
func (e *Engine) SyncPolicies(ctx context.Context, policies map[string]config.ApprovalPolicy) error {
	types := make([]string, 0, len(policies))
	for t, p := range policies {
		if !nameRe.MatchString(t) {
			return fmt.Errorf("approval type %q is invalid", t)
		}
		if err := ValidatePolicy(p); err != nil {
			return fmt.Errorf("approvals.%s: %w", t, err)
		}
		types = append(types, t)
	}
	sort.Strings(types)
	return e.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM approval_policies`); err != nil {
			return err
		}
		for _, t := range types {
			b, _ := json.Marshal(policies[t])
			if _, err := tx.ExecContext(ctx, `INSERT INTO approval_policies(type, policy_json, updated_at) VALUES (?,?,?)`, t, string(b), e.now().Unix()); err != nil {
				return err
			}
		}
		_, err := e.audit.AppendTx(ctx, tx, audit.Entry{Actor: "can-apply", Action: "approval.policies", Target: "company", Detail: map[string]any{"types": types}})
		return err
	})
}

// nameManager returns p with every `manager_of_requester` approver replaced by the named subject.
// The copy is what the request stores, so later stages staff the same person.
func nameManager(p config.ApprovalPolicy, manager string) config.ApprovalPolicy {
	named := func(in []config.Approver) []config.Approver {
		out := append([]config.Approver(nil), in...)
		for i := range out {
			if out[i].ManagerOfRequester {
				out[i] = config.Approver{User: manager}
			}
		}
		return out
	}
	steps := append([]config.ApprovalStep(nil), p.Steps...)
	for i := range steps {
		steps[i].Approvers, steps[i].Fallback = named(steps[i].Approvers), named(steps[i].Fallback)
	}
	p.Steps, p.EscalateTo = steps, named(p.EscalateTo)
	return p
}

// chainApprover reads one name of a manifest default chain: `manager` or a role.
func chainApprover(name string) config.Approver {
	if name == "manager" {
		return config.Approver{ManagerOfRequester: true}
	}
	return config.Approver{Role: name}
}

// policyFor returns the company policy for type, else a policy built from the module's default chain.
// Each entry of the chain is a step; `a|b|c` is a step for a whose fallback (used when nobody but
// the requester could act for a) is b and c.
func (e *Engine) policyFor(ctx context.Context, typ string, chain []string) (config.ApprovalPolicy, error) {
	var js string
	err := e.db.QueryRowContext(ctx, `SELECT policy_json FROM approval_policies WHERE type=?`, typ).Scan(&js)
	switch {
	case err == nil:
		var p config.ApprovalPolicy
		if err := json.Unmarshal([]byte(js), &p); err != nil {
			return p, err
		}
		return p, nil
	case !errors.Is(err, sql.ErrNoRows):
		return config.ApprovalPolicy{}, err
	}
	if len(chain) == 0 {
		return config.ApprovalPolicy{}, ErrNoPolicy
	}
	p := config.ApprovalPolicy{}
	for i, c := range chain {
		names := strings.Split(c, "|")
		step := config.ApprovalStep{Name: fmt.Sprintf("%s-%d", names[0], i+1), Approvers: []config.Approver{chainApprover(names[0])}}
		for _, f := range names[1:] {
			step.Fallback = append(step.Fallback, chainApprover(f))
		}
		p.Steps = append(p.Steps, step)
	}
	if err := ValidatePolicy(p); err != nil {
		return config.ApprovalPolicy{}, fmt.Errorf("%w: default chain: %v", ErrNoPolicy, err)
	}
	return p, nil
}
