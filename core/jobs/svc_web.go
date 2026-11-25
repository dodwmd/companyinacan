package jobs

// Service helpers the web UI (ui*.go) needs and the JSON API did not: what a caller may be offered
// on a page, funnel counts, the interviews assigned to the caller, and a candidate summary that
// carries no sealed field. They sit here, next to the rules they wrap, so the UI files hold no
// authorisation logic of their own (TestUISourceHasNoSecondAuthorizationPath).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Caps is what the caller's token allows, for deciding which links and forms to draw. It is
// presentation only: every action still goes through the Store method, which decides again.
type Caps struct {
	ReadJobs      bool // list and open jobs (admin, recruiter, hiring manager)
	WriteJobs     bool // create, edit, open, pause, close, request approval
	ApproveJobs   bool
	ReadApps      bool
	MoveApps      bool
	HireApps      bool
	AssignPanel   bool
	Score         bool // write scorecards
	ReadCandidate bool // candidate record and its reveal
	EditCandidate bool
	TalentPool    bool
	Reports       bool // diversity aggregates, admin only and without a job-scoped token
	Stages        bool
}

// Caps lists what the principal's role allows.
func (p *Principal) Caps() Caps {
	return Caps{
		ReadJobs: p.Can(ActJobRead), WriteJobs: p.Can(ActJobWrite), ApproveJobs: p.Can(ActApprove), ReadApps: p.Can(ActAppRead),
		MoveApps: p.Can(ActAppMove), HireApps: p.Can(ActHire), AssignPanel: p.Can(ActAssign), Score: p.Can(ActScore),
		ReadCandidate: p.Can(ActCandidateRead), EditCandidate: p.Can(ActCandidateEdit), TalentPool: p.Can(ActTalentPool),
		Reports: p.Can(ActAdmin), Stages: p.Can(ActStages),
	}
}

// Require is the permission check the API guard performs, for a page that draws a form before any
// Store method runs: it returns ErrForbidden (and audits the denial) when the principal's role
// may not perform the action. It decides nothing the Store methods do not decide again.
func (s *Store) Require(ctx context.Context, p *Principal, act Action) error {
	if p.Can(act) {
		return nil
	}
	s.auditDeny(ctx, p, "deny", "ui:"+string(act))
	return ErrForbidden
}

// Funnel counts applications of the jobs a caller may see. It is an aggregate: no names.
type Funnel struct {
	Active    map[string]int `json:"active"` // by stage key
	Rejected  int            `json:"rejected"`
	Withdrawn int            `json:"withdrawn"`
	Hired     int            `json:"hired"`
}

