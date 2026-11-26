package finance

import (
	"context"
	"strings"
	"testing"
)

// signedWithoutTriggers is a completed request whose database can then be edited by hand.
func signedWithoutTriggers(t *testing.T) *signEnv {
	t.Helper()
	e := newSignEnv(t)
	e.send(false, ann)
	if w := e.public("POST", e.link(ann[1]), e.signForm("Ann")); w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	e.app.store.sign.wg.Wait()
	if v := e.verify(); !v.OK {
		t.Fatalf("before: %v", v.Problems)
	}
	rows, err := e.app.store.db.Query(`SELECT name FROM sqlite_master WHERE type = 'trigger'`)
	if err != nil {
		t.Fatal(err)
	}
	var triggers []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		triggers = append(triggers, n)
	}
	_ = rows.Close()
	for _, n := range triggers {
		if _, err := e.app.store.db.Exec(`DROP TRIGGER ` + n); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func TestSigningVerifyCatchesTimesContractAndDeclineFields(t *testing.T) {
	for name, c := range map[string]struct{ q, want string }{
		"created_at":     {`UPDATE signing_envelopes SET created_at = created_at - 86400`, "creation time"},
		"closed_at":      {`UPDATE signing_envelopes SET closed_at = closed_at + 86400`, "closing time"},
		"no closed_at":   {`UPDATE signing_envelopes SET closed_at = NULL`, "closing time"},
		"signed_at":      {`UPDATE contracts SET signed_at = signed_at - 86400`, "signing time"},
		"status":         {`UPDATE contracts SET status = 'draft'`, "status does not match"},
		"title":          {`UPDATE contracts SET title = 'Another agreement'`, "title is not the one"},
		"provider":       {`UPDATE contracts SET esign_provider = ''`, "signed by link"},
		"declined_at":    {`UPDATE signing_signers SET declined_at = signed_at`, "status does not allow"},
		"decline_reason": {`UPDATE signing_signers SET decline_reason = 'I never agreed'`, "status does not allow"},
	} {
		t.Run(name, func(t *testing.T) {
			e := signedWithoutTriggers(t)
			if _, err := e.app.store.db.Exec(c.q); err != nil {
				t.Fatal(err)
			}
			if v := e.verify(); v.OK || !strings.Contains(strings.Join(v.Problems, "; "), c.want) {
				t.Fatalf("want %q: %v", c.want, v.Problems)
			}
		})
	}
}

// QA: records signed before this check read the clock twice, so their event may be dated a second
// or so after the stored time. That is not tampering; an event dated before it, or long after, is.
func TestSigningVerifyAllowsALaterEvent(t *testing.T) {
	for _, column := range []string{"signing_envelopes.created_at", "signing_envelopes.closed_at", "contracts.signed_at"} {
		table, col, _ := strings.Cut(column, ".")
		for later, ok := range map[int]bool{1: true, 5: true, 6: false, -1: false} {
			e := signedWithoutTriggers(t)
			// the event is `later` seconds after the stored time
			if _, err := e.app.store.db.Exec(`UPDATE `+table+` SET `+col+` = `+col+` - ?`, later); err != nil {
				t.Fatal(err)
			}
			if v := e.verify(); v.OK != ok {
				t.Errorf("%s, event %d s later: ok=%v %v", column, later, v.OK, v.Problems)
			}
		}
	}
}

func TestSigningNames(t *testing.T) {
	e := newSignEnv(t)
	blanks := []string{"\u3164", "\u2800\u2800", "\u3164 \u115f\uffa0", "---"}
	for _, name := range blanks {
		e.must(422, adminTok, "POST", e.path("/signing"), obj{"signers": signers([3]string{name, "a@b.example", ""})}, nil)
	}
	e.send(false, ann)
	link := e.link(ann[1])
	for _, name := range blanks {
		if w := e.public("POST", link, e.signForm(name)); w.Code != 422 {
			t.Errorf("typed name %q: %d", name, w.Code)
		}
	}
	// the refusal says what is wrong, not just "type your full name"
	if w := e.public("POST", link, e.signForm("An\u200dn")); w.Code != 422 || !strings.Contains(w.Body.String(), "invisible or text-direction characters") {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}

func TestSigningResendCooldownAndCertificateGeneration(t *testing.T) {
	e := newSignEnv(t)
	env := e.send(false, ann)
	resend := e.path("/signing/signers/" + id(env.Signers[0].ID) + "/resend")
	e.must(200, adminTok, "POST", resend, nil, nil) // right after sending is fine
	e.must(409, adminTok, "POST", resend, nil, nil) // twice in a row is not
	e.advance(resendCooldown)
	e.must(200, adminTok, "POST", resend, nil, nil)

	if w := e.public("POST", e.link(ann[1]), e.signForm("Ann")); w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	e.app.store.sign.wg.Wait()
	e.advance(resendCooldown)
	e.must(200, adminTok, "POST", resend, nil, nil)
	e.must(409, adminTok, "POST", resend, nil, nil)
	// the first send (generation 1) reports a failure late: the newer outcome stays
	var cur Envelope
	e.must(200, adminTok, "GET", e.path("/signing"), nil, &cur)
	e.mail.err = context.DeadlineExceeded
	e.app.store.sendCertificate(context.Background(), &cur, cur.Signers[0], 1)
	e.mail.err = nil
	e.must(200, adminTok, "GET", e.path("/signing"), nil, &cur)
	if cur.Signers[0].CertificateStatus != MailQueued {
		t.Fatalf("certificate status %q", cur.Signers[0].CertificateStatus)
	}
}
