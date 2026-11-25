package people

// Performance reviews (Epic E14b): templates, cycles (draft -> open -> calibration -> closed),
// self and manager reviews, calibration, release with a separate approver, acknowledgement.
// Peer and upward feedback is in reviews_feedback.go. Every authorisation decision is made by
// the policy layer (policy.go); everything written that is free text or a rating is sealed with
// an AAD that binds it to its row and part.
//
// This is a tool, not legal, HR or payroll advice.

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
	"sort"
	"strings"
	"time"
)

// ErrSeparation means the action needs someone other than the people already involved.
var ErrSeparation = fmt.Errorf("%w: separation of duties: someone else must do this", ErrForbidden)

// Question is one template question. Audience is who answers it: all, self, manager, peer, upward.
type Question struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Type     string `json:"type"` // rating | text
	Audience string `json:"audience"`
}

// Answer is one answer: a rating question takes Rating, a text question Text.
type Answer struct {
	Rating *int   `json:"rating,omitempty"`
	Text   string `json:"text,omitempty"`
}

// Answers maps a question id to its answer.
type Answers map[string]Answer

// TemplateIn creates a review template.
type TemplateIn struct {
	Name      string     `json:"name"`
	Questions []Question `json:"questions"`
}

// ReviewTemplate is a stored template.
type ReviewTemplate struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Questions []Question `json:"questions"`
	CreatedAt string     `json:"created_at"`
}

// CycleIn creates a review cycle.
type CycleIn struct {
	Name         string `json:"name"`
	TemplateID   string `json:"template_id"`
	Anonymity    string `json:"anonymity"` // anonymous | attributed (peer and upward feedback)
	MinResponses int    `json:"min_responses"`
	RatingMax    int    `json:"rating_max"`
	DueDate      string `json:"due_date"`
}

// Cycle is a review cycle.
type Cycle struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	State        string     `json:"state"`
	TemplateID   string     `json:"template_id,omitempty"`
	Anonymity    string     `json:"anonymity"`
	MinResponses int        `json:"min_responses"`
	RatingMax    int        `json:"rating_max"`
	DueDate      string     `json:"due_date,omitempty"`
	Questions    []Question `json:"questions"`
	CreatedAt    string     `json:"created_at"`
}

// ReviewSummary is a row in a list of reviews: no content.
type ReviewSummary struct {
	ID         string `json:"id"`
	CycleID    string `json:"cycle_id"`
	PersonID   string `json:"person_id"`
	CycleState string `json:"cycle_state"`
	Status     string `json:"status"`
}

// SelfView is the self-assessment.
type SelfView struct {
	Answers   Answers `json:"answers"`
	Submitted bool    `json:"submitted"`
}

// ManagerView is the manager review. Rating is the manager's proposed rating: the subject sees
// the calibrated outcome instead, never this.
type ManagerView struct {
	Answers   Answers `json:"answers"`
	Rating    *int    `json:"rating,omitempty"`
	Summary   string  `json:"summary,omitempty"`
	Submitted bool    `json:"submitted"`
	AuthorID  string  `json:"author_id,omitempty"`
}

// CalibrationView is the calibration result and notes: hr only, never the subject.
type CalibrationView struct {
	Rating        *int   `json:"rating,omitempty"`
	Notes         string `json:"notes,omitempty"`
	LastRejection string `json:"last_release_rejection,omitempty"`
}

// Outcome is the final rating and summary of a review.
type Outcome struct {
	Rating   int    `json:"rating"`
	Summary  string `json:"summary,omitempty"`
	Released bool   `json:"released"`
}

// GoalRef is a read-only reference to one of the subject's goals.
type GoalRef struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Progress int    `json:"progress"`
}

// Review is the view of one person's review, filtered for the caller. Parts the caller may not
// see are absent, not empty.
type Review struct {
	ID             string           `json:"id"`
	CycleID        string           `json:"cycle_id"`
	PersonID       string           `json:"person_id"`
	CycleState     string           `json:"cycle_state"`
	Status         string           `json:"status"`
	Questions      []Question       `json:"questions,omitempty"`
	Self           *SelfView        `json:"self,omitempty"`
	Manager        *ManagerView     `json:"manager,omitempty"`
	Calibration    *CalibrationView `json:"calibration,omitempty"`
	Outcome        *Outcome         `json:"outcome,omitempty"`
	Peer           *FeedbackView    `json:"peer_feedback,omitempty"`
	Upward         *FeedbackView    `json:"upward_feedback,omitempty"`
	Goals          []GoalRef        `json:"goals,omitempty"`
	ReleasedAt     string           `json:"released_at,omitempty"`
	AcknowledgedAt string           `json:"acknowledged_at,omitempty"`
}

var (
	revIDRe      = regexp.MustCompile(`^rev_[0-9a-f]{24}$`)
	cycleIDRe    = regexp.MustCompile(`^cyc_[0-9a-f]{24}$`)
	templateIDRe = regexp.MustCompile(`^rtp_[0-9a-f]{24}$`)
	questionIDRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
)

const (
	maxQuestions     = 30
	maxAnswerText    = 4000
	maxParticipants  = 500
	maxReviewerSlots = 12
	defaultRatingMax = 5
	minAnonymity     = 3
	minUpward        = 5 // upward feedback about a manager from fewer than five reports is too easy to attribute
)

// ---- templates and cycles --------------------------------------------------------------------

