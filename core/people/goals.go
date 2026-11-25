package people

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"slices"
	"time"
)

// KeyResult is a measurable result under a goal.
type KeyResult struct {
	ID       string  `json:"id"`
	GoalID   string  `json:"goal_id"`
	Title    string  `json:"title"`
	Unit     string  `json:"unit,omitempty"`
	Start    float64 `json:"start_value"`
	Target   float64 `json:"target_value"`
	Current  float64 `json:"current_value"`
	Progress int     `json:"progress"` // 0..100
}

// Goal is an objective: personal (owner), team or company, optionally aligned to a parent goal.
type Goal struct {
	ID          string      `json:"id"`
	Scope       string      `json:"scope"`
	OwnerID     string      `json:"owner_id,omitempty"`
	TeamID      string      `json:"team_id,omitempty"`
	Title       string      `json:"title"`
	Description string      `json:"description,omitempty"`
	Period      string      `json:"period,omitempty"`
	Visibility  string      `json:"visibility"`
	Status      string      `json:"status"`
	ParentID    string      `json:"parent_id,omitempty"`
	Progress    int         `json:"progress"`
	KeyResults  []KeyResult `json:"key_results"`
	CreatedAt   string      `json:"created_at"`
	UpdatedAt   string      `json:"updated_at"`
}

