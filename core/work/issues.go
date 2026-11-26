package work

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/obs"
)

type scanner interface{ Scan(...any) error }

// issueRow is an issue plus the internal ids and project settings the rules need.
type issueRow struct {
	Issue
	id        int64
	projectID int64
	parentID  *int64
	archived  bool
	wf        Workflow
	sla       SLA
}

const issueSelect = `SELECT i.id, i.project_id, p.archived, p.key, i.number, i.type, i.title, i.description, i.priority, i.status,
 i.reporter, i.assignee, i.due_date, i.estimate, i.parent_id,
 CASE WHEN pi.id IS NULL THEN NULL ELSE p.key || '-' || pi.number END,
 i.sprint_id, i.rank, i.version, i.created_at, i.updated_at, i.resolved_at, p.workflow, p.sla
 FROM issues i JOIN projects p ON p.id = i.project_id LEFT JOIN issues pi ON pi.id = i.parent_id`

func scanIssue(sc scanner) (*issueRow, error) {
	var (
		r                       issueRow
		archived                int
		number                  int64
		assignee, due, parent   sql.NullString
		est                     sql.NullFloat64
		parentID, sprint, resol sql.NullInt64
		created, updated        int64
		wf, sla                 string
	)
	if err := sc.Scan(&r.id, &r.projectID, &archived, &r.Project, &number, &r.Type, &r.Title, &r.Description, &r.Priority, &r.Status,
		&r.Reporter, &assignee, &due, &est, &parentID, &parent, &sprint, &r.Rank, &r.Version, &created, &updated, &resol, &wf, &sla); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(wf), &r.wf); err != nil {
		return nil, fmt.Errorf("stored workflow: %w", err)
	}
	if err := json.Unmarshal([]byte(sla), &r.sla); err != nil {
		return nil, fmt.Errorf("stored sla: %w", err)
	}
	r.archived = archived == 1
	r.Key = r.Project + "-" + strconv.FormatInt(number, 10)
	r.Assignee, r.DueDate, r.Parent = nullStr(assignee), nullStr(due), nullStr(parent)
	if est.Valid {
		v := est.Float64
		r.Estimate = &v
	}
	if parentID.Valid {
		v := parentID.Int64
		r.parentID = &v
	}
	if sprint.Valid {
		v := sprint.Int64
		r.SprintID = &v
	}
	r.CreatedAt, r.UpdatedAt, r.ResolvedAt = fromUnix(created), fromUnix(updated), nullTime(resol)
	r.StatusCategory = r.wf.Category(r.Status)
	r.SLAHint = r.sla[r.Priority]
	r.Labels = []string{}
	return &r, nil
}

func splitKey(key string) (project string, number int64, ok bool) {
	m := issueKeyRE.FindStringSubmatch(key)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.ParseInt(m[2], 10, 64)
	return m[1], n, err == nil
}

// loadIssue finds an issue by key. A key in a project outside the caller's scope is
// reported as not found, exactly like a key that does not exist, so keys cannot be probed.
func (s *Store) loadIssue(ctx context.Context, q dbq, a *Principal, key string) (*issueRow, error) {
	project, number, ok := splitKey(key)
	if !ok || !a.CanProject(project) {
		return nil, ErrNotFound
	}
	r, err := scanIssue(q.QueryRowContext(ctx, issueSelect+` WHERE p.key = ? AND i.number = ?`, project, number))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := s.fillLabels(ctx, q, []*issueRow{r}); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *Store) loadIssueByID(ctx context.Context, q dbq, id int64) (*issueRow, error) {
	r, err := scanIssue(q.QueryRowContext(ctx, issueSelect+` WHERE i.id = ?`, id))
	if err != nil {
		return nil, err
	}
	if err := s.fillLabels(ctx, q, []*issueRow{r}); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *Store) fillLabels(ctx context.Context, q dbq, rows []*issueRow) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]any, len(rows))
	byID := make(map[int64]*issueRow, len(rows))
	for i, r := range rows {
		ids[i] = r.id
		byID[r.id] = r
	}
	rs, err := q.QueryContext(ctx, `SELECT issue_id, label FROM issue_labels WHERE issue_id IN (`+placeholders(len(ids))+`) ORDER BY label`, ids...)
	if err != nil {
		return err
	}
	defer func() { _ = rs.Close() }()
	for rs.Next() {
		var id int64
		var l string
		if err := rs.Scan(&id, &l); err != nil {
			return err
		}
		byID[id].Labels = append(byID[id].Labels, l)
	}
	return rs.Err()
}

