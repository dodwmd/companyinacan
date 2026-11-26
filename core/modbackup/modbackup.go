// Package modbackup is the `backup` and `restore` subcommands of the in-house module binaries
// (specs/043-module-backup-hooks). `backup` takes a consistent copy of the module's SQLite database
// (VACUUM INTO over a read-only connection of its own, then integrity_check), lists the files on the
// volume that the copy refers to, and uploads the set through can-core's PUT /v1/dumps. `restore`
// puts such a set back into an empty or stopped instance.
//
// It needs no secret: sealed columns are copied as they are, and the keys that open them are not
// part of the dump (contracts/hooks.md says what a restore needs).
package modbackup

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"
)

// Exit codes of the subcommands.
const (
	ExitOK    = 0
	ExitFail  = 1
	ExitUsage = 2
	// ExitNoData means there is no database to back up. can-core treats it like a module without a
	// volume: fine for a module that never had a dump, reported when it had one.
	ExitNoData = 3
)

// File names of a dump set.
const (
	ManifestName = "manifest.json"
	DatabaseName = "database.sqlite3.gz"
	BlobsName    = "blobs.tsv.gz"
)

// ErrNoData is returned when the database file does not exist.
var ErrNoData = errors.New("there is no database: nothing to back up")

// blobSpec describes the files a database refers to.
type blobSpec struct {
	// defaultDir is the directory when the config names none.
	defaultDir string
	// sharded selects the can-files layout dir/ab/cd/<key>; else dir/<key>.
	sharded bool
	// query lists (key, size, sha256) of every file the database says exists.
	query string
}

// spec is what differs between the services.
type spec struct {
	binary        string
	configEnv     string
	defaultConfig string
	defaultDB     string
	blobs         *blobSpec
}

var specs = map[string]spec{
	"can-forms":     {configEnv: "CAN_FORMS_CONFIG", defaultConfig: "/etc/can-forms/forms.yaml", defaultDB: "/data/forms.db"},
	"can-work":      {configEnv: "CAN_WORK_CONFIG", defaultConfig: "/etc/can-work/work.yaml", defaultDB: "/data/work.db"},
	"can-people":    {configEnv: "CAN_PEOPLE_CONFIG", defaultConfig: "/etc/can-people/people.yaml", defaultDB: "/data/people.db"},
	"can-finance":   {configEnv: "CAN_FINANCE_CONFIG", defaultConfig: "/etc/can-finance/finance.yaml", defaultDB: "/data/finance.db"},
	"can-equity":    {configEnv: "CAN_EQUITY_CONFIG", defaultConfig: "/etc/can-equity/equity.yaml", defaultDB: "/data/equity.db"},
	"can-board":     {configEnv: "CAN_BOARD_CONFIG", defaultConfig: "/etc/can-board/board.yaml", defaultDB: "/data/board.db"},
	"can-provision": {configEnv: "CAN_PROVISIONING_CONFIG", defaultConfig: "/etc/can-provisioning/provisioning.yaml", defaultDB: "/data/provision.db"},
	"can-jobs": {configEnv: "CAN_JOBS_CONFIG", defaultConfig: "/etc/can-jobs/jobs.yaml", defaultDB: "/data/jobs.db",
		blobs: &blobSpec{defaultDir: "/data/resumes",
			query: `SELECT resume_key, resume_size, resume_sha256 FROM applications WHERE resume_key != '' ORDER BY resume_key`}},
	"can-files": {configEnv: "CAN_FILES_CONFIG", defaultConfig: "/etc/can-files/files.yaml", defaultDB: "/data/files.db",
		blobs: &blobSpec{defaultDir: "/data/blobs", sharded: true,
			// A file being purged may already have lost its bytes; the restored service finishes it.
			query: `SELECT v.blob_key, v.size, v.sha256 FROM versions v JOIN files f ON f.id = v.file_id
WHERE v.purged_at IS NULL AND v.blob_key != '' AND f.status IN ('active','deleted') ORDER BY v.blob_key`}},
}

var blobKeyRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (b *blobSpec) path(dir, key string) string {
	if b.sharded {
		return filepath.Join(dir, key[:2], key[2:4], key)
	}
	return filepath.Join(dir, key)
}

