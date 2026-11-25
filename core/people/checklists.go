package people

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
)

var actionRe = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,40}$`)

// TemplateItem is one step of a checklist template.
type TemplateItem struct {
	Title         string `json:"title"`
	AssigneeRole  string `json:"assignee_role"`    // self | manager | hr | admin
	DueOffsetDays int    `json:"due_offset_days"`  // relative to the anchor date (negative = before)
	Action        string `json:"action,omitempty"` // machine hook, e.g. identity.provision; carried on the completion event
}

// Template is an onboarding or offboarding checklist template.
type Template struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	AutoStatus string         `json:"auto_status,omitempty"` // create automatically when a person enters this status
	Items      []TemplateItem `json:"items"`
}

// TemplateInput creates a template.
type TemplateInput struct {
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	AutoStatus string         `json:"auto_status"`
	Items      []TemplateItem `json:"items"`
}

// Task is one checklist task.
type Task struct {
	ID           string `json:"id"`
	ChecklistID  string `json:"checklist_id"`
	PersonID     string `json:"person_id"`
	Title        string `json:"title"`
	AssigneeRole string `json:"assignee_role"`
	AssigneeID   string `json:"assignee_id,omitempty"`
	DueDate      string `json:"due_date"`
	Action       string `json:"action,omitempty"`
	Done         bool   `json:"done"`
	DoneBy       string `json:"done_by,omitempty"`
}

// Checklist is an instance of a template for one person.
type Checklist struct {
	ID         string `json:"id"`
	PersonID   string `json:"person_id"`
	TemplateID string `json:"template_id"`
	Kind       string `json:"kind"`
	AnchorDate string `json:"anchor_date"`
	Tasks      []Task `json:"tasks"`
}

// CreateTemplate stores a template. hr or admin only.
func (s *Service) CreateTemplate(ctx context.Context, a Actor, in TemplateInput) (*Template, error) {
	if !Decide(a, ActTemplateManage, Fact{}) {
		s.auditDeny(ctx, a, "create", "template", nil)
		return nil, ErrForbidden
	}
	if err := validLine("name", in.Name, 100, true); err != nil {
		return nil, err
	}
	if in.Kind != "onboarding" && in.Kind != "offboarding" {
		return nil, invalid("kind must be onboarding or offboarding")
	}
	if in.AutoStatus != "" && in.AutoStatus != string(StOnboarding) && in.AutoStatus != string(StOffboarding) {
		return nil, invalid("auto_status must be onboarding, offboarding or empty")
	}
	if len(in.Items) == 0 || len(in.Items) > 50 {
		return nil, invalid("a template needs 1 to 50 items")
	}
	for i, it := range in.Items {
		if err := validLine("item title", it.Title, 200, true); err != nil {
			return nil, err
		}
		if !slices.Contains([]string{"self", "manager", "hr", "admin"}, it.AssigneeRole) {
			return nil, invalid("item %d: assignee_role must be self, manager, hr or admin", i+1)
		}
		if it.DueOffsetDays < -365 || it.DueOffsetDays > 365 {
			return nil, invalid("item %d: due_offset_days must be within a year", i+1)
		}
		if it.Action != "" && !actionRe.MatchString(it.Action) {
			return nil, invalid("item %d: invalid action", i+1)
		}
		if strings.HasPrefix(it.Action, "identity.") && it.AssigneeRole == "self" {
			return nil, invalid("item %d: an identity.* action cannot be assigned to the person themself", i+1)
		}
	}
	items, _ := json.Marshal(in.Items)
	id := newID("tpl")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO checklist_templates (id, name, kind, auto_status, items, created_at) VALUES (?,?,?,?,?,?)`,
		id, in.Name, in.Kind, in.AutoStatus, string(items), s.now().Unix()); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, conflict("a template named %q already exists", in.Name)
		}
		return nil, err
	}
	if err := s.auditWrite(ctx, a, "create", "template:"+id, nil); err != nil {
		return nil, err
	}
	return &Template{ID: id, Name: in.Name, Kind: in.Kind, AutoStatus: in.AutoStatus, Items: in.Items}, nil
}