func (s *Store) addActivity(ctx context.Context, q dbq, issueID int64, actor, kind string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO activity (issue_id, actor, kind, data, at) VALUES (?, ?, ?, ?, ?)`, issueID, actor, kind, string(b), ts(s.now()))
	return err
}

// ---- hierarchy ------------------------------------------------------------------------

// checkParent validates a parent assignment and returns its row id and key. Rules: an epic
// has no parent; a story, task or bug may sit under an epic; a subtask needs a story, task
// or bug. The parent must be in the same project. childID is 0 for a new issue.
func (s *Store) checkParent(ctx context.Context, q dbq, proj *projectRow, childType string, parent *string, childID int64) (*int64, *string, error) {
	if parent == nil || *parent == "" {
		if childType == "subtask" {
			return nil, nil, invalid("parent", "a subtask needs a parent story, task or bug")
		}
		return nil, nil, nil
	}
	if childType == "epic" {
		return nil, nil, invalid("parent", "an epic cannot have a parent")
	}
	pk, num, ok := splitKey(*parent)
	if !ok || pk != proj.key {
		return nil, nil, invalid("parent", "must be an issue key in project %s", proj.key)
	}
	var pid int64
	var ptype string
	err := q.QueryRowContext(ctx, `SELECT id, type FROM issues WHERE project_id = ? AND number = ?`, proj.id, num).Scan(&pid, &ptype)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, invalid("parent", "%s does not exist", *parent)
	}
	if err != nil {
		return nil, nil, err
	}
	switch childType {
	case "subtask":
		if ptype == "epic" || ptype == "subtask" {
			return nil, nil, invalid("parent", "a subtask goes under a story, task or bug, not a %s", ptype)
		}
	default:
		if ptype != "epic" {
			return nil, nil, invalid("parent", "a %s goes under an epic, not a %s", childType, ptype)
		}
	}
	if childID != 0 {
		var one int
		err := q.QueryRowContext(ctx, `WITH RECURSIVE up(id) AS (SELECT ? UNION SELECT i.parent_id FROM issues i JOIN up ON i.id = up.id WHERE i.parent_id IS NOT NULL) SELECT 1 FROM up WHERE id = ?`, pid, childID).Scan(&one)
		if err == nil {
			return nil, nil, invalid("parent", "would make %s its own ancestor", *parent)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, err
		}
	}
	return &pid, parent, nil
}

func (s *Store) checkSprint(ctx context.Context, q dbq, proj *projectRow, id *int64) error {
	if id == nil {
		return nil
	}
	var state string
	err := q.QueryRowContext(ctx, `SELECT state FROM sprints WHERE id = ? AND project_id = ?`, *id, proj.id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("sprint_id", "sprint %d does not exist in project %s", *id, proj.key)
	}
	if err != nil {
		return err
	}
	if state == "closed" {
		return invalid("sprint_id", "sprint %d is closed", *id)
	}
	return nil
}

// ---- create ---------------------------------------------------------------------------

func (s *Store) nextRank(ctx context.Context, q dbq, projectID int64) (string, error) {
	var max sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT MAX(rank) FROM issues WHERE project_id = ?`, projectID).Scan(&max); err != nil {
		return "", err
	}
	return rankBetween(max.String, ""), nil
}

