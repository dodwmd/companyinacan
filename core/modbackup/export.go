package modbackup

// `<binary> export` (specs/070-company-export): the module's records as JSON Lines and CSV with
// a schema, and the files they refer to, as one tar stream to can-core's PUT /v1/exports. It
// follows the backup hook: a consistent copy of the database, a Job in the module's namespace,
// bounded memory. Rows are read from the live data, so what was erased is not there.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/dodwmd/companyinacan/core/exportfmt"
)

// exportTimeout is inside the export Job's deadline.
const exportTimeout = 55 * time.Minute

// ModuleExport is the manifest.json that ends a module's part of an export.
type ModuleExport struct {
	Format        int            `json:"format"`
	Module        string         `json:"module"`
	Binary        string         `json:"binary"`
	TakenAt       time.Time      `json:"taken_at"`
	SchemaVersion int            `json:"schema_version,omitempty"`
	IncludeSealed bool           `json:"sealed_fields_decrypted"`
	Tables        map[string]int `json:"tables,omitempty"`
	Files         int            `json:"files"`
	FileBytes     int64          `json:"file_bytes"`
	// Notes say how to read this part; Warnings what is missing from it.
	Notes    []string `json:"notes,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// defaultKeyrings are where a service looks when its config names no keyring.
var defaultKeyrings = map[string]string{
	"can-people": "/etc/can-people/keys/keyring", "can-jobs": "/etc/can-jobs/keyring", "can-files": "/etc/can-files/keyring",
	"can-equity": "/etc/can-equity/keys/keyring", "can-board": "/etc/can-board/keys/keyring",
}

// finance seals bank account numbers with a key from the environment.
const financeKeyEnv = "CAN_FINANCE_SEAL_KEY"

func exportMain(s spec, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(s.binary+" export", flag.ContinueOnError)
	flags.SetOutput(stderr)
	cfgPath := flags.String("config", "", "path to the service config file (default "+s.defaultConfig+" when present)")
	out := flags.String("out", "", "write the tar stream to this new file instead of sending it to can-core")
	include := flags.Bool("include-sealed", false, "decrypt the sealed (encrypted) fields into the export; needs the service's keyring")
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", flags.Arg(0))
		return ExitUsage
	}
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
	ctx, cancel := context.WithTimeout(ctx, exportTimeout)
	defer cancel()
	api := newClient(getenv)
	api.export = true
	if *out == "" {
		if err := api.check(); err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return ExitUsage
		}
	}
	e := exporter{s: s, l: l, module: getenv("CAN_MODULE"), include: *include, getenv: getenv, log: stderr}
	err = e.run(ctx, api, *out)
	switch {
	case err == nil:
		fmt.Fprintln(stdout, "export ok")
		return ExitOK
	case errors.Is(err, ErrNoData):
		fmt.Fprintf(stderr, "export: %v (%s)\n", err, l.db)
		return ExitNoData
	default:
		fmt.Fprintf(stderr, "export: %v\n", err)
		return ExitFail
	}
}

type exporter struct {
	s       spec
	l       layout
	module  string
	include bool
	getenv  func(string) string
	log     io.Writer
}

// run takes a consistent copy of the database and writes the export of it.
func (e exporter) run(ctx context.Context, api *client, out string) error {
	tables, ok := exportTables[e.s.binary]
	if !ok {
		return fmt.Errorf("%s has no export", e.s.binary)
	}
	if fi, err := os.Stat(e.l.db); errors.Is(err, os.ErrNotExist) {
		return ErrNoData
	} else if err != nil {
		return err
	} else if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", e.l.db)
	}
	var unseal exportfmt.Opener
	if e.include {
		var err error
		if unseal, err = e.opener(tables); err != nil {
			return err
		}
	}
	// The copy holds what the database holds: sealed columns stay sealed on disk. Decrypted
	// values only ever exist in memory and in the stream.
	tmp, err := os.MkdirTemp("", ".export-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	copyPath := filepath.Join(tmp, "copy.db")
	taken := time.Now().UTC()
	if err := snapshot(ctx, e.l.db, copyPath); err != nil {
		return err
	}
	if err := integrity(ctx, copyPath); err != nil {
		return fmt.Errorf("the copy of the database: %w", err)
	}
	version, err := schemaVersion(ctx, copyPath)
	if err != nil {
		return fmt.Errorf("refusing to export %s: %w", e.l.db, err)
	}
	db, err := open(copyPath, "mode=ro&immutable=1")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	write := func(w io.Writer) error {
		a := exportfmt.NewArchive(w, taken)
		m := ModuleExport{Format: exportfmt.FormatVersion, Module: e.module, Binary: e.s.binary, TakenAt: taken, SchemaVersion: version, IncludeSealed: unseal != nil, Tables: map[string]int{}}
		conn, err := db.Conn(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		schema, warnings, err := exportfmt.Database(ctx, a, conn, tables, exportfmt.Options{Open: unseal})
		if err != nil {
			return err
		}
		m.Warnings = warnings
		for _, t := range schema.Tables {
			m.Tables[t.Name] = t.Rows
		}
		if err := e.files(ctx, a, conn, &m); err != nil {
			return err
		}
		if err := a.JSON(exportfmt.ManifestName, m); err != nil {
			return err
		}
		return a.Close()
	}

	if out != "" {
		f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // operator-chosen output path
		if err != nil {
			return err
		}
		err = write(f)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(out)
		}
		return err
	}
	_, err = api.putStream(ctx, "export.tar", write)
	return err
}

var extRe = regexp.MustCompile(`^[a-z0-9]{1,10}$`)

// extension returns ".ext" for the extension of name (or for name itself when it has no dot),
// "" when it is not a plain short one.
func extension(name string) string {
	ext := strings.ToLower(name)
	if i := strings.LastIndexByte(ext, '.'); i >= 0 {
		ext = ext[i+1:]
	} else if len(ext) > 5 {
		return ""
	}
	if !extRe.MatchString(ext) {
		return ""
	}
	return "." + ext
}

type exportFile struct {
	key, sha, path, ext string
	size                int64
}

// files copies the files the database refers to from the volume into the archive.
func (e exporter) files(ctx context.Context, a *exportfmt.Archive, conn *sql.Conn, m *ModuleExport) error {
	query, ok := exportFiles[e.s.binary]
	if !ok || e.s.blobs == nil {
		return nil
	}
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("list the files: %w", err)
	}
	var list []exportFile
	for rows.Next() {
		var f exportFile
		var name sql.NullString
		if err := rows.Scan(&f.key, &f.size, &f.sha, &f.path, &name); err != nil {
			_ = rows.Close()
			return err
		}
		f.ext = extension(name.String)
		list = append(list, f)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(list) == 0 {
		return nil
	}
	if e.l.backend != "local" {
		m.Warnings = append(m.Warnings, fmt.Sprintf("%d file(s) are stored in the %s bucket and are NOT in this export: copy the bucket yourself", len(list), e.l.backend))
		return nil
	}
	missing, changed := 0, 0
	for _, f := range list {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !blobKeyRe.MatchString(f.key) {
			return errors.New("the database holds a file key this version does not understand")
		}
		p := e.s.blobs.path(e.l.blobDir, f.key)
		fi, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) || (err == nil && !fi.Mode().IsRegular()) {
			missing++
			continue
		} else if err != nil {
			return err
		}
		in, err := os.Open(p) //nolint:gosec // a validated key under the blob directory
		if errors.Is(err, os.ErrNotExist) {
			missing++
			continue
		} else if err != nil {
			return err
		}
		name := f.path + f.ext
		sum, err := a.Copy(name, fi.Size(), in)
		_ = in.Close()
		if err != nil {
			return err
		}
		if f.sha != "" && sum != f.sha {
			changed++
		}
		m.Files++
		m.FileBytes += fi.Size()
	}
	if missing > 0 {
		m.Warnings = append(m.Warnings, fmt.Sprintf("%d file(s) the records refer to are not on the volume (purged while the export ran, or lost): they are NOT in this export", missing))
	}
	if changed > 0 {
		m.Warnings = append(m.Warnings, fmt.Sprintf("%d file(s) do not match their recorded SHA-256", changed))
	}
	return nil
}

// opener loads the key that opens the service's sealed columns. A service without any needs none.
func (e exporter) opener(tables []exportfmt.Table) (exportfmt.Opener, error) {
	has := false
	for _, t := range tables {
		has = has || len(t.Sealed) > 0
	}
	if !has {
		return nil, nil
	}
	if e.s.binary == "can-finance" {
		hexKey := e.getenv(financeKeyEnv)
		if hexKey == "" {
			// Without the key no bank account number was ever sealed; one that is there fails.
			return func(string, string) (string, error) { return "", errors.New(financeKeyEnv + " is not set") }, nil
		}
		return financeOpener(hexKey)
	}
	path := or(e.l.keyring, defaultKeyrings[e.s.binary])
	b, err := os.ReadFile(path) //nolint:gosec // the keyring named by the service config
	if err != nil {
		return nil, fmt.Errorf("--include-sealed needs the keyring: %w", err)
	}
	return keyringOpener(string(b))
}

var errUnseal = errors.New("cannot decrypt field")

func openXChaCha(key []byte, body, aad string) (string, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return "", errUnseal
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(raw) < aead.NonceSize()+aead.Overhead() {
		return "", errUnseal
	}
	pt, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(aad))
	if err != nil {
		return "", errUnseal
	}
	return string(pt), nil
}

// keyringOpener reads the keyring format of people, jobs, files, equity and board ("<id> <hex>"
// per line) and opens their "v1.<id>.<base64url>" values.
func keyringOpener(ring string) (exportfmt.Opener, error) {
	keys := map[string][]byte{}
	for _, line := range strings.Split(ring, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 {
			return nil, errors.New("keyring: each line must be `<key-id> <hex key>`")
		}
		k, err := hex.DecodeString(f[1])
		if err != nil || len(k) != chacha20poly1305.KeySize {
			return nil, fmt.Errorf("keyring: key %q must be %d bytes of hex", f[0], chacha20poly1305.KeySize)
		}
		keys[f[0]] = k
	}
	if len(keys) == 0 {
		return nil, errors.New("keyring: no keys")
	}
	return func(sealed, aad string) (string, error) {
		parts := strings.Split(sealed, ".")
		if len(parts) != 3 || parts[0] != "v1" || keys[parts[1]] == nil {
			return "", errUnseal
		}
		return openXChaCha(keys[parts[1]], parts[2], aad)
	}, nil
}

// financeOpener opens can-finance's "v1.<base64url>" values.
func financeOpener(hexKey string) (exportfmt.Opener, error) {
	k, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil || len(k) != chacha20poly1305.KeySize {
		return nil, errors.New(financeKeyEnv + ": the seal key must be 64 hex characters")
	}
	return func(sealed, aad string) (string, error) {
		body, ok := strings.CutPrefix(sealed, "v1.")
		if !ok {
			return "", errUnseal
		}
		return openXChaCha(k, body, aad)
	}, nil
}
