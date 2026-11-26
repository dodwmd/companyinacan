package people

// CSV import of directory records (specs/047-csv-import). All or nothing: the whole file is
// applied in one transaction with its audit entries and events, and a preview is the same
// transaction rolled back.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/csvimport"
)

// importManager names a row of this or an earlier file.
const importManager = "manager_external_id"

// importJoiners is the query flag that announces new active records too.
const importJoiners = "joiners"

// defaultImportTimeout bounds the write transaction of an import (Service.importTimeout).
const defaultImportTimeout = 15 * time.Second

var importFields = []csvimport.Field{
	{Name: csvimport.ExternalID, Required: true}, {Name: "given_name", Required: true}, {Name: "family_name", Required: true},
	{Name: "kind"}, {Name: "status"}, {Name: "preferred_name"}, {Name: "work_email"}, {Name: "work_phone"}, {Name: "location"},
	{Name: "department"}, {Name: "team"}, {Name: "position"}, {Name: "start_date"}, {Name: importManager},
}

// importRecord is a directory record as the import sees it.
type importRecord struct {
	item      *csvimport.Item
	row       csvimport.Row
	id        string
	isNew     bool
	managerID string // before the import
	changed   []string
	// For a new record: it starts a provisioning joiner run, or it is existing staff that does not.
	joins, existing bool
}

// joinerNotices says what an apply would set off in provisioning.
func joinerNotices(records []*importRecord) []string {
	var joins, existing int
	for _, rec := range records {
		switch {
		case rec.item.Failed():
		case rec.joins:
			joins++
		case rec.existing:
			existing++
		}
	}
	var out []string
	if joins > 0 {
		out = append(out, fmt.Sprintf("%d new rows publish person.created.v1 as onboarding or active: identity provisioning, where enabled, starts a joiner run for each (accounts and checklists)", joins))
	}
	if existing > 0 {
		out = append(out, fmt.Sprintf("%d new active rows are imported as existing staff, without person.created.v1, so no joiner run starts; pass %s=true to start them", existing, importJoiners),
			"provisioning records no accounts for those rows: when one leaves it looks accounts up by work email where a connector can, and ends the run failed, for a person to check, where it cannot or where the directory links no can-core account; moves are reported the same way")
	}
	return out
}

type importAudit struct {
	action, resource string
	fields           []string
}

type peopleImport struct {
	s      *Service
	a      Actor
	tx     *sqlTx
	org    map[string]map[string]string // column -> name -> id
	emails map[string]string            // lower-case work email -> person id, former and erased excluded
	byExt  map[string]*importRecord
	audit  []importAudit
	// joiners also announces new active records (existing staff are otherwise not joiners).
	joiners bool
}

// ImportPeople previews or applies a CSV of people. hr or admin only.
func (s *Service) ImportPeople(ctx context.Context, a Actor, r io.Reader, o csvimport.Options) (*csvimport.Report, error) {
	if !Decide(a, ActDirImport, Fact{}) {
		s.auditDeny(ctx, a, "import", "people", nil)
		return nil, ErrForbidden
	}
	if !s.importing.TryLock() {
		return nil, csvimport.ErrBusy
	}
	defer s.importing.Unlock()
	f, err := csvimport.Read(r, importFields, o.Mapping)
	if err != nil {
		return nil, err
	}
	rep := csvimport.NewReport(f, o)
	wctx, cancel := context.WithTimeout(ctx, s.importTimeout)
	defer cancel()
	resource := "import:people:" + f.SHA256[:16]
	err = s.tx(wctx, func(tx *sqlTx) error {
		ctx := wctx
		im := &peopleImport{s: s, a: a, tx: tx, byExt: map[string]*importRecord{}, joiners: o.Flags[importJoiners]}
		if err := im.load(ctx); err != nil {
			return err
		}
		var records []*importRecord
		for _, row := range f.Rows {
			rec := &importRecord{item: rep.Row(row), row: row}
			if rec.item.Failed() {
				continue
			}
			if err := im.apply(ctx, rec); err != nil {
				if err := rowError(rec.item, err); err != nil {
					return err
				}
				continue
			}
			records = append(records, rec)
			im.byExt[row.Get(csvimport.ExternalID)] = rec
		}
		if err := im.linkManagers(ctx, records); err != nil {
			return err
		}
		for _, rec := range records {
			switch {
			case rec.isNew:
				rec.item.Create(rec.id)
				im.audit = append(im.audit, importAudit{"create", "person:" + rec.id, nil})
			default:
				rec.item.Update(rec.id, rec.changed)
				if len(rec.changed) > 0 {
					im.audit = append(im.audit, importAudit{"write", "person:" + rec.id, rec.changed})
				}
			}
		}
		rep.Notices = joinerNotices(records)
		if !o.Apply || rep.HasErrors() {
			return csvimport.ErrRollback
		}
		im.audit = append(im.audit, importAudit{"import", resource, nil})
		for _, e := range im.audit {
			if err := s.auditIn(ctx, tx, a, e.action, e.resource, e.fields); err != nil {
				return err
			}
		}
		return nil
	})
	switch {
	case errors.Is(err, csvimport.ErrRollback):
		// The report says which records already exist and which match, so a run that wrote
		// nothing is still recorded; without the record there is no report.
		if err := s.auditWrite(ctx, a, "import_preview", resource, nil); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	}
	rep.Finish()
	return rep, nil
}

