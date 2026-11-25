package email

import "github.com/dodwmd/companyinacan/core/server/notify"

func notifyMessage(to, subject, text, html string) notify.Message {
	return notify.Message{To: to, Subject: subject, Text: text, HTML: html}
}
