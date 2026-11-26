package email

import (
	"context"
	"errors"
	"io"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/notify"
)

// sentBody decodes the n-th message the fake relay took.
func (f *fixture) sentBody(n int) (subject, body string) {
	f.t.Helper()
	_, raw, _ := strings.Cut(f.tr.sent[n], "|")
	m, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		f.t.Fatal(err)
	}
	b, err := io.ReadAll(quotedprintable.NewReader(m.Body))
	if err != nil {
		f.t.Fatal(err)
	}
	return m.Header.Get("Subject"), string(b)
}

func authored(module, to string) notify.Message {
	return notify.Message{To: to, ForUser: "user:alice", Module: module, Authored: true, Type: module + ".reset.evil.example",
		Subject: "URGENT reset your password", Text: "Go to http://evil.example now", HTML: "<a href=//evil.example>x</a>"}
}

func TestModuleNotificationsUseFixedTextAndTheModulesRules(t *testing.T) {
	f := newFixture(t, baseCompany+"  modules:\n    crm: {per_hour: 2}\n    off: {enabled: false}\n")
	ctx := context.Background()
	m := NotifyMailer{S: f.s}

	if err := m.Send(ctx, authored("off", "alice@acme.example")); !errors.Is(err, ErrNotAllowed) {
		t.Fatal("a module with email turned off:", err)
	}
	for _, want := range []bool{true, true, false} {
		err := m.Send(ctx, authored("crm", "alice@acme.example"))
		var qe *QuotaError
		if want && err != nil || !want && !errors.As(err, &qe) {
			t.Fatal("module quota:", err)
		}
	}
	// The module used its quota up; the platform's own mail is unaffected.
	if err := m.Send(ctx, notify.Message{To: "alice@acme.example", ForUser: "user:alice", Subject: "Approval needed", Text: "waiting"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var fixed int
	for i := range f.tr.sent {
		subject, body := f.sentBody(i)
		if strings.Contains(subject+body, "password") || strings.Contains(subject+body, "evil.example") {
			t.Fatalf("module wording or a linkable type was mailed: %q %q", subject, body)
		}
		if subject == "Notification from the crm module" && strings.Contains(body, "https://admin.acme.example/notifications") && strings.Contains(body, "crm / reset / evil / example") {
			fixed++
		}
	}
	if len(f.tr.sent) != 3 || fixed != 2 {
		t.Fatal(len(f.tr.sent), fixed)
	}
	f.auditActions()
}

func TestModuleWordingNeedsAnOptIn(t *testing.T) {
	f := newFixture(t, baseCompany+"  modules:\n    crm: {notification_text: true}\n")
	if err := (NotifyMailer{S: f.s}).Send(context.Background(), authored("crm", "alice@acme.example")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if subject, body := f.sentBody(0); subject != "URGENT reset your password" || strings.Contains(body, "<a href") {
		t.Fatal(subject, body)
	}
}

func TestNotificationAddressesMustBeVouchedFor(t *testing.T) {
	const sso = "sso: {enabled: true, issuer: \"https://id.example\", client_id: x, client_secret_ref: sso-client}\n"
	f := newFixture(t, baseCompany+sso)
	ctx := context.Background()
	m := NotifyMailer{S: f.s}
	send := func(to string) error {
		return m.Send(ctx, notify.Message{To: to, ForUser: "user:alice", Subject: "s", Text: "t"})
	}
	if err := send("victim@elsewhere.example"); !errors.Is(err, ErrNotAllowed) {
		t.Fatal("a self-set outside address:", err)
	}
	u, err := f.db.CreateAdmin(ctx, "alice", "hash", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO sso_identities(issuer_hash, subject, user_id, email, name, groups_json, amr_json, created_at, last_login) VALUES ('h','sub',?, 'alice@idp-mail.example','', '[]','[]',1,1)`, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := send("alice@idp-mail.example"); err != nil {
		t.Fatal("the verified single sign-on address:", err)
	}
	if err := m.Send(ctx, notify.Message{To: "alice@idp-mail.example", ForUser: "user:bob", Subject: "s", Text: "t"}); !errors.Is(err, ErrNotAllowed) {
		t.Fatal("somebody else's single sign-on address:", err)
	}
	// An operator list replaces the company domain.
	f.company(baseCompany + "  notification_recipients: [\"@partner.example\"]\n" + sso)
	if err := send("alice@partner.example"); err != nil {
		t.Fatal(err)
	}
	if err := send("alice@acme.example"); !errors.Is(err, ErrNotAllowed) {
		t.Fatal(err)
	}
	// Without verified claims the identity provider's address proves nothing; queued mail is re-checked.
	f.company(baseCompany + strings.Replace(sso, "enabled: true", "enabled: true, require_email_verified: false", 1))
	if _, err := f.s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.tr.sent) != 0 {
		t.Fatal("sent to addresses no longer allowed:", f.tr.sent)
	}
	if err := send("alice@idp-mail.example"); !errors.Is(err, ErrNotAllowed) {
		t.Fatal(err)
	}
}

func TestOnlyTheNotificationChannelSendsAsCanCore(t *testing.T) {
	f := newFixture(t, baseCompany)
	r := req("anyone@elsewhere.example")
	r.Source = SystemSource
	if _, _, err := f.s.Send(context.Background(), r); !errors.Is(err, ErrNotAllowed) {
		t.Fatal(err)
	}
}
