package installer

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// The mailbox the support module fetches (specs/059-support-mailbox): its credentials reach the
// module in the Secret `can-imap`, written like `can-smtp` and empty unless company.yaml asks for
// inbound mail and the operator's Secret can serve it.

const secretIMAP = "can-imap"

// IMAPAccount is what a wrapped helpdesk needs to fetch a mailbox.
type IMAPAccount struct {
	Host               string
	Port               int
	Username, Password string
	Encryption         string // config.IMAPSSL or config.IMAPStartTLS
	Folder             string
	VerifyTLS          bool
}

// wantsMailbox reports whether the module is a helpdesk that fetches a mailbox itself.
func wantsMailbox(id string) bool { return id == "support" }

// mailValues are the chart values of the support inbox: the settings as can-core read and checked
// them, so that a value it refused never reaches the application. nil for other modules.
func mailValues(s reconcile.Spec) map[string]any {
	if !wantsMailbox(s.ID) {
		return nil
	}
	m, issues := config.SupportMailFor(s.ID, s.Domain, s.Email, s.Settings)
	invalid := ""
	for _, is := range issues {
		if is.Invalid {
			invalid = is.Message
			break
		}
	}
	address := m.Address
	if invalid != "" {
		address = ""
	}
	return map[string]any{"mail": map[string]any{"address": address, "name": m.MailboxName, "imapWanted": m.IMAP != nil, "invalid": invalid}}
}

// imapValues is what `can-imap` holds for s: empty without an account.
func (in *Installer) imapValues(s reconcile.Spec) (map[string]string, error) {
	vals := map[string]string{}
	if in.cfg.IMAP == nil || !wantsMailbox(s.ID) {
		return vals, nil
	}
	a, err := in.cfg.IMAP(s)
	if err != nil {
		return nil, fmt.Errorf("reading the inbound mail credentials: %w", err)
	}
	if a != nil {
		vals = map[string]string{"host": a.Host, "port": fmt.Sprint(a.Port), "user": a.Username, "password": a.Password,
			"encryption": a.Encryption, "folder": a.Folder, "verify": fmt.Sprint(a.VerifyTLS)}
	}
	return vals, nil
}

// ensureIMAPSecret writes `can-imap` (as `data`, so that dropped keys go); nil for other modules.
func (in *Installer) ensureIMAPSecret(ctx context.Context, s reconcile.Spec) (map[string]string, error) {
	if !wantsMailbox(s.ID) {
		return nil, nil
	}
	vals, err := in.imapValues(s)
	if err != nil {
		return nil, err
	}
	o := secret(s.Namespace, secretIMAP, s.ID, nil)
	delete(o, "stringData")
	data := map[string]any{}
	for k, v := range vals {
		data[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	o["data"] = data
	if err := in.cfg.API.Apply(ctx, o); err != nil {
		return nil, fmt.Errorf("writing the inbound mail Secret: %w", err)
	}
	return vals, nil
}
