package email

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dodwmd/companyinacan/core/server/notify"
)

// NotifyMailer is the e-mail channel of the notification service. Only plain text is sent.
// Returning nil means the message is in the outbox; its delivery status is shown there.
type NotifyMailer struct{ S *Service }

var _ notify.Mailer = NotifyMailer{}

// Send implements notify.Mailer. A notification a module caused counts against that module and obeys
// its switch. What the module wrote is not mailed unless the operator allowed it: mail from the
// company's own address with a module's wording would be a gift to whoever takes a module over.
func (m NotifyMailer) Send(ctx context.Context, msg notify.Message) error {
	source := SystemSource
	if msg.Module != "" && msg.Module != "can-core" {
		source = msg.Module
	}
	// Mail about an approval a module opened has an allowance of its own for that module: what the
	// module (or the public, through it) sends cannot crowd it out, and a module opening approvals
	// cannot use up can-core's, which invitations and password resets need.
	if msg.Platform && source != SystemSource {
		source = ApprovalSource(source)
	}
	subject, text := msg.Subject, msg.Text
	if msg.Authored && source != SystemSource {
		p, err := m.S.source.Load()
		if err != nil {
			return fmt.Errorf("%w: the email rules cannot be read", ErrNotConfigured)
		}
		if !p.NotificationText(source) {
			subject, text = fixedNotice(p.domain, source, msg.Type, msg.Count)
		}
	}
	subject = strings.Join(strings.FieldsFunc(subject, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if len(subject) > MaxSubjectBytes {
		cut := MaxSubjectBytes
		for cut > 0 && !utf8.RuneStart(subject[cut]) {
			cut--
		}
		subject = subject[:cut]
	}
	if text == "" {
		text = subject
	}
	_, _, err := m.S.Send(ctx, Request{Source: source, Notice: true, ForUser: msg.ForUser, To: []string{msg.To}, Subject: subject, Text: text})
	return err
}

// fixedNotice is the project's wording. The type is module-chosen, so its dots are spaced out: mail
// readers turn "some.host.example" into a link.
func fixedNotice(domain, module, typ string, count int) (subject, text string) {
	kind := strings.ReplaceAll(typ, ".", " / ")
	where := "your inbox in the admin web interface (/notifications)"
	if domain != "" {
		where = "https://admin." + domain + "/notifications"
	}
	if count > 1 {
		return fmt.Sprintf("%d notifications from the %s module", count, module),
			fmt.Sprintf("You have %d new notifications from the %s module.\n\nRead them here: %s\n", count, module, where)
	}
	return fmt.Sprintf("Notification from the %s module", module),
		fmt.Sprintf("You have a new notification from the %s module (type: %s).\n\nRead it here: %s\n", module, kind, where)
}
