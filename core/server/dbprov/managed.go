package dbprov

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/deploy"
)

const (
	managedNamespace = "can-data"
	managedSecret    = "can-postgres"
	managedHost      = "postgres.can-data.svc.cluster.local"
	managedPort      = 5432
)

// Managed provisions module databases on a Postgres that can-core runs itself in can-data
// (deploy/templates/postgres.yaml). The server is created the first time a module needs it, so a
// company without such a module runs none. It is never deleted. MariaDB is not managed.
type Managed struct {
	API kube.API
	// Wait is how long to wait for a fresh server to accept connections (default 3m: image pull and initdb).
	Wait time.Duration
	// Poll is the retry interval (default 3s).
	Poll time.Duration

	// provision creates the database over an admin connection; nil means Postgres (tests replace it).
	provision func(ctx context.Context, adminURL, moduleID, password string) (Credentials, error)

	mu sync.Mutex
}

func postgresProvision(ctx context.Context, adminURL, moduleID, password string) (Credentials, error) {
	p := &Postgres{AdminURL: adminURL, Host: managedHost, Port: managedPort, LockDown: true}
	return p.Ensure(ctx, "postgres", moduleID, password)
}

// Ensure implements Provisioner.
func (m *Managed) Ensure(ctx context.Context, kind, moduleID, password string) (Credentials, error) {
	if kind != "postgres" {
		return Credentials{}, fmt.Errorf("%w: %s (can-core manages Postgres only)", ErrUnsupported, kind)
	}
	if _, err := Names(moduleID); err != nil {
		return Credentials{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuseIfExternal(ctx, moduleID); err != nil {
		return Credentials{}, err
	}
	admin, err := m.adminPassword(ctx)
	if err != nil {
		return Credentials{}, err
	}
	if err := m.apply(ctx); err != nil {
		return Credentials{}, fmt.Errorf("deploying the shared postgres: %w", err)
	}
	wait, poll := m.Wait, m.Poll
	if wait == 0 {
		wait = 3 * time.Minute
	}
	if poll == 0 {
		poll = 3 * time.Second
	}
	provision := m.provision
	if provision == nil {
		provision = postgresProvision
	}
	// The password is hex, so it needs no escaping in the URL.
	url := fmt.Sprintf("postgres://postgres:%s@%s:%d/postgres?sslmode=disable&connect_timeout=5", admin, managedHost, managedPort)
	deadline := time.Now().Add(wait)
	for {
		c, err := provision(ctx, url, moduleID, password)
		if err == nil {
			return c, nil
		}
		if time.Now().After(deadline) {
			return Credentials{}, fmt.Errorf("the shared postgres in %s did not accept the module database within %s: %w", managedNamespace, wait, err)
		}
		select {
		case <-ctx.Done():
			return Credentials{}, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// adminPassword returns the superuser password, creating it once. A failed read is an error, never
// a reason to generate a new one: the data directory keeps the password it was initialised with.
func (m *Managed) adminPassword(ctx context.Context) (string, error) {
	read := func() (string, error) {
		o, err := m.API.Get(ctx, "Secret", managedNamespace, managedSecret)
		if err != nil {
			return "", err
		}
		d, _ := o["data"].(map[string]any)
		enc, _ := d["password"].(string)
		b, err := base64.StdEncoding.DecodeString(enc)
		if err != nil || !passwordRE.Match(b) {
			return "", fmt.Errorf("secret %s/%s has no usable key password", managedNamespace, managedSecret)
		}
		return string(b), nil
	}
	pw, err := read()
	if err == nil {
		return pw, nil
	}
	if !kube.IsNotFound(err) {
		return "", fmt.Errorf("reading the postgres admin Secret: %w", err)
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	if _, err := m.API.Create(ctx, kube.Object{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   map[string]any{"name": managedSecret, "namespace": managedNamespace, "labels": map[string]any{"app.kubernetes.io/managed-by": "can-core"}},
		"stringData": map[string]any{"password": hex.EncodeToString(b)},
	}); err != nil {
		return "", fmt.Errorf("creating the postgres admin Secret: %w", err)
	}
	return read() // whoever won the create
}

// manifests decodes the embedded server manifests.
func manifests() ([]kube.Object, error) {
	b, err := deploy.Templates.ReadFile("templates/postgres.yaml")
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var out []kube.Object
	for {
		var o kube.Object
		err := dec.Decode(&o)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if len(o) > 0 {
			out = append(out, o)
		}
	}
}

func (m *Managed) apply(ctx context.Context) error {
	objs, err := manifests()
	if err != nil {
		return err
	}
	for _, o := range objs {
		if err := m.API.Apply(ctx, o); err != nil {
			return fmt.Errorf("%v: %w", o["kind"], err)
		}
	}
	return nil
}

// externalMarker names the Secret in can-data that records that this installation provisions on a
// Postgres of the operator's own (CAN_POSTGRES_ADMIN_URL).
const externalMarker = "can-postgres-external"

// ErrWasExternal is returned instead of starting an empty managed server for an installation whose
// databases are on another one: its modules would come up with no data and look newly installed.
var ErrWasExternal = errors.New("this installation uses an external Postgres: set CAN_POSTGRES_ADMIN_URL again")

// MarkExternal records that an external server is in use. It is kept in the cluster, not in
// can-core's own state, so that it survives a redeploy that drops the setting.
func MarkExternal(ctx context.Context, api kube.API) error {
	_, err := api.Create(ctx, kube.Object{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   map[string]any{"name": externalMarker, "namespace": managedNamespace, "labels": map[string]any{"app.kubernetes.io/managed-by": "can-core"}},
		"stringData": map[string]any{"note": "module databases are on the server of CAN_POSTGRES_ADMIN_URL; delete this Secret only to start over on an empty server run by can-core"},
	})
	if err != nil {
		return fmt.Errorf("recording the external postgres in %s: %w", managedNamespace, err)
	}
	return nil
}

// External provisions on the operator's server and records that it does.
type External struct {
	API   kube.API
	Inner Provisioner
}

// Ensure implements Provisioner.
func (e *External) Ensure(ctx context.Context, kind, moduleID, password string) (Credentials, error) {
	if err := MarkExternal(ctx, e.API); err != nil {
		return Credentials{}, err
	}
	return e.Inner.Ensure(ctx, kind, moduleID, password)
}

// refuseIfExternal fails closed on any sign that the databases are elsewhere: the marker, a
// `postgres` Service in can-data that can-core did not make (which must not be overwritten either), or
// a module whose recorded database is on another host. A read that fails is an error too.
func (m *Managed) refuseIfExternal(ctx context.Context, moduleID string) error {
	switch _, err := m.API.Get(ctx, "Secret", managedNamespace, externalMarker); {
	case err == nil:
		return fmt.Errorf("%w (Secret %s/%s records it; delete that Secret only to start over on an empty server)", ErrWasExternal, managedNamespace, externalMarker)
	case !kube.IsNotFound(err):
		return fmt.Errorf("checking for an external postgres: %w", err)
	}
	switch svc, err := m.API.Get(ctx, "Service", managedNamespace, "postgres"); {
	case err == nil:
		md, _ := svc["metadata"].(map[string]any)
		labels, _ := md["labels"].(map[string]any)
		if labels["app.kubernetes.io/managed-by"] != "can-core" {
			return fmt.Errorf("%w (Service %s/postgres exists and was not made by can-core)", ErrWasExternal, managedNamespace)
		}
	case !kube.IsNotFound(err):
		return fmt.Errorf("checking for an existing postgres: %w", err)
	}
	switch o, err := m.API.Get(ctx, "Secret", "mod-"+moduleID, "can-db"); {
	case err == nil:
		d, _ := o["data"].(map[string]any)
		enc, _ := d["DB_HOST"].(string)
		host, derr := base64.StdEncoding.DecodeString(enc)
		if derr != nil {
			return fmt.Errorf("secret mod-%s/can-db has an unreadable DB_HOST", moduleID)
		}
		if len(host) > 0 && string(host) != managedHost {
			return fmt.Errorf("%w (module %s has its database on another host)", ErrWasExternal, moduleID)
		}
	case !kube.IsNotFound(err):
		return fmt.Errorf("checking the module's database Secret: %w", err)
	}
	return nil
}
