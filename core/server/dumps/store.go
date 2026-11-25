// Package dumps is can-core's side of the backup (epic E08b, specs/037-backup-dumps): the dump
// store on the `can-dumps` volume, the dump API modules' backup hooks PUT to, can-core's own
// state dump, and the completion marker the host backup script checks before it runs restic.
//
// The directory this package writes is read by a root-owned script on the host, so nothing here
// ever creates a link, every file is created 0600 and renamed into place, and names are
// restricted to a small alphabet. Temporary and staging entries start with a dot, which the
// backup script and restic leave out.
package dumps

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dodwmd/companyinacan/core/backup"
)

// COMPLETE is the completion marker's file name inside the dumps directory.
const COMPLETE = backup.MarkerName

// Limits.
const (
	// DefaultMaxBytes caps one uploaded dump; larger dumps are refused, not truncated.
	DefaultMaxBytes int64 = 4 << 30
	// DefaultMaxFiles caps the files one module may upload in a run.
	DefaultMaxFiles = 16
	// DefaultMaxModuleBytes caps the bytes received from one module in a run, retries included.
	DefaultMaxModuleBytes int64 = 8 << 30
)

// Errors.
var (
	ErrBadName  = errors.New("dump: module or name is not acceptable")
	ErrTooLarge = errors.New("dump: size limit exceeded")
	ErrTooMany  = errors.New("dump: too many files")
	ErrNoRun    = errors.New("dump: no backup hook of this module is running")
	ErrBusy     = errors.New("dump: a dump run is already in progress")
)

var (
	moduleRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	nameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// Store is the dumps directory: <Dir>/modules/<module>/<name> for module dumps, <Dir>/can-core
// for can-core's own, and <Dir>/COMPLETE.
type Store struct {
	Dir            string
	MaxBytes       int64 // per file; 0 = DefaultMaxBytes
	MaxFiles       int   // per module and run; 0 = DefaultMaxFiles
	MaxModuleBytes int64 // per module and run; 0 = DefaultMaxModuleBytes
}

func (s *Store) limit() int64 {
	if s.MaxBytes > 0 {
		return s.MaxBytes
	}
	return DefaultMaxBytes
}

func (s *Store) maxFiles() int {
	if s.MaxFiles > 0 {
		return s.MaxFiles
	}
	return DefaultMaxFiles
}

func (s *Store) maxModuleBytes() int64 {
	if s.MaxModuleBytes > 0 {
		return s.MaxModuleBytes
	}
	return DefaultMaxModuleBytes
}

func validName(module, name string) error {
	if !moduleRe.MatchString(module) || !nameRe.MatchString(name) || name == COMPLETE {
		return ErrBadName
	}
	return nil
}

// plainDir creates dir (0700) if missing and refuses anything that is not a real directory.
func plainDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("dump: %s is not a plain directory", dir)
	}
	return nil
}

// writeTemp streams at most max bytes of r into a new 0600 file in dir and syncs it. On any
// error nothing is left behind.
func writeTemp(dir string, r io.Reader, max int64) (name string, err error) {
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(0o600); err != nil {
		return "", err
	}
	n, err := io.Copy(tmp, io.LimitReader(r, max+1))
	if err != nil {
		return "", err
	}
	if n > max {
		return "", ErrTooLarge
	}
	if err = tmp.Sync(); err != nil {
		return "", err
	}
	if err = tmp.Close(); err != nil {
		return "", err
	}
	return tmp.Name(), nil
}

// replace renames tmp over dst, refusing to replace anything but a regular file.
func replace(tmp, dst string) error {
	if fi, err := os.Lstat(dst); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("dump: %s exists and is not a regular file", dst)
	}
	return os.Rename(tmp, dst)
}

// writeAtomic writes r to dir/name through a temporary file in the same directory.
func (s *Store) writeAtomic(dir, name string, r io.Reader) error {
	tmp, err := writeTemp(dir, r, s.limit())
	if err != nil {
		return err
	}
	if err := replace(tmp, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Open opens the published dump <module>/<name> for reading.
func (s *Store) Open(module, name string) (*os.File, error) {
	if err := validName(module, name); err != nil {
		return nil, err
	}
	p := filepath.Join(s.Dir, "modules", module, name)
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("dump: %s is not a regular file: %w", p, os.ErrNotExist)
	}
	return os.Open(p) //nolint:gosec // validated name under the dumps dir
}

// clean removes what earlier runs left behind: staging directories, temporaries, and the dumps
// of modules that are not in keep (disabled long ago, or purged).
func (s *Store) clean(keep []string) error {
	wanted := map[string]bool{}
	for _, m := range keep {
		wanted[m] = true
	}
	var errs []error
	_ = filepath.WalkDir(s.Dir, func(p string, e os.DirEntry, err error) error {
		if err != nil || p == s.Dir {
			return nil
		}
		stale := strings.HasPrefix(e.Name(), ".") ||
			(e.IsDir() && filepath.Dir(p) == filepath.Join(s.Dir, "modules") && !wanted[e.Name()])
		if !stale {
			return nil
		}
		if rerr := os.RemoveAll(p); rerr != nil {
			errs = append(errs, rerr)
		}
		if e.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	return errors.Join(errs...)
}

// count returns the number of published dump files (not the marker).
func (s *Store) count() int {
	n := 0
	_ = filepath.WalkDir(s.Dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && d.Name() != COMPLETE && d.Name()[0] != '.' {
			n++
		}
		return nil
	})
	return n
}
