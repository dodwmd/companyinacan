package work

// CSV import of one project's issues (specs/047-csv-import). All or nothing: the whole file
// is applied in one transaction with its activity, events and audit entry, and a preview is
// the same transaction rolled back. Creates and updates go through the same code as the API.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/csvimport"
)

const (
	importTimeout = 20 * time.Second
	importParent  = "parent_external_id"
)

var issueImportFields = []csvimport.Field{
	{Name: csvimport.ExternalID, Required: true}, {Name: "title", Required: true}, {Name: "type"}, {Name: "status"},
	{Name: "description"}, {Name: "priority"}, {Name: "assignee"}, {Name: "due_date"}, {Name: "estimate"}, {Name: "labels"},
	{Name: importParent},
}

// importRow is one row with the issue it matches, if any.
type importRow struct {
	row   csvimport.Row
	item  *csvimport.Item
	cur   *issueRow // nil for a new issue
	typ   string
	level int // parents are written before children: epic, then story/task/bug, then subtask
}

func typeLevel(t string) int {
	switch t {
	case "epic":
		return 0
	case "subtask":
		return 2
	}
	return 1
}

// ImportIssues previews or applies a CSV of issues into a project. It needs work.admin (a new
// issue takes its status from the file, which bypasses the workflow) and a principal that is
// not limited to projects.
func (s *Store) ImportIssues(ctx context.Context, a *Principal, project string, r io.Reader, o csvimport.Options) (*csvimport.Report, error) {
	if !a.Can(PermAdmin) || !a.Unscoped() {
		return nil, ErrForbidden
	}
	if !s.importing.TryLock() {
		return nil, csvimport.ErrBusy
	}
	defer s.importing.Unlock()
	f, err := csvimport.Read(r, issueImportFields, o.Mapping)
	if err != nil {
		return nil, err
	}
	rep := csvimport.NewReport(f, o)
	ctx, cancel := context.WithTimeout(ctx, importTimeout)
	defer cancel()
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		proj, err := s.writableProject(ctx, tx, a, project)
		if err != nil {
			return err
		}
		rows := make([]*importRow, 0, len(f.Rows))
		for _, row := range f.Rows {
			ir := &importRow{row: row, item: rep.Row(row)}
			if ir.item.Failed() {
				continue
			}
			if err := s.matchIssue(ctx, tx, a, proj, ir); err != nil {
				return err
			}
			rows = append(rows, ir)
		}
		slices.SortStableFunc(rows, func(x, y *importRow) int { return x.level - y.level })
		keys := map[string]string{} // external id -> issue key, for parents
		for _, ir := range rows {
			if err := s.importIssue(ctx, tx, a, proj, ir, keys); err != nil {
				if err := rowError(ir.item, err); err != nil {
					return err
				}
			}
		}
		if !o.Apply || rep.HasErrors() {
			return csvimport.ErrRollback
		}
		var created, updated []string
		for _, ir := range rows {
			switch ir.item.Action {
			case csvimport.ActionCreate:
				created = append(created, ir.item.ID)
			case csvimport.ActionUpdate:
				updated = append(updated, ir.item.ID)
			}
		}
		return s.audit(ctx, tx, a.Subject, "issue.import", proj.key, map[string]any{"file_sha256": f.SHA256, "rows": len(f.Rows), "created": created, "updated": updated})
	})
	if err != nil && !errors.Is(err, csvimport.ErrRollback) {
		return nil, err
	}
	rep.Finish()
	if rep.Applied {
		issuesCreated.With().Add(float64(rep.Create))
	}
	return rep, nil
}

// rowError records a client mistake against its row and passes everything else on.
func rowError(it *csvimport.Item, err error) error {
	var ie *InvalidError
	if !errors.As(err, &ie) {
		return err
	}
	for _, p := range ie.Problems {
		msg := p.Message
		if p.Field == "labels" { // the shared message quotes the label
			msg = fmt.Sprintf("at most %d labels of 40 characters: lower-case letters, digits and ._:-", maxLabels)
		}
		it.Error(p.Field, msg)
	}
	return nil
}

func (s *Store) matchIssue(ctx context.Context, tx dbq, a *Principal, proj *projectRow, ir *importRow) error {
	var number int64
	err := tx.QueryRowContext(ctx, `SELECT number FROM issues WHERE project_id = ? AND external_id = ?`, proj.id, ir.row.Get(csvimport.ExternalID)).Scan(&number)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if ir.typ = ir.row.Get("type"); ir.typ == "" {
			ir.typ = "task"
		}
	case err != nil:
		return err
	default:
		if ir.cur, err = s.loadIssue(ctx, tx, a, proj.key+"-"+strconv.FormatInt(number, 10)); err != nil {
			return err
		}
		ir.typ = ir.cur.Type
	}
	ir.level = typeLevel(ir.typ)
	return nil
}

