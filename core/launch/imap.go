package launch

import (
	"context"
	"errors"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/config"
)

// SupportIMAP is the Secret can-system/can-support-imap: the mailbox the support module fetches
// (docs/support.md). As for the relay, the password is stored with the host it belongs to.
type SupportIMAP struct {
	Host, Username, Password string
}

// SupportIMAPFrom combines company.yaml's support settings with the credentials of
// --support-imap-credentials-file.
func SupportIMAPFrom(c *config.Company, username, password string) (*SupportIMAP, error) {
	m, _ := config.SupportMailOf(c)
	if m.IMAP == nil {
		return nil, errors.New("IMAP credentials were given but company.yaml has no support module with support_address and inbound.imap.host to use them for")
	}
	if password == "" {
		return nil, errors.New("the IMAP credentials file needs IMAP_PASSWORD")
	}
	if username == "" && m.IMAP.Username == "" {
		return nil, errors.New("give IMAP_USERNAME in the credentials file, or inbound.imap.username in company.yaml")
	}
	return &SupportIMAP{Host: m.IMAP.Host, Username: username, Password: password}, nil
}

func (s SupportIMAP) manifest() ([]byte, error) {
	data := map[string]string{"host": s.Host, "password": s.Password}
	if s.Username != "" {
		data["username"] = s.Username
	}
	return yaml.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   map[string]any{"name": "can-support-imap", "namespace": SystemNS, "labels": map[string]any{"app.kubernetes.io/part-of": "companyinacan"}},
		"stringData": data,
	})
}

func (r *runner) imapSecret(ctx context.Context) error {
	if r.o.IMAP == nil {
		return nil // an existing Secret is left alone
	}
	m, err := r.o.IMAP.manifest()
	if err != nil {
		return err
	}
	_, err = r.apply(ctx, m, true)
	return err
}
