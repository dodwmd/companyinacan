package work

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// dbq is what both *sql.DB and *sql.Tx offer; helpers take it so they work in either.
type dbq interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store is the SQLite-backed work store. All rules (project scoping, hierarchy, workflow,
// concurrency) live here so every API path gets them.
type Store struct {
	db  *sql.DB
	now func() time.Time
	// events turns the outbox on; with no webhook configured nothing is queued.
	events bool
	kick   chan struct{}
}

// OpenStore opens (creating if needed) the database at path and applies migrations.
func OpenStore(path string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "secure_delete(ON)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// One connection: serialises writes (so counters and rank neighbours cannot race) and
	// keeps idle memory minimal.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now, kick: make(chan struct{}, 1)}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	type mig struct {
		version int
		name    string
	}
	var migs []mig
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil {
			return fmt.Errorf("bad migration name %q", e.Name())
		}
		migs = append(migs, mig{v, e.Name()})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	cur, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	for _, m := range migs {
		if m.version <= cur {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + m.name)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, m.version, s.now().Unix()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion returns the highest applied migration (0 when none).
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

// inTx runs fn in a transaction, committing on success. Only the tx may be used inside fn:
// the pool has one connection, so touching s.db would deadlock.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	select {
	case s.kick <- struct{}{}:
	default:
	}
	return nil
}

func ts(t time.Time) int64 { return t.Unix() }

func fromUnix(v int64) time.Time { return time.Unix(v, 0).UTC() }

func nullTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromUnix(v.Int64)
	return &t
}

func nullStr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

// ---- projects -------------------------------------------------------------------------

type projectRow struct {
	id       int64
	key      string
	archived bool
	wf       Workflow
	sla      SLA
}

func (s *Store) loadProject(ctx context.Context, q dbq, a *Principal, key string) (*projectRow, error) {
	if !projectKeyRE.MatchString(key) || !a.CanProject(key) {
		return nil, ErrNotFound
	}
	var (
		r        projectRow
		wf, sla  string
		archived int
	)
	err := q.QueryRowContext(ctx, `SELECT id, key, archived, workflow, sla FROM projects WHERE key = ?`, key).Scan(&r.id, &r.key, &archived, &wf, &sla)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.archived = archived == 1
	if err := json.Unmarshal([]byte(wf), &r.wf); err != nil {
		return nil, fmt.Errorf("project %s: stored workflow: %w", key, err)
	}
	if err := json.Unmarshal([]byte(sla), &r.sla); err != nil {
		return nil, fmt.Errorf("project %s: stored sla: %w", key, err)
	}
	return &r, nil
}

func (s *Store) writableProject(ctx context.Context, q dbq, a *Principal, key string) (*projectRow, error) {
	p, err := s.loadProject(ctx, q, a, key)
	if err != nil {
		return nil, err
	}
	if p.archived {
		return nil, fmt.Errorf("%w: project %s is archived", ErrConflict, key)
	}
	return p, nil
}

// NewProject is the body of a project create.
type NewProject struct {
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Workflow    *Workflow `json:"workflow"`
	SLA         SLA       `json:"sla"`
}

