package email

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dodwmd/companyinacan/core/server/notify"
)

// NotifyMailer is the e-mail channel of the notification service. Only the plain text is sent.
// Returning nil means the message is in the outbox; its delivery status is shown there.
type NotifyMailer struct{ S *Service }

var _ notify.Mailer = NotifyMailer{}

// Send implements notify.Mailer.
func (m NotifyMailer) Send(ctx context.Context, msg notify.Message) error {
	subject := strings.Join(strings.FieldsFunc(msg.Subject, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if len(subject) > MaxSubjectBytes {
		cut := MaxSubjectBytes
		for cut > 0 && !utf8.RuneStart(subject[cut]) {
			cut--
		}
		subject = subject[:cut]
	}
	text := msg.Text
	if text == "" {
		text = subject
	}
	_, _, err := m.S.Send(ctx, Request{Source: SystemSource, To: []string{msg.To}, Subject: subject, Text: text})
	return err
}