func validateQuestions(qs []Question) error {
	if len(qs) == 0 || len(qs) > maxQuestions {
		return invalid("a template needs between 1 and %d questions", maxQuestions)
	}
	seen := map[string]bool{}
	for _, q := range qs {
		if !questionIDRe.MatchString(q.ID) || seen[q.ID] {
			return invalid("question ids must be unique and match %s", questionIDRe)
		}
		seen[q.ID] = true
		if err := validLine("question text", q.Text, 300, true); err != nil {
			return err
		}
		if q.Type != "rating" && q.Type != "text" {
			return invalid("question type must be rating or text")
		}
		if !slices.Contains([]string{"all", "self", "manager", "peer", "upward"}, q.Audience) {
			return invalid("question audience must be all, self, manager, peer or upward")
		}
	}
	return nil
}

// CreateReviewTemplate stores a template. hr or admin.
func (s *Service) CreateReviewTemplate(ctx context.Context, a Actor, in TemplateIn) (*ReviewTemplate, error) {
	if !Decide(a, ActRevManage, Fact{}) {
		s.auditDeny(ctx, a, "write", "review-template", nil)
		return nil, ErrForbidden
	}
	if err := validLine("name", in.Name, 100, true); err != nil {
		return nil, err
	}
	if err := validateQuestions(in.Questions); err != nil {
		return nil, err
	}
	qb, _ := json.Marshal(in.Questions)
	id := newID("rtp")
	now := s.now().Unix()
	err := s.txAudit(ctx, a, "write", "review-template:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO review_templates (id, name, questions, created_by, created_at) VALUES (?,?,?,?,?)`, id, in.Name, string(qb), a.auditID(), now)
		return err
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, conflict("a template with this name exists")
		}
		return nil, err
	}
	return &ReviewTemplate{ID: id, Name: in.Name, Questions: in.Questions, CreatedAt: time.Unix(now, 0).UTC().Format(time.RFC3339)}, nil
}

// ListReviewTemplates lists templates. hr or admin.
func (s *Service) ListReviewTemplates(ctx context.Context, a Actor) ([]ReviewTemplate, error) {
	if !Decide(a, ActRevManage, Fact{}) {
		return nil, ErrForbidden
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, questions, created_at FROM review_templates ORDER BY created_at, id LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ReviewTemplate{}
	for rows.Next() {
		var t ReviewTemplate
		var qs string
		var at int64
		if err := rows.Scan(&t.ID, &t.Name, &qs, &at); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(qs), &t.Questions)
		t.CreatedAt = time.Unix(at, 0).UTC().Format(time.RFC3339)
		out = append(out, t)
	}
	return out, rows.Err()
}

type cycleRow struct {
	Cycle
	createdBy string
}

func (s *Service) loadCycle(ctx context.Context, q querier, id string) (*cycleRow, error) {
	if !cycleIDRe.MatchString(id) {
		return nil, ErrNotFound
	}
	var c cycleRow
	var tpl sql.NullString
	var qs string
	var at int64
	err := q.QueryRowContext(ctx, `SELECT id, name, state, template_id, questions, anonymity, min_responses, rating_max, due_date, created_by, created_at FROM review_cycles WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &c.State, &tpl, &qs, &c.Anonymity, &c.MinResponses, &c.RatingMax, &c.DueDate, &c.createdBy, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.TemplateID = tpl.String
	_ = json.Unmarshal([]byte(qs), &c.Questions)
	c.CreatedAt = time.Unix(at, 0).UTC().Format(time.RFC3339)
	return &c, nil
}

// CreateCycle creates a draft cycle from a template (the questions are copied: later template
// edits do not change a running cycle). hr or admin.
func (s *Service) CreateCycle(ctx context.Context, a Actor, in CycleIn) (*Cycle, error) {
	if !Decide(a, ActRevManage, Fact{}) {
		s.auditDeny(ctx, a, "write", "review-cycle", nil)
		return nil, ErrForbidden
	}
	if err := validLine("name", in.Name, 100, true); err != nil {
		return nil, err
	}
	if in.Anonymity != "anonymous" && in.Anonymity != "attributed" {
		return nil, invalid("anonymity must be anonymous or attributed")
	}
	if in.MinResponses == 0 {
		in.MinResponses = max(minAnonymity, s.minResponses)
	}
	if in.MinResponses < minAnonymity || in.MinResponses > 50 {
		return nil, invalid("min_responses must be between %d and 50: fewer responses could identify the author", minAnonymity)
	}
	if in.RatingMax == 0 {
		in.RatingMax = defaultRatingMax
	}
	if in.RatingMax < 3 || in.RatingMax > 10 {
		return nil, invalid("rating_max must be between 3 and 10")
	}
	if in.DueDate != "" {
		if err := validDate(in.DueDate); err != nil {
			return nil, err
		}
	}
	if !templateIDRe.MatchString(in.TemplateID) {
		return nil, invalid("template_id is required")
	}
	id := newID("cyc")
	now := s.now().Unix()
	err := s.txAudit(ctx, a, "write", "review-cycle:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		var qs string
		if err := tx.QueryRowContext(ctx, `SELECT questions FROM review_templates WHERE id = ?`, in.TemplateID).Scan(&qs); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return invalid("unknown template")
			}
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO review_cycles (id, name, state, template_id, questions, anonymity, min_responses, rating_max, due_date, created_by, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			id, in.Name, "draft", in.TemplateID, qs, in.Anonymity, in.MinResponses, in.RatingMax, in.DueDate, a.auditID(), now)
		return err
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, conflict("a cycle with this name exists")
		}
		return nil, err
	}
	return s.GetCycle(ctx, a, id)
}

// cycleVisible reports whether the actor may know the cycle exists: hr, or someone who
// participates in it (as subject, as a manager of a participant, or as a chosen reviewer).
func (s *Service) cycleVisible(ctx context.Context, a Actor, cycleID string) (bool, error) {
	if !a.permits("people.reviews.read") {
		return false, nil
	}
	if a.hrAdmin() {
		return true, nil
	}
	if !a.HasPerson || a.blocked() {
		return false, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_participants rp JOIN people p ON p.id = rp.person_id
		WHERE rp.cycle_id = ? AND (rp.person_id = ? OR p.manager_id = ?
		OR EXISTS (SELECT 1 FROM review_requests rq WHERE rq.participant_id = rp.id AND rq.reviewer_id = ? AND rq.status IN ('approved','submitted')))`,
		cycleID, a.PersonID, a.PersonID, a.PersonID).Scan(&n)
	return n > 0, err
}

