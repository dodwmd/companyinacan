package dbprov

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	_ "github.com/lib/pq" // Postgres driver (pure Go)
)

// Postgres provisions databases on one Postgres server through an administrative connection.
type Postgres struct {
	// AdminURL is the connection URL of a role that may CREATE ROLE and CREATE DATABASE.
	AdminURL string
	// Host and Port are what modules use to reach the same server (a Service name in can-data),
	// which usually differs from how can-core reaches it.
	Host string
	Port int
}

var passwordRE = regexp.MustCompile(`^[A-Za-z0-9]{16,128}$`)

// Ensure implements Provisioner.
func (p *Postgres) Ensure(ctx context.Context, kind, moduleID, password string) (Credentials, error) {
	if kind != "postgres" {
		return Credentials{}, fmt.Errorf("%w: %s", ErrUnsupported, kind)
	}
	name, err := Names(moduleID)
	if err != nil {
		return Credentials{}, err
	}
	// The password is spliced into a statement (CREATE ROLE does not take parameters), so only
	// can-core's own generated alphanumeric passwords are accepted.
	if !passwordRE.MatchString(password) {
		return Credentials{}, errors.New("dbprov: password must be 16-128 alphanumeric characters")
	}
	db, err := sql.Open("postgres", p.AdminURL)
	if err != nil {
		return Credentials{}, err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		return Credentials{}, fmt.Errorf("connecting to postgres: %w", err)
	}
	// name matches ^mod_[a-z0-9_]+$ (Names), so it is safe as an identifier.
	var one int
	switch err := db.QueryRowContext(ctx, `SELECT 1 FROM pg_roles WHERE rolname = $1`, name).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		_, err = db.ExecContext(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, name, password)) //nolint:gosec // validated identifier and password
		if err != nil {
			return Credentials{}, fmt.Errorf("creating role: %w", err)
		}
	case err != nil:
		return Credentials{}, err
	default:
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`ALTER ROLE %s WITH LOGIN PASSWORD '%s'`, name, password)); err != nil { //nolint:gosec // as above
			return Credentials{}, fmt.Errorf("setting password: %w", err)
		}
	}
	switch err := db.QueryRowContext(ctx, `SELECT 1 FROM pg_database WHERE datname = $1`, name).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		// Needs the creating role to be able to SET ROLE to the owner on PG16+; CREATEROLE admins are members.
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, name, name)); err != nil { //nolint:gosec // validated identifier
			return Credentials{}, fmt.Errorf("creating database: %w", err)
		}
	case err != nil:
		return Credentials{}, err
	}
	// Other modules' roles must not be able to connect here: PUBLIC has CONNECT by default.
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`REVOKE ALL ON DATABASE %s FROM PUBLIC`, name)); err != nil { //nolint:gosec // validated identifier
		return Credentials{}, fmt.Errorf("revoking public access: %w", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`GRANT ALL ON DATABASE %s TO %s`, name, name)); err != nil { //nolint:gosec // validated identifier
		return Credentials{}, fmt.Errorf("granting access: %w", err)
	}
	port := p.Port
	if port == 0 {
		port = 5432
	}
	return Credentials{Kind: "postgres", Host: p.Host, Port: port, Database: name, User: name, Password: password}, nil
}
