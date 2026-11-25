package jobs

import "strings"

// Visibility scope. Every query that returns jobs or applications to an API caller ANDs one
// of these predicates, parenthesised, onto the whole WHERE clause, so a filter written with OR
// can never widen what a token may see, and a by-id lookup uses the same predicate as a list
// (an object outside the scope is indistinguishable from one that does not exist).
//
//   admin, recruiter  every job, narrowed to the token's own job list when it has one
//   hiring_manager    jobs whose hiring_manager is the caller, narrowed by the token's list
//   interviewer       only applications the caller is assigned to interview, narrowed likewise

// jobPred returns a parenthesised predicate over a jobs table alias.
func (p *Principal) jobPred(j string) (string, []any) {
	var parts []string
	var args []any
	if len(p.Jobs) > 0 {
		parts = append(parts, j+".id IN ("+placeholders(len(p.Jobs))+")")
		for _, id := range p.Jobs {
			args = append(args, id)
		}
	}
	switch p.Role {
	case RoleAdmin, RoleRecruiter:
	case RoleHiringManager:
		parts = append(parts, j+".hiring_manager = ?")
		args = append(args, p.Subject)
	default: // interviewers (and anything unknown) reach jobs only through an assigned application
		parts = append(parts, "0")
	}
	if len(parts) == 0 {
		return "(1)", nil
	}
	return "(" + strings.Join(parts, " AND ") + ")", args
}

// appPred returns a parenthesised predicate over an applications alias a joined to jobs j.
func (p *Principal) appPred(a, j string) (string, []any) {
	var parts []string
	var args []any
	if len(p.Jobs) > 0 {
		parts = append(parts, a+".job_id IN ("+placeholders(len(p.Jobs))+")")
		for _, id := range p.Jobs {
			args = append(args, id)
		}
	}
	switch p.Role {
	case RoleAdmin, RoleRecruiter:
	case RoleHiringManager:
		parts = append(parts, j+".hiring_manager = ?")
		args = append(args, p.Subject)
	case RoleInterviewer:
		parts = append(parts, "EXISTS (SELECT 1 FROM interviewers iv WHERE iv.application_id = "+a+".id AND iv.subject = ?)")
		args = append(args, p.Subject)
	default:
		parts = append(parts, "0")
	}
	if len(parts) == 0 {
		return "(1)", nil
	}
	return "(" + strings.Join(parts, " AND ") + ")", args
}

func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// Field visibility. Candidate fields are masked by role in one place (maskCandidate) and the
// same mask is used for every representation: detail, list, talent pool, activity and events
// (events carry no candidate fields at all).
const (
	fieldEmail     = "email"
	fieldPhone     = "phone"
	fieldAddress   = "address"
	fieldSalaryExp = "salary_expectation"
	fieldNotes     = "notes"
	fieldCover     = "cover_letter"
	fieldRejNote   = "reject_note"
	fieldComments  = "scorecard_comments"
)

// canSee reports whether a role may read a sealed or contact field.
func (p *Principal) canSee(field string) bool {
	switch p.Role {
	case RoleAdmin, RoleRecruiter:
		return true
	case RoleHiringManager:
		return field == fieldEmail || field == fieldCover || field == fieldComments
	}
	return false // interviewers see a name, the stage and the resume file: nothing else
}
