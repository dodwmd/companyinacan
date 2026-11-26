package work

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Project membership (specs/065-work-manager) is a roster of who works on a project. It feeds the
// workload view (a member with nothing to do is shown) and the assignee suggestions. It is not an
// access list: what a caller may see and change is still decided by permissions and token scope.

const maxMembers = 200

// Member is one person on a project.
type Member struct {
	Subject string    `json:"subject"`
	AddedBy string    `json:"added_by"`
	AddedAt time.Time `json:"added_at"`
}

// ListMembers returns the members of a project the caller may see, by identifier.
func (s *Store) ListMembers(ctx context.Context, a *Principal, project string) ([]Member, error) {
	p, err := s.loadProject(ctx, s.db, a, project)
	if err != nil {
		return nil, err
	}
	rs, err := s.db.QueryContext(ctx, `SELECT subject, added_by, added_at FROM project_members WHERE project_id = ? ORDER BY subject`, p.id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	out := []Member{}
	for rs.Next() {
		var m Member
		var at int64
		if err := rs.Scan(&m.Subject, &m.AddedBy, &at); err != nil {
			return nil, err
		}
		m.AddedAt = fromUnix(at)
		out = append(out, m)
	}
	return out, rs.Err()
}

// SetMember adds (on) or removes a project member. It needs work.admin; repeating it is fine. The
// members of an archived project cannot change.
func (s *Store) SetMember(ctx context.Context, a *Principal, project, subject string, on bool) error {
	if !a.Can(PermAdmin) {
		return ErrForbidden
	}
	if !validSubject(subject) {
		return invalid("subject", "must be a non-empty identifier of at most %d characters", maxSubject)
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		p, err := s.writableProject(ctx, tx, a, project)
		if err != nil {
			return err
		}
		var res sql.Result
		action := "project.member_add"
		if on {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_members WHERE project_id = ? AND subject <> ?`, p.id, subject).Scan(&n); err != nil {
				return err
			}
			if n >= maxMembers {
				return fmt.Errorf("%w: a project has at most %d members", ErrConflict, maxMembers)
			}
			res, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO project_members (project_id, subject, added_by, added_at) VALUES (?, ?, ?, ?)`, p.id, subject, a.Subject, ts(s.now()))
		} else {
			action = "project.member_remove"
			res, err = tx.ExecContext(ctx, `DELETE FROM project_members WHERE project_id = ? AND subject = ?`, p.id, subject)
		}
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		return s.audit(ctx, tx, a.Subject, action, p.key, map[string]any{"subject": subject})
	})
}
