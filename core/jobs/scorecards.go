package jobs

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"
)

// Scorecard is one interviewer's structured feedback.
type Scorecard struct {
	ID             string     `json:"id"`
	ApplicationID  string     `json:"application_id"`
	Author         string     `json:"author"`
	Rating         int        `json:"rating"` // 1 (poor) to 4 (excellent)
	Recommendation string     `json:"recommendation"`
	Comments       string     `json:"comments"`
	Status         string     `json:"status"` // draft or submitted
	SubmittedAt    *time.Time `json:"submitted_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// ScorecardInput is the body of a scorecard save.
type ScorecardInput struct {
	Rating         int    `json:"rating"`
	Recommendation string `json:"recommendation"`
	Comments       string `json:"comments"`
}

// ScorecardList is what a caller may see of an application's feedback.
type ScorecardList struct {
	Items []Scorecard `json:"items"`
	// OthersHidden is true when other people's submitted feedback exists but stays hidden because
	// the caller is (or was) an interviewer on this application and has not submitted their own.
	// It is a flag, not a count, so how many colleagues have finished is not leaked.
	OthersHidden bool `json:"others_hidden"`
}

func (s *Store) isInterviewer(ctx context.Context, q dbq, appID, subject string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM interviewers WHERE application_id = ? AND subject = ? AND removed_at IS NULL`, appID, subject).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// SaveScorecard creates or updates the caller's own draft. Only people assigned to the
// application write feedback, and only for themselves; a submitted scorecard is final.
func (s *Store) SaveScorecard(ctx context.Context, p *Principal, appID string, in ScorecardInput) (*Scorecard, error) {
	if !p.Can(ActScore) {
		return nil, ErrForbidden
	}
	var pr problems
	if in.Rating < 1 || in.Rating > 4 {
		pr.add("rating", "between 1 and 4")
	}
	if !slices.Contains(Recommendations, in.Recommendation) {
		pr.add("recommendation", "must be one of %v", Recommendations)
	}
	pr.text("comments", in.Comments, 5000, false, true)
	if err := pr.err(); err != nil {
		return nil, err
	}
	var id string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		st, err := s.loadAppForWrite(ctx, tx, p, appID)
		if err != nil {
			return err
		}
		if ok, err := s.isInterviewer(ctx, tx, appID, p.Subject); err != nil {
			return err
		} else if !ok {
			return ErrNotFound // not assigned: the application is not theirs to score
		}
		if st.status != AppActive {
			return conflict("the application is %s; feedback is closed", st.status)
		}
		var status string
		err = tx.QueryRowContext(ctx, `SELECT id, status FROM scorecards WHERE application_id = ? AND author = ?`, appID, p.Subject).Scan(&id, &status)
		now := ts(s.now())
		switch {
		case errors.Is(err, sql.ErrNoRows):
			id = newID("sc")
			sealed, err := s.seal("scorecards", id, "comments_sealed", in.Comments)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO scorecards (id, application_id, author, rating, recommendation, comments_sealed, status, created_at, updated_at) VALUES (?,?,?,?,?,?,'draft',?,?)`,
				id, appID, p.Subject, in.Rating, in.Recommendation, sealed, now, now); err != nil {
				return err
			}
		case err != nil:
			return err
		case status == "submitted":
			return conflict("this scorecard was submitted and is final")
		default:
			sealed, err := s.seal("scorecards", id, "comments_sealed", in.Comments)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE scorecards SET rating = ?, recommendation = ?, comments_sealed = ?, updated_at = ? WHERE id = ?`, in.Rating, in.Recommendation, sealed, now, id); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, p.Subject, "scorecard.save", "scorecard:"+id, []string{"scorecard_comments"}, "ok")
	})
	if err != nil {
		return nil, err
	}
	return s.ownScorecard(ctx, appID, p.Subject)
}

