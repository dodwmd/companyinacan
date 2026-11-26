package forms

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestMentions(t *testing.T) {
	const a = "bob@example.com"
	for _, text := range []string{
		"bob@example.com", "mail bob@example.com.", "<bob@example.com>", "_bob@example.com_", "-bob@example.com-", "cc-bob@example.com",
		"email_bob@example.com", "bob@example.com-urgent", "bob@example.com_old", "联系bob@example.com谢谢", "ébob@example.comé", "mailto:bob@example.com?x=1",
		"jimbob@example.com and bob@example.com",
	} {
		if !mentions(folded(text), folded(a)) {
			t.Errorf("%q should name %s", text, a)
		}
	}
	for _, text := range []string{"jimbob@example.com", "2bob@example.com", "bob@example.community", "bob@example.com.au", "bob@example.co"} {
		if mentions(folded(text), folded(a)) {
			t.Errorf("%q is another address", text)
		}
	}
	// composed and decomposed spellings of the same letters
	if !mentions(folded("JOSÉ@example.com"), folded("josé@example.com")) {
		t.Error("normalisation")
	}
}

// A scrub that did not finish (the process stopped after the commit) is finished later.
func TestScrubIsResumed(t *testing.T) {
	h := newHarness(t, harnessCfg)
	ctx := context.Background()
	st := h.app.store
	l := h.insert("a@b.example", map[string]string{"message": "RESUME-TRACE-31"})
	// what a crash between the commit and the scrub leaves: the row gone, the tombstone unscrubbed
	if _, err := st.db.ExecContext(ctx, `DELETE FROM leads WHERE id = ?`, l.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO erasures (subject, class, created_at) VALUES (?, ?, 1)`, l.ID, ClassLead); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(h.dbBytes(), []byte("RESUME-TRACE-31")) {
		t.Fatal("no trace left to scrub: the test would prove nothing")
	}
	unscrubbed := func() (n int) {
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM erasures WHERE scrubbed = 0`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := st.ResumeScrub(ctx); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(h.dbBytes(), []byte("RESUME-TRACE-31")) || unscrubbed() != 0 {
		t.Error("not scrubbed")
	}
	// an erasure that completes is marked at once
	l2 := h.insert("a@b.example", nil)
	if _, err := st.Erase(ctx, "t", l2.ID, nil); err != nil || unscrubbed() != 0 {
		t.Error(err, unscrubbed())
	}
}

func TestEraseAnswerSaysHowToRecordIt(t *testing.T) {
	h := newHarness(t, harnessCfg)
	l := h.insert("a@b.example", nil)
	rec := h.apiDo("DELETE", "/v1/leads/"+l.ID, adminToken, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "can backup ledger-add --subject "+l.ID+" --class lead-submission") {
		t.Fatal(rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "erases it again") {
		t.Error("nothing records the erasure by itself: no such promise")
	}
}
