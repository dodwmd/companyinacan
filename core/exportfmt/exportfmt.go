// Package exportfmt writes the open formats of `can export` (specs/071-company-export): a tar
// stream of JSON Lines and CSV tables with a machine-readable schema, written without holding a
// table or a file in memory and without staging plaintext on disk.
package exportfmt

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"
	"unicode"
)

// FormatVersion is the version of the archive layout.
const FormatVersion = 1

// ManifestName is the last entry of an archive and of a module's part of it: without it the
// export is incomplete.
const ManifestName = "manifest.json"

// maxNameLen bounds an entry name.
const maxNameLen = 255

// ValidName reports whether name is acceptable as an entry of an archive: relative, clean, with
// no element that could leave the directory it is unpacked into.
func ValidName(name string) bool {
	if len(name) == 0 || len(name) > maxNameLen || !fs.ValidPath(name) || name == "." {
		return false
	}
	for _, r := range name {
		if r == '\\' || r == ':' || r == unicode.ReplacementChar || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Archive writes entries into a tar stream.
type Archive struct {
	tw   *tar.Writer
	seen map[string]bool
	// Time is the modification time of every entry.
	Time time.Time
}

// NewArchive writes to w. Close finishes the tar stream, not w.
func NewArchive(w io.Writer, now time.Time) *Archive {
	return &Archive{tw: tar.NewWriter(w), seen: map[string]bool{}, Time: now.UTC().Truncate(time.Second)}
}

// Close writes the end of the tar stream. A failed export must not call it: a missing end and a
// missing manifest are how a reader sees that the archive is incomplete.
func (a *Archive) Close() error { return a.tw.Close() }

func (a *Archive) header(name string, size int64) error {
	if !ValidName(name) {
		return fmt.Errorf("export: unacceptable entry name %q", name)
	}
	if a.seen[name] {
		return fmt.Errorf("export: entry %q written twice", name)
	}
	a.seen[name] = true
	return a.tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: size, Mode: 0o600, ModTime: a.Time, Format: tar.FormatPAX})
}

// Bytes adds a small entry.
func (a *Archive) Bytes(name string, b []byte) error {
	if err := a.header(name, int64(len(b))); err != nil {
		return err
	}
	_, err := a.tw.Write(b)
	return err
}

// JSON adds v, indented.
func (a *Archive) JSON(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return a.Bytes(name, append(b, '\n'))
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// Render adds an entry of unknown size without storing it: render is called twice, first to
// count the bytes and then to write them, so it must produce the same bytes both times (read
// from one database snapshot or transaction, in a fixed order).
func (a *Archive) Render(name string, render func(io.Writer) error) error {
	var c countWriter
	if err := render(&c); err != nil {
		return err
	}
	if err := a.header(name, c.n); err != nil {
		return err
	}
	if err := render(a.tw); err != nil {
		if errors.Is(err, tar.ErrWriteTooLong) {
			return fmt.Errorf("export: %s changed while it was written", name)
		}
		return err
	}
	// A shorter second pass is caught by the next header or by Close.
	if err := a.tw.Flush(); err != nil {
		return fmt.Errorf("export: %s changed while it was written: %w", name, err)
	}
	return nil
}

// Copy adds size bytes of r and returns their SHA-256. It fails if r holds fewer: an entry with
// made-up content is worse than no archive.
func (a *Archive) Copy(name string, size int64, r io.Reader) (string, error) {
	if size < 0 {
		return "", fmt.Errorf("export: %s has no size", name)
	}
	if err := a.header(name, size); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(a.tw, h), r, size); err != nil {
		return "", fmt.Errorf("export: %s: %w", name, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// CSVSafe defuses spreadsheet formula injection (OWASP "CSV injection"): a text cell that begins
// with = + - @, a tab or a line break (after optional spaces) would be run as a formula by Excel,
// LibreOffice or Sheets, so it is prefixed with an apostrophe. NUL bytes are dropped. The JSON
// Lines copy of the same table holds the exact value.
func CSVSafe(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	t := strings.TrimLeftFunc(s, func(r rune) bool { return r == ' ' || r == 0xFEFF })
	if t == "" {
		return s
	}
	switch []rune(t)[0] {
	// The full-width forms are read as operators by some spreadsheet locales.
	case '=', '+', '-', '@', '\t', '\r', '\n', '＝', '＋', '－', '＠':
		return "'" + s
	}
	return s
}
