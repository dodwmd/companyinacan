package adminui

// Messages of the email delivery page (E03d, specs/039-email-send).
func init() {
	for k, v := range map[string]string{
		"nav.email":            "Email",
		"mail.title":           "Email delivery",
		"mail.relay_ok":        "Mail is sent through %s port %d (%s) from %s.",
		"mail.relay_none":      "No mail can be sent: %s. Set the relay in company.yaml and the Secret can-smtp-relay (see the email documentation).",
		"mail.help":            "The newest 100 messages. Addresses are shortened and message contents are never shown. Failed messages are retried; after that they can be sent again for seven days.",
		"mail.empty":           "No email has been queued yet.",
		"mail.when":            "Queued",
		"mail.source":          "From module",
		"mail.to":              "Recipient",
		"mail.status":          "Status",
		"mail.attempts":        "Attempts",
		"mail.error":           "Last error",
		"mail.action":          "Action",
		"mail.requeue":         "Send again",
		"mail.requeue_label":   "Send message %s again",
		"mail.st_pending":      "Waiting",
		"mail.st_sent":         "Sent",
		"mail.st_dead":         "Failed",
		"flash.email_requeued": "The message was queued again.",
		"lead.mail":            "Notification email",
		"lead.mail_failed":     "Email notification failed",
		"lead.mail_retrying":   "Email notification retrying",
	} {
		messagesEN[k] = v
	}
}