// layout is where one instance keeps its data, read from its config file.
type layout struct {
	db      string
	blobDir string
	backend string // "local", "s3" or "" (no blobs)
}

// loadLayout reads only the keys that say where data is. The service's own strict loader is not
// used: it also checks settings (forms, keys) a backup has no business with. A missing file means
// the built-in defaults, unless the path was given explicitly.
func (s spec) loadLayout(path string, must bool) (layout, error) {
	var c struct {
		DBPath    string `yaml:"db_path"`
		ResumeDir string `yaml:"resume_dir"`
		Backend   struct {
			Type  string `yaml:"type"`
			Local struct {
				Dir string `yaml:"dir"`
			} `yaml:"local"`
		} `yaml:"backend"`
	}
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
	switch {
	case errors.Is(err, os.ErrNotExist) && !must:
	case err != nil:
		return layout{}, err
	default:
		if err := yaml.Unmarshal(b, &c); err != nil {
			return layout{}, fmt.Errorf("config %s: %w", path, err)
		}
	}
	l := layout{db: or(c.DBPath, s.defaultDB)}
	if s.blobs != nil {
		l.backend = or(c.Backend.Type, "local")
		l.blobDir = or(c.ResumeDir, c.Backend.Local.Dir, s.blobs.defaultDir)
	}
	return l, nil
}

func or(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// Handles reports whether args name one of the subcommands of this package.
func Handles(args []string) bool {
	return len(args) > 0 && (args[0] == "backup" || args[0] == "restore")
}

// Main runs `<binary> backup|restore` and returns the exit code.
func Main(binary string, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	s, ok := specs[binary]
	if !ok || !Handles(args) {
		fmt.Fprintf(stderr, "%s: no such backup command\n", binary)
		return ExitUsage
	}
	s.binary = binary
	cmd := args[0]
	fs := flag.NewFlagSet(binary+" "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to the service config file (default "+s.defaultConfig+" when present)")
	var out, from, blobs *string
	var replace, allowMissing *bool
	if cmd == "backup" {
		out = fs.String("out", "", "write the dump set into this directory instead of uploading it to can-core")
	} else {
		from = fs.String("from", "", "directory holding the dump set (default: download it from can-core, which allows that only in restore mode)")
		blobs = fs.String("blobs", "", "copy referenced files that are missing or wrong from this directory (a restored volume's blob or resume directory)")
		replace = fs.Bool("replace", false, "replace an existing database; the service must be stopped")
		allowMissing = fs.Bool("allow-missing-blobs", false, "restore even if referenced files cannot be found")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return ExitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", fs.Arg(0))
		return ExitUsage
	}
	// A named file must exist. The hook Job's mounted ConfigMap (CAN_BACKUP_CONFIG) and the default
	// path may be absent, which means the built-in paths.
	path, must := *cfgPath, true
	if path == "" {
		path = getenv(s.configEnv)
	}
	if path == "" {
		path, must = or(getenv("CAN_BACKUP_CONFIG"), s.defaultConfig), false
	}
	l, err := s.loadLayout(path, must)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return ExitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 14*time.Minute) // inside the hook Job's 15
	defer cancel()
	api := newClient(getenv)
	if cmd == "backup" {
		if *out == "" {
			if err := api.check(); err != nil {
				fmt.Fprintf(stderr, "%v\n", err)
				return ExitUsage
			}
		}
		err = runBackup(ctx, s, l, getenv("CAN_MODULE"), api, *out, stdout)
	} else {
		if *from == "" {
			if err := api.check(); err != nil {
				fmt.Fprintf(stderr, "restore needs -from DIR, or can-core: %v\n", err)
				return ExitUsage
			}
		}
		err = runRestore(ctx, s, l, getenv("CAN_MODULE"), api, restoreOptions{from: *from, blobs: *blobs, replace: *replace, allowMissing: *allowMissing}, stdout)
	}
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, ErrNoData):
		fmt.Fprintf(stderr, "%s: %v (%s)\n", cmd, err, l.db)
		return ExitNoData
	default:
		fmt.Fprintf(stderr, "%s: %v\n", cmd, err)
		return ExitFail
	}
}