func (s *Store) createInTx(ctx context.Context, tx dbq, a *Principal, proj *projectRow, in *CreateIssue) (*Issue, error) {
	parentID, _, err := s.checkParent(ctx, tx, proj, in.Type, in.Parent, 0)
	if err != nil {
		return nil, err
	}
	if err := s.checkSprint(ctx, tx, proj, in.SprintID); err != nil {
		return nil, err
	}
	rank, err := s.nextRank(ctx, tx, proj.id)
	if err != nil {
		return nil, err
	}
	now := ts(s.now())
	var number int64
	if err := tx.QueryRowContext(ctx, `UPDATE projects SET next_number = next_number + 1, updated_at = ? WHERE id = ? RETURNING next_number - 1`, now, proj.id).Scan(&number); err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO issues (project_id, number, type, title, description, priority, status, reporter, assignee, due_date, estimate, parent_id, sprint_id, rank, created_at, updated_at, priority_since)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		proj.id, number, in.Type, in.Title, in.Description, in.Priority, proj.wf.Initial, a.Subject, in.Assignee, in.DueDate, in.Estimate, parentID, in.SprintID, rank, now, now, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if err := s.maybeRebalance(ctx, tx, proj.id, rank); err != nil {
		return nil, err
	}
	for _, l := range dedupe(in.Labels) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO issue_labels (issue_id, label) VALUES (?, ?)`, id, l); err != nil {
			return nil, err
		}
	}
	watchers := []string{a.Subject}
	if in.Assignee != nil && *in.Assignee != a.Subject {
		watchers = append(watchers, *in.Assignee)
	}
	for _, w := range watchers {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO watchers (issue_id, subject) VALUES (?, ?)`, id, w); err != nil {
			return nil, err
		}
	}
	row, err := s.loadIssueByID(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := s.addActivity(ctx, tx, id, a.Subject, "created", map[string]any{"type": in.Type, "priority": in.Priority}); err != nil {
		return nil, err
	}
	if err := s.emit(ctx, tx, EventIssueCreated, issueCreatedData{
		IssueKey: row.Key, Project: row.Project, Type: row.Type, Title: row.Title, Priority: row.Priority,
		Status: row.Status, Reporter: row.Reporter, Assignee: row.Assignee, Parent: row.Parent, CreatedAt: row.CreatedAt,
	}); err != nil {
		return nil, err
	}
	if row.Priority == "P0" && !in.imported && row.StatusCategory != CategoryDone {
		if err := s.emit(ctx, tx, EventIssueP0, issueP0Data{IssueID: row.Key, Project: row.Project, Assignee: row.Assignee}); err != nil {
			return nil, err
		}
	}
	return &row.Issue, nil
}