// FunnelCounts counts applications per pipeline stage for one job (jobID set; a job outside the
// caller's scope is not found) or for every job the caller may see (jobID empty).
func (s *Store) FunnelCounts(ctx context.Context, p *Principal, jobID string) (*Funnel, error) {
	if !p.Can(ActJobRead) {
		return nil, ErrForbidden
	}
	if jobID != "" {
		if _, err := s.GetJob(ctx, p, jobID); err != nil {
			return nil, err
		}
	}
	pred, args := p.appPred("a", "j")
	rows, err := s.rdb.QueryContext(ctx, `SELECT a.stage, a.status, COUNT(*) FROM applications a JOIN jobs j ON j.id = a.job_id WHERE (? = '' OR a.job_id = ?) AND `+pred+` GROUP BY a.stage, a.status`,
		append([]any{jobID, jobID}, args...)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	f := &Funnel{Active: map[string]int{}}
	for rows.Next() {
		var stage, status string
		var n int
		if err := rows.Scan(&stage, &status, &n); err != nil {
			return nil, err
		}
		switch status {
		case AppActive:
			f.Active[stage] += n
		case AppRejected:
			f.Rejected += n
		case AppWithdrawn:
			f.Withdrawn += n
		case AppHired:
			f.Hired += n
		}
	}
	return f, rows.Err()
}

// Interview is one application the caller is assigned to interview.
type Interview struct {
	ApplicationID string `json:"application_id"`
	JobTitle      string `json:"job_title"`
	CandidateName string `json:"candidate_name"`
	Stage         string `json:"stage"`
	Scorecard     string `json:"scorecard"` // "", draft or submitted: the caller's own
}

// ListMyInterviews lists the active applications the caller is currently assigned to (a soft
// removed assignment is not listed), newest first.
func (s *Store) ListMyInterviews(ctx context.Context, p *Principal, cursor int64, limit int) ([]Interview, int64, error) {
	if !p.Can(ActScore) {
		return nil, 0, ErrForbidden
	}
	limit = clampLimit(limit)
	if cursor <= 0 {
		cursor = 1 << 62
	}
	pred, pargs := p.appPred("a", "j")
	args := append([]any{p.Subject, p.Subject, cursor}, pargs...)
	rows, err := s.rdb.QueryContext(ctx, `SELECT a.rowid, a.id, j.title, c.name, a.stage, COALESCE((SELECT sc.status FROM scorecards sc WHERE sc.application_id = a.id AND sc.author = ?), '')
		FROM applications a JOIN jobs j ON j.id = a.job_id JOIN candidates c ON c.id = a.candidate_id
		WHERE a.status = 'active' AND EXISTS (SELECT 1 FROM interviewers iv WHERE iv.application_id = a.id AND iv.subject = ? AND iv.removed_at IS NULL) AND (a.rowid < ?) AND `+pred+` ORDER BY a.rowid DESC LIMIT ?`,
		append(args, limit+1)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var out []Interview
	var rowids []int64
	for rows.Next() {
		var rid int64
		var i Interview
		if err := rows.Scan(&rid, &i.ApplicationID, &i.JobTitle, &i.CandidateName, &i.Stage, &i.Scorecard); err != nil {
			return nil, 0, err
		}
		out, rowids = append(out, i), append(rowids, rid)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	_ = rows.Close()
	var next int64
	if len(out) > limit {
		out = out[:limit]
		next = rowids[limit-1]
	}
	if out == nil {
		out = []Interview{}
	}
	if err := s.auditRead(ctx, p, "interviews:list", []string{"name"}); err != nil {
		return nil, 0, err
	}
	return out, next, nil
}

// IsAssigned reports whether the caller is currently assigned to interview the application. An
// application outside the caller's scope is not found; a removed assignment is "not assigned".
func (s *Store) IsAssigned(ctx context.Context, p *Principal, appID string) (bool, error) {
	if !p.Can(ActAppRead) {
		return false, ErrForbidden
	}
	pred, args := p.appPred("a", "j")
	var one int
	err := s.rdb.QueryRowContext(ctx, `SELECT 1 FROM applications a JOIN jobs j ON j.id = a.job_id WHERE (a.id = ?) AND `+pred, append([]any{appID}, args...)...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return s.isInterviewer(ctx, s.rdb, appID, p.Subject)
}

// SearchTalentPool lists talent-pool members whose name starts with prefix (empty: all), optionally
// with one tag. It is ListTalentPool plus a bounded name filter; the scope and the audit are the same.
func (s *Store) SearchTalentPool(ctx context.Context, p *Principal, prefix, tag string, cursor int64, limit int) ([]*Candidate, int64, error) {
	if !p.Can(ActTalentPool) {
		return nil, 0, ErrForbidden
	}
	if tag != "" && !tagRE.MatchString(tag) {
		return nil, 0, invalid("tag", "not a valid tag")
	}
	if len(prefix) > 100 {
		return nil, 0, invalid("name", "at most 100 characters")
	}
	return s.queryCandidates(ctx, p, `c.talent_pool = 1 AND c.erased_at IS NULL AND (? = '' OR c.name LIKE ? ESCAPE '\') AND (? = '' OR EXISTS (SELECT 1 FROM json_each(c.tags) WHERE value = ?))`,
		[]any{prefix, likePrefix(prefix), tag, tag}, cursor, limit, "talent-pool:search")
}

// CandidateSummary is the part of a candidate record that holds no sealed or contact field.
type CandidateSummary struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Tags       []string `json:"tags"`
	Source     string   `json:"source"`
	TalentPool bool     `json:"talent_pool"`
	Erased     bool     `json:"erased"`
}

// GetCandidateSummary returns a candidate's name, tags and talent-pool flag. Nothing sealed is
// unsealed, so it reads no sensitive field (audited as a name read).
func (s *Store) GetCandidateSummary(ctx context.Context, p *Principal, id string) (*CandidateSummary, error) {
	if !p.Can(ActCandidateRead) {
		return nil, ErrForbidden
	}
	pred, args := p.candidatePred("c")
	var c CandidateSummary
	var tags string
	var pool int
	var erased sql.NullInt64
	err := s.rdb.QueryRowContext(ctx, `SELECT c.id, c.name, c.tags, c.source, c.talent_pool, c.erased_at FROM candidates c WHERE (c.id = ?) AND `+pred, append([]any{id}, args...)...).
		Scan(&c.ID, &c.Name, &tags, &c.Source, &pool, &erased)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(tags), &c.Tags); err != nil {
		return nil, err
	}
	if c.Tags == nil {
		c.Tags = []string{}
	}
	c.TalentPool, c.Erased = pool == 1, erased.Valid
	if err := s.auditRead(ctx, p, "candidate:"+id, []string{"name"}); err != nil {
		return nil, err
	}
	return &c, nil
}
