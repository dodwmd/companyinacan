package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite" // reading the dumped state database

	"github.com/dodwmd/companyinacan/core/config"
)

// Files can-core writes into the dumps directory, relative to it.
const (
	StateDump   = "can-core/state.db"
	SecretsDump = "can-core/secrets.json"
	CompanyDump = "company.yaml"
)

// SecretsExport is the Kubernetes Secrets can-core exports into the dumps directory
// (SPEC.md section 6.5), so that a restore re-creates the module keys that sealed data depends on.
// It holds secret values: it is written 0600 and leaves the server only inside the encrypted
// restic repository.
type SecretsExport struct {
	Version int              `json:"version"`
	Secrets []ExportedSecret `json:"secrets"`
}

// ExportedSecret is one Secret. Data values are base64, as the Kubernetes API returns them.
type ExportedSecret struct {
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Type      string            `json:"type,omitempty"`
	Data      map[string]string `json:"data"`
}

// ParseSecretsExport reads the content of SecretsDump.
func ParseSecretsExport(b []byte) (*SecretsExport, error) {
	var e SecretsExport
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, errors.New("the Secrets export is not readable JSON") // never echo its content
	}
	if e.Version != 1 {
		return nil, fmt.Errorf("the Secrets export has version %d (want 1)", e.Version)
	}
	return &e, nil
}

// RecordedModule is one module record of the dumped state database.
type RecordedModule struct {
	ID       string
	State    string
	Manifest config.Module
}

// Contents is what a set of dumps holds, and what a restore of it could not do.
type Contents struct {
	Modules []RecordedModule
	Dumped  []string // module ids with a dump in the set
	Admins  int
	// HasSecrets is false for sets made before can-core exported Secrets.
	HasSecrets bool
	// NoRestoreHook are modules with a dump but no way to load it. A dump set of the in-house
	// subcommands (specs/043-module-backup-hooks: manifest.json and database.sqlite3.gz) is loaded
	// by the module's own binary, whether or not its record names the hook yet.
	NoRestoreHook []string
	// NotBackedUp are database-server modules whose database is not in the set.
	NotBackedUp []string
}

// Problems lists, one line each, what a restore of these dumps would not bring back.
func (c *Contents) Problems() []string {
	var out []string
	if !c.HasSecrets {
		out = append(out, "no Secrets export (the snapshot was made by an older release): module keys are lost, sealed data would be unreadable (restore needs --without-secrets)")
	}
	for _, m := range c.NoRestoreHook {
		out = append(out, fmt.Sprintf("module %q has a dump but no restore hook: its dump cannot be loaded (restore needs --accept-unrestored %s)", m, m))
	}
	for _, m := range c.NotBackedUp {
		out = append(out, fmt.Sprintf("module %q: DATABASE IS NOT BACKED UP: the set holds no dump of it (restore needs --accept-unrestored %s)", m, m))
	}
	return out
}

// DumpFiles are the files a check needs from a set besides the list of names: restore them (and
// nothing else) from a snapshot to run CheckContents.
var DumpFiles = []string{StateDump, SecretsDump, CompanyDump, DumpedList}

// CheckContents checks that dir holds what a restore needs. files are the names of every file of
// the set relative to dir (the backup manifest's), so dir itself only needs DumpFiles. An error
// means the set cannot be restored at all; Contents.Problems lists what would be incomplete.
func CheckContents(ctx context.Context, dir string, files []string) (*Contents, error) {
	have := map[string]bool{}
	withDump := map[string]bool{}
	for _, f := range files {
		f = path.Clean(f)
		have[f] = true
		if p := strings.Split(f, "/"); len(p) >= 3 && p[0] == "modules" {
			withDump[p[1]] = true
		}
	}
	for _, f := range []string{StateDump, CompanyDump} {
		if !have[f] {
			return nil, fmt.Errorf("the dumps lack %s: they cannot be restored", f)
		}
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
			return nil, fmt.Errorf("the dumps lack %s: %w", f, err)
		}
	}
	c := &Contents{}
	if err := c.readState(ctx, filepath.Join(dir, filepath.FromSlash(StateDump))); err != nil {
		return nil, fmt.Errorf("%s: %w", StateDump, err)
	}
	exported := map[string]bool{}
	if have[SecretsDump] {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(SecretsDump))) //nolint:gosec // our restore directory
		if err != nil {
			return nil, err
		}
		e, err := ParseSecretsExport(b)
		if err != nil {
			return nil, err
		}
		c.HasSecrets = true
		for _, s := range e.Secrets {
			if s.Data["DB_HOST"] != "" { // a database that was provisioned
				exported[s.Namespace] = true
			}
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, DumpedList)) //nolint:gosec // our restore directory
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	dumpedNow := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			dumpedNow[f[0]] = true
			if !withDump[f[0]] {
				return nil, fmt.Errorf("%s says module %q was dumped but the set holds none of its files", DumpedList, f[0])
			}
		}
	}
	for id := range withDump {
		c.Dumped = append(c.Dumped, id)
	}
	sort.Strings(c.Dumped)
	for _, m := range c.Modules {
		db := m.Manifest.Requires.Database
		server := db == "postgres" || db == "mariadb"
		switch {
		case withDump[m.ID] && m.Manifest.Hooks.Restore == "" && !subcommandSet(have, m.ID):
			c.NoRestoreHook = append(c.NoRestoreHook, m.ID)
		case !withDump[m.ID] && server && exported["mod-"+m.ID]:
			// A database that was provisioned (its Secret names a server) but has no dump.
			c.NotBackedUp = append(c.NotBackedUp, m.ID)
		}
	}
	return c, nil
}

func (c *Contents) readState(ctx context.Context, p string) error {
	db, err := sql.Open("sqlite", "file:"+p+"?mode=ro&immutable=1")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	var res string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return fmt.Errorf("integrity check failed: %s", res)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&c.Admins); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT id, state, manifest_json FROM modules ORDER BY id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var m RecordedModule
		var mj string
		if err := rows.Scan(&m.ID, &m.State, &mj); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(mj), &m.Manifest); err != nil {
			return fmt.Errorf("module %s: %w", m.ID, err)
		}
		c.Modules = append(c.Modules, m)
	}
	return rows.Err()
}

// subcommandSet reports whether the files hold a dump set of the in-house backup subcommands.
func subcommandSet(have map[string]bool, id string) bool {
	return have["modules/"+id+"/manifest.json"] && have["modules/"+id+"/database.sqlite3.gz"]
}
