package people

// Peer and upward feedback for reviews (Epic E14b).
//
// Anonymity rules, enforced here and by the schema:
//   - in an anonymous cycle a response row has no reviewer and no timestamp, its id is random,
//     and the order comments are returned in is a hash of response and question (never the order
//     of submission, and different per question, so a reviewer's comments cannot be lined up
//     across questions);
//   - feedback content (aggregates and comments) is shown only once min_responses (at least 3)
//     answers exist, to everyone including hr;
//   - the subject never sees comments, who was asked, or who proposed whom: they see aggregates
//     after release and their own proposals;
//   - the API sets no ETag or Last-Modified on these responses, and says nothing about who has
//     not answered yet.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"sort"
	"strings"
)

// ApprovalRequest asks the approval engine (can-core, specs/015-approvals-notifications) to
// decide. Types are declared in module.yaml: timeoff, review-release.
type ApprovalRequest struct {
	Type      string
	Ref       string // the request or review id
	Requester string // the requester's subject
}

// ApprovalStatus is the engine's answer.
type ApprovalStatus struct {
	Outcome   string // pending | approved | rejected | cancelled | expired
	DecidedBy string // subject of the final approver
}

// ApprovalEngine is the seam to can-core's approval engine. The engine resolves the approver
// through the reporting line (the `directory` capability this module provides). Without an engine
// the service decides itself: the policy layer says who may (the live direct manager or hr) and
// the same separation-of-duties function applies. The engine's decider is held to the
// separation rules too.
type ApprovalEngine interface {
	Request(ctx context.Context, req ApprovalRequest) (ref string, err error)
	Status(ctx context.Context, ref string) (ApprovalStatus, error)
	Cancel(ctx context.Context, ref string) error
}

