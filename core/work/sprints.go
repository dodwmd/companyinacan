package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const sprintSelect = `SELECT s.id, p.key, s.name, s.goal, s.state, s.starts_on, s.ends_on, s.created_at, s.updated_at FROM sprints s JOIN projects p ON p.id = s.project_id`

func scanSprint(sc scanner) (*Sprint, error) {
	var sp Sprint
	var starts, ends sql.NullString
	var c, u int64
	if err := sc.Scan(&sp.ID, &sp.Project, &sp.Name, &sp.Goal, &sp.State, &starts, &ends, &c, &u); err != nil {
		return nil, err
	}
	sp.StartsOn, sp.EndsOn = nullStr(starts), nullStr(ends)
	sp.CreatedAt, sp.UpdatedAt = fromUnix(c), fromUnix(u)
	return &sp, nil
}

// NewSprint is the body of a sprint create.
type NewSprint struct {
	Name     string  `json:"name"`
	Goal     string  `json:"goal"`
	StartsOn *string `json:"starts_on"`
	EndsOn   *string `json:"ends_on"`
}

// SprintPatch is the body of a sprint update. Closed sprints are read-only.
type SprintPatch struct {
	Name     *string     `json:"name"`
	Goal     *string     `json:"goal"`
	StartsOn Opt[string] `json:"starts_on"`
	EndsOn   Opt[string] `json:"ends_on"`
}

func validSprintDates(starts, ends *string, p *problems) {
	if starts != nil && !validDate(*starts) {
		p.add("starts_on", "must be YYYY-MM-DD")
	}
	if ends != nil && !validDate(*ends) {
		p.add("ends_on", "must be YYYY-MM-DD")
	}
	if starts != nil && ends != nil && validDate(*starts) && validDate(*ends) && *ends < *starts {
		p.add("ends_on", "must not be before starts_on")
	}
}

func validSprintText(name, goal string, p *problems) {
	if strings.TrimSpace(name) == "" || !validText(name, 100) || strings.ContainsAny(name, "\r\n") {
		p.add("name", "required, one line, at most 100 characters")
	}
	if !validText(goal, 1000) {
		p.add("goal", "at most 1000 characters")
	}
}

// CreateSprint plans a sprint in a project (state planned).
func (s *Store) CreateSprint(ctx context.Context, a *Principal, project string, in NewSprint) (*Sprint, error) {
	var p problems
	validSprintText(in.Name, in.Goal, &p)
	validSprintDates(in.StartsOn, in.EndsOn, &p)
	if err := p.err(); err != nil {
		return nil, err
	}
	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		proj, err := s.writableProject(ctx, tx, a, project)
		if err != nil {
			return err
		}
		now := ts(s.now())
		res, err := tx.ExecContext(ctx, `INSERT INTO sprints (project_id, name, goal, starts_on, ends_on, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			proj.id, in.Name, in.Goal, in.StartsOn, in.EndsOn, now, now)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetSprint(ctx, a, id)
}

// GetSprint returns a sprint in a project the caller may see.
func (s *Store) GetSprint(ctx context.Context, a *Principal, id int64) (*Sprint, error) {
	sp, err := scanSprint(s.db.QueryRowContext(ctx, sprintSelect+` WHERE s.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !a.CanProject(sp.Project)) {
		return nil, ErrNotFound
	}
	return sp, err
}

