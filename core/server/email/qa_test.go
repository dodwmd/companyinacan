package email

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type stallingTransport struct {
	*fakeTransport
	stall string // recipients starting with this get no answer
}

func (s stallingTransport) Send(ctx context.Context, r Relay, to string, msg []byte) error {
	if strings.HasPrefix(to, s.stall) {
		return &SendError{Stage: "RCPT TO", Stalled: true}
	}
	return s.fakeTransport.Send(ctx, r, to, msg)
}

// QA of #52, 11: a message the relay never answers for must not hold everybody's mail back, however often it
// comes round; several different ones in a row still mean the relay is the problem.
func TestOneStallingMessageDoesNotPauseDelivery(t *testing.T) {
	f := newFixture(t, baseCompany)
	f.s.transport = stallingTransport{f.tr, "slow"}
	ctx := context.Background()
	send := func(to string) {
		t.Helper()
		if _, _, err := f.s.Send(ctx, req(to)); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(time.Second)
	}
	pass := func() {
		t.Helper()
		if _, err := f.s.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	send("slow1@acme.example")
	for range 3 {
		pass()
		f.now = f.now.Add(time.Hour)
	}
	send("ok1@acme.example")
	pass()
	if len(f.tr.sent) != 1 || f.s.pausedUntil.Load() != 0 {
		t.Fatalf("sent %d, paused until %d", len(f.tr.sent), f.s.pausedUntil.Load())
	}
	f.now = f.now.Add(time.Hour)
	send("slow2@acme.example")
	send("slow3@acme.example")
	pass()
	if f.s.pausedUntil.Load() <= f.now.Unix() {
		t.Fatal("three different messages without an answer must pause delivery")
	}
}

func TestAMissingAnswerForOneMessageIsNotASessionFailure(t *testing.T) {
	for _, verb := range []string{"MAIL", "RCPT", "DATA"} {
		relay := newFakeRelay(t, false, false)
		relay.replies[verb] = noReply
		err := SMTP{AllowPrivate: true, Timeout: 2 * time.Second}.Send(context.Background(), relay.relay(TLSNone), "a@acme.example", []byte("x\r\n"))
		var se *SendError
		if !errors.As(err, &se) || !se.Stalled || se.Session || se.Permanent {
			t.Errorf("%s: %+v", verb, err)
		}
	}
}

// QA of #52, 5: an address stored while unverified claims were accepted is not trusted later, and none is while
// single sign-on is off.
func TestOnlyAddressesVerifiedWhenStoredAreTrusted(t *testing.T) {
	const sso = "sso: {enabled: true, issuer: \"https://id.example\", client_id: x, client_secret_ref: sso-client}\n"
	f := newFixture(t, baseCompany+sso)
	ctx := context.Background()
	for name, verified := range map[string]int{"alice": 0, "bob": 1} {
		u, err := f.db.CreateAdmin(ctx, name, "hash", "secret")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(`INSERT INTO sso_identities(issuer_hash, subject, user_id, email, email_verified, name, groups_json, amr_json, created_at, last_login) VALUES ('h',?,?,?,?,'','[]','[]',1,1)`,
			name, u.ID, name+"@idp-mail.example", verified); err != nil {
			t.Fatal(err)
		}
	}
	allowed := func(name string) bool {
		t.Helper()
		p, err := f.s.source.Load()
		if err != nil {
			t.Fatal(err)
		}
		ok, err := f.s.noticeAllowed(ctx, p, "user:"+name, name+"@idp-mail.example")
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if allowed("alice") || !allowed("bob") {
		t.Fatal("only the address that was verified when it was stored:", allowed("alice"), allowed("bob"))
	}
	f.company(baseCompany + strings.Replace(sso, "enabled: true", "enabled: false", 1))
	if allowed("bob") {
		t.Fatal("trusted with single sign-on turned off")
	}
}
