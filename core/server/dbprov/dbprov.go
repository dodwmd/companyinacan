// Package dbprov creates one database and one user per module in the shared database servers in
// can-data (SPEC §4.5) and hands the credentials back for injection into the module's Secret.
package dbprov

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
)

// Credentials are what a module needs to reach its database.
type Credentials struct {
	Kind     string // postgres or mariadb
	Host     string
	Port     int
	Database string
	User     string
	Password string
}

// URL returns a connection URL for the credentials. Kind-specific, no driver needed.
func (c Credentials) URL() string {
	switch c.Kind {
	case "mariadb":
		return fmt.Sprintf("mysql://%s:%s@%s:%d/%s", c.User, c.Password, c.Host, c.Port, c.Database)
	default:
		return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", c.User, c.Password, c.Host, c.Port, c.Database)
	}
}

// Provisioner ensures a module's database and user exist and that the user's password is password.
// It must be idempotent: running it again changes nothing but re-asserts the password, and it never
// drops anything.
type Provisioner interface {
	Ensure(ctx context.Context, kind, moduleID, password string) (Credentials, error)
}

// ErrUnsupported is returned for a database kind the provisioner is not configured for.
var ErrUnsupported = errors.New("database kind is not configured")

var idRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// Names derives the database and user name of a module (the same for both). Module ids contain
// dashes, which are not valid in unquoted identifiers, so they become underscores; the "mod_" prefix
// keeps module names apart from system databases. Returns an error for an invalid id.
func Names(moduleID string) (string, error) {
	if !idRE.MatchString(moduleID) {
		return "", fmt.Errorf("invalid module id %q", moduleID)
	}
	out := []byte("mod_")
	for i := 0; i < len(moduleID); i++ {
		c := moduleID[i]
		if c == '-' {
			c = '_'
		}
		out = append(out, c)
	}
	return string(out), nil
}

// Fake records what it was asked to do.
type Fake struct {
	mu        sync.Mutex
	Host      string
	Databases map[string]string // "<kind>/<name>" -> password
	Calls     int
	Fail      error
}

// NewFake returns an empty fake.
func NewFake() *Fake {
	return &Fake{Host: "postgres.can-data.svc.cluster.local", Databases: map[string]string{}}
}

// Ensure implements Provisioner.
func (f *Fake) Ensure(_ context.Context, kind, moduleID, password string) (Credentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	if f.Fail != nil {
		return Credentials{}, f.Fail
	}
	n, err := Names(moduleID)
	if err != nil {
		return Credentials{}, err
	}
	f.Databases[kind+"/"+n] = password
	port := 5432
	if kind == "mariadb" {
		port = 3306
	}
	return Credentials{Kind: kind, Host: f.Host, Port: port, Database: n, User: n, Password: password}, nil
}

// Unconfigured fails every request with a clear message: can-core runs with it until database
// admin credentials are provided.
type Unconfigured struct{}

// Ensure implements Provisioner.
func (Unconfigured) Ensure(_ context.Context, kind, _ string, _ string) (Credentials, error) {
	return Credentials{}, fmt.Errorf("%w: %s (set CAN_POSTGRES_ADMIN_URL)", ErrUnsupported, kind)
}
