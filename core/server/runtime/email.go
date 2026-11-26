package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/email"
	"github.com/dodwmd/companyinacan/core/server/installer"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/tokenreview"
)

// defaultSMTPSecretDir is where deploy/can-core mounts the optional Secret can-smtp-relay.
const defaultSMTPSecretDir = "/etc/can/smtp"

func (c Config) envBool(k string) (bool, error) {
	v := c.env(k)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %q is not a boolean", k, v)
	}
	return b, nil
}

// emailService builds the email.send outbox (epic E03d, docs/email.md).
func (c Config) emailService(db *store.DB, al *audit.Log, log *slog.Logger) (*email.Service, error) {
	insecure, err := c.envBool("CAN_SMTP_ALLOW_INSECURE")
	if err != nil {
		return nil, err
	}
	private, err := c.envBool("CAN_SMTP_ALLOW_PRIVATE")
	if err != nil {
		return nil, err
	}
	if insecure {
		log.Warn("CAN_SMTP_ALLOW_INSECURE=true: mail may leave without TLS; development only")
	}
	src := c.emailSource()
	src.AllowInsecure = insecure
	return email.New(db, al, src, email.SMTP{AllowPrivate: private}, log), nil
}

// emailSource is where the relay settings and sending rules are read from.
func (c Config) emailSource() email.Source {
	dir := c.env("CAN_SMTP_SECRET_DIR")
	if dir == "" {
		dir = defaultSMTPSecretDir
	}
	return email.Source{CompanyFile: c.CompanyFile, SecretDir: dir, Domain: c.CompanyDomain}
}

// appRelay is the relay handed to a wrapped app that speaks SMTP itself and that the company opted
// in (docs/email-policy.md). Such mail does not pass email.send, so the recipient allow-list and
// quotas do not apply to it. The settings come from the company.yaml being applied, the credentials
// from the relay Secret. A relay without TLS is never handed out.
func (c Config) appRelay(spec reconcile.Spec) (*installer.SMTPRelay, error) {
	p, err := c.emailSource().For(&config.Company{Domain: spec.Domain, Email: spec.Email})
	if err != nil {
		return nil, err
	}
	// Unreadable is not "no relay": answering so would empty the module's can-smtp.
	if errors.Is(p.RelayErr, email.ErrSecretUnreadable) {
		return nil, p.RelayErr
	}
	if p.RelayErr != nil {
		return nil, nil
	}
	r := p.Relay
	var enc string
	switch r.TLS {
	case email.TLSStartTLS:
		enc = "tls"
	case email.TLSImplicit:
		enc = "ssl"
	default:
		return nil, nil
	}
	return &installer.SMTPRelay{Host: r.Host, Port: r.Port, Username: r.Username, Password: r.Password, From: r.From, Encryption: enc}, nil
}

// devModuleSuffix marks the records devModules creates, so a later start can remove them.
const devModuleSuffix = " (development identity)"

// isDevModule reports whether m is a record devModules created.
func isDevModule(m store.Module) bool {
	return m.Version == "dev" && strings.HasSuffix(m.Manifest.Name, devModuleSuffix)
}

// devModules registers fixed module identities for a stack without Kubernetes (docker-compose.yml):
// CAN_DEV_MODULE_TOKENS="forms:<token>". It works only in a binary built with -tags candevstack, outside
// Kubernetes, with the fake token review and no installer,
// so it cannot exist next to real module identities, and the only capability it grants is
// email.send. Records of identities no longer asked for are removed at start.
func (c Config) devModules(ctx context.Context, db *store.DB, rev tokenreview.Reviewer, log *slog.Logger) error {
	want := map[string]string{} // module id -> token
	if spec := c.env("CAN_DEV_MODULE_TOKENS"); spec != "" {
		if !devBuild {
			return errors.New("CAN_DEV_MODULE_TOKENS: this can-core was not built for development and cannot use it")
		}
		if c.env("KUBERNETES_SERVICE_HOST") != "" {
			return errors.New("CAN_DEV_MODULE_TOKENS: refused inside Kubernetes")
		}
		if c.TokenReview != "fake" || c.Installer != "none" {
			return errors.New("CAN_DEV_MODULE_TOKENS needs CAN_TOKEN_REVIEW=fake and CAN_INSTALLER=none (development only)")
		}
		for _, e := range splitList(spec) {
			id, token, ok := strings.Cut(e, ":")
			if !ok || id == "" || len(token) < 24 {
				return errors.New("CAN_DEV_MODULE_TOKENS: want <module>:<token of at least 24 characters>[,...]")
			}
			want[id] = token
		}
	}
	mods, err := db.ListModules(ctx)
	if err != nil {
		return err
	}
	for _, m := range mods {
		if _, ok := want[m.ID]; !ok && isDevModule(m) {
			if _, err := db.ExecContext(ctx, `DELETE FROM modules WHERE id=?`, m.ID); err != nil {
				return fmt.Errorf("removing the development identity %q: %w", m.ID, err)
			}
			log.Warn("stale development module identity removed", "module", m.ID)
		}
	}
	if len(want) == 0 {
		return nil
	}
	fake, ok := rev.(*tokenreview.Fake)
	if !ok {
		return errors.New("CAN_DEV_MODULE_TOKENS needs the fake token review")
	}
	for id, token := range want {
		fake.Add(token, "mod-"+id, id)
		if _, err := db.ModuleByIdentity(ctx, "mod-"+id, id); err == nil {
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		var m config.Module
		m.ID, m.Name, m.Version = id, id+devModuleSuffix, "dev"
		m.Requires.Capabilities = []string{"email.send"}
		if err := db.UpsertModule(ctx, store.Module{ID: id, Version: "dev", Namespace: "mod-" + id, ServiceAccount: id, State: store.StateEnabled, Manifest: m}, nil, nil); err != nil {
			return fmt.Errorf("registering development module %q: %w", id, err)
		}
		log.Warn("development module identity registered", "module", id)
	}
	return nil
}
