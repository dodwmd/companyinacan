// Package restoreops is can-core's side of a restore (specs/042-restore): the restore-mode latch
// and state import done at start-up, and the operations `can restore` runs inside the pod.
package restoreops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // checking the databases

	"github.com/dodwmd/companyinacan/core/backup"
)

// Latch files beside the state database. Restore mode is latched on disk, not read from the
// environment, so that nothing short of `can-core restore complete` ends it: re-applying the
// normal manifests half way through a restore leaves can-core in restore mode.
const (
	pendingFile  = "restore.pending"  // restore mode, holding the restore id
	importedFile = "restore.imported" // the state dump of that id was imported
	completeFile = "restore.complete" // that restore finished
)

func readID(p string) (string, error) {
	b, err := os.ReadFile(p) //nolint:gosec // beside can-core's database
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(b)), err
}

func writeID(p, id string) error {
	f, err := os.OpenFile(p+".tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // beside can-core's database
	if err != nil {
		return err
	}
	_, err = f.WriteString(id + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// Pending returns the id of the restore in progress, "" when can-core is not in restore mode.
func Pending(dbPath string) (string, error) {
	return readID(filepath.Join(filepath.Dir(dbPath), pendingFile))
}

// Boot is the start-up part: it latches restore mode when requested (id, from CAN_RESTORE) and,
// in restore mode, imports the state dump once. It returns whether can-core is in restore mode.
// A database that already holds an admin or modules is replaced only when overwrite is set (the
// previous one is kept as <db>.pre-restore).
func Boot(ctx context.Context, dbPath, dumpsDir, id string, overwrite bool) (bool, error) {
	dir := filepath.Dir(dbPath)
	pending, err := Pending(dbPath)
	if err != nil {
		return false, err
	}
	if id != "" && id != pending {
		done, err := readID(filepath.Join(dir, completeFile))
		if err != nil {
			return false, err
		}
		if done != id {
			if err := writeID(filepath.Join(dir, pendingFile), id); err != nil {
				return false, fmt.Errorf("latch restore mode: %w", err)
			}
			pending = id
		}
	}
	if pending == "" {
		return false, nil
	}
	imported, err := readID(filepath.Join(dir, importedFile))
	if err != nil {
		return false, err
	}
	if imported == pending {
		return true, nil
	}
	if dumpsDir == "" {
		return false, errors.New("restore mode needs CAN_DUMPS_DIR")
	}
	if err := importState(ctx, filepath.Join(dumpsDir, filepath.FromSlash(backup.StateDump)), dbPath, overwrite); err != nil {
		return false, fmt.Errorf("import the state dump: %w", err)
	}
	return true, writeID(filepath.Join(dir, importedFile), pending)
}

func query(ctx context.Context, path, q string) (string, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	var v string
	err = db.QueryRowContext(ctx, q).Scan(&v)
	return v, err
}

func importState(ctx context.Context, src, dst string, overwrite bool) error {
	res, err := query(ctx, src, `PRAGMA integrity_check`)
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	if res != "ok" {
		return fmt.Errorf("%s: integrity check failed: %s", src, res)
	}
	if _, err := os.Stat(dst); err == nil {
		n, err := query(ctx, dst, `SELECT (SELECT COUNT(*) FROM admin_users) + (SELECT COUNT(*) FROM modules)`)
		if err != nil || n != "0" {
			if !overwrite {
				return fmt.Errorf("%s already holds data; refusing to replace it without a confirmed overwrite", dst)
			}
			if err := os.Rename(dst, dst+".pre-restore"); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	in, err := os.Open(src) //nolint:gosec // our dumps directory
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp := dst + ".import"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // can-core's database
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	// A write-ahead log of the replaced database must not be applied to the imported one.
	for _, ext := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(dst + ext); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.Rename(tmp, dst)
}

// finish ends restore mode for the next start.
func finish(dbPath string) error {
	dir := filepath.Dir(dbPath)
	id, err := Pending(dbPath)
	if err != nil || id == "" {
		return err
	}
	if err := writeID(filepath.Join(dir, completeFile), id); err != nil {
		return err
	}
	for _, f := range []string{importedFile, pendingFile} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
