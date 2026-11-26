package email

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/store"
)

type fakeTransport struct {
	mu   sync.Mutex
	err  error
	sent []string // "to|message"
}

func (f *fakeTransport) Send(_ context.Context, _ Relay, to string, msg []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, to+"|"+string(msg))
	return nil
}

type fixture struct {
	t     *testing.T
	db    *store.DB
	al    *audit.Log
	tr    *fakeTransport
	s     *Service
	now   time.Time
	logs  *bytes.Buffer
	dir   string
	cfile string
}

const baseCompany = `schema: 1
name: Acme
domain: acme.example
provider: {type: local}
modules:
  forms: {settings: {notify_email: [owner@personal.example]}}
email:
  relay: {host: smtp.relay.example}
`

func newFixture(t *testing.T, company string) *fixture {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := &fixture{t: t, db: db, al: audit.New(db), tr: &fakeTransport{}, now: time.Unix(1_800_000_000, 0), logs: &bytes.Buffer{}, dir: t.TempDir()}
	f.cfile = filepath.Join(f.dir, "company.yaml")
	f.company(company)
	f.s = New(db, f.al, Source{CompanyFile: f.cfile}, f.tr, slog.New(slog.NewTextHandler(f.logs, nil)))
	f.s.Now = func() time.Time { return f.now }
	db.Now = f.s.Now
	return f
}

