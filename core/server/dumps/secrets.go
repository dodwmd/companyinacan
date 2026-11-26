package dumps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/server/kube"
)

// SystemSecrets are the operator-provided Secrets of can-system that a restore brings back.
// They are optional.
var SystemSecrets = []string{"can-smtp-relay", "sso-client", "can-postgres-admin"}

// Not exported: the installer's own record of a release (rewritten by the restore install) and
// tokens Kubernetes issues itself.
const (
	// ReleaseSecret is also how a module that was never installed is told apart.
	ReleaseSecret      = "can-release"
	relaySecret        = "can-smtp" // written from company.yaml and the relay Secret at every apply
	serviceAccountType = "kubernetes.io/service-account-token"
)

// ModuleRef is what the Secrets export needs to know of a recorded module.
type ModuleRef struct {
	ID      string
	Enabled bool
}

// ExportSecrets reads the Secrets a restore must re-create: every Secret of every recorded
// module's namespace and the named ones of can-system. A module without a namespace has none.
// An enabled module whose Secrets cannot be read fails the export (a backup without them could
// not be restored); a disabled or failed one is reported in warnings, so that it never stops
// backups.
func ExportSecrets(ctx context.Context, api kube.API, mods []ModuleRef) (*backup.SecretsExport, []string, error) {
	e := &backup.SecretsExport{Version: 1}
	var warnings []string
	add := func(ns, name string, o kube.Object) {
		typ, _ := o["type"].(string)
		if typ == serviceAccountType || name == ReleaseSecret || name == relaySecret {
			return
		}
		data := map[string]string{}
		d, _ := o["data"].(obj)
		for k, v := range d {
			if s, ok := v.(string); ok {
				data[k] = s
			}
		}
		e.Secrets = append(e.Secrets, backup.ExportedSecret{Namespace: ns, Name: name, Type: typ, Data: data})
	}
	for _, m := range mods {
		ns := "mod-" + m.ID
		err := func() error {
			if _, err := api.Get(ctx, "Namespace", "", ns); kube.IsNotFound(err) {
				return nil
			} else if err != nil {
				return err
			}
			items, err := api.List(ctx, "Secret", ns, "")
			if err != nil {
				return err
			}
			for _, o := range items {
				md, _ := o["metadata"].(obj)
				name, _ := md["name"].(string)
				add(ns, name, o)
			}
			return nil
		}()
		switch {
		case err == nil:
		case m.Enabled:
			return nil, nil, fmt.Errorf("read the Secrets of module %s: %w", m.ID, err)
		default:
			warnings = append(warnings, fmt.Sprintf("module %q: its Secrets are NOT in the backup (%v)", m.ID, err))
		}
	}
	for _, name := range SystemSecrets {
		o, err := api.Get(ctx, "Secret", "can-system", name)
		if kube.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read Secret can-system/%s: %w", name, err)
		}
		add("can-system", name, o)
	}
	sort.Slice(e.Secrets, func(i, j int) bool {
		a, b := e.Secrets[i], e.Secrets[j]
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
	return e, warnings, nil
}

// dumpSecrets writes the export beside the state dump. Without a Secrets source (no cluster) any
// earlier export is removed, so a stale one is never backed up as current.
func (d *Dumper) dumpSecrets(ctx context.Context) ([]string, error) {
	dir := filepath.Join(d.Store.Dir, "can-core")
	file := filepath.Base(backup.SecretsDump)
	if d.Secrets == nil {
		if err := os.Remove(filepath.Join(dir, file)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, nil
	}
	e, warnings, err := d.Secrets(ctx)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return warnings, d.Store.writeAtomic(dir, file, bytes.NewReader(b))
}