// GoalInput creates a goal.
type GoalInput struct {
	Scope       string `json:"scope"`
	OwnerID     string `json:"owner_id"`
	TeamID      string `json:"team_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Period      string `json:"period"`
	Visibility  string `json:"visibility"`
	Status      string `json:"status"`
	ParentID    string `json:"parent_id"`
}

// GoalPatch updates a goal; nil means unchanged, ParentID "" clears the alignment.
type GoalPatch struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Period      *string `json:"period"`
	Visibility  *string `json:"visibility"`
	Status      *string `json:"status"`
	ParentID    *string `json:"parent_id"`
}

var (
	goalVisibilities = []string{"private", "manager", "team", "company"}
	goalStatuses     = []string{"draft", "active", "achieved", "abandoned"}
)

func krProgress(start, target, current float64) int {
	if target == start {
		return 0
	}
	p := (current - start) / (target - start) * 100
	return int(math.Max(0, math.Min(100, math.Round(p))))
}

func (s *Service) goalRow(ctx context.Context, q querier, id string) (*Goal, error) {
	var g Goal
	var owner, team, parent sql.NullString
	var c, u int64
	err := q.QueryRowContext(ctx, `SELECT id, scope, owner_id, team_id, title, description, period, visibility, status, parent_id, created_at, updated_at FROM goals WHERE id = ?`, id).
		Scan(&g.ID, &g.Scope, &owner, &team, &g.Title, &g.Description, &g.Period, &g.Visibility, &g.Status, &parent, &c, &u)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	g.OwnerID, g.TeamID, g.ParentID = owner.String, team.String, parent.String
	g.CreatedAt, g.UpdatedAt = time.Unix(c, 0).UTC().Format(time.RFC3339), time.Unix(u, 0).UTC().Format(time.RFC3339)
	g.KeyResults = []KeyResult{}
	return &g, nil
}

func (s *Service) loadKRs(ctx context.Context, q querier, g *Goal) error {
	rows, err := q.QueryContext(ctx, `SELECT id, goal_id, title, unit, start_value, target_value, current_value FROM key_results WHERE goal_id = ? ORDER BY created_at, id`, g.ID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	sum := 0
	for rows.Next() {
		var k KeyResult
		if err := rows.Scan(&k.ID, &k.GoalID, &k.Title, &k.Unit, &k.Start, &k.Target, &k.Current); err != nil {
			return err
		}
		k.Progress = krProgress(k.Start, k.Target, k.Current)
		sum += k.Progress
		g.KeyResults = append(g.KeyResults, k)
	}
	if n := len(g.KeyResults); n > 0 {
		g.Progress = sum / n
	}
	if g.Status == "achieved" {
		g.Progress = 100
	}
	return rows.Err()
}

// goalFact computes the actor's relationship to a goal.
func (s *Service) goalFact(ctx context.Context, q querier, a Actor, g *Goal) (Fact, error) {
	f := Fact{Scope: g.Scope, Visibility: g.Visibility}
	switch g.Scope {
	case "person":
		pf, _, err := s.factFor(ctx, q, a, g.OwnerID)
		if err != nil {
			return f, err
		}
		pf.Scope, pf.Visibility = g.Scope, g.Visibility
		return pf, nil
	case "team":
		var lead sql.NullString
		if err := q.QueryRowContext(ctx, `SELECT lead_id FROM teams WHERE id = ?`, g.TeamID).Scan(&lead); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return f, err
		}
		f.TeamLead = a.HasPerson && lead.String == a.PersonID
		if a.HasPerson {
			var mine sql.NullString
			if err := q.QueryRowContext(ctx, `SELECT team_id FROM people WHERE id = ?`, a.PersonID).Scan(&mine); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return f, err
			}
			f.SameTeam = mine.String == g.TeamID
		}
	}
	return f, nil
}

func (s *Service) goalForRead(ctx context.Context, a Actor, id string) (*Goal, Fact, error) {
	g, err := s.goalRow(ctx, s.db, id)
	if err != nil {
		return nil, Fact{}, err
	}
	f, err := s.goalFact(ctx, s.db, a, g)
	if err != nil {
		return nil, f, err
	}
	if !Decide(a, ActGoalRead, f) {
		s.auditDeny(ctx, a, "read", "goal:"+id, nil)
		return nil, f, ErrNotFound
	}
	return g, f, nil
}

func (s *Service) goalEvent(ctx context.Context, q querier, g *Goal, change string) error {
	// A private goal is for its owner alone: even an id-only event would tell every subscriber
	// that the person has a private goal and when they work on it. No event is published.
	if g.Visibility == "private" {
		return nil
	}
	if err := s.loadKRs(ctx, q, g); err != nil {
		return err
	}
	data := map[string]any{"goal_id": g.ID, "scope": g.Scope, "change": change, "status": g.Status, "progress": g.Progress}
	if g.OwnerID != "" {
		data["owner_party_id"] = g.OwnerID
	}
	if g.ParentID != "" {
		data["parent_goal_id"] = g.ParentID
	}
	return s.enqueue(ctx, q, EventGoalUpdated, data)
}

// checkAlignment validates parent: the actor can read it, it is not the goal itself or a
// descendant (no cycles), and it is a sensible ancestor: a company goal, the owner's team goal,
// or a goal owned by someone in the owner's manager chain.
func (s *Service) checkAlignment(ctx context.Context, q querier, a Actor, g *Goal, parentID string) error {
	if parentID == "" {
		return nil
	}
	p, err := s.goalRow(ctx, q, parentID)
	if err != nil {
		return invalid("parent goal does not exist")
	}
	pf, err := s.goalFact(ctx, q, a, p)
	if err != nil {
		return err
	}
	if !Decide(a, ActGoalRead, pf) {
		return invalid("parent goal does not exist") // do not reveal goals the caller cannot see
	}
	// Walk up from the proposed parent with a visited set: a pre-existing cycle terminates, and
	// the bound is only a safety net far above any real alignment depth.
	seen := map[string]bool{}
	for cur := p; cur != nil; {
		if cur.ID == g.ID {
			return invalid("alignment would create a cycle")
		}
		if seen[cur.ID] || len(seen) >= 10000 {
			return invalid("the parent goal's alignment chain is already cyclic or too deep")
		}
		seen[cur.ID] = true
		if cur.ParentID == "" {
			break
		}
		if cur, err = s.goalRow(ctx, q, cur.ParentID); err != nil {
			return err
		}
	}
	ok := false
	switch {
	case p.Scope == "company":
		ok = g.Scope != "company"
	case g.Scope == "person" && p.Scope == "team":
		var team sql.NullString
		if err := q.QueryRowContext(ctx, `SELECT team_id FROM people WHERE id = ?`, g.OwnerID).Scan(&team); err != nil {
			return err
		}
		ok = team.String != "" && team.String == p.TeamID
	case g.Scope == "person" && p.Scope == "person":
		chain, err := managerChain(ctx, q, g.OwnerID)
		if err != nil {
			return err
		}
		ok = slices.Contains(chain, p.OwnerID)
	}
	if !ok {
		return invalid("a goal can align only to a company goal, its team's goal, or a goal of someone in the owner's management chain")
	}
	return nil
}

// CreateGoal creates a goal after the policy and alignment checks.
func (s *Service) CreateGoal(ctx context.Context, a Actor, in GoalInput) (*Goal, error) {
	if in.Scope == "person" && in.OwnerID == "" {
		in.OwnerID = a.PersonID
	}
	g := &Goal{ID: newID("goal"), Scope: in.Scope, OwnerID: in.OwnerID, TeamID: in.TeamID, Title: in.Title, Description: in.Description, Period: in.Period,
		Visibility: in.Visibility, Status: in.Status, ParentID: in.ParentID}
	if g.Status == "" {
		g.Status = "active"
	}
	if err := validLine("title", g.Title, 200, true); err != nil {
		return nil, err
	}
	if err := validText("description", g.Description, 4000, false); err != nil {
		return nil, err
	}
	if err := validLine("period", g.Period, 20, false); err != nil {
		return nil, err
	}
	if !slices.Contains(goalStatuses, g.Status) {
		return nil, invalid("status must be one of %v", goalStatuses)
	}
	var f Fact
	switch g.Scope {
	case "person":
		if !personIDRe.MatchString(g.OwnerID) {
			return nil, invalid("owner_id is required for a personal goal")
		}
		g.TeamID = ""
		if g.Visibility == "" {
			g.Visibility = "manager"
		}
		// An owner the caller cannot see answers exactly like one that does not exist (404):
		// no existence oracle through goal creation.
		pf, _, err := s.personFact(ctx, a, g.OwnerID)
		if err != nil {
			return nil, err
		}
		f = pf
	case "team":
		g.OwnerID = ""
		if g.Visibility == "" {
			g.Visibility = "team"
		}
		if g.Visibility != "team" && g.Visibility != "company" {
			return nil, invalid("a team goal is visible to the team or the company")
		}
		var lead sql.NullString
		if err := s.db.QueryRowContext(ctx, `SELECT lead_id FROM teams WHERE id = ?`, g.TeamID).Scan(&lead); err != nil {
			return nil, invalid("team does not exist")
		}
		f.TeamLead = a.HasPerson && lead.String == a.PersonID
	case "company":
		g.OwnerID, g.TeamID, g.Visibility = "", "", "company"
	default:
		return nil, invalid("scope must be person, team or company")
	}
	if !slices.Contains(goalVisibilities, g.Visibility) {
		return nil, invalid("visibility must be one of %v", goalVisibilities)
	}
	f.Scope = g.Scope
	if !Decide(a, ActGoalCreate, f) {
		s.auditDeny(ctx, a, "create", "goal", nil)
		if g.Scope == "person" && !f.Self && !f.Direct {
			return nil, ErrNotFound // do not confirm that the other person exists
		}
		return nil, ErrForbidden
	}
	now := s.now().Unix()
	err := s.tx(ctx, func(tx *sqlTx) error {
		if err := s.checkAlignment(ctx, tx, a, g, g.ParentID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO goals (id, scope, owner_id, team_id, title, description, period, visibility, status, parent_id, created_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			g.ID, g.Scope, nullStr(g.OwnerID), nullStr(g.TeamID), g.Title, g.Description, g.Period, g.Visibility, g.Status, nullStr(g.ParentID), a.auditID(), now, now); err != nil {
			return err
		}
		return s.goalEvent(ctx, tx, g, "created")
	})
	if err != nil {
		return nil, err
	}
	if err := s.auditWrite(ctx, a, "create", "goal:"+g.ID, nil); err != nil {
		return nil, err
	}
	return s.GetGoal(ctx, a, g.ID)
}

// GetGoal returns a goal the caller may see (ErrNotFound otherwise). Reads of someone else's
// goal are audited.
func (s *Service) GetGoal(ctx context.Context, a Actor, id string) (*Goal, error) {
	g, f, err := s.goalForRead(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if g.Scope != "person" || !f.Self {
		if err := s.auditRead(ctx, a, "goal:"+id, []string{"goal"}); err != nil {
			return nil, errors.New("audit unavailable, read refused")
		}
	}
	return g, s.loadKRs(ctx, s.db, g)
}

// GoalFilter narrows ListGoals.
type GoalFilter struct {
	OwnerID  string
	TeamID   string
	ParentID string
	Scope    string
}

// ListGoals returns the goals the caller may see. One audit event covers the foreign goals returned.
func (s *Service) ListGoals(ctx context.Context, a Actor, flt GoalFilter) ([]Goal, error) {
	if a.blocked() {
		return []Goal{}, nil
	}
	q := `SELECT id FROM goals WHERE 1=1`
	var args []any
	for col, v := range map[string]string{"owner_id": flt.OwnerID, "team_id": flt.TeamID, "parent_id": flt.ParentID, "scope": flt.Scope} {
		if v != "" {
			q += " AND " + col + " = ?"
			args = append(args, v)
		}
	}
	// The cap applies to what the caller may see, never to the rows scanned: a limit in SQL
	// would let invisible goals crowd visible ones out of the page.
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY created_at, id LIMIT 20000", args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	out := []Goal{}
	var foreign []string
	for _, id := range ids {
		g, err := s.goalRow(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		f, err := s.goalFact(ctx, s.db, a, g)
		if err != nil {
			return nil, err
		}
		if !Decide(a, ActGoalRead, f) {
			continue
		}
		if err := s.loadKRs(ctx, s.db, g); err != nil {
			return nil, err
		}
		if g.Scope != "person" || !f.Self {
			foreign = append(foreign, g.ID)
		}
		out = append(out, *g)
		if len(out) == 500 {
			break
		}
	}
	if len(foreign) > 0 {
		if err := s.auditRead(ctx, a, "goals", foreign); err != nil {
			return nil, errors.New("audit unavailable, read refused")
		}
	}
	return out, nil
}

func (s *Service) goalForWrite(ctx context.Context, a Actor, id string) (*Goal, error) {
	g, f, err := s.goalForRead(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if !Decide(a, ActGoalWrite, f) {
		s.auditDeny(ctx, a, "write", "goal:"+id, nil)
		return nil, ErrForbidden
	}
	return g, nil
}

// UpdateGoal changes a goal. Owner, the owner's direct manager, the team lead, or hr/admin.
func (s *Service) UpdateGoal(ctx context.Context, a Actor, id string, p GoalPatch) (*Goal, error) {
	g, err := s.goalForWrite(ctx, a, id)
	if err != nil {
		return nil, err
	}
	change := "updated"
	if p.Title != nil {
		if err := validLine("title", *p.Title, 200, true); err != nil {
			return nil, err
		}
		g.Title = *p.Title
	}
	if p.Description != nil {
		if err := validText("description", *p.Description, 4000, false); err != nil {
			return nil, err
		}
		g.Description = *p.Description
	}
	if p.Period != nil {
		if err := validLine("period", *p.Period, 20, false); err != nil {
			return nil, err
		}
		g.Period = *p.Period
	}
	if p.Visibility != nil {
		if !slices.Contains(goalVisibilities, *p.Visibility) || (g.Scope == "team" && (*p.Visibility == "private" || *p.Visibility == "manager")) || (g.Scope == "company" && *p.Visibility != "company") {
			return nil, invalid("visibility %q is not allowed for a %s goal", *p.Visibility, g.Scope)
		}
		g.Visibility = *p.Visibility
	}
	if p.Status != nil {
		if !slices.Contains(goalStatuses, *p.Status) {
			return nil, invalid("status must be one of %v", goalStatuses)
		}
		if *p.Status != g.Status {
			change = "status"
		}
		g.Status = *p.Status
	}
	err = s.tx(ctx, func(tx *sqlTx) error {
		if p.ParentID != nil {
			if err := s.checkAlignment(ctx, tx, a, g, *p.ParentID); err != nil {
				return err
			}
			g.ParentID = *p.ParentID
		}
		if _, err := tx.ExecContext(ctx, `UPDATE goals SET title=?, description=?, period=?, visibility=?, status=?, parent_id=?, updated_at=? WHERE id=?`,
			g.Title, g.Description, g.Period, g.Visibility, g.Status, nullStr(g.ParentID), s.now().Unix(), id); err != nil {
			return err
		}
		return s.goalEvent(ctx, tx, g, change)
	})
	if err != nil {
		return nil, err
	}
	if err := s.auditWrite(ctx, a, "write", "goal:"+id, nil); err != nil {
		return nil, err
	}
	return s.GetGoal(ctx, a, id)
}

// KeyResultInput creates a key result.
type KeyResultInput struct {
	Title  string  `json:"title"`
	Unit   string  `json:"unit"`
	Start  float64 `json:"start_value"`
	Target float64 `json:"target_value"`
}

func finite(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 1e12 {
			return false
		}
	}
	return true
}

// AddKeyResult adds a key result to a goal the caller may write.
func (s *Service) AddKeyResult(ctx context.Context, a Actor, goalID string, in KeyResultInput) (*KeyResult, error) {
	g, err := s.goalForWrite(ctx, a, goalID)
	if err != nil {
		return nil, err
	}
	if err := validLine("title", in.Title, 200, true); err != nil {
		return nil, err
	}
	if err := validLine("unit", in.Unit, 20, false); err != nil {
		return nil, err
	}
	if !finite(in.Start, in.Target) || in.Start == in.Target {
		return nil, invalid("start_value and target_value must be finite and different")
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM key_results WHERE goal_id = ?`, goalID).Scan(&n); err != nil {
		return nil, err
	}
	if n >= 20 {
		return nil, conflict("a goal holds at most 20 key results")
	}
	k := &KeyResult{ID: newID("kr"), GoalID: goalID, Title: in.Title, Unit: in.Unit, Start: in.Start, Target: in.Target, Current: in.Start}
	err = s.tx(ctx, func(tx *sqlTx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO key_results (id, goal_id, title, unit, start_value, target_value, current_value, created_at) VALUES (?,?,?,?,?,?,?,?)`,
			k.ID, goalID, k.Title, k.Unit, k.Start, k.Target, k.Current, s.now().Unix()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE goals SET updated_at = ? WHERE id = ?`, s.now().Unix(), goalID); err != nil {
			return err
		}
		return s.goalEvent(ctx, tx, g, "key_result_added")
	})
	if err != nil {
		return nil, err
	}
	return k, s.auditWrite(ctx, a, "write", "goal:"+goalID, nil)
}