func (s *Store) importIssue(ctx context.Context, tx dbq, a *Principal, proj *projectRow, ir *importRow, keys map[string]string) error {
	row := ir.row
	var estimate *float64
	if v := row.Get("estimate"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || !(n >= 0 && n <= 10000) { // also refuses NaN
			return invalid("estimate", "must be a number between 0 and 10000")
		}
		estimate = &n
	}
	var parent *string
	if ext := row.Get(importParent); ext != "" {
		key, ok := keys[ext]
		if !ok {
			var number int64
			err := tx.QueryRowContext(ctx, `SELECT number FROM issues WHERE project_id = ? AND external_id = ?`, proj.id, ext).Scan(&number)
			if errors.Is(err, sql.ErrNoRows) {
				return invalid(importParent, "no issue of this project has that external_id (or its row has an error)")
			}
			if err != nil {
				return err
			}
			key = proj.key + "-" + strconv.FormatInt(number, 10)
		}
		parent = &key
	}
	labels := strings.FieldsFunc(row.Get("labels"), func(r rune) bool { return r == ';' || r == ',' || r == ' ' || r == '\n' || r == '\t' })
	if row.Get("title") == "" {
		return invalid("title", "required")
	}
	if ir.cur == nil {
		return s.importNewIssue(ctx, tx, a, proj, ir, parent, estimate, labels, keys)
	}
	return s.importChangedIssue(ctx, tx, a, ir, parent, estimate, labels, keys)
}

func (s *Store) importNewIssue(ctx context.Context, tx dbq, a *Principal, proj *projectRow, ir *importRow, parent *string, estimate *float64, labels []string, keys map[string]string) error {
	row := ir.row
	opt := func(field string) *string {
		if v := row.Get(field); v != "" {
			return &v
		}
		return nil
	}
	in := CreateIssue{Type: ir.typ, Title: row.Get("title"), Description: row.Get("description"), Priority: row.Get("priority"),
		Assignee: opt("assignee"), DueDate: opt("due_date"), Estimate: estimate, Parent: parent, Labels: labels}
	if err := in.validate(); err != nil {
		return err
	}
	// The new issue starts where the file says instead of at the workflow's initial status.
	start := *proj
	if status := row.Get("status"); status != "" {
		if proj.wf.Category(status) == "" {
			return invalid("status", "not a status of this project")
		}
		start.wf.Initial = status
	}
	is, err := s.createInTx(ctx, tx, a, &start, &in)
	if err != nil {
		return err
	}
	_, number, _ := splitKey(is.Key)
	var resolved any
	if proj.wf.Category(start.wf.Initial) == CategoryDone {
		resolved = ts(s.now())
	}
	if _, err := tx.ExecContext(ctx, `UPDATE issues SET external_id = ?, resolved_at = ? WHERE project_id = ? AND number = ?`, row.Get(csvimport.ExternalID), resolved, proj.id, number); err != nil {
		return err
	}
	keys[row.Get(csvimport.ExternalID)] = is.Key
	ir.item.Create(is.Key)
	return nil
}

func (s *Store) importChangedIssue(ctx context.Context, tx dbq, a *Principal, ir *importRow, parent *string, estimate *float64, labels []string, keys map[string]string) error {
	row, cur := ir.row, ir.cur
	if v := row.Get("type"); v != "" && v != cur.Type {
		ir.item.Warn("type", "type is only set when an issue is first imported")
	}
	if v := row.Get("status"); v != "" && v != cur.Status {
		ir.item.Warn("status", "status is only set when an issue is first imported; move it along the workflow")
	}
	set := func(field string) Opt[string] {
		v := row.Get(field)
		return Opt[string]{Set: v != "", V: v} // an empty cell keeps the stored value
	}
	pt := Patch{Title: set("title"), Description: set("description"), Priority: set("priority"), Assignee: set("assignee"), DueDate: set("due_date")}
	if estimate != nil {
		pt.Estimate = Opt[float64]{Set: true, V: *estimate}
	}
	if parent != nil {
		pt.Parent = Opt[string]{Set: true, V: *parent}
	}
	if len(labels) > 0 {
		pt.Labels = Opt[[]string]{Set: true, V: labels}
	}
	if err := pt.validate(); err != nil {
		return err
	}
	out, err := s.updateTx(ctx, tx, a, cur.Key, &pt, nil)
	if err != nil {
		return err
	}
	var changed []string
	for _, c := range []struct {
		field string
		same  bool
	}{
		{"title", out.Title == cur.Title}, {"description", out.Description == cur.Description}, {"priority", out.Priority == cur.Priority},
		{"assignee", eqStrPtr(out.Assignee, cur.Assignee)}, {"due_date", eqStrPtr(out.DueDate, cur.DueDate)},
		{"estimate", eqFloatPtr(out.Estimate, cur.Estimate)}, {"parent", eqStrPtr(out.Parent, cur.Parent)}, {"labels", slices.Equal(out.Labels, cur.Labels)},
	} {
		if !c.same {
			changed = append(changed, c.field)
		}
	}
	keys[row.Get(csvimport.ExternalID)] = cur.Key
	ir.item.Update(cur.Key, changed)
	return nil
}

// importIssues is POST /v1/projects/{key}/issues/import: the body is the CSV.
func (a *api) importIssues(w http.ResponseWriter, r *http.Request, p *Principal) {
	var rep *csvimport.Report
	o, err := csvimport.ParseQuery(r.URL.Query())
	if err == nil {
		rep, err = a.store.ImportIssues(r.Context(), p, r.PathValue("key"), r.Body, o)
	}
	if status, code, ok := csvimport.Failure(err); ok {
		writeError(w, status, code, err.Error())
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, rep.Status(), rep)
}
