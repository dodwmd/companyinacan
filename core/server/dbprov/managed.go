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
	p := &Postgres{AdminURL: adminURL, Host: managedHost, Port: managedPort}
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