func (f *fixture) company(s string) {
	f.t.Helper()
	if err := os.WriteFile(f.cfile, []byte(s), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func req(to ...string) Request {
	return Request{Source: "forms", To: to, Subject: "New Contact submission", Text: "name: Visitor Person\nsecret-words in the body", ReplyTo: "lead@visitor.example"}
}

func (f *fixture) status(id string) Message {
	f.t.Helper()
	m, err := f.s.Get(context.Background(), "forms", id)
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

func (f *fixture) auditActions() []string {
	f.t.Helper()
	rs, err := f.al.List(context.Background(), 0, 1000)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, r := range rs {
		out = append(out, r.Action)
		for _, pii := range []string{"@acme.example", "visitor", "secret-words", "Contact submission"} {
			if strings.Contains(r.Target+r.Detail+r.Actor, pii) {
				f.t.Errorf("audit record %s contains %q: %s %s", r.Action, pii, r.Target, r.Detail)
			}
		}
	}
	return out
}

func (f *fixture) content(id string) string {
	f.t.Helper()
	var s string
	if err := f.db.QueryRow(`SELECT recipient||reply_to||subject||body FROM email_outbox WHERE id=?`, id).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func TestQueueSendAndBlankContent(t *testing.T) {
	f := newFixture(t, baseCompany)
	ctx := context.Background()
	ids, dup, err := f.s.Send(ctx, req("sales@acme.example", "owner@personal.example"))
	if err != nil || dup || len(ids) != 2 {
		t.Fatal(ids, dup, err)
	}
	if got := f.auditActions(); strings.Join(got, ",") != "email.queue,email.queue" {
		t.Fatalf("audit before return: %v", got)
	}
	if n, err := f.s.RunOnce(ctx); n != 2 || err != nil {
		t.Fatal(n, err)
	}
	if len(f.tr.sent) != 2 || !strings.Contains(f.tr.sent[0], "From: <no-reply@acme.example>") {
		t.Fatalf("%v", f.tr.sent)
	}
	m := f.status(ids[0])
	if m.Status != StatusSent || m.Attempts != 1 || m.FinishedAt.IsZero() || !strings.Contains(m.Recipient, "***@") {
		t.Fatalf("%+v", m)
	}
	if c := f.content(ids[0]); c != "" {
		t.Fatalf("content kept after sending: %q", c)
	}
	for _, pii := range []string{"sales@", "owner@", "visitor", "secret-words"} {
		if strings.Contains(f.logs.String(), pii) {
			t.Errorf("log contains %q", pii)
		}
	}
	f.auditActions()
}

func TestRecipientsAreRestricted(t *testing.T) {
	f := newFixture(t, baseCompany)
	ctx := context.Background()
	for _, to := range [][]string{{"victim@evil.example"}, {"sales@acme.example", "victim@evil.example"}, {"victim%evil.example@acme.example"}} {
		if _, _, err := f.s.Send(ctx, req(to...)); !errors.Is(err, ErrNotAllowed) && !errors.Is(err, ErrInvalid) {
			t.Fatalf("%v: %v", to, err)
		}
	}
	// Another module does not inherit forms' notify_email.
	other := req("owner@personal.example")
	other.Source = "crm"
	if _, _, err := f.s.Send(ctx, other); !errors.Is(err, ErrNotAllowed) {
		t.Fatal(err)
	}
	// An explicit list replaces the default; an empty one allows nobody.
	f.company(baseCompany + "  modules:\n    forms: {recipients: [only@partner.example]}\n    crm: {enabled: false}\n")
	if _, _, err := f.s.Send(ctx, req("sales@acme.example")); !errors.Is(err, ErrNotAllowed) {
		t.Fatal(err)
	}
	if _, _, err := f.s.Send(ctx, req("only@partner.example")); err != nil {
		t.Fatal(err)
	}
	off := req("a@acme.example")
	off.Source = "crm"
	if _, _, err := f.s.Send(ctx, off); !errors.Is(err, ErrNotAllowed) {
		t.Fatal(err)
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM email_outbox`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("refused mail was stored: %d %v", n, err)
	}
	// Tightening the rules also stops mail already queued.
	f.company(baseCompany + "  modules:\n    forms: {recipients: []}\n")
	if _, err := f.s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.tr.sent) != 0 {
		t.Fatalf("sent despite the new rules: %v", f.tr.sent)
	}
}

func TestInvalidRequestsAreRefused(t *testing.T) {
	f := newFixture(t, baseCompany)
	for name, mut := range map[string]func(*Request){
		"no recipient":    func(r *Request) { r.To = nil },
		"too many":        func(r *Request) { r.To = make([]string, MaxRecipients+1) },
		"to injection":    func(r *Request) { r.To = []string{"a@acme.example\r\nBcc: v@evil.example"} },
		"reply injection": func(r *Request) { r.ReplyTo = "a@b.example\nBcc: v@evil.example" },
		"subject":         func(r *Request) { r.Subject = "x\r\nBcc: v@evil.example" },
		"nul body":        func(r *Request) { r.Text = "a\x00" },
		"source id":       func(r *Request) { r.SourceID = "no spaces" },
	} {
		r := req("a@acme.example")
		mut(&r)
		if _, _, err := f.s.Send(context.Background(), r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSourceIDIsIdempotent(t *testing.T) {
	f := newFixture(t, baseCompany+"  modules:\n    forms: {per_hour: 1}\n")
	ctx := context.Background()
	r := req("sales@acme.example")
	r.SourceID = "lead:1"
	first, _, err := f.s.Send(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	// A retry succeeds although the quota is now used up.
	again, dup, err := f.s.Send(ctx, r)
	if err != nil || !dup || again[0] != first[0] {
		t.Fatal(again, dup, err)
	}
	r.Text = "something else"
	if _, _, err := f.s.Send(ctx, r); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}

func TestQuotas(t *testing.T) {
	f := newFixture(t, baseCompany+"  modules:\n    forms: {per_hour: 3, per_day: 4}\n")
	ctx := context.Background()
	send := func(n int) error {
		var to []string
		for i := range n {
			to = append(to, string(rune('a'+i))+"@acme.example")
		}
		_, _, err := f.s.Send(ctx, req(to...))
		return err
	}
	if err := send(3); err != nil {
		t.Fatal(err)
	}
	var qe *QuotaError
	if err := send(1); !errors.As(err, &qe) || qe.Limit != "per hour" {
		t.Fatal(err)
	}
	f.now = f.now.Add(2 * time.Hour)
	if err := send(2); !errors.As(err, &qe) || qe.Limit != "per day" {
		t.Fatal(err)
	}
	if err := send(1); err != nil {
		t.Fatal(err)
	}
}

func TestRetryBackoffDeadLetterRequeueAndPrune(t *testing.T) {
	f := newFixture(t, baseCompany)
	ctx := context.Background()
	f.tr.err = &SendError{Stage: "connect"}
	ids, _, err := f.s.Send(ctx, req("sales@acme.example"))
	if err != nil {
		t.Fatal(err)
	}
	id := ids[0]
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if n, err := f.s.RunOnce(ctx); n != 1 || err != nil {
			t.Fatal(attempt, n, err)
		}
		m := f.status(id)
		if attempt < MaxAttempts {
			if m.Status != StatusPending || m.Attempts != attempt || m.NextAttempt.Sub(f.now) != Backoff(attempt) || m.LastError != "connect failed" {
				t.Fatalf("attempt %d: %+v", attempt, m)
			}
			if n, _ := f.s.RunOnce(ctx); n != 0 {
				t.Fatal("retried before it was due")
			}
		} else if m.Status != StatusDead || !m.Requeueable {
			t.Fatalf("%+v", m)
		}
		f.now = f.now.Add(2 * time.Hour)
	}
	if err := f.s.Requeue(ctx, "user:root", "eml_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := f.s.Requeue(ctx, "user:root", id); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Requeue(ctx, "user:root", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a pending message cannot be requeued: %v", err)
	}
	// A permanent refusal is a dead letter at once.
	f.tr.err = &SendError{Stage: "RCPT TO", Code: 550, Permanent: true}
	if _, err := f.s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if m := f.status(id); m.Status != StatusDead || m.Attempts != 1 {
		t.Fatalf("%+v", m)
	}
	f.now = f.now.Add(deadContentTTL + time.Hour)
	if err := f.s.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if c := f.content(id); c != "" {
		t.Fatalf("dead letter content kept: %q", c)
	}
	if err := f.s.Requeue(ctx, "user:root", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nothing left to send: %v", err)
	}
	f.now = f.now.Add(rowTTL)
	if err := f.s.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Get(ctx, "forms", id); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	acts := strings.Join(f.auditActions(), ",")
	for _, want := range []string{"email.retry", "email.dead", "email.requeue", "email.prune"} {
		if !strings.Contains(acts, want) {
			t.Errorf("no %s in the audit log: %s", want, acts)
		}
	}
}

// The relay takes the message but the outcome cannot be written: no stream of repeats.
func TestUnrecordedOutcomeDoesNotRepeatAtOnce(t *testing.T) {
	f := newFixture(t, baseCompany)
	ctx := context.Background()
	ids, _, err := f.s.Send(ctx, req("sales@acme.example"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_outcome BEFORE UPDATE OF status ON email_outbox BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RunOnce(ctx); err == nil {
		t.Fatal("want the write error")
	}
	if n, err := f.s.RunOnce(ctx); n != 0 || err != nil || len(f.tr.sent) != 1 {
		t.Fatal("repeated at once:", n, err, len(f.tr.sent))
	}
	for range 2 * MaxAttempts {
		f.now = f.now.Add(2 * time.Hour)
		_, _ = f.s.RunOnce(ctx)
	}
	if len(f.tr.sent) != MaxAttempts {
		t.Fatalf("%d sends, want at most %d", len(f.tr.sent), MaxAttempts)
	}
	if m := f.status(ids[0]); m.Attempts != MaxAttempts {
		t.Fatalf("%+v", m)
	}
}

// cancellingTransport accepts the message while can-core is being told to shut down.
type cancellingTransport struct {
	fakeTransport
	cancel context.CancelFunc
}

func (c *cancellingTransport) Send(ctx context.Context, r Relay, to string, msg []byte) error {
	c.cancel()
	return c.fakeTransport.Send(ctx, r, to, msg)
}

func TestShutdownAfterAcceptanceDoesNotSendTwice(t *testing.T) {
	f := newFixture(t, baseCompany)
	ids, _, err := f.s.Send(context.Background(), req("sales@acme.example", "owner@personal.example"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tr := &cancellingTransport{cancel: cancel}
	f.s.transport = tr
	if _, err := f.s.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if m := f.status(ids[0]); m.Status+f.status(ids[1]).Status != "sentpending" && m.Status+f.status(ids[1]).Status != "pendingsent" {
		t.Fatalf("the accepted message must be recorded as sent: %+v %+v", m, f.status(ids[1]))
	}
	f.now = f.now.Add(2 * time.Hour)
	f.s.transport = &tr.fakeTransport
	if _, err := f.s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(tr.sent) != 2 {
		t.Fatalf("%d sends for 2 messages", len(tr.sent))
	}
}

func TestRelayFailurePausesAndSendersTakeTurns(t *testing.T) {
	f := newFixture(t, baseCompany)
	ctx := context.Background()
	for _, to := range []string{"a1@acme.example", "a2@acme.example", "a3@acme.example"} {
		if _, _, err := f.s.Send(ctx, req(to)); err != nil {
			t.Fatal(err)
		}
	}
	other := req("b1@acme.example")
	other.Source = "crm"
	f.now = f.now.Add(time.Second) // queued after all of forms' mail
	if _, _, err := f.s.Send(ctx, other); err != nil {
		t.Fatal(err)
	}
	f.tr.err = &SendError{Stage: "connect", Session: true}
	if n, err := f.s.RunOnce(ctx); n != 1 || err != nil {
		t.Fatal("the pass must stop at the first session failure:", n, err)
	}
	if n, _ := f.s.RunOnce(ctx); n != 0 {
		t.Fatal("not paused:", n)
	}
	var untouched int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM email_outbox WHERE attempts=0`).Scan(&untouched); err != nil || untouched != 3 {
		t.Fatal(untouched, err)
	}
	f.tr.err = nil
	f.now = f.now.Add(2 * time.Minute)
	if _, err := f.s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, s := range f.tr.sent {
		to, _, _ := strings.Cut(s, "|")
		order = append(order, to[:1])
	}
	if len(order) != 4 || order[0] != "b" && order[1] != "b" {
		t.Fatalf("the second sender waited behind the first one's backlog: %v", order)
	}
}

func TestUnexpectedTransportErrorsAreNotStored(t *testing.T) {
	f := newFixture(t, baseCompany)
	f.tr.err = errors.New("something about sales@acme.example")
	ids, _, err := f.s.Send(context.Background(), req("sales@acme.example"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m := f.status(ids[0]); m.LastError != "delivery failed" {
		t.Fatal(m.LastError)
	}
	f.auditActions()
}

func TestOthersMessagesAreIndistinguishableFromMissingOnes(t *testing.T) {
	f := newFixture(t, baseCompany)
	ids, _, err := f.s.Send(context.Background(), req("sales@acme.example"))
	if err != nil {
		t.Fatal(err)
	}
	_, other := f.s.Get(context.Background(), "crm", ids[0])
	_, missing := f.s.Get(context.Background(), "crm", "eml_missing")
	if !errors.Is(other, ErrNotFound) || other.Error() != missing.Error() {
		t.Fatal(other, missing)
	}
}

func TestNoRelayRefusesAndKeepsQueuedMailWithoutCountingAttempts(t *testing.T) {
	f := newFixture(t, "schema: 1\nname: Acme\ndomain: acme.example\nprovider: {type: local}\n")
	ctx := context.Background()
	if _, _, err := f.s.Send(ctx, req("sales@acme.example")); !errors.Is(err, ErrNotConfigured) {
		t.Fatal(err)
	}
	f.company(baseCompany)
	ids, _, err := f.s.Send(ctx, req("sales@acme.example"))
	if err != nil {
		t.Fatal(err)
	}
	f.company("schema: 1\nname: Acme\ndomain: acme.example\nprovider: {type: local}\n")
	if n, err := f.s.RunOnce(ctx); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	if m := f.status(ids[0]); m.Status != StatusPending || m.Attempts != 0 {
		t.Fatalf("%+v", m)
	}
	f.now = f.now.Add(maxPendingAge + time.Hour)
	if _, err := f.s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if m := f.status(ids[0]); m.Status != StatusDead {
		t.Fatalf("%+v", m)
	}
	// An unreadable company.yaml means the rules are unknown: refuse.
	f.company("email: [")
	if _, _, err := f.s.Send(ctx, req("sales@acme.example")); !errors.Is(err, ErrNotConfigured) {
		t.Fatal(err)
	}
}

func TestRelayResolution(t *testing.T) {
	dir := t.TempDir()
	cfile := filepath.Join(dir, "company.yaml")
	secrets := filepath.Join(dir, "secret")
	if err := os.Mkdir(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(path, s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	load := func(company string, secret map[string]string, insecure bool) Policy {
		t.Helper()
		write(cfile, "schema: 1\nname: Acme\ndomain: acme.example\nprovider: {type: local}\n"+company)
		for _, k := range []string{"host", "port", "tls", "from", "username", "password"} {
			_ = os.Remove(filepath.Join(secrets, k))
			if v, ok := secret[k]; ok {
				write(filepath.Join(secrets, k), v+"\n")
			}
		}
		p, err := Source{CompanyFile: cfile, SecretDir: secrets, AllowInsecure: insecure}.Load()
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	creds := map[string]string{"host": "smtp.relay.example", "username": "u", "password": "hunter2-relay"}

	p := load("email: {relay: {host: smtp.relay.example, port: 465, from: hello@acme.example}}\n", creds, false)
	if p.RelayErr != nil || p.Relay.TLS != TLSImplicit || p.Relay.Password != "hunter2-relay" || p.Relay.From != "hello@acme.example" {
		t.Fatalf("%+v %v", p.Relay.TLS, p.RelayErr)
	}
	p = load("", creds, false) // the wizard's Secret alone is enough
	if p.RelayErr != nil || p.Relay.Port != 587 || p.Relay.TLS != TLSStartTLS || p.Relay.From != "no-reply@acme.example" {
		t.Fatalf("%v", p.RelayErr)
	}
	for name, tc := range map[string]Policy{
		"company.yaml redirects the credentials": load("email: {relay: {host: attacker.example}}\n", creds, false),
		"credentials without a host":             load("email: {relay: {host: smtp.relay.example}}\n", map[string]string{"username": "u", "password": "hunter2-relay"}, false),
		"password without user":                  load("", map[string]string{"host": "h.example", "password": "hunter2-relay"}, false),
		"tls none refused":                       load("email: {relay: {host: sink, tls: none}}\n", nil, false),
		"no credentials in clear":                load("", map[string]string{"host": "sink.example", "tls": "none", "username": "u", "password": "hunter2-relay"}, true),
		"bad tls":                                load("", map[string]string{"host": "h.example", "tls": "off"}, true),
		"nothing":                                load("", nil, false),
	} {
		if !errors.Is(tc.RelayErr, ErrNotConfigured) || tc.Relay.Password != "" {
			t.Errorf("%s: accepted (%v)", name, tc.RelayErr)
		} else if strings.Contains(tc.RelayErr.Error(), "hunter2") {
			t.Errorf("%s: the error leaks the password", name)
		}
	}
	if p := load("email: {relay: {host: sink, tls: none}}\n", nil, true); p.RelayErr != nil {
		t.Fatal(p.RelayErr)
	}
}