// ListTemplates lists templates. hr or admin only.
func (s *Service) ListTemplates(ctx context.Context, a Actor) ([]Template, error) {
	if !Decide(a, ActTemplateManage, Fact{}) {
		return nil, ErrForbidden
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, kind, auto_status, items FROM checklist_templates ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Template{}
	for rows.Next() {
		var t Template
		var items string
		if err := rows.Scan(&t.ID, &t.Name, &t.Kind, &t.AutoStatus, &items); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(items), &t.Items); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateChecklist instantiates a template for a person. hr/admin, or the person's direct
// manager. anchor defaults to today.
func (s *Service) CreateChecklist(ctx context.Context, a Actor, personID, templateID, anchor string) (*Checklist, error) {
	f, si, err := s.personFact(ctx, a, personID)
	if err != nil {
		return nil, err
	}
	if !Decide(a, ActChecklistCreate, f) {
		s.auditDeny(ctx, a, "create", "checklist:"+personID, nil)
		return nil, ErrForbidden
	}
	if si.erased || si.status == StFormer {
		return nil, conflict("cannot create a checklist for a %s record", si.status)
	}
	if anchor == "" {
		anchor = s.today()
	}
	if err := validDate(anchor); err != nil {
		return nil, err
	}
	var cl *Checklist
	err = s.tx(ctx, func(tx *sqlTx) error {
		c, err := s.instantiate(ctx, tx, personID, templateID, anchor, a.auditID())
		cl = c
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := s.auditWrite(ctx, a, "create", "checklist:"+cl.ID, nil); err != nil {
		return nil, err
	}
	return cl, nil
}

func (s *Service) instantiate(ctx context.Context, q querier, personID, templateID, anchor, by string) (*Checklist, error) {
	var kind, items string
	err := q.QueryRowContext(ctx, `SELECT kind, items FROM checklist_templates WHERE id = ?`, templateID).Scan(&kind, &items)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, invalid("template %q does not exist", templateID)
	}
	if err != nil {
		return nil, err
	}
	var tis []TemplateItem
	if err := json.Unmarshal([]byte(items), &tis); err != nil {
		return nil, err
	}
	var mgr sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT manager_id FROM people WHERE id = ?`, personID).Scan(&mgr); err != nil {
		return nil, err
	}
	base, err := time.Parse(dateLayout, anchor)
	if err != nil {
		return nil, invalid("anchor_date must be YYYY-MM-DD")
	}
	cl := &Checklist{ID: newID("chk"), PersonID: personID, TemplateID: templateID, Kind: kind, AnchorDate: anchor}
	if _, err := q.ExecContext(ctx, `INSERT INTO checklists (id, person_id, template_id, kind, anchor_date, created_at, created_by) VALUES (?,?,?,?,?,?,?)`,
		cl.ID, personID, templateID, kind, anchor, s.now().Unix(), by); err != nil {
		return nil, err
	}
	for i, it := range tis {
		t := Task{ID: newID("task"), ChecklistID: cl.ID, PersonID: personID, Title: it.Title, AssigneeRole: it.AssigneeRole, Action: it.Action,
			DueDate: base.AddDate(0, 0, it.DueOffsetDays).Format(dateLayout)}
		switch it.AssigneeRole {
		case "self":
			t.AssigneeID = personID
		case "manager":
			t.AssigneeID = mgr.String // shown now; resolved live on every read
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO checklist_tasks (id, checklist_id, person_id, ord, title, assignee_role, assignee_id, due_date, action) VALUES (?,?,?,?,?,?,?,?,?)`,
			t.ID, cl.ID, personID, i, t.Title, t.AssigneeRole, nullStr(t.AssigneeID), t.DueDate, t.Action); err != nil {
			return nil, err
		}
		cl.Tasks = append(cl.Tasks, t)
	}
	return cl, nil
}

// autoChecklists instantiates the templates configured for a status, once per person and template.
func (s *Service) autoChecklists(ctx context.Context, q querier, personID string, st Status, a Actor) error {
	if st != StOnboarding && st != StOffboarding {
		return nil
	}
	rows, err := q.QueryContext(ctx, `SELECT id FROM checklist_templates WHERE auto_status = ? AND id NOT IN (SELECT template_id FROM checklists WHERE person_id = ?)`, string(st), personID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	for _, id := range ids {
		if _, err := s.instantiate(ctx, q, personID, id, s.today(), a.auditID()); err != nil {
			return err
		}
	}
	return nil
}

// assigneeExpr resolves who a task is assigned to *now*: a manager-role task follows the live
// reporting line (a new manager inherits it, a former manager loses it); other roles use the
// person fixed when the checklist was created.
const assigneeExpr = `CASE WHEN t.assignee_role = 'manager' THEN (SELECT manager_id FROM people WHERE id = t.person_id) ELSE t.assignee_id END`

const taskCols = `t.id, t.checklist_id, t.person_id, t.title, t.assignee_role, COALESCE(` + assigneeExpr + `,''), t.due_date, t.action, t.done_at IS NOT NULL, t.done_by`

func scanTask(sc interface{ Scan(...any) error }) (Task, error) {
	var t Task
	err := sc.Scan(&t.ID, &t.ChecklistID, &t.PersonID, &t.Title, &t.AssigneeRole, &t.AssigneeID, &t.DueDate, &t.Action, &t.Done, &t.DoneBy)
	return t, err
}

// ListChecklists returns a person's checklists. The person, their direct manager and hr/admin.
func (s *Service) ListChecklists(ctx context.Context, a Actor, personID string) ([]Checklist, error) {
	f, _, err := s.personFact(ctx, a, personID)
	if err != nil {
		return nil, err
	}
	if !Decide(a, ActChecklistRead, f) {
		s.auditDeny(ctx, a, "read", "checklist:"+personID, nil)
		return nil, ErrForbidden
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, template_id, kind, anchor_date FROM checklists WHERE person_id = ? ORDER BY created_at, id`, personID)
	if err != nil {
		return nil, err
	}
	out := []Checklist{}
	for rows.Next() {
		c := Checklist{PersonID: personID}
		if err := rows.Scan(&c.ID, &c.TemplateID, &c.Kind, &c.AnchorDate); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	_ = rows.Close()
	for i := range out {
		trs, err := s.db.QueryContext(ctx, `SELECT `+taskCols+` FROM checklist_tasks t WHERE t.checklist_id = ? ORDER BY t.ord`, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Tasks = []Task{}
		for trs.Next() {
			t, err := scanTask(trs)
			if err != nil {
				_ = trs.Close()
				return nil, err
			}
			out[i].Tasks = append(out[i].Tasks, t)
		}
		_ = trs.Close()
	}
	return out, nil
}

// MyTasks lists the open tasks assigned to the caller, plus the unassigned hr/admin tasks when
// the caller holds that role.
func (s *Service) MyTasks(ctx context.Context, a Actor) ([]Task, error) {
	if !Decide(a, ActTaskListOwn, Fact{}) && !a.hrAdmin() {
		return nil, ErrForbidden
	}
	q := `SELECT ` + taskCols + ` FROM checklist_tasks t WHERE t.done_at IS NULL AND ((` + assigneeExpr + `) = ?`
	args := []any{a.PersonID}
	var roles []any
	for _, r := range []string{RoleHR, RoleAdmin} {
		if a.elevated(r) {
			roles = append(roles, r)
		}
	}
	if len(roles) > 0 {
		q += ` OR ((` + assigneeExpr + `) IS NULL AND t.assignee_role IN (?` + strings.Repeat(",?", len(roles)-1) + `))`
		args = append(args, roles...)
	}
	rows, err := s.db.QueryContext(ctx, q+`) ORDER BY t.due_date, t.id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CompleteTask marks a task done. The assignee or hr/admin; everyone else gets ErrNotFound.
// Completion emits checklist.task_completed.v1, which carries the task's action so a later
// identity.provision integration can react.
func (s *Service) CompleteTask(ctx context.Context, a Actor, taskID string) (*Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM checklist_tasks t WHERE t.id = ?`, taskID)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f := Fact{Assigned: a.HasPerson && t.AssigneeID == a.PersonID}
	if !Decide(a, ActTaskComplete, f) {
		s.auditDeny(ctx, a, "write", "task:"+taskID, nil)
		return nil, ErrNotFound
	}
	// Nobody completes an identity.* task about themself (a person cannot provision or
	// approve their own access); another assignee, hr or admin must do it.
	if strings.HasPrefix(t.Action, "identity.") && a.HasPerson && a.PersonID == t.PersonID {
		s.auditDeny(ctx, a, "write", "task:"+taskID, nil)
		return nil, ErrForbidden
	}
	if t.Done {
		return &t, nil // idempotent
	}
	now := s.now()
	err = s.tx(ctx, func(tx *sqlTx) error {
		res, err := tx.ExecContext(ctx, `UPDATE checklist_tasks SET done_at = ?, done_by = ? WHERE id = ? AND done_at IS NULL`, now.Unix(), a.auditID(), taskID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		data := map[string]any{"task_id": taskID, "checklist_id": t.ChecklistID, "party_id": t.PersonID, "completed_at": now.UTC().Format(time.RFC3339)}
		if t.Action != "" {
			data["action"] = t.Action
		}
		return s.enqueue(ctx, tx, EventChecklistTaskDone, data)
	})
	if err != nil {
		return nil, err
	}
	if err := s.auditWrite(ctx, a, "write", "task:"+taskID, nil); err != nil {
		return nil, err
	}
	t.Done, t.DoneBy = true, a.auditID()
	return &t, nil
}