// rowError records a client mistake against its row and passes everything else on.
func rowError(it *csvimport.Item, err error) error {
	var inv *InvalidError
	var con *ConflictError
	switch {
	case errors.As(err, &inv):
		it.Error("", inv.Msg)
	case errors.As(err, &con):
		it.Error("", con.Msg)
	default:
		return err
	}
	return nil
}

func (im *peopleImport) load(ctx context.Context) error {
	pairs := func(query string, into map[string]string) error {
		rows, err := im.tx.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				return err
			}
			into[k] = v
		}
		return rows.Err()
	}
	im.org = map[string]map[string]string{"department": {}, "team": {}, "position": {}}
	im.emails = map[string]string{}
	for query, into := range map[string]map[string]string{
		`SELECT name, id FROM departments`: im.org["department"],
		`SELECT name, id FROM teams`:       im.org["team"],
		`SELECT title, id FROM positions`:  im.org["position"],
		`SELECT lower(work_email), id FROM people WHERE work_email <> '' AND erased_at IS NULL AND status <> 'former'`: im.emails,
	} {
		if err := pairs(query, into); err != nil {
			return err
		}
	}
	return nil
}

// apply creates or updates the record of one row, without its manager (linkManagers).
func (im *peopleImport) apply(ctx context.Context, rec *importRecord) error {
	row := rec.row
	orgIDs := map[string]string{}
	for column, ids := range im.org {
		if name := row.Get(column); name != "" {
			id, ok := ids[name]
			if !ok {
				return invalid("there is no %s with that name; create it first", column)
			}
			orgIDs[column] = id
		}
	}
	if d := row.Get("start_date"); d != "" {
		if _, err := time.Parse(dateLayout, d); err != nil {
			return invalid("start_date must be YYYY-MM-DD")
		}
	}

	var cur PersonInput
	var status string
	var erased sql.NullInt64
	err := im.tx.QueryRowContext(ctx, `SELECT id, kind, status, erased_at, given_name, family_name, preferred_name, work_email, work_phone, location,
		COALESCE(department_id,''), COALESCE(team_id,''), COALESCE(position_id,''), start_date, COALESCE(manager_id,'') FROM people WHERE external_id = ?`, row.Get(csvimport.ExternalID)).
		Scan(&rec.id, &cur.Kind, &status, &erased, &cur.GivenName, &cur.FamilyName, &cur.PreferredName, &cur.WorkEmail, &cur.WorkPhone, &cur.Location,
			&cur.DepartmentID, &cur.TeamID, &cur.PositionID, &cur.StartDate, &rec.managerID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		rec.isNew = true
		cur = PersonInput{Kind: "employee", Status: StActive}
		if v := row.Get("kind"); v != "" {
			cur.Kind = v
		}
		if v := row.Get("status"); v != "" {
			cur.Status = Status(v)
		}
	case err != nil:
		return err
	case erased.Valid:
		return invalid("this external_id belongs to an erased record; remove the row")
	case Status(status) == StFormer:
		return invalid("this external_id belongs to a former record, which an import does not change; remove the row")
	default:
		if v := row.Get("kind"); v != "" && v != cur.Kind {
			rec.item.Warn("kind", "kind is only set when a record is first imported")
		}
		if v := row.Get("status"); v != "" && v != status {
			rec.item.Warn("status", "status is only set when a record is first imported; change it with a status change")
		}
		cur.Status = StActive // only for validate: an update never writes it
	}

	oldEmail := strings.ToLower(cur.WorkEmail)
	var sets []string
	var args []any
	for _, c := range []struct {
		column string
		value  string
		dst    *string
		ref    bool
	}{
		{"given_name", row.Get("given_name"), &cur.GivenName, false},
		{"family_name", row.Get("family_name"), &cur.FamilyName, false},
		{"preferred_name", row.Get("preferred_name"), &cur.PreferredName, false},
		{"work_email", row.Get("work_email"), &cur.WorkEmail, false},
		{"work_phone", row.Get("work_phone"), &cur.WorkPhone, false},
		{"location", row.Get("location"), &cur.Location, false},
		{"department_id", orgIDs["department"], &cur.DepartmentID, true},
		{"team_id", orgIDs["team"], &cur.TeamID, true},
		{"position_id", orgIDs["position"], &cur.PositionID, true},
		{"start_date", row.Get("start_date"), &cur.StartDate, false},
	} {
		if c.value == "" || c.value == *c.dst { // an empty cell keeps the stored value
			continue
		}
		*c.dst = c.value
		rec.changed = append(rec.changed, c.column)
		sets = append(sets, c.column+" = ?")
		if c.ref {
			args = append(args, nullStr(c.value))
		} else {
			args = append(args, c.value)
		}
	}
	// A required name left empty in the file is a mistake in the file, even on an update.
	if row.Get("given_name") == "" || row.Get("family_name") == "" {
		return invalid("given_name and family_name are required")
	}
	if err := cur.validate(); err != nil {
		return err
	}
	email := strings.ToLower(cur.WorkEmail)
	if email != "" && (rec.isNew || email != oldEmail) {
		if other, taken := im.emails[email]; taken && other != rec.id {
			return invalid("work_email is already used by another record or row")
		}
	}

	if rec.isNew {
		rec.id = newID("pers")
		// Existing staff are not joiners: an active record is announced only when asked for.
		announce := cur.Status != StActive || im.joiners
		rec.existing = !announce
		rec.joins = announce && (cur.Status == StActive || cur.Status == StOnboarding)
		if err := im.s.insertPerson(ctx, im.tx, im.a, rec.id, cur, "", row.Get(csvimport.ExternalID), announce); err != nil {
			return err
		}
	} else if len(sets) > 0 {
		args = append(args, im.s.now().Unix(), rec.id)
		if _, err := im.tx.ExecContext(ctx, `UPDATE people SET `+strings.Join(sets, ", ")+`, updated_at = ? WHERE id = ?`, args...); err != nil { //nolint:gosec // the column names are constants above
			return err
		}
	}
	if email != oldEmail && im.emails[oldEmail] == rec.id {
		delete(im.emails, oldEmail)
	}
	if email != "" {
		im.emails[email] = rec.id
	}
	return nil
}