// GetCycle returns a cycle to hr or to a participant.
func (s *Service) GetCycle(ctx context.Context, a Actor, id string) (*Cycle, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	c, err := s.loadCycle(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	ok, err := s.cycleVisible(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		s.auditDeny(ctx, a, "read", "review-cycle:"+id, nil)
		return nil, ErrNotFound
	}
	return &c.Cycle, nil
}

// ListCycles lists the cycles the caller may see.
func (s *Service) ListCycles(ctx context.Context, a Actor) ([]Cycle, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	// Visibility is applied in SQL, before the limit: a caller's own cycles must not be pushed out
	// of the first 500 rows by other people's.
	q := `SELECT id FROM review_cycles c ORDER BY created_at, id LIMIT 500`
	var qargs []any
	if !a.hrAdmin() {
		q = `SELECT id FROM review_cycles c WHERE EXISTS (SELECT 1 FROM review_participants rp JOIN people p ON p.id = rp.person_id
			WHERE rp.cycle_id = c.id AND (rp.person_id = ? OR p.manager_id = ?
			OR EXISTS (SELECT 1 FROM review_requests rq WHERE rq.participant_id = rp.id AND rq.reviewer_id = ? AND rq.status IN ('approved','submitted'))))
			ORDER BY created_at, id LIMIT 500`
		qargs = []any{a.PersonID, a.PersonID, a.PersonID}
	}
	rows, err := s.db.QueryContext(ctx, q, qargs...)
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
	out := []Cycle{}
	for _, id := range ids {
		ok, err := s.cycleVisible(ctx, a, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		c, err := s.loadCycle(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, c.Cycle)
	}
	return out, nil
}

// AddParticipants adds people to a draft or open cycle and returns their review ids (in input
// order; a person already in the cycle returns the existing id). hr or admin.
func (s *Service) AddParticipants(ctx context.Context, a Actor, cycleID string, personIDs []string) ([]string, error) {
	if !Decide(a, ActRevManage, Fact{}) {
		s.auditDeny(ctx, a, "write", "review-cycle:"+cycleID, nil)
		return nil, ErrForbidden
	}
	if len(personIDs) == 0 || len(personIDs) > maxParticipants {
		return nil, invalid("between 1 and %d people per call", maxParticipants)
	}
	ids := make([]string, 0, len(personIDs))
	err := s.txAudit(ctx, a, "write", "review-cycle:"+cycleID, []string{"participants"}, func(ctx context.Context, tx *sqlTx) error {
		cy, err := s.loadCycle(ctx, tx, cycleID)
		if err != nil {
			return err
		}
		if cy.State != "draft" && cy.State != "open" {
			return conflict("people cannot be added to a %s cycle", cy.State)
		}
		for _, pid := range personIDs {
			if !personIDRe.MatchString(pid) {
				return invalid("%q is not a person id", pid)
			}
			si, err := s.subject(ctx, tx, pid)
			if err != nil {
				return invalid("unknown person %s", pid)
			}
			if si.erased || (si.status != StActive && si.status != StOnboarding && si.status != StLeave) {
				return invalid("person %s is not current staff", pid)
			}
			var existing string
			err = tx.QueryRowContext(ctx, `SELECT id FROM review_participants WHERE cycle_id = ? AND person_id = ?`, cycleID, pid).Scan(&existing)
			if err == nil {
				ids = append(ids, existing)
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			rid := newID("rev")
			now := s.now().Unix()
			if _, err := tx.ExecContext(ctx, `INSERT INTO review_participants (id, cycle_id, person_id, created_at, updated_at) VALUES (?,?,?,?,?)`, rid, cycleID, pid, now, now); err != nil {
				return err
			}
			ids = append(ids, rid)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

var cycleNext = map[string]string{"draft": "open", "open": "calibration", "calibration": "closed"}

// SetCycleState moves a cycle forward one state (draft -> open -> calibration -> closed). hr or admin.
func (s *Service) SetCycleState(ctx context.Context, a Actor, cycleID, to string) (*Cycle, error) {
	if !Decide(a, ActRevManage, Fact{}) {
		s.auditDeny(ctx, a, "write", "review-cycle:"+cycleID, []string{"state"})
		return nil, ErrForbidden
	}
	err := s.txAudit(ctx, a, "write", "review-cycle:"+cycleID, []string{"state"}, func(ctx context.Context, tx *sqlTx) error {
		cy, err := s.loadCycle(ctx, tx, cycleID)
		if err != nil {
			return err
		}
		if cycleNext[cy.State] != to {
			return conflict("a %s cycle cannot move to %q", cy.State, to)
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_participants WHERE cycle_id = ?`, cycleID).Scan(&n); err != nil {
			return err
		}
		now := s.now().Unix()
		switch to {
		case "open":
			if n == 0 {
				return conflict("a cycle needs at least one participant before it opens")
			}
			if _, err := tx.ExecContext(ctx, `UPDATE review_cycles SET state = 'open', opened_at = ? WHERE id = ?`, now, cycleID); err != nil {
				return err
			}
			return s.enqueue(ctx, tx, EventReviewCycleOpened, map[string]any{"cycle_id": cycleID, "opened_on": s.today(), "participants": n})
		case "calibration":
			_, err = tx.ExecContext(ctx, `UPDATE review_cycles SET state = 'calibration', calibration_at = ? WHERE id = ?`, now, cycleID)
		case "closed":
			var pending int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_participants WHERE cycle_id = ? AND release_state = 'requested'`, cycleID).Scan(&pending); err != nil {
				return err
			}
			if pending > 0 {
				return conflict("%d release requests are waiting for a decision", pending)
			}
			_, err = tx.ExecContext(ctx, `UPDATE review_cycles SET state = 'closed', closed_at = ? WHERE id = ?`, now, cycleID)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetCycle(ctx, a, cycleID)
}

// ---- one review ---------------------------------------------------------------------------------

type revRow struct {
	id, cycleID, personID                       string
	self, mgr, calib, final, ack, relNote       sql.NullString
	selfAt, mgrAt, releasedAt, ackAt            sql.NullInt64
	mgrAuthor, calibratedBy, relReqBy, relDecBy sql.NullString
	releaseState, approvalRef                   string
	touched                                     []string
}

const revCols = `id, cycle_id, person_id, self_sealed, self_submitted_at, mgr_sealed, mgr_author, mgr_submitted_at, calib_sealed, calibrated_by,
 final_sealed, release_state, release_requested_by, release_decided_by, approval_ref, release_note_sealed, released_at, ack_at, ack_sealed, touched_by`

func scanRev(sc interface{ Scan(...any) error }) (*revRow, error) {
	var r revRow
	var touched string
	if err := sc.Scan(&r.id, &r.cycleID, &r.personID, &r.self, &r.selfAt, &r.mgr, &r.mgrAuthor, &r.mgrAt, &r.calib, &r.calibratedBy,
		&r.final, &r.releaseState, &r.relReqBy, &r.relDecBy, &r.approvalRef, &r.relNote, &r.releasedAt, &r.ackAt, &r.ack, &touched); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(touched), &r.touched)
	return &r, nil
}

func (s *Service) loadRev(ctx context.Context, q querier, id string) (*revRow, error) {
	if !revIDRe.MatchString(id) {
		return nil, ErrNotFound
	}
	r, err := scanRev(q.QueryRowContext(ctx, `SELECT `+revCols+` FROM review_participants WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

func revAAD(id, part string) string { return "review:" + id + ":" + part }

func (s *Service) openJSON(sealed sql.NullString, aad string, v any) error {
	if !sealed.Valid || sealed.String == "" {
		return nil
	}
	plain, err := s.keys.Open(sealed.String, aad)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(plain), v)
}

func (s *Service) sealJSON(v any, aad string) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return s.keys.Seal(string(b), aad)
}

func hashID(s string) string {
	h := sha256.Sum256([]byte(canonSubject(s)))
	return hex.EncodeToString(h[:])
}

// addTouched records the actor in the review's separation-of-duties history (hashes, no names).
func addTouched(touched []string, a Actor) []string {
	for _, id := range []string{a.Subject, a.PersonID} {
		if canonSubject(id) == "" {
			continue
		}
		if h := hashID(id); !slices.Contains(touched, h) {
			touched = append(touched, h)
		}
	}
	return touched
}

func touchedJSON(t []string) string { b, _ := json.Marshal(t); return string(b) }

// sodChainOK is sodOK plus the stored history of hashed identities.
func sodChainOK(a Actor, touched []string, others ...string) bool {
	if !sodOK(a, others...) {
		return false
	}
	for _, id := range []string{a.Subject, a.PersonID} {
		if canonSubject(id) != "" && slices.Contains(touched, hashID(id)) {
			return false
		}
	}
	return true
}

// revFact resolves the caller's relationship to a review and the review's state. It answers
// ErrNotFound unless the caller may know the review exists (404, not 403).
func (s *Service) revFact(ctx context.Context, q querier, a Actor, id string) (*revRow, *cycleRow, Fact, error) {
	var f Fact
	row, err := s.loadRev(ctx, q, id)
	if err != nil {
		return nil, nil, f, err
	}
	cy, err := s.loadCycle(ctx, q, row.cycleID)
	if err != nil {
		return nil, nil, f, err
	}
	if a.HasPerson {
		f, _, err = s.factFor(ctx, q, a, row.personID)
		if err != nil {
			return nil, nil, f, err
		}
	}
	f.CycleState = cy.State
	f.SelfDone, f.MgrDone = row.selfAt.Valid, row.mgrAt.Valid
	f.Released, f.ReleaseRequested = row.releaseState == "released", row.releaseState == "requested"
	f.Status = ""
	if !Decide(a, ActRevMeta, f) {
		return nil, nil, f, ErrNotFound // the caller audits the refusal outside any transaction
	}
	return row, cy, f, nil
}

func reviewStatus(r *revRow) string {
	switch {
	case r.ackAt.Valid:
		return "acknowledged"
	case r.releaseState == "released":
		return "released"
	case r.releaseState == "requested":
		return "release_requested"
	case r.mgrAt.Valid:
		return "manager_submitted"
	case r.selfAt.Valid:
		return "self_submitted"
	}
	return "in_progress"
}

func ts(n int64) string { return time.Unix(n, 0).UTC().Format(time.RFC3339) }

func audienceQuestions(qs []Question, audiences ...string) []Question {
	var out []Question
	for _, q := range qs {
		if q.Audience == "all" || slices.Contains(audiences, q.Audience) {
			out = append(out, q)
		}
	}
	return out
}

// validateAnswers checks answers against the questions the audience may answer.
func validateAnswers(qs []Question, ratingMax int, in Answers) error {
	byID := map[string]Question{}
	for _, q := range qs {
		byID[q.ID] = q
	}
	if len(in) > len(byID) {
		return invalid("too many answers")
	}
	for id, ans := range in {
		q, ok := byID[id]
		if !ok {
			return invalid("question %q is not part of this review", id)
		}
		switch q.Type {
		case "rating":
			if ans.Rating == nil || *ans.Rating < 1 || *ans.Rating > ratingMax || ans.Text != "" {
				return invalid("question %q takes a rating from 1 to %d", id, ratingMax)
			}
		case "text":
			if ans.Rating != nil {
				return invalid("question %q takes text", id)
			}
			if err := validText("answer", ans.Text, maxAnswerText, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// GetReview returns one review, filtered for the caller. The read is audited before the data is
// returned; if the audit cannot be written the read is refused.
func (s *Service) GetReview(ctx context.Context, a Actor, id string) (*Review, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	row, cy, f, err := s.revFact(ctx, s.db, a, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			s.auditDeny(ctx, a, "read", "review:"+id, nil)
		}
		return nil, err
	}
	v := &Review{ID: row.id, CycleID: row.cycleID, PersonID: row.personID, CycleState: cy.State, Status: reviewStatus(row)}
	var parts []string
	if row.releasedAt.Valid {
		v.ReleasedAt = ts(row.releasedAt.Int64)
	}
	if row.ackAt.Valid {
		v.AcknowledgedAt = ts(row.ackAt.Int64)
	}
	if Decide(a, ActRevSelfRead, f) && row.self.Valid {
		sv := &SelfView{Answers: Answers{}, Submitted: row.selfAt.Valid}
		if err := s.openJSON(row.self, revAAD(row.id, "self"), &sv.Answers); err != nil {
			return nil, errors.New("cannot decrypt review")
		}
		v.Self = sv
		parts = append(parts, "self")
	}
	var mgr struct {
		Answers Answers `json:"answers"`
		Rating  *int    `json:"rating"`
		Summary string  `json:"summary"`
	}
	if Decide(a, ActRevMgrRead, f) && row.mgr.Valid {
		if err := s.openJSON(row.mgr, revAAD(row.id, "mgr"), &mgr); err != nil {
			return nil, errors.New("cannot decrypt review")
		}
		mv := &ManagerView{Answers: mgr.Answers, Submitted: row.mgrAt.Valid}
		if !f.Self { // the subject sees the calibrated outcome, not the manager's proposal
			mv.Rating, mv.Summary, mv.AuthorID = mgr.Rating, mgr.Summary, row.mgrAuthor.String
		}
		v.Manager = mv
		parts = append(parts, "manager")
	}
	if Decide(a, ActRevCalibRead, f) && (row.calib.Valid || row.relNote.Valid) {
		cv := &CalibrationView{}
		var cb struct {
			Rating *int   `json:"rating"`
			Notes  string `json:"notes"`
		}
		if err := s.openJSON(row.calib, revAAD(row.id, "calib"), &cb); err != nil {
			return nil, errors.New("cannot decrypt review")
		}
		cv.Rating, cv.Notes = cb.Rating, cb.Notes
		var note string
		if row.relNote.Valid {
			plain, err := s.keys.Open(row.relNote.String, revAAD(row.id, "relnote"))
			if err != nil {
				return nil, errors.New("cannot decrypt review")
			}
			note = plain
		}
		cv.LastRejection = note
		v.Calibration = cv
		parts = append(parts, "calibration")
	}
	if Decide(a, ActRevOutcomeRead, f) && row.final.Valid {
		var fin struct {
			Rating  int    `json:"rating"`
			Summary string `json:"summary"`
		}
		if err := s.openJSON(row.final, revAAD(row.id, "final"), &fin); err != nil {
			return nil, errors.New("cannot decrypt review")
		}
		v.Outcome = &Outcome{Rating: fin.Rating, Summary: fin.Summary, Released: row.releaseState == "released"}
		parts = append(parts, "outcome")
	}
	if Decide(a, ActRevPeerRead, f) {
		raw := Decide(a, ActRevPeerRaw, f)
		for _, kind := range []string{"peer", "upward"} {
			fv, err := s.feedbackView(ctx, cy, row.id, kind, raw)
			if err != nil {
				return nil, err
			}
			if fv == nil {
				continue
			}
			if kind == "peer" {
				v.Peer = fv
			} else {
				v.Upward = fv
			}
		}
		if v.Peer != nil || v.Upward != nil {
			parts = append(parts, "feedback")
		}
	}
	if Decide(a, ActRevSelfRead, f) || Decide(a, ActRevMgrRead, f) {
		v.Questions = cy.Questions
		if f.Self {
			v.Questions = audienceQuestions(cy.Questions, "self")
		} else if !a.hrAdmin() {
			v.Questions = audienceQuestions(cy.Questions, "self", "manager")
		}
		if gl, err := s.ListGoals(ctx, a, GoalFilter{OwnerID: row.personID, Scope: "person"}); err == nil {
			for _, g := range gl {
				v.Goals = append(v.Goals, GoalRef{ID: g.ID, Title: g.Title, Status: g.Status, Progress: g.Progress})
			}
		}
	}
	sort.Strings(parts)
	if err := s.auditRead(ctx, a, "review:"+id, parts); err != nil {
		return nil, errors.New("audit unavailable, read refused")
	}
	return v, nil
}

// ListReviews lists the reviews the caller may know about: their own, their direct reports'
// (live reporting line), released ones further down their chain, and for hr every one.
func (s *Service) ListReviews(ctx context.Context, a Actor, cycleID string) ([]ReviewSummary, error) {
	if a.blocked() || !a.permits("people.reviews.read") {
		return []ReviewSummary{}, nil
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	q := `SELECT rp.id, rp.cycle_id, rp.person_id, c.state, rp.self_submitted_at, rp.mgr_submitted_at, rp.release_state, rp.released_at, rp.ack_at, p.manager_id
		FROM review_participants rp JOIN review_cycles c ON c.id = rp.cycle_id JOIN people p ON p.id = rp.person_id WHERE 1=1`
	var args []any
	if cycleID != "" {
		if !cycleIDRe.MatchString(cycleID) {
			return []ReviewSummary{}, nil
		}
		q += ` AND rp.cycle_id = ?`
		args = append(args, cycleID)
	}
	if !a.hrAdmin() {
		// own review, or anyone below the caller in the reporting line; applied before the limit
		q += ` AND (rp.person_id = ? OR rp.person_id IN (WITH RECURSIVE r(id) AS (SELECT id FROM people WHERE manager_id = ?
			UNION SELECT p2.id FROM people p2 JOIN r ON p2.manager_id = r.id) SELECT id FROM r))`
		args = append(args, a.PersonID, a.PersonID)
	}
	q += ` ORDER BY rp.created_at, rp.id LIMIT 1000`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	type cand struct {
		sum ReviewSummary
		row revRow
		mgr sql.NullString
	}
	var cands []cand
	for rows.Next() {
		var c cand
		var selfAt, mgrAt, relAt, ackAt sql.NullInt64
		if err := rows.Scan(&c.sum.ID, &c.sum.CycleID, &c.sum.PersonID, &c.sum.CycleState, &selfAt, &mgrAt, &c.row.releaseState, &relAt, &ackAt, &c.mgr); err != nil {
			_ = rows.Close()
			return nil, err
		}
		c.row.selfAt, c.row.mgrAt, c.row.releasedAt, c.row.ackAt = selfAt, mgrAt, relAt, ackAt
		cands = append(cands, c)
	}
	_ = rows.Close()
	out := []ReviewSummary{}
	for _, c := range cands {
		keep := a.hrAdmin() && c.sum.PersonID != a.PersonID
		if !keep && a.HasPerson {
			switch {
			case c.sum.PersonID == a.PersonID:
				keep = true
			case a.elevated(RoleManager):
				if c.mgr.String == a.PersonID {
					keep = true
				} else if c.row.releaseState == "released" {
					chain, err := managerChain(ctx, s.db, c.sum.PersonID)
					if err != nil {
						return nil, err
					}
					keep = slices.Contains(chain, a.PersonID)
				}
			}
		}
		if keep || a.hrAdmin() {
			c.sum.Status = reviewStatus(&c.row)
			out = append(out, c.sum)
		}
	}
	return out, nil
}

// ---- writing --------------------------------------------------------------------------------

// SaveSelf stores the self-assessment; submit locks it. Only the subject, while the cycle is open.
func (s *Service) SaveSelf(ctx context.Context, a Actor, id string, in Answers, submit bool) error {
	return s.revMutate(ctx, a, "write", id, []string{"self"}, func(ctx context.Context, tx *sqlTx) error {
		row, cy, f, err := s.revFact(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if !Decide(a, ActRevSelfWrite, f) {
			return s.denyWrite(ctx, a, id, f, cy.State == "open" && f.SelfDone, "the self-assessment was already submitted")
		}
		if err := validateAnswers(audienceQuestions(cy.Questions, "self"), cy.RatingMax, in); err != nil {
			return err
		}
		sealed, err := s.sealJSON(in, revAAD(id, "self"))
		if err != nil {
			return err
		}
		var at any
		if submit {
			at = s.now().Unix()
		}
		_, err = tx.ExecContext(ctx, `UPDATE review_participants SET self_sealed = ?, self_submitted_at = ?, updated_at = ? WHERE id = ?`, sealed, at, s.now().Unix(), row.id)
		return err
	})
}

// denyWrite maps a refused write: a locked part is a conflict, anything else is forbidden (the
// caller already passed the 404 check in revFact).
func (s *Service) denyWrite(_ context.Context, _ Actor, _ string, _ Fact, locked bool, msg string) error {
	if locked {
		return conflict("%s", msg)
	}
	return ErrForbidden
}

// ManagerIn is the manager review.
type ManagerIn struct {
	Answers Answers `json:"answers"`
	Rating  *int    `json:"rating"`
	Summary string  `json:"summary"`
	Submit  bool    `json:"submit"`
}

// SaveManager stores the manager review; submit locks it. Only the live direct manager, while open.
func (s *Service) SaveManager(ctx context.Context, a Actor, id string, in ManagerIn) error {
	return s.revMutate(ctx, a, "write", id, []string{"manager"}, func(ctx context.Context, tx *sqlTx) error {
		row, cy, f, err := s.revFact(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if !Decide(a, ActRevMgrWrite, f) {
			return s.denyWrite(ctx, a, id, f, cy.State == "open" && f.MgrDone && a.managerOf(f), "the manager review was already submitted")
		}
		if err := validateAnswers(audienceQuestions(cy.Questions, "manager"), cy.RatingMax, in.Answers); err != nil {
			return err
		}
		if in.Rating != nil && (*in.Rating < 1 || *in.Rating > cy.RatingMax) {
			return invalid("rating must be from 1 to %d", cy.RatingMax)
		}
		if err := validText("summary", in.Summary, 2000, false); err != nil {
			return err
		}
		if in.Submit && in.Rating == nil {
			return invalid("an overall rating is required to submit")
		}
		blob := map[string]any{"answers": in.Answers, "rating": in.Rating, "summary": in.Summary}
		sealed, err := s.sealJSON(blob, revAAD(id, "mgr"))
		if err != nil {
			return err
		}
		var at any
		if in.Submit {
			at = s.now().Unix()
		}
		touched := addTouched(row.touched, a)
		_, err = tx.ExecContext(ctx, `UPDATE review_participants SET mgr_sealed = ?, mgr_author = ?, mgr_submitted_at = ?, touched_by = ?, updated_at = ? WHERE id = ?`,
			sealed, a.PersonID, at, touchedJSON(touched), s.now().Unix(), id)
		return err
	})
}

// Calibrate sets the calibrated rating and the calibration notes during the calibration stage. hr.
func (s *Service) Calibrate(ctx context.Context, a Actor, id string, rating int, notes string) error {
	return s.revMutate(ctx, a, "write", id, []string{"calibration"}, func(ctx context.Context, tx *sqlTx) error {
		row, cy, f, err := s.revFact(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if !Decide(a, ActRevCalibWrite, f) {
			if a.hrOther(f) {
				return conflict("calibration happens while the cycle is in calibration and before release")
			}
			return s.denyWrite(ctx, a, id, f, false, "")
		}
		if rating < 1 || rating > cy.RatingMax {
			return invalid("rating must be from 1 to %d", cy.RatingMax)
		}
		if err := validText("notes", notes, 4000, false); err != nil {
			return err
		}
		sealed, err := s.sealJSON(map[string]any{"rating": rating, "notes": notes}, revAAD(id, "calib"))
		if err != nil {
			return err
		}
		touched := addTouched(row.touched, a)
		_, err = tx.ExecContext(ctx, `UPDATE review_participants SET calib_sealed = ?, calibrated_by = ?, touched_by = ?, updated_at = ? WHERE id = ?`,
			sealed, a.auditID(), touchedJSON(touched), s.now().Unix(), id)
		return err
	})
}

// ---- release and acknowledgement ---------------------------------------------------------------

func (s *Service) mgrBlob(row *revRow) (rating *int, summary string, err error) {
	var m struct {
		Rating  *int   `json:"rating"`
		Summary string `json:"summary"`
	}
	if err := s.openJSON(row.mgr, revAAD(row.id, "mgr"), &m); err != nil {
		return nil, "", errors.New("cannot decrypt review")
	}
	return m.Rating, m.Summary, nil
}

// RequestRelease freezes the outcome (calibrated rating, else the manager's) and asks for it to
// be released to the subject. hr or admin, never for their own review. The approver must be
// someone else (DecideRelease).
func (s *Service) RequestRelease(ctx context.Context, a Actor, id string) error {
	return s.revMutate(ctx, a, "release.request", id, nil, func(ctx context.Context, tx *sqlTx) error {
		row, cy, f, err := s.revFact(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if !Decide(a, ActRevRelease, f) {
			if a.hrOther(f) {
				return conflict("a release needs the calibration stage, a submitted manager review and no release already requested or done")
			}
			return s.denyWrite(ctx, a, id, f, false, "")
		}
		rating, summary, err := s.mgrBlob(row)
		if err != nil {
			return err
		}
		var cb struct {
			Rating *int `json:"rating"`
		}
		if err := s.openJSON(row.calib, revAAD(id, "calib"), &cb); err != nil {
			return errors.New("cannot decrypt review")
		}
		if cb.Rating != nil {
			rating = cb.Rating
		}
		if rating == nil {
			return conflict("the review has no rating")
		}
		sealed, err := s.sealJSON(map[string]any{"rating": *rating, "summary": summary}, revAAD(id, "final"))
		if err != nil {
			return err
		}
		ref := ""
		if s.approvals != nil {
			if ref, err = s.approvals.Request(ctx, ApprovalRequest{Type: "review-release", Ref: id, Requester: canonSubject(a.Subject)}); err != nil {
				return err
			}
		}
		touched := addTouched(row.touched, a)
		_, err = tx.ExecContext(ctx, `UPDATE review_participants SET release_state = 'requested', final_sealed = ?, release_requested_by = ?, approval_ref = ?, touched_by = ?, updated_at = ? WHERE id = ?`,
			sealed, a.auditID(), ref, touchedJSON(touched), s.now().Unix(), id)
		_ = cy
		return err
	})
}

// DecideRelease approves or rejects a requested release. Approving needs someone who has not
// touched the review: not the subject, the manager who wrote it, whoever calibrated it, nor any
// person who ever requested its release (a rejection and a new request does not clear that
// history). With an approval engine configured the engine must have approved, and its decider
// is held to the same rule.
func (s *Service) DecideRelease(ctx context.Context, a Actor, id string, approve bool, reason string) error {
	if err := validText("reason", reason, 500, false); err != nil {
		return err
	}
	var denied bool
	err := s.revMutate(ctx, a, "release.decide", id, nil, func(ctx context.Context, tx *sqlTx) error {
		row, cy, f, err := s.revFact(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if !Decide(a, ActRevReleaseDecide, f) {
			if a.hrOther(f) {
				return conflict("no release is waiting for a decision")
			}
			return s.denyWrite(ctx, a, id, f, false, "")
		}
		now := s.now().Unix()
		if !approve {
			note := any(nil)
			if reason != "" {
				if note, err = s.keys.Seal(reason, revAAD(id, "relnote")); err != nil {
					return err
				}
			}
			if s.approvals != nil && row.approvalRef != "" {
				_ = s.approvals.Cancel(ctx, row.approvalRef)
			}
			_, err = tx.ExecContext(ctx, `UPDATE review_participants SET release_state = 'none', final_sealed = NULL, approval_ref = '', release_note_sealed = ?, updated_at = ? WHERE id = ?`, note, now, id)
			return err
		}
		chain := append(s.idents(ctx, tx, row.personID), row.mgrAuthor.String, row.calibratedBy.String, row.relReqBy.String)
		approver := a
		if s.approvals != nil {
			st, err := s.approvals.Status(ctx, row.approvalRef)
			if err != nil {
				return err
			}
			if st.Outcome != "approved" {
				return conflict("the approval engine has not approved this release (outcome %q)", st.Outcome)
			}
			approver = Actor{Subject: st.DecidedBy, PersonID: s.personIDForSubject(ctx, tx, st.DecidedBy)}
		}
		if !sodChainOK(a, row.touched, chain...) || !sodChainOK(approver, row.touched, chain...) {
			denied = true
			return ErrSeparation
		}
		touched := addTouched(addTouched(row.touched, a), approver)
		if _, err := tx.ExecContext(ctx, `UPDATE review_participants SET release_state = 'released', released_at = ?, release_decided_by = ?, touched_by = ?, updated_at = ? WHERE id = ?`,
			now, approver.auditIDOrSubject(), touchedJSON(touched), now, id); err != nil {
			return err
		}
		return s.enqueue(ctx, tx, EventReviewReleased, map[string]any{"cycle_id": cy.ID, "review_id": id, "person_id": row.personID, "released_on": s.today()})
	})
	if denied {
		s.auditDeny(ctx, a, "release.decide", "review:"+id, nil)
	}
	return err
}

// auditIDOrSubject names an approver for the record.
func (a Actor) auditIDOrSubject() string {
	if a.HasPerson || a.PersonID != "" {
		return a.PersonID
	}
	return "svc:" + a.Subject
}

// personIDForSubject maps an SSO subject (compared canonically) to a person id, or "".
func (s *Service) personIDForSubject(ctx context.Context, q querier, subject string) string {
	want := canonSubject(subject)
	if want == "" {
		return ""
	}
	rows, err := q.QueryContext(ctx, `SELECT id, sso_subject FROM people WHERE sso_subject IS NOT NULL`)
	if err != nil {
		return ""
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, subj string
		if rows.Scan(&id, &subj) == nil && canonSubject(subj) == want {
			return id
		}
	}
	return ""
}

// Acknowledge records that the subject has read a released review. Once.
func (s *Service) Acknowledge(ctx context.Context, a Actor, id, comment string) error {
	if err := validText("comment", comment, 1000, false); err != nil {
		return err
	}
	return s.revMutate(ctx, a, "write", id, []string{"acknowledgement"}, func(ctx context.Context, tx *sqlTx) error {
		row, _, f, err := s.revFact(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if !Decide(a, ActRevAck, f) {
			return s.denyWrite(ctx, a, id, f, false, "")
		}
		if row.ackAt.Valid {
			return conflict("the review was already acknowledged")
		}
		var sealed any
		if comment != "" {
			if sealed, err = s.keys.Seal(comment, revAAD(id, "ack")); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE review_participants SET ack_at = ?, ack_sealed = ?, updated_at = ? WHERE id = ?`, s.now().Unix(), sealed, s.now().Unix(), id)
		return err
	})
}

// revMutate runs a review write in one transaction with its audit entry. Refusals are audited
// after the transaction ends (the single connection cannot serve the audit while it is held).
func (s *Service) revMutate(ctx context.Context, a Actor, action, id string, fields []string, fn func(ctx context.Context, tx *sqlTx) error) error {
	err := s.txAudit(ctx, a, action, "review:"+id, fields, fn)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) {
		s.auditDeny(ctx, a, action, "review:"+id, fields)
	}
	return err
}
