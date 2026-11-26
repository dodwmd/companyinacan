package work

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// The manager views (specs/065-work-manager): who is on what, how old the urgent work is and what
// has stopped moving. All three read the unresolved issues of the projects the caller may see,
// archived projects left out, in one pass on the read pool, and keep only counts and a bounded list.

const (
	reportPeople  = 500 // people listed in the workload
	reportCurrent = 5   // in-progress issues named per person
	reportItems   = 200 // issues listed in ageing and stale work

	// DefaultStaleDays is how long an issue in progress may be quiet before it counts as stale.
	DefaultStaleDays = 7
	maxStaleDays     = 365
)

// ReportIssue is an issue as the reports name it.
type ReportIssue struct {
	Key      string  `json:"key"`
	Title    string  `json:"title"`
	Priority string  `json:"priority"`
	Status   string  `json:"status"`
	Category string  `json:"status_category"`
	Assignee *string `json:"assignee"`
}

// WorkloadRow is one person's unresolved work; an empty Person is the unassigned work.
type WorkloadRow struct {
	Person     string        `json:"person"`
	Open       int           `json:"open"`
	P0         int           `json:"p0"`
	P1         int           `json:"p1"`
	P2         int           `json:"p2"`
	P3         int           `json:"p3"`
	InProgress int           `json:"in_progress"`
	Blocked    int           `json:"blocked"`
	Overdue    int           `json:"overdue"`
	Current    []ReportIssue `json:"current"`
}

// Workload is who is on what. Epics are containers and are not counted.
type Workload struct {
	Rows      []WorkloadRow `json:"items"`
	Truncated bool          `json:"truncated"`
}

// AgeBands counts unresolved issues of one priority by how long they have had it.
type AgeBands struct {
	Priority   string `json:"priority"`
	Under2d    int    `json:"under_2_days"`
	Under7d    int    `json:"days_2_to_7"`
	Under28d   int    `json:"days_7_to_28"`
	Over28d    int    `json:"over_28_days"`
	PastTarget int    `json:"past_target"`
}

// AgedIssue is an unresolved P0 or P1.
type AgedIssue struct {
	ReportIssue
	CreatedAt     time.Time `json:"created_at"`
	PrioritySince time.Time `json:"priority_since"`
	OpenDays      int       `json:"open_days"`
	PriorityDays  int       `json:"priority_days"`
	PastTarget    bool      `json:"past_target"` // longer than the project's resolution hint
}

// Ageing is the unresolved P0 and P1 issues, highest priority and longest at it first.
type Ageing struct {
	Bands     []AgeBands  `json:"bands"`
	Items     []AgedIssue `json:"items"`
	Truncated bool        `json:"truncated"`
}

// StaleIssue is an issue in progress that nothing has happened to.
type StaleIssue struct {
	ReportIssue
	LastActivity time.Time `json:"last_activity"`
	QuietDays    int       `json:"quiet_days"`
}

// Stale is the in-progress work that has been quiet for Days days or more, quietest first.
type Stale struct {
	Days      int          `json:"days"`
	Items     []StaleIssue `json:"items"`
	Truncated bool         `json:"truncated"`
}

type reportProject struct {
	wf  Workflow
	sla SLA
}

type reportRow struct {
	ReportIssue
	due                    sql.NullString
	created, since, lastAt int64
	blocked                bool
}