// ListSprints lists a project's sprints, newest first.
func (s *Store) ListSprints(ctx context.Context, a *Principal, project string) ([]Sprint, error) {
	if _, err := s.loadProject(ctx, s.db, a, project); err != nil {
		return nil, err
	}
	rs, err := s.db.QueryContext(ctx, sprintSelect+` WHERE p.key = ? ORDER BY s.id DESC`, project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	out := []Sprint{}
	for rs.Next() {
		sp, err := scanSprint(rs)
		if err != nil {
			return nil, err
		}
		out = append(out, *sp)
	}
	return out, rs.Err()
}

func (s *Store) sprintForWrite(ctx context.Context, tx dbq, a *Principal, id int64) (*Sprint, error) {
	sp, err := scanSprint(tx.QueryRowContext(ctx, sprintSelect+` WHERE s.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !a.CanProject(sp.Project)) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := s.writableProject(ctx, tx, a, sp.Project); err != nil {
		return nil, err
	}
	return sp, nil
}

// UpdateSprint edits a planned or active sprint.
func (s *Store) UpdateSprint(ctx context.Context, a *Principal, id int64, in SprintPatch) (*Sprint, error) {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		sp, err := s.sprintForWrite(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if sp.State == "closed" {
			return fmt.Errorf("%w: sprint %d is closed", ErrConflict, id)
		}
		if in.Name != nil {
			sp.Name = *in.Name
		}
		if in.Goal != nil {
			sp.Goal = *in.Goal
		}
		if in.StartsOn.Set {
			sp.StartsOn = nil
			if !in.StartsOn.Null {
				sp.StartsOn = &in.StartsOn.V
			}
		}
		if in.EndsOn.Set {
			sp.EndsOn = nil
			if !in.EndsOn.Null {
				sp.EndsOn = &in.EndsOn.V
			}
		}
		var p problems
		validSprintText(sp.Name, sp.Goal, &p)
		validSprintDates(sp.StartsOn, sp.EndsOn, &p)
		if err := p.err(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE sprints SET name = ?, goal = ?, starts_on = ?, ends_on = ?, updated_at = ? WHERE id = ?`, sp.Name, sp.Goal, sp.StartsOn, sp.EndsOn, ts(s.now()), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetSprint(ctx, a, id)
}

// StartSprint makes a planned sprint the project's active sprint.
func (s *Store) StartSprint(ctx context.Context, a *Principal, id int64) (*Sprint, error) {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		sp, err := s.sprintForWrite(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if sp.State != "planned" {
			return fmt.Errorf("%w: sprint %d is %s, only a planned sprint can start", ErrConflict, id, sp.State)
		}
		_, err = tx.ExecContext(ctx, `UPDATE sprints SET state = 'active', updated_at = ? WHERE id = ?`, ts(s.now()), id)
		if err != nil && strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("%w: project %s already has an active sprint", ErrConflict, sp.Project)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetSprint(ctx, a, id)
}

// CompleteSprint closes an active sprint. Unresolved issues move to moveTo (a planned
// sprint of the same project) or back to the backlog when moveTo is nil.
func (s *Store) CompleteSprint(ctx context.Context, a *Principal, id int64, moveTo *int64) (*Sprint, error) {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		sp, err := s.sprintForWrite(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if sp.State != "active" {
			return fmt.Errorf("%w: sprint %d is %s, only an active sprint can be completed", ErrConflict, id, sp.State)
		}
		var target any
		if moveTo != nil {
			dst, err := s.sprintForWrite(ctx, tx, a, *moveTo)
			if err != nil || dst.Project != sp.Project || dst.State != "planned" {
				return invalid("move_to", "must be a planned sprint of project %s", sp.Project)
			}
			target = *moveTo
		}
		now := ts(s.now())
		// Moved issues get the same trail as any other sprint change: a new version, an
		// activity entry and an updated event.
		rs, err := tx.QueryContext(ctx, `SELECT id FROM issues WHERE sprint_id = ? AND resolved_at IS NULL ORDER BY id`, id)
		if err != nil {
			return err
		}
		var moved []int64
		for rs.Next() {
			var iid int64
			if err := rs.Scan(&iid); err != nil {
				_ = rs.Close()
				return err
			}
			moved = append(moved, iid)
		}
		if err := rs.Close(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE issues SET sprint_id = ?, version = version + 1, updated_at = ? WHERE sprint_id = ? AND resolved_at IS NULL`, target, now, id); err != nil {
			return err
		}
		var to *int64
		if moveTo != nil {
			to = moveTo
		}
		from := id
		for _, iid := range moved {
			changes := map[string]Change{"sprint_id": {From: from, To: to}}
			if err := s.addActivity(ctx, tx, iid, a.Subject, "updated", changes); err != nil {
				return err
			}
			is, err := s.loadIssueByID(ctx, tx, iid)
			if err != nil {
				return err
			}
			if err := s.emit(ctx, tx, EventIssueUpdated, issueUpdatedData{IssueKey: is.Key, Project: is.Project, Version: is.Version, Actor: a.Subject, Changes: changes, UpdatedAt: is.UpdatedAt}); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE sprints SET state = 'closed', updated_at = ? WHERE id = ?`, now, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetSprint(ctx, a, id)
}

// ---- board and rank -------------------------------------------------------------------

// BoardColumn is one workflow status on a board.
type BoardColumn struct {
	Status   string  `json:"status"`
	Category string  `json:"category"`
	Count    int     `json:"count"`
	Issues   []Issue `json:"issues"`
}

// Board is the board view data for a project: one column per workflow status, issues in
// rank order. Selection is "active" (the active sprint), "backlog" (no sprint), "all" or a
// sprint id.
type Board struct {
	Project   string        `json:"project"`
	Selection string        `json:"selection"`
	Sprint    *Sprint       `json:"sprint"`
	Columns   []BoardColumn `json:"columns"`
}

const boardColumnLimit = 200

// GetBoard returns the board for a project.
func (s *Store) GetBoard(ctx context.Context, a *Principal, project, selection string) (*Board, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	proj, err := s.loadProject(ctx, s.rdb, a, project)
	if err != nil {
		return nil, err
	}
	if selection == "" {
		selection = "active"
	}
	b := &Board{Project: project, Selection: selection}
	var where string
	var args []any
	switch selection {
	case "all":
		where = "1=1"
	case "backlog":
		where = "i.sprint_id IS NULL"
	case "active":
		err := s.rdb.QueryRowContext(ctx, `SELECT id FROM sprints WHERE project_id = ? AND state = 'active'`, proj.id).Scan(new(int64))
		if errors.Is(err, sql.ErrNoRows) {
			where = "0=1" // no active sprint: empty columns, not an error
		} else if err != nil {
			return nil, err
		} else {
			where = "i.sprint_id IN (SELECT id FROM sprints WHERE project_id = ? AND state = 'active')"
			args = append(args, proj.id)
		}
	default:
		n, err := strconv.ParseInt(selection, 10, 64)
		if err != nil || n < 1 {
			return nil, invalid("sprint", "must be active, backlog, all or a sprint id")
		}
		sp, err := s.GetSprint(ctx, a, n)
		if err != nil || sp.Project != project {
			return nil, ErrNotFound
		}
		b.Sprint = sp
		where = "i.sprint_id = ?"
		args = append(args, n)
	}
	if selection == "active" {
		rs, err := s.rdb.QueryContext(ctx, sprintSelect+` WHERE s.project_id = ? AND s.state = 'active'`, proj.id)
		if err != nil {
			return nil, err
		}
		if rs.Next() {
			b.Sprint, err = scanSprint(rs)
		}
		_ = rs.Close()
		if err != nil {
			return nil, err
		}
	}
	for _, st := range proj.wf.Statuses {
		b.Columns = append(b.Columns, BoardColumn{Status: st.Name, Category: st.Category, Issues: []Issue{}})
	}
	rs, err := s.rdb.QueryContext(ctx, issueSelect+` WHERE i.project_id = ? AND `+where+` ORDER BY i.rank, i.id`, append([]any{proj.id}, args...)...)
	if err != nil {
		return nil, err
	}
	var rows []*issueRow
	for rs.Next() {
		r, err := scanIssue(rs)
		if err != nil {
			_ = rs.Close()
			return nil, err
		}
		rows = append(rows, r)
	}
	if err := rs.Close(); err != nil {
		return nil, err
	}
	if err := s.fillLabels(ctx, s.rdb, rows); err != nil {
		return nil, err
	}
	for _, r := range rows {
		for i := range b.Columns {
			if b.Columns[i].Status == r.Status {
				b.Columns[i].Count++
				if len(b.Columns[i].Issues) < boardColumnLimit {
					b.Columns[i].Issues = append(b.Columns[i].Issues, r.Issue)
				}
			}
		}
	}
	return b, nil
}

// RankRequest places an issue relative to another, or at the top or bottom.
type RankRequest struct {
	Before   string `json:"before"`
	After    string `json:"after"`
	Position string `json:"position"` // first or last
}

// Rank moves an issue within its project's ordering by writing one fractional rank.
func (s *Store) Rank(ctx context.Context, a *Principal, key string, req RankRequest) (*Issue, error) {
	n := 0
	for _, v := range []string{req.Before, req.After, req.Position} {
		if v != "" {
			n++
		}
	}
	if n != 1 || (req.Position != "" && req.Position != "first" && req.Position != "last") {
		return nil, invalid("rank", "give exactly one of before, after or position (first|last)")
	}
	var out *Issue
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		row, err := s.writeRow(ctx, tx, a, key)
		if err != nil {
			return err
		}
		neighbour := func(k string) (string, error) {
			pk, num, ok := splitKey(k)
			if !ok || pk != row.Project {
				return "", invalid("rank", "%q must be an issue key in project %s", k, row.Project)
			}
			var r string
			err := tx.QueryRowContext(ctx, `SELECT rank FROM issues WHERE project_id = ? AND number = ? AND id <> ?`, row.projectID, num, row.id).Scan(&r)
			if errors.Is(err, sql.ErrNoRows) {
				return "", invalid("rank", "%s does not exist", k)
			}
			return r, err
		}
		var lo, hi string
		switch {
		case req.Position == "first":
			err = tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(rank), '') FROM issues WHERE project_id = ? AND id <> ?`, row.projectID, row.id).Scan(&hi)
		case req.Position == "last":
			err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(rank), '') FROM issues WHERE project_id = ? AND id <> ?`, row.projectID, row.id).Scan(&lo)
		case req.Before != "":
			if hi, err = neighbour(req.Before); err == nil {
				err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(rank), '') FROM issues WHERE project_id = ? AND id <> ? AND rank < ?`, row.projectID, row.id, hi).Scan(&lo)
			}
		default:
			if lo, err = neighbour(req.After); err == nil {
				err = tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(rank), '') FROM issues WHERE project_id = ? AND id <> ? AND rank > ?`, row.projectID, row.id, lo).Scan(&hi)
			}
		}
		if err != nil {
			return err
		}
		// Reordering is not an edit of the issue: version and updated_at stay put (an ETag held
		// by a client is still good), but the move is recorded in the activity log.
		newRank := rankBetween(lo, hi)
		if _, err := tx.ExecContext(ctx, `UPDATE issues SET rank = ? WHERE id = ?`, newRank, row.id); err != nil {
			return err
		}
		if err := s.maybeRebalance(ctx, tx, row.projectID, newRank); err != nil {
			return err
		}
		if err := s.addActivity(ctx, tx, row.id, a.Subject, "ranked", req); err != nil {
			return err
		}
		r, err := s.loadIssueByID(ctx, tx, row.id)
		if err != nil {
			return err
		}
		out = &r.Issue
		return nil
	})
	return out, err
}
