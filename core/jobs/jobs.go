package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

const jobCols = `id, slug, title, department, city, region, country, postal_code, remote, applicant_countries, employment_type,
	salary_min, salary_max, salary_currency, salary_period, publish_salary, sections, hiring_manager, headcount, status, approval,
	valid_through, opened_at, closed_at, close_reason, created_by, created_at, updated_at, version`

func scanJob(sc scanner) (*Job, error) {
	var j Job
	var ac, secs string
	var smin, smax sql.NullFloat64
	var cur, per string
	var pub int
	var valid, opened, closed sql.NullInt64
	var created, updated int64
	if err := sc.Scan(&j.ID, &j.Slug, &j.Title, &j.Department, &j.Location.City, &j.Location.Region, &j.Location.Country, &j.Location.PostalCode,
		&j.Remote, &ac, &j.EmploymentType, &smin, &smax, &cur, &per, &pub, &secs, &j.HiringManager, &j.Headcount, &j.Status, &j.Approval,
		&valid, &opened, &closed, &j.CloseReason, &j.CreatedBy, &created, &updated, &j.Version); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(ac), &j.ApplicantCountries); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(secs), &j.Sections); err != nil {
		return nil, err
	}
	if j.ApplicantCountries == nil {
		j.ApplicantCountries = []string{}
	}
	if smin.Valid && smax.Valid {
		j.Salary = &Salary{Min: smin.Float64, Max: smax.Float64, Currency: cur, Period: per}
	}
	j.PublishSalary = pub == 1
	j.ValidThrough, j.OpenedAt, j.ClosedAt = nullTime(valid), nullTime(opened), nullTime(closed)
	j.CreatedAt, j.UpdatedAt = fromUnix(created), fromUnix(updated)
	return &j, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(title, id string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(s) > 60 {
		s = strings.Trim(s[:60], "-")
	}
	if s == "" {
		s = "job"
	}
	return s + "-" + id[len(id)-6:]
}

func parseDate(field, v string, p *problems) *time.Time {
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		p.add(field, "must be a date like 2026-12-31")
		return nil
	}
	// Valid through the END of that day (UTC).
	t = t.Add(24*time.Hour - time.Second)
	return &t
}

func nullFloat(s *Salary, pick func(*Salary) float64) any {
	if s == nil {
		return nil
	}
	return pick(s)
}

func salaryCols(s *Salary) (cur, per string) {
	if s == nil {
		return "", ""
	}
	return s.Currency, s.Period
}

// CreateJob creates a draft job.
func (s *Store) CreateJob(ctx context.Context, p *Principal, in NewJob) (*Job, error) {
	if !p.Can(ActJobWrite) {
		return nil, ErrForbidden
	}
	id := newID("job")
	now := s.now().UTC()
	j := &Job{ID: id, Slug: slugify(in.Title, id), Title: strings.TrimSpace(in.Title), Department: in.Department, Location: in.Location, Remote: in.Remote,
		ApplicantCountries: in.ApplicantCountries, EmploymentType: in.EmploymentType, Salary: in.Salary, PublishSalary: in.PublishSalary,
		Sections: in.Sections, HiringManager: in.HiringManager, Headcount: in.Headcount, Status: StatusDraft, Approval: ApprovalNone,
		CreatedBy: p.Subject, CreatedAt: now, UpdatedAt: now, Version: 1}
	if j.Headcount == 0 {
		j.Headcount = 1
	}
	if j.ApplicantCountries == nil {
		j.ApplicantCountries = []string{}
	}
	var pr problems
	if in.ValidThrough != nil {
		j.ValidThrough = parseDate("valid_through", *in.ValidThrough, &pr)
		if j.ValidThrough != nil && !j.ValidThrough.After(now) {
			pr.add("valid_through", "must be in the future")
		}
	}
	if verr := j.validate(); verr != nil || len(pr) > 0 {
		return nil, mergeInvalid(pr, verr)
	}
	if !p.Unscoped() {
		return nil, ErrForbidden // a job-scoped token cannot mint a job outside its own list
	}
	if err := s.writeJob(ctx, p, j, true); err != nil {
		return nil, err
	}
	return s.GetJob(ctx, p, id)
}

// mergeInvalid combines two validation errors into one InvalidError.
func mergeInvalid(a problems, b error) error {
	var ie *InvalidError
	if errors.As(b, &ie) {
		a = append(a, ie.Problems...)
	}
	return a.err()
}