func (s *Store) reportProjects(ctx context.Context) (map[int64]*reportProject, error) {
	rs, err := s.rdb.QueryContext(ctx, `SELECT id, workflow, sla FROM projects`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	out := map[int64]*reportProject{}
	for rs.Next() {
		var id int64
		var wf, sla string
		if err := rs.Scan(&id, &wf, &sla); err != nil {
			return nil, err
		}
		p := &reportProject{}
		if err := json.Unmarshal([]byte(wf), &p.wf); err != nil {
			return nil, fmt.Errorf("stored workflow: %w", err)
		}
		if err := json.Unmarshal([]byte(sla), &p.sla); err != nil {
			return nil, fmt.Errorf("stored sla: %w", err)
		}
		out[id] = p
	}
	return out, rs.Err()
}

// eachOpen calls fn for every unresolved issue the caller may see (of one project when named), in
// the order given. extra and order are fixed SQL fragments of this file, never input.
func (s *Store) eachOpen(ctx context.Context, a *Principal, project, extra, order string, fn func(*reportRow, *reportProject)) error {
	if project != "" {
		if _, err := s.loadProject(ctx, s.db, a, project); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	projects, err := s.reportProjects(ctx)
	if err != nil {
		return err
	}
	// A blocker in a project outside the caller's scope is not counted: it would reveal it.
	var scope []any
	for _, k := range a.Projects {
		scope = append(scope, k)
	}
	blocked := `EXISTS (SELECT 1 FROM issue_links k JOIN issues b ON b.id = k.source_id JOIN projects bp ON bp.id = b.project_id
		WHERE k.type = 'blocks' AND k.target_id = i.id AND b.resolved_at IS NULL`
	where := `i.resolved_at IS NULL AND p.archived = 0`
	var args []any
	if len(scope) > 0 {
		blocked += ` AND bp.key IN (` + placeholders(len(scope)) + `)`
		where += ` AND p.key IN (` + placeholders(len(scope)) + `)`
		args = append(append(args, scope...), scope...)
	}
	if project != "" {
		where += ` AND p.key = ?`
		args = append(args, project)
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT i.project_id, p.key, i.number, i.title, i.priority, i.status, i.assignee, i.due_date,
		i.created_at, i.priority_since,
		COALESCE((SELECT MAX(h.at) FROM activity h WHERE h.issue_id = i.id AND h.kind <> 'ranked'), i.updated_at) AS last_at,
		`+blocked+`)
		FROM issues i JOIN projects p ON p.id = i.project_id WHERE `+where+extra+` ORDER BY `+order, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r reportRow
		var pid, number int64
		var key string
		var assignee sql.NullString
		if err := rows.Scan(&pid, &key, &number, &r.Title, &r.Priority, &r.Status, &assignee, &r.due, &r.created, &r.since, &r.lastAt, &r.blocked); err != nil {
			return err
		}
		p := projects[pid]
		if p == nil {
			continue // created between the two reads
		}
		r.Key = key + "-" + strconv.FormatInt(number, 10)
		r.Assignee, r.Category = nullStr(assignee), p.wf.Category(r.Status)
		if r.since == 0 {
			r.since = r.created
		}
		fn(&r, p)
	}
	return rows.Err()
}

// Workload returns each person's unresolved work. Members of the projects looked at who have none
// are listed too. The most urgent come first, the unassigned work last.
func (s *Store) Workload(ctx context.Context, a *Principal, project string) (*Workload, error) {
	people := map[string]*WorkloadRow{}
	row := func(name string) *WorkloadRow {
		w := people[name]
		if w == nil {
			w = &WorkloadRow{Person: name, Current: []ReportIssue{}}
			people[name] = w
		}
		return w
	}
	today := s.now().UTC().Format(time.DateOnly)
	err := s.eachOpen(ctx, a, project, ` AND i.type <> 'epic'`, `i.priority, i.rank, i.id`, func(r *reportRow, _ *reportProject) {
		name := ""
		if r.Assignee != nil {
			name = *r.Assignee
		}
		w := row(name)
		w.Open++
		switch r.Priority {
		case "P0":
			w.P0++
		case "P1":
			w.P1++
		case "P2":
			w.P2++
		default:
			w.P3++
		}
		if r.blocked {
			w.Blocked++
		}
		if r.due.Valid && r.due.String < today {
			w.Overdue++
		}
		if r.Category == CategoryActive {
			w.InProgress++
			if len(w.Current) < reportCurrent {
				w.Current = append(w.Current, r.ReportIssue)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	if err := s.eachMember(ctx, a, project, func(subject string) { row(subject) }); err != nil {
		return nil, err
	}
	out := &Workload{Rows: make([]WorkloadRow, 0, len(people))}
	for _, w := range people {
		out.Rows = append(out.Rows, *w)
	}
	slices.SortFunc(out.Rows, func(x, y WorkloadRow) int {
		return cmp.Or(
			cmp.Compare(boolInt(x.Person == ""), boolInt(y.Person == "")),
			cmp.Compare(y.P0, x.P0), cmp.Compare(y.P1, x.P1), cmp.Compare(y.Open, x.Open),
			cmp.Compare(x.Person, y.Person))
	})
	if len(out.Rows) > reportPeople {
		out.Rows, out.Truncated = out.Rows[:reportPeople], true
	}
	return out, nil
}

// eachMember calls fn for the members of the unarchived projects the caller may see.
func (s *Store) eachMember(ctx context.Context, a *Principal, project string, fn func(subject string)) error {
	rs, err := s.rdb.QueryContext(ctx, `SELECT p.key, m.subject FROM project_members m JOIN projects p ON p.id = m.project_id WHERE p.archived = 0`)
	if err != nil {
		return err
	}
	defer func() { _ = rs.Close() }()
	for rs.Next() {
		var key, subject string
		if err := rs.Scan(&key, &subject); err != nil {
			return err
		}
		if a.CanProject(key) && (project == "" || project == key) {
			fn(subject)
		}
	}
	return rs.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func wholeDays(now time.Time, since int64) int {
	return max(0, int(now.Sub(fromUnix(since))/(24*time.Hour)))
}

// Ageing returns the unresolved P0 and P1 issues and how long they have been that urgent.
func (s *Store) Ageing(ctx context.Context, a *Principal, project string) (*Ageing, error) {
	now := s.now()
	out := &Ageing{Bands: []AgeBands{{Priority: "P0"}, {Priority: "P1"}}, Items: []AgedIssue{}}
	err := s.eachOpen(ctx, a, project, ` AND i.priority IN ('P0', 'P1')`, `i.priority, i.priority_since, i.id`, func(r *reportRow, p *reportProject) {
		b := &out.Bands[0]
		if r.Priority == "P1" {
			b = &out.Bands[1]
		}
		age := now.Sub(fromUnix(r.since))
		switch {
		case age < 48*time.Hour:
			b.Under2d++
		case age < 7*24*time.Hour:
			b.Under7d++
		case age < 28*24*time.Hour:
			b.Under28d++
		default:
			b.Over28d++
		}
		target := p.sla[r.Priority].ResolveHours
		past := target > 0 && age > time.Duration(target)*time.Hour
		if past {
			b.PastTarget++
		}
		if len(out.Items) == reportItems {
			out.Truncated = true
			return
		}
		out.Items = append(out.Items, AgedIssue{ReportIssue: r.ReportIssue, CreatedAt: fromUnix(r.created), PrioritySince: fromUnix(r.since),
			OpenDays: wholeDays(now, r.created), PriorityDays: wholeDays(now, r.since), PastTarget: past})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// StaleWork returns the issues in progress with no activity for at least days days (0 means the
// default). Moving a card up or down is not activity.
func (s *Store) StaleWork(ctx context.Context, a *Principal, project string, days int) (*Stale, error) {
	if days == 0 {
		days = DefaultStaleDays
	}
	if days < 1 || days > maxStaleDays {
		return nil, invalid("days", "must be between 1 and %d", maxStaleDays)
	}
	now := s.now()
	cutoff := ts(now.Add(-time.Duration(days) * 24 * time.Hour))
	out := &Stale{Days: days, Items: []StaleIssue{}}
	err := s.eachOpen(ctx, a, project, ` AND i.type <> 'epic'`, `last_at, i.id`, func(r *reportRow, _ *reportProject) {
		if r.Category != CategoryActive || r.lastAt > cutoff {
			return
		}
		if len(out.Items) == reportItems {
			out.Truncated = true
			return
		}
		out.Items = append(out.Items, StaleIssue{ReportIssue: r.ReportIssue, LastActivity: fromUnix(r.lastAt), QuietDays: wholeDays(now, r.lastAt)})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
