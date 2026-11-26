package launch

import (
	"context"
	"errors"
	"strconv"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/config"
)

// SMTPRelay is the Secret can-system/can-smtp-relay that can-core's email.send reads
// (docs/email.md). The credentials are stored together with the host they belong to.
type SMTPRelay struct {
	Host, TLS, From    string
	Port               int
	Username, Password string
}

// SMTPFrom combines company.yaml's email.relay with the credentials of --smtp-credentials-file.
func SMTPFrom(c *config.Company, username, password string) (*SMTPRelay, error) {
	if c.Email == nil || c.Email.Relay == nil || c.Email.Relay.Host == "" {
		return nil, errors.New("SMTP credentials were given but company.yaml has no email.relay.host to send them to")
	}
	if username == "" || password == "" {
		return nil, errors.New("the SMTP credentials file needs both SMTP_USERNAME and SMTP_PASSWORD")
	}
	r := c.Email.Relay
	if r.TLS == "none" {
		return nil, errors.New("email.relay.tls: none cannot be used with credentials")
	}
	return &SMTPRelay{Host: r.Host, Port: r.Port, TLS: r.TLS, From: r.From, Username: username, Password: password}, nil
}

// manifest renders the Secret. It is applied server-side from stdin, so the password is never in an
// argument list or in a last-applied annotation.
func (s SMTPRelay) manifest() ([]byte, error) {
	data := map[string]string{"host": s.Host, "username": s.Username, "password": s.Password}
	if s.Port > 0 {
		data["port"] = strconv.Itoa(s.Port)
	}
	if s.TLS != "" {
		data["tls"] = s.TLS
	}
	if s.From != "" {
		data["from"] = s.From
	}
	return yaml.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   map[string]any{"name": "can-smtp-relay", "namespace": SystemNS, "labels": map[string]any{"app.kubernetes.io/part-of": "companyinacan"}},
		"stringData": data,
	})
}

func (r *runner) relaySecret(ctx context.Context) error {
	if r.o.SMTP == nil {
		return nil // an existing Secret (an earlier run, the setup wizard) is left alone
	}
	m, err := r.o.SMTP.manifest()
	if err != nil {
		return err
	}
	_, err = r.apply(ctx, m, true)
	return err
}