// CreateProject creates a project. Only an unscoped admin may: a project-scoped token
// cannot create projects outside its own scope.
func (s *Store) CreateProject(ctx context.Context, a *Principal, in NewProject) (*Project, error) {
	if !a.Unscoped() {
		return nil, ErrForbidden
	}
	var p problems
	if !projectKeyRE.MatchString(in.Key) {
		p.add("key", "must match %s (2-10 upper-case letters or digits, starting with a letter)", projectKeyRE)
	}
	if strings.TrimSpace(in.Name) == "" || !validText(in.Name, 100) || strings.ContainsAny(in.Name, "\r\n") {
		p.add("name", "required, one line, at most 100 characters")
	}
	if !validText(in.Description, 2000) {
		p.add("description", "at most 2000 characters")
	}
	wf := DefaultWorkflow()
	if in.Workflow != nil {
		wf = *in.Workflow
	}
	if err := wf.Validate(); err != nil {
		p.add("workflow", "%v", err)
	}
	sla := in.SLA
	if sla == nil {
		sla = DefaultSLA()
	}
	if err := sla.Validate(); err != nil {
		p.add("sla", "%v", err)
	}
	if err := p.err(); err != nil {
		return nil, err
	}
	wfJSON, _ := json.Marshal(wf)
	slaJSON, _ := json.Marshal(sla)
	now := ts(s.now())
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO projects (key, name, description, workflow, sla, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			in.Key, in.Name, in.Description, string(wfJSON), string(slaJSON), now, now); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return fmt.Errorf("%w: project %s already exists", ErrConflict, in.Key)
			}
			return err
		}
		return s.audit(ctx, tx, a.Subject, "project.create", in.Key, map[string]any{"name": in.Name})
	})
	if err != nil {
		return nil, err
	}
	return s.GetProject(ctx, a, in.Key)
}

const projectSelect = `SELECT p.key, p.name, p.description, p.workflow, p.sla, p.archived, p.next_number - 1, p.created_at, p.updated_at FROM projects p`

func scanProject(sc interface{ Scan(...any) error }) (*Project, error) {
	var (
		p        Project
		wf, sla  string
		archived int
		c, u     int64
	)
	if err := sc.Scan(&p.Key, &p.Name, &p.Description, &wf, &sla, &archived, &p.IssueCount, &c, &u); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(wf), &p.Workflow); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(sla), &p.SLA); err != nil {
		return nil, err
	}
	p.Archived = archived == 1
	p.CreatedAt, p.UpdatedAt = fromUnix(c), fromUnix(u)
	return &p, nil
}

// GetProject returns one project the caller may see.
func (s *Store) GetProject(ctx context.Context, a *Principal, key string) (*Project, error) {
	if !projectKeyRE.MatchString(key) || !a.CanProject(key) {
		return nil, ErrNotFound
	}
	p, err := scanProject(s.db.QueryRowContext(ctx, projectSelect+` WHERE p.key = ?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// ListProjects lists the projects the caller may see.
func (s *Store) ListProjects(ctx context.Context, a *Principal) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, projectSelect+` ORDER BY p.key`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		if a.CanProject(p.Key) {
			out = append(out, *p)
		}
	}
	return out, rows.Err()
}

// ProjectPatch is the body of a project update.
type ProjectPatch struct {
	Name        *string   `json:"name"`
	Description *string   `json:"description"`
	Workflow    *Workflow `json:"workflow"`
	SLA         SLA       `json:"sla"`
	Archived    *bool     `json:"archived"`
}

// UpdateProject changes a project's settings. A new workflow must still contain every
// status in use; resolved_at follows the new status categories.
func (s *Store) UpdateProject(ctx context.Context, a *Principal, key string, in ProjectPatch) (*Project, error) {
	var p problems
	if in.Name != nil && (strings.TrimSpace(*in.Name) == "" || !validText(*in.Name, 100) || strings.ContainsAny(*in.Name, "\r\n")) {
		p.add("name", "required, one line, at most 100 characters")
	}
	if in.Description != nil && !validText(*in.Description, 2000) {
		p.add("description", "at most 2000 characters")
	}
	if in.Workflow != nil {
		if err := in.Workflow.Validate(); err != nil {
			p.add("workflow", "%v", err)
		}
	}
	if in.SLA != nil {
		if err := in.SLA.Validate(); err != nil {
			p.add("sla", "%v", err)
		}
	}
	if err := p.err(); err != nil {
		return nil, err
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		cur, err := s.loadProject(ctx, tx, a, key)
		if err != nil {
			return err
		}
		changed := map[string]any{}
		now := ts(s.now())
		if in.Name != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE projects SET name = ? WHERE id = ?`, *in.Name, cur.id); err != nil {
				return err
			}
			changed["name"] = *in.Name
		}
		if in.Description != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE projects SET description = ? WHERE id = ?`, *in.Description, cur.id); err != nil {
				return err
			}
			changed["description"] = true
		}
		if in.Archived != nil {
			v := 0
			if *in.Archived {
				v = 1
			}
			if _, err := tx.ExecContext(ctx, `UPDATE projects SET archived = ? WHERE id = ?`, v, cur.id); err != nil {
				return err
			}
			changed["archived"] = *in.Archived
		}
		if in.SLA != nil {
			b, _ := json.Marshal(in.SLA)
			if _, err := tx.ExecContext(ctx, `UPDATE projects SET sla = ? WHERE id = ?`, string(b), cur.id); err != nil {
				return err
			}
			changed["sla"] = in.SLA
		}
		if in.Workflow != nil {
			if err := s.applyWorkflow(ctx, tx, cur, in.Workflow, now); err != nil {
				return err
			}
			changed["workflow"] = in.Workflow
		}
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET updated_at = ? WHERE id = ?`, now, cur.id); err != nil {
			return err
		}
		return s.audit(ctx, tx, a.Subject, "project.update", key, changed)
	})
	if err != nil {
		return nil, err
	}
	return s.GetProject(ctx, a, key)
}

