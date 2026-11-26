package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/installer"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// defaultIMAPSecretDir is where deploy/can-core mounts the optional Secret can-support-imap.
const defaultIMAPSecretDir = "/etc/can/support-imap"

const maxIMAPSecretValue = 4 << 10

// appIMAP is the mailbox the support module fetches (specs/059-support-mailbox): company.yaml says
// that it does and where, the Secret can-system/can-support-imap holds the credentials. As for the
// relay, they are bound to the host stored next to them: editing company.yaml cannot have the
// password sent to another server. nil means nothing usable; an unreadable Secret is an error, so
// that a module keeps what it has.
func (c Config) appIMAP(spec reconcile.Spec) (*installer.IMAPAccount, error) {
	m, _ := config.SupportMailFor(spec.ID, spec.Domain, spec.Email, spec.Settings)
	if m.IMAP == nil {
		return nil, nil
	}
	dir := c.env("CAN_SUPPORT_IMAP_SECRET_DIR")
	if dir == "" {
		dir = defaultIMAPSecretDir
	}
	vals := map[string]string{}
	for _, k := range []string{"host", "username", "password", "port", "encryption", "tls_verify"} {
		v, err := readSecretKey(filepath.Join(dir, k))
		if err != nil {
			return nil, fmt.Errorf("the Secret can-support-imap is there but key %q cannot be read", k)
		}
		vals[k] = v
	}
	if !strings.EqualFold(vals["host"], m.IMAP.Host) || vals["password"] == "" {
		return nil, nil // no Secret, one for another host, or one without a password
	}
	a := &installer.IMAPAccount{Host: m.IMAP.Host, Port: m.IMAP.Port, Username: m.IMAP.Username, Password: vals["password"],
		Encryption: m.IMAP.Encryption, Folder: m.IMAP.Folder, VerifyTLS: vals["tls_verify"] != "false"}
	if v := vals["username"]; v != "" {
		a.Username = v
	}
	if v := vals["encryption"]; v == config.IMAPSSL || v == config.IMAPStartTLS {
		a.Encryption = v
	}
	if v := vals["port"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return nil, nil
		}
		a.Port = n
	}
	if a.Port == 0 {
		a.Port = 993
		if a.Encryption == config.IMAPStartTLS {
			a.Port = 143
		}
	}
	if a.Username == "" {
		return nil, nil
	}
	return a, nil
}

// readSecretKey reads one file of a mounted Secret; "" when it is not there.
func readSecretKey(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // a fixed key name under the mount directory
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxIMAPSecretValue+1))
	if err != nil || len(b) > maxIMAPSecretValue {
		return "", errors.New("unreadable or too large")
	}
	return string(bytes.TrimRight(b, "\r\n")), nil
}
