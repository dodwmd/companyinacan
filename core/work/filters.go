package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Saved filters (specs/065-work-manager): a name for a search-language query, in one project or in
// all of them. Its owner sees it; a shared one is seen by everyone who can see what it is about.
// Running a filter is an ordinary issue search by the caller, so a filter never widens what anyone
// may see, and "me" in a shared filter means whoever runs it.

const (
	maxFilters    = 50
	maxFilterName = 80
)

// SavedFilter is one saved filter. Mine says whether the caller owns it.
type SavedFilter struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Project   string    `json:"project"`
	Query     string    `json:"query"`
	Sort      string    `json:"sort"`
	Shared    bool      `json:"shared"`
	Owner     string    `json:"owner"`
	Mine      bool      `json:"mine"`
	CreatedAt time.Time `json:"created_at"`
}

// NewFilter is the body of a saved filter create.
type NewFilter struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	Query   string `json:"query"`
	Sort    string `json:"sort"`
	Shared  bool   `json:"shared"`
}

const filterSelect = `SELECT id, owner, name, project, query, sort, shared, created_at FROM saved_filters`

func scanFilter(sc scanner, a *Principal) (*SavedFilter, error) {
	var f SavedFilter
	var shared int
	var at int64
	if err := sc.Scan(&f.ID, &f.Owner, &f.Name, &f.Project, &f.Query, &f.Sort, &shared, &at); err != nil {
		return nil, err
	}
	f.Shared, f.Mine, f.CreatedAt = shared == 1, f.Owner == a.Subject, fromUnix(at)
	return &f, nil
}

// canSeeFilter: your own or a shared one, never about a project outside your scope. A caller limited
// to some projects does not see other people's filters over every project, whose names and queries
// may be about projects it cannot see.
func canSeeFilter(a *Principal, f *SavedFilter) bool {
	if f.Project != "" && !a.CanProject(f.Project) {
		return false
	}
	return f.Mine || (f.Shared && (f.Project != "" || a.Unscoped()))
}

// ListFilters returns the filters the caller may use, by name.
func (s *Store) ListFilters(ctx context.Context, a *Principal) ([]SavedFilter, error) {
	rs, err := s.db.QueryContext(ctx, filterSelect+` WHERE owner = ? OR shared = 1 ORDER BY name COLLATE NOCASE, id LIMIT 500`, a.Subject)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	out := []SavedFilter{}
	for rs.Next() {
		f, err := scanFilter(rs, a)
		if err != nil {
			return nil, err
		}
		if canSeeFilter(a, f) {
			out = append(out, *f)
		}
	}
	return out, rs.Err()
}

// GetFilter returns one filter the caller may use.
func (s *Store) GetFilter(ctx context.Context, a *Principal, id int64) (*SavedFilter, error) {
	f, err := scanFilter(s.db.QueryRowContext(ctx, filterSelect+` WHERE id = ?`, id), a)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !canSeeFilter(a, f)) {
		return nil, ErrNotFound
	}
	return f, err
}

// CreateFilter saves a filter for the caller.
func (s *Store) CreateFilter(ctx context.Context, a *Principal, in NewFilter) (*SavedFilter, error) {
	if !a.Can(PermWrite) {
		return nil, ErrForbidden
	}
	in.Name = strings.TrimSpace(in.Name)
	var p problems
	if in.Name == "" || !validText(in.Name, maxFilterName) || hasControl(in.Name) {
		p.add("name", "required, one line, at most %d characters", maxFilterName)
	}
	if !validText(in.Query, maxQueryLen) {
		p.add("query", "at most %d characters", maxQueryLen)
	} else if _, err := ParseQueryScoped(in.Query, s.now(), a.Subject, a.Projects); err != nil {
		p.add("query", "%v", err)
	}
	if in.Sort == "" {
		in.Sort = "priority"
	}
	if _, ok := sortClauses[in.Sort]; !ok {
		p.add("sort", "unknown sort order")
	}
	if err := p.err(); err != nil {
		return nil, err
	}
	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if in.Project != "" {
			if _, err := s.loadProject(ctx, tx, a, in.Project); err != nil {
				return err
			}
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM saved_filters WHERE owner = ?`, a.Subject).Scan(&n); err != nil {
			return err
		}
		if n >= maxFilters {
			return fmt.Errorf("%w: at most %d saved filters each; delete one first", ErrConflict, maxFilters)
		}
		shared := 0
		if in.Shared {
			shared = 1
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO saved_filters (owner, name, project, query, sort, shared, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			a.Subject, in.Name, in.Project, in.Query, in.Sort, shared, ts(s.now()))
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return fmt.Errorf("%w: you already have a filter called %q", ErrConflict, in.Name)
			}
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetFilter(ctx, a, id)
}

// DeleteFilter removes a filter: its owner may, and an administrator may remove a shared one.
func (s *Store) DeleteFilter(ctx context.Context, a *Principal, id int64) error {
	if !a.Can(PermWrite) {
		return ErrForbidden
	}
	f, err := s.GetFilter(ctx, a, id)
	if err != nil {
		return err
	}
	if !f.Mine && !a.Can(PermAdmin) {
		return ErrForbidden
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM saved_filters WHERE id = ?`, id); err != nil {
			return err
		}
		if f.Mine {
			return nil
		}
		return s.audit(ctx, tx, a.Subject, "filter.delete", f.Name, map[string]any{"id": id})
	})
}

// personTermRE finds the values of the terms that name people.
var personTermRE = regexp.MustCompile(`(?i)(?:^|\s)-?(?:assignee|reporter)[:=]("[^"]*"|\S+)`)

// filterPeople returns the identifiers a filter query names.
func filterPeople(query string) []string {
	var out []string
	for _, m := range personTermRE.FindAllStringSubmatch(query, -1) {
		for _, v := range strings.Split(strings.ReplaceAll(m[1], `"`, ""), ",") {
			if v != "" && v != "me" && v != "none" && !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	return out
}