// idemRun runs fn at most once per (subject, key). A retry with the same key and the same
// request returns the stored response and replayed=true; the same key with a different
// request is an error. An empty key disables the check. It must run inside a transaction.
func idemRun[T any](ctx context.Context, s *Store, tx dbq, subject, key, scope string, req any, fn func() (T, error)) (v T, replayed bool, err error) {
	if key == "" {
		v, err = fn()
		return v, false, err
	}
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return v, false, err
	}
	sum := sha256.Sum256(append([]byte(scope+"\x00"), reqJSON...))
	hash := hex.EncodeToString(sum[:])
	var oldHash, resp string
	err = tx.QueryRowContext(ctx, `SELECT request_hash, response FROM idempotency WHERE subject = ? AND key = ?`, subject, key).Scan(&oldHash, &resp)
	switch {
	case err == nil:
		if oldHash != hash {
			return v, false, ErrIdempotencyMismatch
		}
		if err := json.Unmarshal([]byte(resp), &v); err != nil {
			return v, false, err
		}
		return v, true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return v, false, err
	}
	if v, err = fn(); err != nil {
		return v, false, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return v, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO idempotency (subject, key, request_hash, response, status, created_at) VALUES (?, ?, ?, ?, 201, ?)`, subject, key, hash, string(b), ts(s.now()))
	return v, false, err
}

// CreateIssue creates an issue in the initial status. idemKey (the Idempotency-Key header)
// wins over the body's idempotency_key. It returns replayed=true when the key was seen.
func (s *Store) CreateIssue(ctx context.Context, a *Principal, project string, in CreateIssue, idemKey string) (*Issue, bool, error) {
	if err := in.validate(); err != nil {
		return nil, false, err
	}
	if idemKey == "" {
		idemKey = in.IdempotencyKey
	}
	in.IdempotencyKey = ""
	var out *Issue
	var replayed bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		proj, err := s.writableProject(ctx, tx, a, project)
		if err != nil {
			return err
		}
		out, replayed, err = idemRun(ctx, s, tx, a.Subject, idemKey, "issue:"+project, in, func() (*Issue, error) {
			return s.createInTx(ctx, tx, a, proj, &in)
		})
		return err
	})
	if err == nil && !replayed {
		issuesCreated.With().Inc()
	}
	return out, replayed, err
}

// issuesCreated counts issues created (a replayed idempotent request is not a new issue). No
// project key, title or user is ever a label.
var issuesCreated = obs.Default.Counter("can_work_issues_created_total", "Issues created.")

var batchRefRE = regexp.MustCompile(`^\$([0-9]{1,3})$`)

// batchRef reads a parent of the form "$N": the Nth item (from 0) of the same bulk request,
// so a planning list can say "this story belongs under the epic above" without knowing keys.
func batchRef(parent *string) (int, bool) {
	if parent == nil {
		return 0, false
	}
	m := batchRefRE.FindStringSubmatch(*parent)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	return n, true
}

// BulkCreate creates up to 100 issues atomically: all or none. The whole request is
// idempotent on idemKey, which is what a retrying planner script should send.
func (s *Store) BulkCreate(ctx context.Context, a *Principal, project string, items []CreateIssue, idemKey string) ([]Issue, bool, error) {
	if len(items) == 0 || len(items) > maxBulk {
		return nil, false, invalid("issues", "give between 1 and %d issues", maxBulk)
	}
	var p problems
	for i := range items {
		check := items[i]
		if n, ok := batchRef(check.Parent); ok {
			if n >= i {
				p.add(fmt.Sprintf("issues[%d].parent", i), "%s must refer to an earlier item in the same request", *check.Parent)
			}
			check.Parent = nil // the hierarchy is checked when the parent exists
		}
		err := check.validate()
		items[i].Priority = check.Priority // validate fills the default
		if err != nil {
			var ie *InvalidError
			if errors.As(err, &ie) {
				for _, pf := range ie.Problems {
					p.add(fmt.Sprintf("issues[%d].%s", i, pf.Field), "%s", pf.Message)
				}
			}
		}
		items[i].IdempotencyKey = ""
	}
	if err := p.err(); err != nil {
		return nil, false, err
	}
	var out []Issue
	var replayed bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		proj, err := s.writableProject(ctx, tx, a, project)
		if err != nil {
			return err
		}
		out, replayed, err = idemRun(ctx, s, tx, a.Subject, idemKey, "bulk:"+project, items, func() ([]Issue, error) {
			res := make([]Issue, 0, len(items))
			for i := range items {
				if n, ok := batchRef(items[i].Parent); ok {
					items[i].Parent = &res[n].Key
				}
				is, err := s.createInTx(ctx, tx, a, proj, &items[i])
				if err != nil {
					var ie *InvalidError
					if errors.As(err, &ie) {
						var q problems
						for _, pf := range ie.Problems {
							q.add(fmt.Sprintf("issues[%d].%s", i, pf.Field), "%s", pf.Message)
						}
						return nil, q.err()
					}
					return nil, err
				}
				res = append(res, *is)
			}
			return res, nil
		})
		return err
	})
	if err == nil && !replayed {
		issuesCreated.With().Add(float64(len(out)))
	}
	return out, replayed, err
}

// ---- read -----------------------------------------------------------------------------

// GetIssue returns an issue with its relations.
func (s *Store) GetIssue(ctx context.Context, a *Principal, key string) (*IssueDetail, error) {
	row, err := s.loadIssue(ctx, s.db, a, key)
	if err != nil {
		return nil, err
	}
	d := &IssueDetail{Issue: row.Issue, Children: []string{}, Links: []Link{}, Watchers: []string{}, Attachments: []Attachment{}}
	rs, err := s.db.QueryContext(ctx, `SELECT p.key || '-' || c.number FROM issues c JOIN projects p ON p.id = c.project_id WHERE c.parent_id = ? ORDER BY c.rank`, row.id)
	if err != nil {
		return nil, err
	}
	for rs.Next() {
		var k string
		if err := rs.Scan(&k); err != nil {
			_ = rs.Close()
			return nil, err
		}
		d.Children = append(d.Children, k)
	}
	if err := rs.Close(); err != nil {
		return nil, err
	}
	if d.Links, err = s.linksFor(ctx, s.db, a, row.id); err != nil {
		return nil, err
	}
	ws, err := s.db.QueryContext(ctx, `SELECT subject FROM watchers WHERE issue_id = ? ORDER BY subject`, row.id)
	if err != nil {
		return nil, err
	}
	for ws.Next() {
		var w string
		if err := ws.Scan(&w); err != nil {
			_ = ws.Close()
			return nil, err
		}
		d.Watchers = append(d.Watchers, w)
	}
	if err := ws.Close(); err != nil {
		return nil, err
	}
	if d.Attachments, err = s.attachmentsFor(ctx, s.db, row.id); err != nil {
		return nil, err
	}
	return d, nil
}

var sortClauses = map[string]string{
	"created":   "i.id ASC",
	"-created":  "i.id DESC",
	"updated":   "i.updated_at ASC, i.id ASC",
	"-updated":  "i.updated_at DESC, i.id DESC",
	"priority":  "i.priority ASC, i.id DESC",
	"due":       "i.due_date IS NULL, i.due_date ASC, i.id ASC",
	"rank":      "i.rank ASC, i.id ASC",
	"-priority": "i.priority DESC, i.id DESC",
}

const (
	maxOffset    = 10000
	queryTimeout = 4 * time.Second
)

// ListIssues searches issues the caller may see. q is the search language (query.go);
// project narrows to one project; the result has at most limit items and next is the
// offset to continue from (-1 when done).
func (s *Store) ListIssues(ctx context.Context, a *Principal, project, q, sortBy string, limit, offset int) (items []Issue, next int, err error) {
	limit = clampLimit(limit)
	if offset < 0 || offset > maxOffset {
		return nil, -1, invalid("cursor", "out of range")
	}
	order, ok := sortClauses[sortBy]
	if sortBy == "" {
		order, ok = sortClauses["-created"], true
	}
	if !ok {
		return nil, -1, invalid("sort", "must be one of created, -created, updated, -updated, priority, -priority, due, rank")
	}
	f, err := ParseQueryScoped(q, s.now(), a.Subject, a.Projects)
	if err != nil {
		return nil, -1, invalid("q", "%v", err)
	}
	where, args := f.SQL()
	where = "(" + where + ")" // the scope below is ANDed to the whole query, never to part of it
	if project != "" {
		if !projectKeyRE.MatchString(project) {
			return nil, -1, invalid("project", "must be a project key")
		}
		where += " AND p.key = ?"
		args = append(args, project)
	}
	if !a.Unscoped() {
		where += " AND p.key IN (" + placeholders(len(a.Projects)) + ")"
		for _, k := range a.Projects {
			args = append(args, k)
		}
	}
	// Searches run on the read pool with a deadline: a slow query neither blocks writers nor
	// holds a connection for longer than queryTimeout.
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	rows, err := s.rdb.QueryContext(ctx, issueSelect+` WHERE `+where+` ORDER BY `+order+` LIMIT ? OFFSET ?`, append(args, limit+1, offset)...)
	if err != nil {
		return nil, -1, err
	}
	var res []*issueRow
	for rows.Next() {
		r, err := scanIssue(rows)
		if err != nil {
			_ = rows.Close()
			return nil, -1, err
		}
		res = append(res, r)
	}
	if err := rows.Close(); err != nil {
		return nil, -1, err
	}
	next = -1
	if len(res) > limit {
		res = res[:limit]
		next = offset + limit
	}
	if err := s.fillLabels(ctx, s.rdb, res); err != nil {
		return nil, -1, err
	}
	items = make([]Issue, len(res))
	for i, r := range res {
		items[i] = r.Issue
	}
	return items, next, nil
}

// ---- update ---------------------------------------------------------------------------

func eqStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func eqFloatPtr(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func eqIntPtr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// UpdateIssue applies a patch. ifMatch is the version from If-Match; a version inside the
// patch is the same guard. A stale version fails with ErrVersionMismatch.
func (s *Store) UpdateIssue(ctx context.Context, a *Principal, key string, pt *Patch, ifMatch *int64) (*Issue, error) {
	if err := pt.validate(); err != nil {
		return nil, err
	}
	var out *Issue
	err := s.inTx(ctx, func(tx *sql.Tx) (err error) {
		out, err = s.updateTx(ctx, tx, a, key, pt, ifMatch)
		return err
	})
	return out, err
}

func (s *Store) updateTx(ctx context.Context, tx dbq, a *Principal, key string, pt *Patch, ifMatch *int64) (*Issue, error) {
	row, err := s.loadIssue(ctx, tx, a, key)
	if err != nil {
		return nil, err
	}
	if row.archived {
		return nil, fmt.Errorf("%w: project %s is archived", ErrConflict, row.Project)
	}
	want := ifMatch
	if want == nil && pt.Version.Set && !pt.Version.Null {
		v := pt.Version.V
		want = &v
	}
	if want != nil && *want != row.Version {
		return nil, ErrVersionMismatch
	}
	proj := &projectRow{id: row.projectID, key: row.Project}
	n := row.Issue
	parentID := row.parentID
	changes := map[string]Change{}

	if pt.Title.Set && pt.Title.V != n.Title {
		changes["title"] = Change{n.Title, pt.Title.V}
		n.Title = pt.Title.V
	}
	if pt.Description.Set && pt.Description.V != n.Description {
		changes["description"] = Change{"(changed)", "(changed)"}
		n.Description = pt.Description.V
	}
	if pt.Priority.Set && pt.Priority.V != n.Priority {
		changes["priority"] = Change{n.Priority, pt.Priority.V}
		n.Priority = pt.Priority.V
	}
	if pt.Assignee.Set {
		var v *string
		if !pt.Assignee.Null {
			v = &pt.Assignee.V
		}
		if !eqStrPtr(v, n.Assignee) {
			changes["assignee"] = Change{n.Assignee, v}
			n.Assignee = v
		}
	}
	if pt.DueDate.Set {
		var v *string
		if !pt.DueDate.Null {
			v = &pt.DueDate.V
		}
		if !eqStrPtr(v, n.DueDate) {
			changes["due_date"] = Change{n.DueDate, v}
			n.DueDate = v
		}
	}
	if pt.Estimate.Set {
		var v *float64
		if !pt.Estimate.Null {
			v = &pt.Estimate.V
		}
		if !eqFloatPtr(v, n.Estimate) {
			changes["estimate"] = Change{n.Estimate, v}
			n.Estimate = v
		}
	}
	if pt.Parent.Set {
		var v *string
		if !pt.Parent.Null {
			v = &pt.Parent.V
		}
		pid, pkey, err := s.checkParent(ctx, tx, proj, row.Type, v, row.id)
		if err != nil {
			return nil, err
		}
		if !eqStrPtr(pkey, n.Parent) {
			changes["parent"] = Change{n.Parent, pkey}
			n.Parent, parentID = pkey, pid
		}
	}
	if pt.SprintID.Set {
		var v *int64
		if !pt.SprintID.Null {
			v = &pt.SprintID.V
		}
		if !eqIntPtr(v, n.SprintID) {
			if err := s.checkSprint(ctx, tx, proj, v); err != nil {
				return nil, err
			}
			changes["sprint_id"] = Change{n.SprintID, v}
			n.SprintID = v
		}
	}
	var newLabels []string
	switch {
	case pt.Labels.Set:
		newLabels = dedupe(pt.Labels.V)
	case len(pt.LabelsAdd) > 0 || len(pt.LabelsRemove) > 0:
		set := slices.Clone(n.Labels)
		set = append(set, pt.LabelsAdd...)
		set = slices.DeleteFunc(set, func(l string) bool { return slices.Contains(pt.LabelsRemove, l) })
		newLabels = dedupe(set)
	default:
		newLabels = n.Labels
	}
	if len(newLabels) > maxLabels {
		return nil, invalid("labels", "at most %d labels", maxLabels)
	}
	if !slices.Equal(newLabels, row.Labels) {
		changes["labels"] = Change{row.Labels, newLabels}
	}
	if len(changes) == 0 {
		return &row.Issue, nil
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE issues SET title = ?, description = ?, priority = ?, assignee = ?, due_date = ?, estimate = ?, parent_id = ?, sprint_id = ?, version = version + 1, updated_at = ? WHERE id = ?`,
		n.Title, n.Description, n.Priority, n.Assignee, n.DueDate, n.Estimate, parentID, n.SprintID, ts(now), row.id); err != nil {
		return nil, err
	}
	if _, ok := changes["labels"]; ok {
		if _, err := tx.ExecContext(ctx, `DELETE FROM issue_labels WHERE issue_id = ?`, row.id); err != nil {
			return nil, err
		}
		for _, l := range newLabels {
			if _, err := tx.ExecContext(ctx, `INSERT INTO issue_labels (issue_id, label) VALUES (?, ?)`, row.id, l); err != nil {
				return nil, err
			}
		}
	}
	if n.Assignee != nil {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO watchers (issue_id, subject) VALUES (?, ?)`, row.id, *n.Assignee); err != nil {
			return nil, err
		}
	}
	if err := s.addActivity(ctx, tx, row.id, a.Subject, "updated", changes); err != nil {
		return nil, err
	}
	out, err := s.loadIssueByID(ctx, tx, row.id)
	if err != nil {
		return nil, err
	}
	if err := s.emit(ctx, tx, EventIssueUpdated, issueUpdatedData{
		IssueKey: out.Key, Project: out.Project, Version: out.Version, Actor: a.Subject, Changes: changes, UpdatedAt: out.UpdatedAt,
	}); err != nil {
		return nil, err
	}
	if _, ok := changes["priority"]; ok {
		if err := s.priorityChanged(ctx, tx, a, row.id, row.Priority, &out.Issue); err != nil {
			return nil, err
		}
	}
	_, raised := changes["priority"]
	_, handed := changes["assignee"]
	if out.Priority == "P0" && (raised || (handed && out.Assignee != nil)) && out.StatusCategory != CategoryDone {
		if err := s.emit(ctx, tx, EventIssueP0, issueP0Data{IssueID: out.Key, Project: out.Project, Assignee: out.Assignee}); err != nil {
			return nil, err
		}
	}
	return &out.Issue, nil
}

// Transition moves an issue along its project's workflow.
func (s *Store) Transition(ctx context.Context, a *Principal, key, to string, ifMatch *int64) (*Issue, error) {
	if !statusNameRE.MatchString(to) {
		return nil, invalid("to", "must be a status name")
	}
	var out *Issue
	err := s.inTx(ctx, func(tx *sql.Tx) (err error) {
		out, err = s.transitionTx(ctx, tx, a, key, to, ifMatch)
		return err
	})
	return out, err
}

func (s *Store) transitionTx(ctx context.Context, tx dbq, a *Principal, key, to string, ifMatch *int64) (*Issue, error) {
	row, err := s.loadIssue(ctx, tx, a, key)
	if err != nil {
		return nil, err
	}
	if row.archived {
		return nil, fmt.Errorf("%w: project %s is archived", ErrConflict, row.Project)
	}
	if ifMatch != nil && *ifMatch != row.Version {
		return nil, ErrVersionMismatch
	}
	if row.wf.Category(to) == "" {
		return nil, invalid("to", "%q is not a status of project %s", to, row.Project)
	}
	if !row.wf.Allows(row.Status, to) {
		return nil, fmt.Errorf("%w: cannot move %s from %s to %s (allowed: %s)", ErrConflict, row.Key, row.Status, to, strings.Join(row.wf.Next(row.Status), ", "))
	}
	now := s.now()
	var resolved any
	if row.wf.Category(to) == CategoryDone {
		resolved = ts(now)
		if row.ResolvedAt != nil {
			resolved = ts(*row.ResolvedAt)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE issues SET status = ?, resolved_at = ?, version = version + 1, updated_at = ? WHERE id = ?`, to, resolved, ts(now), row.id); err != nil {
		return nil, err
	}
	if err := s.addActivity(ctx, tx, row.id, a.Subject, "transitioned", map[string]string{"from": row.Status, "to": to}); err != nil {
		return nil, err
	}
	out, err := s.loadIssueByID(ctx, tx, row.id)
	if err != nil {
		return nil, err
	}
	if err := s.emit(ctx, tx, EventIssueTransition, issueTransitionedData{
		IssueKey: out.Key, Project: out.Project, From: row.Status, To: to, ToCategory: out.StatusCategory, Actor: a.Subject, TransitionedAt: out.UpdatedAt,
	}); err != nil {
		return nil, err
	}
	return &out.Issue, nil
}