// linkManagers sets the reporting lines after every row exists, so a manager may come later
// in the file. The lines that change are cleared first, so the cycle check sees only lines
// that will remain and a file that reverses a line is judged by its result.
func (im *peopleImport) linkManagers(ctx context.Context, records []*importRecord) error {
	type link struct {
		rec     *importRecord
		manager string
	}
	var links []link
	for _, rec := range records {
		ext := rec.row.Get(importManager)
		if ext == "" {
			continue
		}
		var mgr string
		if m, ok := im.byExt[ext]; ok {
			mgr = m.id
		} else if err := im.tx.QueryRowContext(ctx, `SELECT id FROM people WHERE external_id = ?`, ext).Scan(&mgr); errors.Is(err, sql.ErrNoRows) {
			rec.item.Error(importManager, "no record has that external_id (or its row has an error)")
			continue
		} else if err != nil {
			return err
		}
		if mgr != rec.managerID {
			links = append(links, link{rec, mgr})
		}
	}
	for _, l := range links {
		if _, err := im.tx.ExecContext(ctx, `UPDATE people SET manager_id = NULL WHERE id = ?`, l.rec.id); err != nil {
			return err
		}
	}
	for _, l := range links {
		if err := im.s.checkManager(ctx, im.tx, l.rec.id, l.manager); err != nil {
			var inv *InvalidError
			if !errors.As(err, &inv) {
				return err
			}
			// The message may name an id that only exists inside this transaction.
			msg := inv.Msg
			if strings.Contains(msg, l.manager) {
				msg = "that manager cannot be assigned"
			}
			l.rec.item.Error(importManager, msg)
			continue
		}
		if _, err := im.tx.ExecContext(ctx, `UPDATE people SET manager_id = ?, updated_at = ? WHERE id = ?`, l.manager, im.s.now().Unix(), l.rec.id); err != nil {
			return err
		}
		if !slices.Contains(l.rec.changed, "manager_id") {
			l.rec.changed = append(l.rec.changed, "manager_id")
		}
	}
	return nil
}
