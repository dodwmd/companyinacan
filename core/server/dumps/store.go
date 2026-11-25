// Package dumps is can-core's side of the backup (epic E08b, specs/037-backup-dumps): the dump
// store on the `can-dumps` volume, the dump API modules' backup hooks PUT to, can-core's own
// state dump, and the completion marker the host backup script checks before it runs restic.
//
// The directory this package writes is read by a root-owned script on the host, so nothing here
// ever creates a link, every file is created 0600 and renamed into place, and names are
// restricted to a small alphabet.
package dumps

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/dodwmd/companyinacan/core/backup"
)

// COMPLETE is the completion marker's file name inside the dumps directory.
const COMPLETE = backup.MarkerName

// DefaultMaxBytes caps one uploaded dump (a hook archive); larger dumps are refused, not truncated.
const DefaultMaxBytes int64 = 4 << 30

// Errors.
var (
	ErrBadName  = errors.New("dump: module or name is not acceptable")
	ErrTooLarge = errors.New("dump: body exceeds the size limit")
	ErrNoRun    = errors.New("dump: no dump run is in progress")
	ErrBusy     = errors.New("dump: a dump run is already in progress")
)

var (
	moduleRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	nameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// Store is the dumps directory: <Dir>/modules/<module>/<name> for module dumps, <Dir>/can-core
// for can-core's own, and <Dir>/COMPLETE.
type Store struct {
	Dir      string
	MaxBytes int64 // 0 = DefaultMaxBytes
}

func (s *Store) limit() int64 {
	if s.MaxBytes > 0 {
		return s.MaxBytes
	}
	return DefaultMaxBytes
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

// Put stores r as the dump <module>/<name>, replacing it atomically.
func (s *Store) Put(module, name string, r io.Reader) error {
	if err := validName(module, name); err != nil {
		return err
	}
	dir := filepath.Join(s.Dir, "modules", module)
	if err := plainDir(filepath.Join(s.Dir, "modules")); err != nil {
		return err
	}
	if err := plainDir(dir); err != nil {
		return err
	}
	return s.writeAtomic(dir, name, r)
}

// writeAtomic writes r to dir/name through a 0600 temp file in the same directory.
func (s *Store) writeAtomic(dir, name string, r io.Reader) (err error) {
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(0o600); err != nil {
		return err
	}
	n, err := io.Copy(tmp, io.LimitReader(r, s.limit()+1))
	if err != nil {
		return err
	}
	if n > s.limit() {
		return ErrTooLarge
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	dst := filepath.Join(dir, name)
	if fi, lerr := os.Lstat(dst); lerr == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("dump: %s exists and is not a regular file", dst)
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// Open opens the dump <module>/<name> for reading.
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

// count returns the number of regular dump files (not the marker).
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