// RecordProgress sets a key result's current value and logs the update.
func (s *Service) RecordProgress(ctx context.Context, a Actor, krID string, value float64, comment string) (*Goal, error) {
	var goalID string
	err := s.db.QueryRowContext(ctx, `SELECT goal_id FROM key_results WHERE id = ?`, krID).Scan(&goalID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	g, err := s.goalForWrite(ctx, a, goalID)
	if err != nil {
		return nil, err
	}
	if !finite(value) {
		return nil, invalid("value must be a finite number")
	}
	if err := validText("comment", comment, 1000, false); err != nil {
		return nil, err
	}
	err = s.tx(ctx, func(tx *sqlTx) error {
		now := s.now().Unix()
		if _, err := tx.ExecContext(ctx, `UPDATE key_results SET current_value = ? WHERE id = ?`, value, krID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO goal_updates (id, goal_id, kr_id, author_id, value, comment, created_at) VALUES (?,?,?,?,?,?,?)`,
			newID("upd"), goalID, krID, a.auditID(), value, comment, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE goals SET updated_at = ? WHERE id = ?`, now, goalID); err != nil {
			return err
		}
		return s.goalEvent(ctx, tx, g, "progress")
	})
	if err != nil {
		return nil, err
	}
	if err := s.auditWrite(ctx, a, "write", "goal:"+goalID, nil); err != nil {
		return nil, err
	}
	return s.GetGoal(ctx, a, goalID)
}

// GoalUpdate is one progress-log entry.
type GoalUpdate struct {
	ID        string   `json:"id"`
	KRID      string   `json:"key_result_id,omitempty"`
	AuthorID  string   `json:"author_id"`
	Value     *float64 `json:"value,omitempty"`
	Comment   string   `json:"comment,omitempty"`
	CreatedAt string   `json:"created_at"`
}

// ListUpdates returns the progress log of a goal the caller may read.
func (s *Service) ListUpdates(ctx context.Context, a Actor, goalID string) ([]GoalUpdate, error) {
	if _, err := s.GetGoal(ctx, a, goalID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(kr_id,''), author_id, value, comment, created_at FROM goal_updates WHERE goal_id = ? ORDER BY created_at DESC, id LIMIT 200`, goalID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []GoalUpdate{}
	for rows.Next() {
		var u GoalUpdate
		var v sql.NullFloat64
		var c int64
		if err := rows.Scan(&u.ID, &u.KRID, &u.AuthorID, &v, &u.Comment, &c); err != nil {
			return nil, err
		}
		if v.Valid {
			u.Value = &v.Float64
		}
		u.CreatedAt = time.Unix(c, 0).UTC().Format(time.RFC3339)
		out = append(out, u)
	}
	return out, rows.Err()
}

// CanWriteGoal reports whether the caller may change the goal, using the same policy decision
// UpdateGoal enforces. The web UI uses it only to decide whether to show the edit forms; an
// unreadable or unknown goal is false.
func (s *Service) CanWriteGoal(ctx context.Context, a Actor, id string) bool {
	_, f, err := s.goalForRead(ctx, a, id)
	return err == nil && Decide(a, ActGoalWrite, f)
}