func (s *Store) writeJob(ctx context.Context, p *Principal, j *Job, create bool) error {
	ac, _ := json.Marshal(j.ApplicantCountries)
	secs, _ := json.Marshal(j.Sections)
	cur, per := salaryCols(j.Salary)
	pub := 0
	if j.PublishSalary {
		pub = 1
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if create {
			if _, err := tx.ExecContext(ctx, `INSERT INTO jobs (id, slug, title, department, city, region, country, postal_code, remote, applicant_countries, employment_type,
				salary_min, salary_max, salary_currency, salary_period, publish_salary, sections, hiring_manager, headcount, status, approval, valid_through,
				created_by, created_at, updated_at, version) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1)`,
				j.ID, j.Slug, j.Title, j.Department, j.Location.City, j.Location.Region, j.Location.Country, j.Location.PostalCode, j.Remote, string(ac), j.EmploymentType,
				nullFloat(j.Salary, func(s *Salary) float64 { return s.Min }), nullFloat(j.Salary, func(s *Salary) float64 { return s.Max }), cur, per, pub, string(secs),
				j.HiringManager, j.Headcount, j.Status, j.Approval, nullUnix(j.ValidThrough), j.CreatedBy, ts(j.CreatedAt), ts(j.UpdatedAt)); err != nil {
				return err
			}
			return s.audit(ctx, tx, p.Subject, "job.create", "job:"+j.ID, nil, "ok")
		}
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET title=?, department=?, city=?, region=?, country=?, postal_code=?, remote=?, applicant_countries=?, employment_type=?,
			salary_min=?, salary_max=?, salary_currency=?, salary_period=?, publish_salary=?, sections=?, hiring_manager=?, headcount=?, valid_through=?, updated_at=?, approval=?, status=?, version=version+1
			WHERE id=? AND version=?`,
			j.Title, j.Department, j.Location.City, j.Location.Region, j.Location.Country, j.Location.PostalCode, j.Remote, string(ac), j.EmploymentType,
			nullFloat(j.Salary, func(s *Salary) float64 { return s.Min }), nullFloat(j.Salary, func(s *Salary) float64 { return s.Max }), cur, per, pub, string(secs),
			j.HiringManager, j.Headcount, nullUnix(j.ValidThrough), ts(j.UpdatedAt), j.Approval, j.Status, j.ID, j.Version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrVersionMismatch
		}
		return s.audit(ctx, tx, p.Subject, "job.update", "job:"+j.ID, nil, "ok")
	})
}

// GetJob returns a job the caller may see.
func (s *Store) GetJob(ctx context.Context, p *Principal, id string) (*Job, error) {
	if !p.Can(ActJobRead) {
		return nil, ErrForbidden
	}
	pred, args := p.jobPred("jobs")
	j, err := scanJob(s.rdb.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE (id = ?) AND `+pred, append([]any{id}, args...)...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// JobFilter narrows a job list.
type JobFilter struct {
	Status     string
	Department string
	Cursor     int64
	Limit      int
}

// ListJobs lists the jobs the caller may see, newest first.
func (s *Store) ListJobs(ctx context.Context, p *Principal, f JobFilter) ([]*Job, int64, error) {
	if !p.Can(ActJobRead) {
		return nil, 0, ErrForbidden
	}
	limit := clampLimit(f.Limit)
	pred, args := p.jobPred("jobs")
	if f.Cursor <= 0 {
		f.Cursor = 1 << 62
	}
	q := `SELECT rowid, ` + jobCols + ` FROM jobs WHERE (? = '' OR status = ?) AND (? = '' OR department = ?) AND (rowid < ?) AND ` + pred + ` ORDER BY rowid DESC LIMIT ?`
	a := append([]any{f.Status, f.Status, f.Department, f.Department, f.Cursor}, args...)
	rows, err := s.rdb.QueryContext(ctx, q, append(a, limit+1)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Job
	var rowids []int64
	for rows.Next() {
		var rid int64
		// rowid is the first column; scanJob expects the rest.
		j, err := scanJob(&prefixScanner{rows: rows, dst: &rid})
		if err != nil {
			return nil, 0, err
		}
		out = append(out, j)
		rowids = append(rowids, rid)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var next int64
	if len(out) > limit {
		out = out[:limit]
		next = rowids[limit-1]
	}
	if out == nil {
		out = []*Job{}
	}
	return out, next, nil
}

// prefixScanner scans one leading column into dst and hands the rest to the caller's Scan.
type prefixScanner struct {
	rows *sql.Rows
	dst  *int64
}

func (p *prefixScanner) Scan(dest ...any) error {
	return p.rows.Scan(append([]any{p.dst}, dest...)...)
}

// PatchJob edits a job that is not closed. ifMatch, when non-nil, must equal the job's version.
func (s *Store) PatchJob(ctx context.Context, p *Principal, id string, in JobPatch, ifMatch *int64) (*Job, error) {
	if !p.Can(ActJobWrite) {
		return nil, ErrForbidden
	}
	j, err := s.GetJob(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if ifMatch != nil && *ifMatch != j.Version {
		return nil, ErrVersionMismatch
	}
	if j.Status == StatusClosed {
		return nil, conflict("a closed job cannot be edited")
	}
	var pr problems
	if in.Title != nil {
		j.Title = strings.TrimSpace(*in.Title)
	}
	if in.Department != nil {
		j.Department = *in.Department
	}
	if in.Location != nil {
		j.Location = *in.Location
	}
	if in.Remote != nil {
		j.Remote = *in.Remote
	}
	if in.ApplicantCountries != nil {
		j.ApplicantCountries = *in.ApplicantCountries
	}
	if in.EmploymentType != nil {
		j.EmploymentType = *in.EmploymentType
	}
	if in.ClearSalary {
		j.Salary, j.PublishSalary = nil, false
	}
	if in.Salary != nil {
		j.Salary = in.Salary
	}
	if in.PublishSalary != nil {
		j.PublishSalary = *in.PublishSalary
	}
	if in.Sections != nil {
		j.Sections = *in.Sections
	}
	if in.HiringManager != nil {
		j.HiringManager = *in.HiringManager
	}
	if in.Headcount != nil {
		j.Headcount = *in.Headcount
	}
	now := s.now().UTC()
	if in.ValidThrough != nil {
		j.ValidThrough = parseDate("valid_through", *in.ValidThrough, &pr)
		if j.ValidThrough != nil && !j.ValidThrough.After(now) {
			pr.add("valid_through", "must be in the future")
		}
	}
	if err := j.validate(); err != nil || len(pr) > 0 {
		return nil, mergeInvalid(pr, err)
	}
	j.UpdatedAt = now
	// An approved requisition approved a specific text. When approval is required, any edit
	// withdraws it, and an open job is paused until it is approved again, so unapproved content
	// is never live.
	if s.cfg.Approval.Required && j.Approval == ApprovalApproved {
		j.Approval = ApprovalNone
		if j.Status == StatusOpen {
			j.Status = StatusPaused
		}
	}
	if err := s.writeJob(ctx, p, j, false); err != nil {
		return nil, err
	}
	out, err := s.GetJob(ctx, p, id)
	if err == nil && out.Status == StatusOpen {
		s.syndicate(ctx, out, "update")
	} else if err == nil && out.Status == StatusPaused {
		s.syndicate(ctx, out, "close")
	}
	return out, err
}

// RequestApproval asks the Approver to approve a draft job. It is a no-op error unless the
// job is a draft whose approval is none or rejected.
func (s *Store) RequestApproval(ctx context.Context, p *Principal, id string) (*Job, error) {
	if !p.Can(ActJobWrite) {
		return nil, ErrForbidden
	}
	j, err := s.GetJob(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if (j.Status != StatusDraft && j.Status != StatusPaused) || (j.Approval != ApprovalNone && j.Approval != ApprovalRejected) {
		return nil, conflict("approval can be requested for a draft or paused job that has none pending or granted (status %s, approval %s)", j.Status, j.Approval)
	}
	ref, state, err := s.approver.Request(ctx, j, p.Subject)
	if err != nil {
		return nil, err
	}
	if state != ApprovalApproved && state != ApprovalPending {
		return nil, invalid("approval", "the approver returned %q", state)
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET approval=?, approval_requested_by=?, approval_ref=?, updated_at=?, version=version+1 WHERE id=?`,
			state, p.Subject, ref, ts(s.now()), id); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Subject, "job.approval_request", "job:"+id, nil, "ok")
	})
	if err != nil {
		return nil, err
	}
	return s.GetJob(ctx, p, id)
}

// DecideApproval records the approver's decision on a pending job. Admin only, and never the
// person who asked for it (separation of duties).
func (s *Store) DecideApproval(ctx context.Context, p *Principal, id string, approve bool) (*Job, error) {
	if !p.Can(ActApprove) {
		return nil, ErrForbidden
	}
	j, err := s.GetJob(ctx, p, id)
	if err != nil {
		return nil, err
	}
	var by string
	if err := s.db.QueryRowContext(ctx, `SELECT approval_requested_by FROM jobs WHERE id = ?`, id).Scan(&by); err != nil {
		return nil, err
	}
	if j.Approval != ApprovalPending {
		return nil, conflict("the job has no pending approval request")
	}
	if by == p.Subject {
		return nil, conflict("the person who requested an approval cannot decide it")
	}
	state := ApprovalRejected
	if approve {
		state = ApprovalApproved
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET approval=?, updated_at=?, version=version+1 WHERE id=? AND approval='pending'`, state, ts(s.now()), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return conflict("the approval was decided or withdrawn meanwhile")
		}
		return s.audit(ctx, tx, p.Subject, "job.approval_"+state, "job:"+id, nil, "ok")
	})
	if err != nil {
		return nil, err
	}
	return s.GetJob(ctx, p, id)
}

// CloseReasons is the fixed vocabulary for closing a job.
var CloseReasons = []string{"filled", "cancelled", "expired", "other"}

// TransitionJob moves a job through draft -> open <-> paused -> closed.
func (s *Store) TransitionJob(ctx context.Context, p *Principal, id, to, reason string) (*Job, error) {
	if !p.Can(ActJobWrite) {
		return nil, ErrForbidden
	}
	j, err := s.GetJob(ctx, p, id)
	if err != nil {
		return nil, err
	}
	allowed := map[string][]string{
		StatusDraft:  {StatusOpen, StatusClosed},
		StatusOpen:   {StatusPaused, StatusClosed},
		StatusPaused: {StatusOpen, StatusClosed},
	}
	ok := false
	for _, t := range allowed[j.Status] {
		ok = ok || t == to
	}
	if !ok {
		return nil, conflict("a %s job cannot become %s", j.Status, to)
	}
	now := s.now().UTC()
	reopened := j.OpenedAt != nil
	valid := nullUnix(j.ValidThrough)
	switch to {
	case StatusOpen:
		if s.cfg.Approval.Required && j.Approval != ApprovalApproved {
			return nil, conflict("this job needs an approved requisition before it can open (approval is %s)", j.Approval)
		}
		if j.ValidThrough == nil || !j.ValidThrough.After(now) {
			if j.ValidThrough != nil && j.Status == StatusPaused {
				return nil, conflict("valid_through is in the past; set a new date with PATCH before reopening")
			}
			t := now.AddDate(0, 0, s.cfg.Apply.DefaultValidDays)
			valid = t.Unix()
		}
	case StatusClosed:
		found := false
		for _, r := range CloseReasons {
			found = found || r == reason
		}
		if !found {
			return nil, invalid("reason", "closing needs a reason: one of filled, cancelled, expired, other")
		}
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		q := `UPDATE jobs SET status=?, updated_at=?, version=version+1, valid_through=?`
		args := []any{to, ts(now), valid}
		switch to {
		case StatusOpen:
			q += `, opened_at = COALESCE(opened_at, ?)`
			args = append(args, ts(now))
		case StatusClosed:
			q += `, closed_at = ?, close_reason = ?`
			args = append(args, ts(now), reason)
		}
		q += ` WHERE id = ? AND status = ?`
		args = append(args, id, j.Status)
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrVersionMismatch
		}
		if to == StatusOpen {
			if err := s.emit(ctx, tx, EventJobOpened, jobOpenedData{JobID: id, Slug: j.Slug, Title: j.Title, Department: j.Department, EmploymentType: j.EmploymentType,
				Remote: j.Remote, Reopened: reopened, OpenedAt: now, Actor: p.Subject}); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, p.Subject, "job."+to, "job:"+id, nil, "ok")
	})
	if err != nil {
		return nil, err
	}
	out, err := s.GetJob(ctx, p, id)
	if err != nil {
		return nil, err
	}
	switch to {
	case StatusOpen:
		s.syndicate(ctx, out, "publish")
	default:
		s.syndicate(ctx, out, "close")
	}
	return out, nil
}

// ---- syndication ---------------------------------------------------------------------------

// BoardState is the last known state of a job on one board.
type BoardState struct {
	Board       string    `json:"board"`
	Status      string    `json:"status"`
	ExternalRef string    `json:"external_ref"`
	LastError   string    `json:"last_error"`
	UpdatedAt   time.Time `json:"updated_at"`
	Caps        BoardCaps `json:"capabilities"`
}

// syndicate pushes a job's state to every connector. Failures are recorded and logged to the
// row, never returned: syndication must not undo a lifecycle change.
func (s *Store) syndicate(ctx context.Context, j *Job, op string) {
	for _, b := range s.synd {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		var ref string
		_ = s.db.QueryRowContext(cctx, `SELECT external_ref FROM syndication WHERE job_id = ? AND board = ?`, j.ID, b.Name()).Scan(&ref)
		var err error
		status := "published"
		switch {
		case op == "close":
			if ref != "" {
				err = b.Close(cctx, ExternalRef(ref))
			}
			status = "closed"
		case ref == "":
			var r ExternalRef
			r, err = b.Publish(cctx, s.site, j)
			ref = string(r)
		default:
			err = b.Update(cctx, s.site, ExternalRef(ref), j)
		}
		cancel()
		if b.Caps().Status == BoardNeedsPartner && err == nil {
			err = ErrPartnerAccess // a stub never pushed anything, so it never has a state to close
		}
		msg := ""
		switch {
		case errors.Is(err, ErrPartnerAccess):
			status = string(BoardNeedsPartner)
		case err != nil:
			status, msg = "error", shortLine(err.Error())
		case b.Caps().Status == BoardFeed && status == "published":
			status = string(BoardFeed)
		}
		_, _ = s.db.ExecContext(ctx, `INSERT INTO syndication (job_id, board, external_ref, status, last_error, updated_at) VALUES (?,?,?,?,?,?)
			ON CONFLICT (job_id, board) DO UPDATE SET external_ref=excluded.external_ref, status=excluded.status, last_error=excluded.last_error, updated_at=excluded.updated_at`,
			j.ID, b.Name(), ref, status, msg, ts(s.now()))
	}
}

func shortLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// JobSyndication lists the board states of a job the caller can see.
func (s *Store) JobSyndication(ctx context.Context, p *Principal, id string) ([]BoardState, error) {
	if _, err := s.GetJob(ctx, p, id); err != nil {
		return nil, err
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT board, status, external_ref, last_error, updated_at FROM syndication WHERE job_id = ? ORDER BY board`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	have := map[string]BoardState{}
	for rows.Next() {
		var b BoardState
		var at int64
		if err := rows.Scan(&b.Board, &b.Status, &b.ExternalRef, &b.LastError, &at); err != nil {
			return nil, err
		}
		b.UpdatedAt = fromUnix(at)
		have[b.Board] = b
	}
	out := []BoardState{}
	for _, sy := range s.synd {
		b, ok := have[sy.Name()]
		if !ok {
			b = BoardState{Board: sy.Name(), Status: "not_published"}
		}
		b.Caps = sy.Caps()
		out = append(out, b)
	}
	return out, rows.Err()
}

// Boards lists the configured connectors and what each can do.
func (s *Store) Boards() []BoardCaps {
	out := make([]BoardCaps, 0, len(s.synd))
	for _, b := range s.synd {
		out = append(out, b.Caps())
	}
	return out
}

// ---- public reads ----------------------------------------------------------------------------

// PublicJobs lists open, unexpired jobs for the careers page and the feeds. No principal: the
// data is public by design (publish_salary governs the pay range at render time).
func (s *Store) PublicJobs(ctx context.Context) ([]*Job, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE status = 'open' AND (valid_through IS NULL OR valid_through > ?) ORDER BY COALESCE(opened_at, created_at) DESC, id LIMIT 1000`, ts(s.now()))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// PublicJob returns a job by slug for the careers page, in any state except draft. A draft is
// invisible to the public; closed and paused jobs are returned so the page can say so (with
// noindex) instead of a bare 404 for people holding the link.
func (s *Store) PublicJob(ctx context.Context, slug string) (*Job, error) {
	j, err := scanJob(s.rdb.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE slug = ? AND status != 'draft'`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// Accepting reports whether a job takes applications now.
func (j *Job) accepting(now time.Time) bool {
	return j.Status == StatusOpen && (j.ValidThrough == nil || j.ValidThrough.After(now))
}
