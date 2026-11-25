package jobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ErrTooLarge means an upload exceeded the configured size.
var ErrTooLarge = errors.New("file too large")

// ResumeStore keeps resume file bytes. The database holds only metadata (opaque key, name,
// type, size, hash, scan verdict): no file bytes are ever stored in SQLite. The default
// implementation writes to a directory on the module's volume; an object-store
// implementation can satisfy the same interface.
type ResumeStore interface {
	// Put streams r into the store, refusing more than max bytes (ErrTooLarge, nothing left
	// behind). It returns an opaque key, the size and the hex SHA-256.
	Put(ctx context.Context, r io.Reader, max int64) (key string, n int64, sha string, err error)
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete is idempotent.
	Delete(ctx context.Context, key string) error
}

var resumeKeyRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// FSResumeStore stores resumes as 0600 files with random names in one directory.
type FSResumeStore struct{ dir string }

// NewFSResumeStore creates the directory (0700) when needed.
func NewFSResumeStore(dir string) (*FSResumeStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &FSResumeStore{dir: dir}, nil
}

// Put implements ResumeStore.
func (s *FSResumeStore) Put(ctx context.Context, r io.Reader, max int64) (string, int64, string, error) {
	var kb [16]byte
	if _, err := rand.Read(kb[:]); err != nil {
		return "", 0, "", err
	}
	key := hex.EncodeToString(kb[:])
	path := filepath.Join(s.dir, key)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // random name inside the module's own directory
	if err != nil {
		return "", 0, "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), &ctxReader{ctx: ctx, r: io.LimitReader(r, max+1)})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > max {
		err = ErrTooLarge
	}
	if err != nil {
		_ = os.Remove(path)
		return "", 0, "", err
	}
	return key, n, hex.EncodeToString(h.Sum(nil)), nil
}

// Open implements ResumeStore.
func (s *FSResumeStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	if !resumeKeyRE.MatchString(key) {
		return nil, ErrNotFound
	}
	f, err := os.Open(filepath.Join(s.dir, key)) //nolint:gosec // key validated above
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// Delete implements ResumeStore. The file is overwritten with zeros before it is unlinked,
// which helps on plain filesystems and is not a guarantee on copy-on-write or SSD storage;
// see docs/recruiting.md.
func (s *FSResumeStore) Delete(_ context.Context, key string) error {
	if !resumeKeyRE.MatchString(key) {
		return nil
	}
	path := filepath.Join(s.dir, key)
	if f, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil { //nolint:gosec // key validated above
		if st, err := f.Stat(); err == nil {
			_, _ = io.CopyN(f, zeroReader{}, st.Size())
			_ = f.Sync()
		}
		_ = f.Close()
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// ---- type checks -------------------------------------------------------------------------------

var resumeTypes = map[string]string{
	"pdf":  "application/pdf",
	"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"odt":  "application/vnd.oasis.opendocument.text",
	"txt":  "text/plain; charset=utf-8",
	"rtf":  "application/rtf",
}

// ErrResumeType means the file is not an accepted resume.
var ErrResumeType = errors.New("unsupported resume type: use PDF, DOCX, ODT, RTF or plain text")

// SniffResume checks that the extension is an allowed one and that the leading bytes really
// are that kind of file (the declared Content-Type is never trusted). It returns the
// content type to store and serve.
func SniffResume(name string, head []byte) (string, error) {
	i := strings.LastIndexByte(name, '.')
	if i < 1 || i == len(name)-1 {
		return "", ErrResumeType
	}
	ext := strings.ToLower(name[i+1:])
	ct, ok := resumeTypes[ext]
	if !ok {
		return "", ErrResumeType
	}
	okMagic := false
	switch ext {
	case "pdf":
		okMagic = bytes.HasPrefix(head, []byte("%PDF-"))
	case "docx", "odt":
		okMagic = bytes.HasPrefix(head, []byte("PK\x03\x04"))
	case "rtf":
		okMagic = bytes.HasPrefix(head, []byte(`{\rtf`))
	case "txt":
		okMagic = isText(head)
	}
	if !okMagic {
		return "", fmt.Errorf("%w (the file content does not match .%s)", ErrResumeType, ext)
	}
	return ct, nil
}

func isText(b []byte) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	for trim := 0; trim <= 3 && trim <= len(b); trim++ { // the sniff window may cut a rune
		if utf8.Valid(b[:len(b)-trim]) {
			return true
		}
	}
	return false
}

// SafeFilename reduces an uploaded name to a short, single-line base name for display and
// Content-Disposition. It never influences where the file is stored.
func SafeFilename(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == ';' {
			return ' '
		}
		return r
	}, name)
	name = strings.Join(strings.Fields(name), " ")
	if len(name) > 100 {
		ext := ""
		if i := strings.LastIndexByte(name, '.'); i > 0 && len(name)-i <= 8 {
			ext = name[i:]
		}
		stem := name[:len(name)-len(ext)]
		for len(stem) > 96 || !utf8.ValidString(stem) {
			stem = stem[:len(stem)-1]
		}
		name = stem + ext
	}
	if name == "" {
		return "resume"
	}
	return name
}

// ---- virus scanning ---------------------------------------------------------------------------

// ScanVerdict is the outcome of a scan.
type ScanVerdict string

// Scan verdicts. "skipped" means no scanner is configured: the file is kept but labelled so
// recruiters know it was never scanned.
const (
	ScanClean    ScanVerdict = "clean"
	ScanInfected ScanVerdict = "infected"
	ScanSkipped  ScanVerdict = "skipped"
)

// Scanner is the virus-scan hook. An implementation (for example a ClamAV client) is plugged
// in with Options.Scanner; this slice ships only the interface, a no-op and a fake. An
// error means "could not scan" and the upload is refused, never let through unscanned.
type Scanner interface {
	Scan(ctx context.Context, filename string, r io.Reader) (ScanVerdict, error)
}

// NoopScanner is the default: it scans nothing and says so.
type NoopScanner struct{}

// Scan implements Scanner.
func (NoopScanner) Scan(context.Context, string, io.Reader) (ScanVerdict, error) {
	return ScanSkipped, nil
}

// FakeScanner flags content that contains InfectedMarker; for tests.
type FakeScanner struct {
	InfectedMarker string
	Err            error
}

// Scan implements Scanner.
func (f *FakeScanner) Scan(_ context.Context, _ string, r io.Reader) (ScanVerdict, error) {
	if f.Err != nil {
		return "", f.Err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	if f.InfectedMarker != "" && bytes.Contains(b, []byte(f.InfectedMarker)) {
		return ScanInfected, nil
	}
	return ScanClean, nil
}