// SubmitScorecard makes the caller's draft final. From then on they can read the other
// interviewers' submitted feedback.
func (s *Store) SubmitScorecard(ctx context.Context, p *Principal, appID string) (*Scorecard, error) {
	if !p.Can(ActScore) {
		return nil, ErrForbidden
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.loadAppForWrite(ctx, tx, p, appID); err != nil {
			return err
		}
		now := ts(s.now())
		res, err := tx.ExecContext(ctx, `UPDATE scorecards SET status = 'submitted', submitted_at = ?, updated_at = ? WHERE application_id = ? AND author = ? AND status = 'draft'`, now, now, appID, p.Subject)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var one int
			if tx.QueryRowContext(ctx, `SELECT 1 FROM scorecards WHERE application_id = ? AND author = ?`, appID, p.Subject).Scan(&one) == nil {
				return conflict("this scorecard was already submitted")
			}
			return ErrNotFound
		}
		return s.audit(ctx, tx, p.Subject, "scorecard.submit", "application:"+appID, nil, "ok")
	})
	if err != nil {
		return nil, err
	}
	return s.ownScorecard(ctx, appID, p.Subject)
}

func (s *Store) ownScorecard(ctx context.Context, appID, author string) (*Scorecard, error) {
	l, err := s.readScorecards(ctx, appID, author, true)
	if err != nil || len(l.Items) == 0 {
		return nil, errors.Join(err, ErrNotFound)
	}
	return &l.Items[0], nil
}

// readScorecards loads the scorecards a viewer may see: their own in any state, and other
// people's once submitted unless blind is set (the viewer is an interviewer who has not
// submitted yet). When ownOnly is set only the viewer's card is returned.
func (s *Store) readScorecards(ctx context.Context, appID, viewer string, ownOnly bool) (*ScorecardList, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, application_id, author, rating, recommendation, comments_sealed, status, submitted_at, updated_at FROM scorecards WHERE application_id = ? ORDER BY created_at, id`, appID)
	if err != nil {
		return nil, err
	}
	type row struct {
		sc     Scorecard
		sealed sql.NullString
	}
	var all []row
	for rows.Next() {
		var r row
		var sub sql.NullInt64
		var upd int64
		if err := rows.Scan(&r.sc.ID, &r.sc.ApplicationID, &r.sc.Author, &r.sc.Rating, &r.sc.Recommendation, &r.sealed, &r.sc.Status, &sub, &upd); err != nil {
			_ = rows.Close()
			return nil, err
		}
		r.sc.SubmittedAt, r.sc.UpdatedAt = nullTime(sub), fromUnix(upd)
		all = append(all, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Blind until you have submitted, for anyone who has EVER been assigned or has any
	// scorecard row (draft or submitted) here, whatever their role and even after removal.
	var ever int
	if err := s.rdb.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM interviewers WHERE application_id = ?1 AND subject = ?2) OR EXISTS (SELECT 1 FROM scorecards WHERE application_id = ?1 AND author = ?2)`, appID, viewer).Scan(&ever); err != nil {
		return nil, err
	}
	assigned := ever == 1
	mySubmitted := false
	for _, r := range all {
		if r.sc.Author == viewer && r.sc.Status == "submitted" {
			mySubmitted = true
		}
	}
	blind := assigned && !mySubmitted // feedback stays independent until you have given yours
	out := &ScorecardList{Items: []Scorecard{}}
	for _, r := range all {
		mine := r.sc.Author == viewer
		switch {
		case mine:
		case ownOnly:
			continue
		case r.sc.Status != "submitted":
			continue // other people's drafts are never visible
		case blind:
			out.OthersHidden = true
			continue
		}
		c, err := s.unseal("scorecards", r.sc.ID, "comments_sealed", r.sealed)
		if err != nil {
			return nil, err
		}
		r.sc.Comments = c
		out.Items = append(out.Items, r.sc)
	}
	return out, nil
}

// ListScorecards returns the feedback the caller is allowed to see. Whoever is assigned as an
// interviewer on the application sees other people's submitted feedback only after submitting
// their own, whatever their role. Reads are audited.
func (s *Store) ListScorecards(ctx context.Context, p *Principal, appID string) (*ScorecardList, error) {
	if !p.Can(ActScore) {
		return nil, ErrForbidden
	}
	pred, args := p.appPred("a", "j")
	var one int
	if err := s.rdb.QueryRowContext(ctx, `SELECT 1 FROM applications a JOIN jobs j ON j.id = a.job_id WHERE (a.id = ?) AND `+pred, append([]any{appID}, args...)...).Scan(&one); err != nil {
		return nil, ErrNotFound
	}
	l, err := s.readScorecards(ctx, appID, p.Subject, false)
	if err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, p, "application:"+appID, []string{"scorecards"}); err != nil {
		return nil, err
	}
	return l, nil
}