// Nomination is a proposed or approved reviewer for a review.
type Nomination struct {
	ID         string `json:"id"`
	ReviewID   string `json:"review_id"`
	ReviewerID string `json:"reviewer_id"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	ProposedBy string `json:"proposed_by,omitempty"` // manager and hr only
	// Status is proposed, approved or declined. An answered request still reads approved: whether a
	// particular reviewer has answered is not shown to anyone but the reviewer.
}

// FeedbackRequest is what a reviewer sees: who they review and the questions. Not who asked.
type FeedbackRequest struct {
	ID        string     `json:"id"`
	CycleID   string     `json:"cycle_id"`
	SubjectID string     `json:"subject_id"`
	Kind      string     `json:"kind"`
	Anonymous bool       `json:"anonymous"`
	Questions []Question `json:"questions"`
}

// RatingAgg is the aggregate of one rating question.
type RatingAgg struct {
	QuestionID string  `json:"question_id"`
	Average    float64 `json:"average"`
	N          int     `json:"n"`
}

// FeedbackComment is one text answer. ReviewerID is present only in attributed cycles, for the
// manager and hr.
type FeedbackComment struct {
	QuestionID string `json:"question_id"`
	Text       string `json:"text"`
	ReviewerID string `json:"reviewer_id,omitempty"`
}

// FeedbackView is feedback about one person. Until min_responses answers exist Shown is false and
// nothing but the count is returned.
type FeedbackView struct {
	Kind      string            `json:"kind"`
	Shown     bool              `json:"shown"`
	Required  int               `json:"required_responses"`
	Responses int               `json:"responses,omitempty"`
	Ratings   []RatingAgg       `json:"ratings,omitempty"`
	Comments  []FeedbackComment `json:"comments,omitempty"`
}

var reqIDRe = regexp.MustCompile(`^frq_[0-9a-f]{24}$`)

// Nominate proposes (the subject) or adds (their manager, or hr) a reviewer. The subject can only
// propose peers, and a proposal waits for the manager; additions by the manager or hr are
// approved at once. Upward feedback comes only from the subject's direct reports.
func (s *Service) Nominate(ctx context.Context, a Actor, reviewID, reviewerID, kind string) (*Nomination, error) {
	var out *Nomination
	err := s.revMutate(ctx, a, "write", reviewID, []string{"nomination"}, func(ctx context.Context, tx *sqlTx) error {
		row, cy, f, err := s.revFact(ctx, tx, a, reviewID)
		if err != nil {
			return err
		}
		if !Decide(a, ActRevNominate, f) {
			return s.denyWrite(ctx, a, reviewID, f, false, "")
		}
		if kind != "peer" && kind != "upward" {
			return invalid("kind must be peer or upward")
		}
		if kind == "upward" && f.Self {
			return invalid("upward feedback is requested by the manager or hr, not by the person")
		}
		if !personIDRe.MatchString(reviewerID) {
			return invalid("reviewer_id must be a person id")
		}
		if reviewerID == row.personID {
			return invalid("a person cannot review themselves")
		}
		si, err := s.subject(ctx, tx, reviewerID)
		if err != nil || si.erased || si.status != StActive {
			return invalid("the reviewer must be an active person")
		}
		if kind == "upward" && si.manager != row.personID {
			return invalid("upward feedback must come from a direct report of the person")
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_requests WHERE participant_id = ?`, row.id).Scan(&n); err != nil {
			return err
		}
		if n >= maxReviewerSlots {
			return conflict("at most %d reviewers per review", maxReviewerSlots)
		}
		status, by := "approved", any(a.auditID())
		if f.Self {
			status, by = "proposed", nil
		}
		id := newID("frq")
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_requests (id, participant_id, reviewer_id, kind, status, proposed_by, decided_by, created_at) VALUES (?,?,?,?,?,?,?,?)`,
			id, row.id, reviewerID, kind, status, a.auditID(), by, s.now().Unix()); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return conflict("this person is already a reviewer")
			}
			return err
		}
		_ = cy
		out = &Nomination{ID: id, ReviewID: row.id, ReviewerID: reviewerID, Kind: kind, Status: status, ProposedBy: a.auditID()}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out.Status == "proposed" { // the proposer is the subject: no need to say who proposed
		out.ProposedBy = ""
	}
	return out, nil
}

// DecideNomination approves or declines a proposal. The live direct manager, or hr for someone
// else's review; never the subject, and never the person proposed.
func (s *Service) DecideNomination(ctx context.Context, a Actor, nomID string, approve bool) error {
	if !reqIDRe.MatchString(nomID) {
		return ErrNotFound
	}
	var reviewID string
	if err := s.db.QueryRowContext(ctx, `SELECT participant_id FROM review_requests WHERE id = ?`, nomID).Scan(&reviewID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return s.revMutate(ctx, a, "write", reviewID, []string{"nomination"}, func(ctx context.Context, tx *sqlTx) error {
		_, _, f, err := s.revFact(ctx, tx, a, reviewID)
		if err != nil {
			return err
		}
		if !Decide(a, ActRevNominationDecide, f) {
			return s.denyWrite(ctx, a, reviewID, f, false, "")
		}
		var status, reviewer string
		if err := tx.QueryRowContext(ctx, `SELECT status, reviewer_id FROM review_requests WHERE id = ?`, nomID).Scan(&status, &reviewer); err != nil {
			return err
		}
		if status != "proposed" {
			return conflict("the nomination was already decided")
		}
		if !sodOK(a, s.idents(ctx, tx, reviewer)...) {
			return ErrSeparation
		}
		to := "declined"
		if approve {
			to = "approved"
		}
		_, err = tx.ExecContext(ctx, `UPDATE review_requests SET status = ?, decided_by = ? WHERE id = ?`, to, a.auditID(), nomID)
		return err
	})
}

// ListNominations lists a review's nominations. The subject sees only what they proposed
// themselves; the manager and hr see all of them, with who proposed.
func (s *Service) ListNominations(ctx context.Context, a Actor, reviewID string) ([]Nomination, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	row, _, f, err := s.revFact(ctx, s.db, a, reviewID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			s.auditDeny(ctx, a, "read", "review:"+reviewID, nil)
		}
		return nil, err
	}
	full := Decide(a, ActRevPeerRaw, f)
	if !full && !f.Self {
		return nil, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, reviewer_id, kind, status, proposed_by FROM review_requests WHERE participant_id = ? ORDER BY created_at, id`, row.id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Nomination{}
	for rows.Next() {
		n := Nomination{ReviewID: row.id}
		if err := rows.Scan(&n.ID, &n.ReviewerID, &n.Kind, &n.Status, &n.ProposedBy); err != nil {
			return nil, err
		}
		if n.Status == "submitted" {
			n.Status = "approved"
		}
		if !full {
			if n.ProposedBy != a.PersonID {
				continue // the subject does not see who else was asked, or by whom
			}
			n.ProposedBy = ""
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MyFeedbackRequests lists the open requests addressed to the caller: who to review and the
// questions. It says nothing about who nominated the caller.
func (s *Service) MyFeedbackRequests(ctx context.Context, a Actor) ([]FeedbackRequest, error) {
	if !a.staff() || !a.permits("people.reviews.read") {
		return []FeedbackRequest{}, nil
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT rq.id, rp.cycle_id, rp.person_id, rq.kind FROM review_requests rq
		JOIN review_participants rp ON rp.id = rq.participant_id JOIN review_cycles c ON c.id = rp.cycle_id
		WHERE rq.reviewer_id = ? AND rq.status = 'approved' AND c.state = 'open' ORDER BY rq.created_at, rq.id LIMIT 100`, a.PersonID)
	if err != nil {
		return nil, err
	}
	var out []FeedbackRequest
	for rows.Next() {
		var fr FeedbackRequest
		if err := rows.Scan(&fr.ID, &fr.CycleID, &fr.SubjectID, &fr.Kind); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, fr)
	}
	_ = rows.Close()
	for i := range out {
		cy, err := s.loadCycle(ctx, s.db, out[i].CycleID)
		if err != nil {
			return nil, err
		}
		out[i].Anonymous = cy.Anonymity == "anonymous"
		out[i].Questions = audienceQuestions(cy.Questions, out[i].Kind)
	}
	if out == nil {
		out = []FeedbackRequest{}
	}
	return out, nil
}

// SubmitFeedback records the reviewer's answers, once. In an anonymous cycle the stored response
// carries no reviewer.
func (s *Service) SubmitFeedback(ctx context.Context, a Actor, reqID string, in Answers) error {
	if !reqIDRe.MatchString(reqID) || !a.HasPerson {
		return ErrNotFound
	}
	err := s.wtx(ctx, func(ctx context.Context, tx *sqlTx) error {
		var participant, reviewer, kind, status, anonymity, cycleID, state string
		var subject string
		err := tx.QueryRowContext(ctx, `SELECT rq.participant_id, rq.reviewer_id, rq.kind, rq.status, c.anonymity, c.id, c.state, rp.person_id
			FROM review_requests rq JOIN review_participants rp ON rp.id = rq.participant_id JOIN review_cycles c ON c.id = rp.cycle_id WHERE rq.id = ?`, reqID).
			Scan(&participant, &reviewer, &kind, &status, &anonymity, &cycleID, &state, &subject)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && reviewer != a.PersonID) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch status {
		case "submitted":
			return conflict("this request was already answered")
		case "approved":
		default:
			return ErrNotFound
		}
		f := Fact{Assigned: true, Self: subject == a.PersonID, CycleState: state}
		if !Decide(a, ActRevFeedbackGive, f) {
			if state != "open" {
				return conflict("the cycle is %s: feedback is closed", state)
			}
			return ErrForbidden
		}
		cy, err := s.loadCycle(ctx, tx, cycleID)
		if err != nil {
			return err
		}
		if len(in) == 0 {
			return invalid("an answer is required")
		}
		if err := validateAnswers(audienceQuestions(cy.Questions, kind), cy.RatingMax, in); err != nil {
			return err
		}
		rid := newID("frs")
		sealed, err := s.sealJSON(in, "review_response:"+rid)
		if err != nil {
			return err
		}
		var by any
		if anonymity == "attributed" {
			by = reviewer
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_responses (id, participant_id, kind, reviewer_id, answers_sealed) VALUES (?,?,?,?,?)`, rid, participant, kind, by, sealed); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE review_requests SET status = 'submitted' WHERE id = ? AND status = 'approved'`, reqID); err != nil {
			return err
		}
		// The audit entry says that a request was answered, never what. In an anonymous cycle it
		// names the request, not the review.
		if anonymity == "anonymous" {
			// No actor and no request id: the audit trail must not link who answered to when.
			return s.auditIn(ctx, tx, Actor{PersonID: "anonymous-reviewer", HasPerson: true}, "write", "review-cycle:"+cycleID, []string{"feedback"})
		}
		return s.auditIn(ctx, tx, a, "write", "feedback-request:"+reqID, []string{"feedback"})
	})
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) {
		s.auditDeny(ctx, a, "write", "feedback-request:"+reqID, nil)
	}
	return err
}

// feedbackView aggregates the responses about one participant. raw adds the comments. nil means
// there is nothing to say (no requests of this kind were ever approved).
func (s *Service) feedbackView(ctx context.Context, cy *cycleRow, participantID, kind string, raw bool) (*FeedbackView, error) {
	var asked int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_requests WHERE participant_id = ? AND kind = ? AND status IN ('approved','submitted')`, participantID, kind).Scan(&asked); err != nil {
		return nil, err
	}
	if asked == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, reviewer_id, answers_sealed FROM review_responses WHERE participant_id = ? AND kind = ?`, participantID, kind)
	if err != nil {
		return nil, err
	}
	type resp struct {
		id, reviewer string
		answers      Answers
	}
	var rs []resp
	for rows.Next() {
		var r resp
		var by sql.NullString
		var sealed string
		if err := rows.Scan(&r.id, &by, &sealed); err != nil {
			_ = rows.Close()
			return nil, err
		}
		r.reviewer = by.String
		plain, err := s.keys.Open(sealed, "review_response:"+r.id)
		if err != nil {
			_ = rows.Close()
			return nil, errors.New("cannot decrypt feedback")
		}
		if err := json.Unmarshal([]byte(plain), &r.answers); err != nil {
			_ = rows.Close()
			return nil, errors.New("cannot decrypt feedback")
		}
		rs = append(rs, r)
	}
	_ = rows.Close()
	need := cy.MinResponses
	if kind == "upward" {
		need = max(need, minUpward)
	}
	// Feedback is revealed once, after the cycle leaves "open", and only from `min` answers. Never
	// incrementally: a view that changes when one more person answers lets whoever watched it work
	// out that person's rating (the new average times the new count, minus the old total).
	fv := &FeedbackView{Kind: kind, Required: need, Shown: cy.State != "open" && len(rs) >= need}
	if !fv.Shown {
		return fv, nil // nothing, not even a count: a count that moves says who has answered
	}
	fv.Responses = len(rs)
	for _, q := range audienceQuestions(cy.Questions, kind) {
		switch q.Type {
		case "rating":
			sum, n := 0, 0
			for _, r := range rs {
				if a, ok := r.answers[q.ID]; ok && a.Rating != nil {
					sum += *a.Rating
					n++
				}
			}
			if n > 0 {
				fv.Ratings = append(fv.Ratings, RatingAgg{QuestionID: q.ID, Average: math.Round(float64(sum)/float64(n)*100) / 100, N: n})
			}
		case "text":
			if !raw {
				continue
			}
			type keyed struct {
				key string
				c   FeedbackComment
			}
			var cs []keyed
			for _, r := range rs {
				if a, ok := r.answers[q.ID]; ok && strings.TrimSpace(a.Text) != "" {
					c := FeedbackComment{QuestionID: q.ID, Text: a.Text}
					if cy.Anonymity == "attributed" {
						c.ReviewerID = r.reviewer
					}
					// Order by a hash of response and question: stable, unrelated to submission order.
					h := sha256.Sum256([]byte(r.id + ":" + q.ID))
					cs = append(cs, keyed{hex.EncodeToString(h[:]), c})
				}
			}
			sort.Slice(cs, func(i, j int) bool { return cs[i].key < cs[j].key })
			for _, k := range cs {
				fv.Comments = append(fv.Comments, k.c)
			}
		}
	}
	return fv, nil
}
