package people

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestRotateKeysReSealsReviewsAndLeave(t *testing.T) {
	x := newToEnv(t)
	_, id := x.openCycle("attributed")
	x.must(x.s.SaveSelf(x.ctx, x.annA, id, Answers{"q2": {Text: "rotate me"}}, true))
	lr := x.submit(x.annA, x.vac, "2026-04-06", "2026-04-07")
	_ = lr
	_, err := x.s.AdjustLeave(x.ctx, x.hr, x.ann.ID, x.vac.ID, 2, "rotate my reason")
	x.must(err)
	n, _ := x.s.Nominate(x.ctx, x.mgrA, id, x.ben.ID, "peer")
	_ = n
	reqs, _ := x.s.MyFeedbackRequests(x.ctx, x.benA)
	x.must(x.s.SubmitFeedback(x.ctx, x.benA, reqs[0].ID, Answers{"q2": {Text: "rotate feedback"}}))

	rotated, err := ParseKeyring("k2 " + hexKeyB + "\nk1 " + hexKeyA + "\n")
	x.must(err)
	x.s.keys = rotated
	got, err := x.s.RotateKeys(x.ctx)
	x.must(err)
	if got < 4 {
		t.Fatalf("re-sealed %d values", got)
	}
	var sealed string
	x.must(x.s.db.QueryRow(`SELECT self_sealed FROM review_participants WHERE id = ?`, id).Scan(&sealed))
	if !strings.HasPrefix(sealed, "v1.k2.") {
		t.Fatalf("self_sealed = %q", sealed)
	}
	x.must(x.s.db.QueryRow(`SELECT note_sealed FROM leave_ledger WHERE kind = 'adjust'`).Scan(&sealed))
	if !strings.HasPrefix(sealed, "v1.k2.") {
		t.Fatalf("ledger note = %q", sealed)
	}
	// Still readable with only the new key.
	only, _ := ParseKeyring("k2 " + hexKeyB + "\n")
	x.s.keys = only
	rv, err := x.s.GetReview(x.ctx, x.annA, id)
	x.must(err)
	if rv.Self.Answers["q2"].Text != "rotate me" {
		t.Fatalf("self = %+v", rv.Self)
	}
	if got, _ := x.s.RotateKeys(x.ctx); got != 0 {
		t.Fatalf("second rotation re-sealed %d", got)
	}
}

func TestRetentionPurgesClosedReviewsAndSkipsLegalHold(t *testing.T) {
	x := newToEnv(t)
	cy, id := x.openCycle("attributed")
	x.must(x.s.SaveSelf(x.ctx, x.annA, id, Answers{"q2": {Text: "RETAIN-ME-NOT"}}, true))
	// Ben is in the cycle too, under legal hold.
	ids, err := x.s.AddParticipants(x.ctx, x.hr, cy.ID, []string{x.ben.ID})
	x.must(err)
	x.must(x.s.SaveSelf(x.ctx, x.benA, ids[0], Answers{"q2": {Text: "HELD"}}, true))
	x.must(x.s.SetLegalHold(x.ctx, x.hr, x.ben.ID, true, "dispute"))
	for _, st := range []string{"calibration", "closed"} {
		_, err = x.s.SetCycleState(x.ctx, x.hr, cy.ID, st)
		x.must(err)
	}
	*x.clock = x.clock.AddDate(2, 1, 0) // past the 2y default
	rep, err := x.s.RunRetention(x.ctx)
	x.must(err)
	if rep.Purged[ClassReviews] != 1 {
		t.Fatalf("report = %+v", rep)
	}
	var n int
	x.must(x.s.db.QueryRow(`SELECT COUNT(*) FROM review_participants`).Scan(&n))
	if n != 1 {
		t.Fatalf("participants left = %d (the legal-hold one must stay)", n)
	}
	raw, _ := os.ReadFile(x.path)
	if wal, err := os.ReadFile(x.path + "-wal"); err == nil {
		raw = append(raw, wal...)
	}
	if strings.Contains(string(raw), "RETAIN-ME-NOT") {
		t.Fatal("purged review text found in the files")
	}
	_ = time.Now
}