// AllowedTransitions lists the statuses an issue can move to now.
func (s *Store) AllowedTransitions(ctx context.Context, a *Principal, key string) ([]string, error) {
	row, err := s.loadIssue(ctx, s.db, a, key)
	if err != nil {
		return nil, err
	}
	return row.wf.Next(row.Status), nil
}

// BulkResult is the outcome for one key of a bulk update.
type BulkResult struct {
	Key     string `json:"key"`
	OK      bool   `json:"ok"`
	Code    string `json:"code,omitempty"`
	Error   string `json:"error,omitempty"`
	Version int64  `json:"version,omitempty"`
}

// ErrorCode names an error class for API responses.
func ErrorCode(err error) string {
	var ie *InvalidError
	switch {
	case errors.As(err, &ie):
		return "invalid"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrVersionMismatch):
		return "version_mismatch"
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, ErrForbidden):
		return "forbidden"
	case errors.Is(err, ErrIdempotencyMismatch):
		return "idempotency_mismatch"
	}
	return "internal"
}

// BulkUpdate applies the same patch and/or transition to up to 100 issues. Each issue
// succeeds or fails on its own; the response says which. Optimistic versions are not
// accepted here: the same version cannot apply to many issues.
func (s *Store) BulkUpdate(ctx context.Context, a *Principal, keys []string, set *Patch, transition string) ([]BulkResult, error) {
	if len(keys) == 0 || len(keys) > maxBulk {
		return nil, invalid("keys", "give between 1 and %d issue keys", maxBulk)
	}
	if set == nil {
		set = &Patch{}
	}
	if set.Version.Set {
		return nil, invalid("set.version", "not supported in bulk updates")
	}
	if set.empty() && transition == "" {
		return nil, invalid("set", "give set and/or transition")
	}
	if set.Parent.Set {
		return nil, invalid("set.parent", "not supported in bulk updates")
	}
	if err := set.validate(); err != nil {
		return nil, err
	}
	if transition != "" && !statusNameRE.MatchString(transition) {
		return nil, invalid("transition", "must be a status name")
	}
	seen := map[string]bool{}
	res := make([]BulkResult, 0, len(keys))
	for _, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		r := BulkResult{Key: k}
		err := s.inTx(ctx, func(tx *sql.Tx) error {
			var last *Issue
			if !set.empty() {
				is, err := s.updateTx(ctx, tx, a, k, set, nil)
				if err != nil {
					return err
				}
				last = is
			}
			if transition != "" {
				is, err := s.transitionTx(ctx, tx, a, k, transition, nil)
				if err != nil {
					return err
				}
				last = is
			}
			r.Version = last.Version
			return nil
		})
		if err != nil {
			r.Code, r.Error = ErrorCode(err), err.Error()
			if r.Code == "internal" {
				r.Error = "internal error"
			}
		} else {
			r.OK = true
		}
		res = append(res, r)
	}
	return res, nil
}
