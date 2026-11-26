package cli

import (
	"fmt"
	"strings"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/launch"
)

// smtpRelay reads --smtp-credentials-file (the same KEY=VALUE format as the backup credentials);
// empty path means "leave the relay Secret alone". The password is registered for redaction.
func (t *target) smtpRelay(path string) (*launch.SMTPRelay, error) {
	if path == "" {
		return nil, nil
	}
	env, err := backup.LoadEnvFile(path)
	if err != nil {
		return nil, fmt.Errorf("--smtp-credentials-file: %w", err)
	}
	var user, pass string
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		switch k {
		case "SMTP_USERNAME":
			user = v
		case "SMTP_PASSWORD":
			pass = v
		}
	}
	if pass != "" {
		t.redact.Add(pass)
	}
	r, err := launch.SMTPFrom(t.company, user, pass)
	if err != nil {
		return nil, fmt.Errorf("--smtp-credentials-file: %w", err)
	}
	return r, nil
}

// imapAccount reads --support-imap-credentials-file (modules/support/README.md).
func (t *target) imapAccount(path string) (*launch.SupportIMAP, error) {
	if path == "" {
		return nil, nil
	}
	env, err := backup.LoadEnvFile(path)
	if err != nil {
		return nil, fmt.Errorf("--support-imap-credentials-file: %w", err)
	}
	var user, pass string
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		switch k {
		case "IMAP_USERNAME":
			user = v
		case "IMAP_PASSWORD":
			pass = v
		}
	}
	if pass != "" {
		t.redact.Add(pass)
	}
	a, err := launch.SupportIMAPFrom(t.company, user, pass)
	if err != nil {
		return nil, fmt.Errorf("--support-imap-credentials-file: %w", err)
	}
	return a, nil
}