func (s *Store) applyWorkflow(ctx context.Context, tx *sql.Tx, cur *projectRow, wf *Workflow, now int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT status FROM issues WHERE project_id = ?`, cur.id)
	if err != nil {
		return err
	}
	var missing []string
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			_ = rows.Close()
			return err
		}
		if wf.Category(st) == "" {
			missing = append(missing, st)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(missing) > 0 {
		return &InvalidError{Problems: []ProblemField{{Field: "workflow", Message: "statuses still in use are missing: " + strings.Join(missing, ", ")}}}
	}
	b, _ := json.Marshal(wf)
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET workflow = ? WHERE id = ?`, string(b), cur.id); err != nil {
		return err
	}
	var done []any
	for _, st := range wf.Statuses {
		if st.Category == CategoryDone {
			done = append(done, st.Name)
		}
	}
	// Issues now in a done status get resolved_at (kept when already set); all others lose it.
	if _, err := tx.ExecContext(ctx, `UPDATE issues SET resolved_at = CASE WHEN status IN (`+placeholders(len(done))+`) THEN COALESCE(resolved_at, ?) ELSE NULL END WHERE project_id = ?`,
		append(done, now, cur.id)...); err != nil {
		return err
	}
	return nil
}

// ---- audit ----------------------------------------------------------------------------

// AuditEntry is one recorded admin action.
type AuditEntry struct {
	ID     int64           `json:"id"`
	Actor  string          `json:"actor"`
	Action string          `json:"action"`
	Target string          `json:"target"`
	Detail json.RawMessage `json:"detail"`
	At     time.Time       `json:"at"`
}

func (s *Store) audit(ctx context.Context, q dbq, actor, action, target string, detail any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO audit_log (actor, action, target, detail, at) VALUES (?, ?, ?, ?, ?)`, actor, action, target, string(b), ts(s.now()))
	return err
}

// ListAudit returns admin actions, newest first, after the cursor id (exclusive; 0 = start).
func (s *Store) ListAudit(ctx context.Context, before int64, limit int) ([]AuditEntry, error) {
	limit = clampLimit(limit)
	q := `SELECT id, actor, action, target, detail, at FROM audit_log`
	var args []any
	if before > 0 {
		q += ` WHERE id < ?`
		args = append(args, before)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var d string
		var at int64
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.Target, &d, &at); err != nil {
			return nil, err
		}
		e.Detail, e.At = json.RawMessage(d), fromUnix(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return 50
	case n > 100:
		return 100
	}
	return n
}
