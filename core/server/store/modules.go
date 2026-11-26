package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
)

var errNoRows = sql.ErrNoRows

// Module states.
const (
	StateEnabled  = "enabled"
	StateDisabled = "disabled"
	StateFailed   = "failed"
)

// Module is the recorded (installed) state of one module.
type Module struct {
	ID             string
	Version        string
	Namespace      string
	ServiceAccount string
	State          string
	SettingsHash   string
	HMACSecret     string
	Manifest       config.Module
	LastError      string
	UpdatedAt      time.Time
}

// Provider is one module's provision of a capability.
type Provider struct {
	Capability string
	ModuleID   string
	Service    string
	Port       int
	Schema     string
	Namespace  string // the module's namespace (joined from the module record; ignored by UpsertModule)
}

// Subscription is a module's event webhook.
type Subscription struct {
	ModuleID string
	Event    string
	Service  string
	Port     int
	Webhook  string
}

// Subscriber is a Subscription of an enabled module with its signing secret.
type Subscriber struct {
	Subscription
	Namespace  string
	HMACSecret string
}

// UpsertModule records the module and replaces its provider and subscription rows atomically.
func (db *DB) UpsertModule(ctx context.Context, m Module, providers []Provider, subs []Subscription) error {
	mj, err := json.Marshal(m.Manifest)
	if err != nil {
		return err
	}
	return db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO modules(id,version,namespace,service_account,state,settings_hash,hmac_secret,manifest_json,last_error,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET version=excluded.version, namespace=excluded.namespace, service_account=excluded.service_account,
			  state=excluded.state, settings_hash=excluded.settings_hash, hmac_secret=excluded.hmac_secret, manifest_json=excluded.manifest_json,
			  last_error=excluded.last_error, updated_at=excluded.updated_at`,
			m.ID, m.Version, m.Namespace, m.ServiceAccount, m.State, m.SettingsHash, m.HMACSecret, string(mj), m.LastError, db.Now().Unix()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM capability_providers WHERE module_id=?`, m.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM subscriptions WHERE module_id=?`, m.ID); err != nil {
			return err
		}
		for _, p := range providers {
			if _, err := tx.ExecContext(ctx, `INSERT INTO capability_providers(capability,module_id,service,port,schema) VALUES (?,?,?,?,?)`,
				p.Capability, m.ID, p.Service, p.Port, p.Schema); err != nil {
				return err
			}
		}
		for _, s := range subs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO subscriptions(module_id,event,service,port,webhook) VALUES (?,?,?,?,?)`,
				m.ID, s.Event, s.Service, s.Port, s.Webhook); err != nil {
				return err
			}
		}
		return nil
	})
}

const moduleCols = `id,version,namespace,service_account,state,settings_hash,hmac_secret,manifest_json,last_error,updated_at`

func scanModule(row interface{ Scan(...any) error }) (Module, error) {
	var m Module
	var mj string
	var up int64
	if err := row.Scan(&m.ID, &m.Version, &m.Namespace, &m.ServiceAccount, &m.State, &m.SettingsHash, &m.HMACSecret, &mj, &m.LastError, &up); err != nil {
		return m, notFound(err)
	}
	m.UpdatedAt = time.Unix(up, 0)
	if mj != "" {
		if err := json.Unmarshal([]byte(mj), &m.Manifest); err != nil {
			return m, err
		}
	}
	return m, nil
}

// GetModule returns one module record.
func (db *DB) GetModule(ctx context.Context, id string) (Module, error) {
	return scanModule(db.QueryRowContext(ctx, `SELECT `+moduleCols+` FROM modules WHERE id=?`, id))
}

// ModuleByIdentity finds the module bound to a ServiceAccount.
func (db *DB) ModuleByIdentity(ctx context.Context, namespace, serviceAccount string) (Module, error) {
	return scanModule(db.QueryRowContext(ctx, `SELECT `+moduleCols+` FROM modules WHERE namespace=? AND service_account=?`, namespace, serviceAccount))
}

// ListModules returns all recorded modules ordered by id.
func (db *DB) ListModules(ctx context.Context) ([]Module, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+moduleCols+` FROM modules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Module
	for rows.Next() {
		m, err := scanModule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetModuleState changes a module's state and last error.
func (db *DB) SetModuleState(ctx context.Context, id, state, lastErr string) error {
	res, err := db.ExecContext(ctx, `UPDATE modules SET state=?, last_error=?, updated_at=? WHERE id=?`, state, lastErr, db.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ForgetSettingsHash empties a module's recorded settings hash, which no desired state matches, so
// the next plan upgrades the module again (used after a failed upgrade).
func (db *DB) ForgetSettingsHash(ctx context.Context, id string) error {
	res, err := db.ExecContext(ctx, `UPDATE modules SET settings_hash='', updated_at=? WHERE id=?`, db.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteDisabledModule removes the record of a module that is disabled (its providers and
// subscriptions are removed with it). Any other state is refused, so a running module's identity is
// never dropped; used by the explicit purge flow.
func (db *DB) DeleteDisabledModule(ctx context.Context, id string) error {
	res, err := db.ExecContext(ctx, `DELETE FROM modules WHERE id=? AND state='disabled'`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Providers returns the enabled modules that provide a capability, ordered by module id.
func (db *DB) Providers(ctx context.Context, capability string) ([]Provider, error) {
	rows, err := db.QueryContext(ctx, `SELECT p.capability,p.module_id,p.service,p.port,p.schema,m.namespace FROM capability_providers p
		JOIN modules m ON m.id=p.module_id WHERE p.capability=? AND m.state='enabled' ORDER BY p.module_id`, capability)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Provider
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.Capability, &p.ModuleID, &p.Service, &p.Port, &p.Schema, &p.Namespace); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AllProviders returns every provision by an enabled module.
func (db *DB) AllProviders(ctx context.Context) ([]Provider, error) {
	rows, err := db.QueryContext(ctx, `SELECT p.capability,p.module_id,p.service,p.port,p.schema,m.namespace FROM capability_providers p
		JOIN modules m ON m.id=p.module_id WHERE m.state='enabled' ORDER BY p.capability, p.module_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Provider
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.Capability, &p.ModuleID, &p.Service, &p.Port, &p.Schema, &p.Namespace); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetPrimaries replaces the operator's primary-provider choices (company.yaml `primary:`).
func (db *DB) SetPrimaries(ctx context.Context, primaries map[string]string) error {
	return db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM primary_providers`); err != nil {
			return err
		}
		for c, m := range primaries {
			if _, err := tx.ExecContext(ctx, `INSERT INTO primary_providers(capability,module_id) VALUES (?,?)`, c, m); err != nil {
				return err
			}
		}
		return nil
	})
}

// Primary returns the designated primary module for a capability ("" when none).
func (db *DB) Primary(ctx context.Context, capability string) (string, error) {
	var id string
	err := db.QueryRowContext(ctx, `SELECT module_id FROM primary_providers WHERE capability=?`, capability).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

// Subscribers returns enabled modules subscribed to an event type.
func (db *DB) Subscribers(ctx context.Context, event string) ([]Subscriber, error) {
	rows, err := db.QueryContext(ctx, `SELECT s.module_id,s.event,s.service,s.port,s.webhook,m.namespace,m.hmac_secret FROM subscriptions s
		JOIN modules m ON m.id=s.module_id WHERE s.event=? AND m.state='enabled' ORDER BY s.module_id`, event)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Subscriber
	for rows.Next() {
		var s Subscriber
		if err := rows.Scan(&s.ModuleID, &s.Event, &s.Service, &s.Port, &s.Webhook, &s.Namespace, &s.HMACSecret); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
