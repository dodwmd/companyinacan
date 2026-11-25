package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// Backend keeps file bytes. The database holds only metadata: no file content is ever stored in
// SQLite. Keys are random 128-bit hex strings made by the service, so nothing a user sends can
// reach a path, and the same content uploaded twice is stored twice (research.md R3).
type Backend interface {
	// Put stores exactly size bytes from r under key and verifies that they hash to sha (hex
	// SHA-256). A mismatch or a short body leaves nothing stored.
	Put(ctx context.Context, key string, r io.Reader, size int64, sha string) error
	// Open returns the object, or ErrNotFound.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete is idempotent and ignores keys that are not ours.
	Delete(ctx context.Context, key string) error
	// Ping checks that the backend answers (readiness).
	Ping(ctx context.Context) error
}

var blobKeyRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// shard spreads keys over directories by their first bytes: ab/cd/abcd....
func shard(key string) string { return key[:2] + "/" + key[2:4] + "/" + key }

// LocalBackend stores blobs as 0600 files under a 0700 directory, sharded by key prefix. A write
// goes to a temporary file, is fsynced and renamed into place, so a crash never leaves a
// partial blob at a real key.
type LocalBackend struct{ dir string }

// NewLocalBackend creates the directory (0700) when needed. A symlink is refused: the blob
// directory is on the module's own volume and must be the real thing.
func NewLocalBackend(dir string) (*LocalBackend, error) {
	if st, err := os.Lstat(dir); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("blob directory %s must not be a symlink", dir)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".tmp"), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Join(dir, ".tmp"), 0o700); err != nil {
		return nil, err
	}
	return &LocalBackend{dir: dir}, nil
}

// SweepTemp removes temporary files a crash left behind (a rename never happened, so no database
// row names them). Only the serving process calls it, at start: a maintenance command running next
// to it must not remove a write in flight.
func (b *LocalBackend) SweepTemp() {
	ents, _ := os.ReadDir(filepath.Join(b.dir, ".tmp"))
	for _, e := range ents {
		_ = os.Remove(filepath.Join(b.dir, ".tmp", e.Name()))
	}
}

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

// Put implements Backend.
func (b *LocalBackend) Put(ctx context.Context, key string, r io.Reader, size int64, sha string) error {
	if !blobKeyRe.MatchString(key) {
		return errors.New("invalid blob key")
	}
	tmp, err := os.CreateTemp(filepath.Join(b.dir, ".tmp"), "put-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), &ctxReader{ctx: ctx, r: io.LimitReader(r, size+1)})
	if err != nil {
		return err
	}
	if n != size || hex.EncodeToString(h.Sum(nil)) != sha {
		return ErrIntegrity
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	final := filepath.Join(b.dir, filepath.FromSlash(shard(key)))
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return err
	}
	if err := os.Rename(tmpName, final); err != nil {
		return err
	}
	done = true
	if d, err := os.Open(filepath.Dir(final)); err == nil { //nolint:gosec // path built from a validated key
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Open implements Backend.
func (b *LocalBackend) Open(_ context.Context, key string) (io.ReadCloser, error) {
	if !blobKeyRe.MatchString(key) {
		return nil, ErrNotFound
	}
	f, err := os.Open(filepath.Join(b.dir, filepath.FromSlash(shard(key)))) //nolint:gosec // key validated above
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// Delete implements Backend. The file is overwritten with zeros before it is unlinked, which
// helps on plain filesystems and is not a guarantee on copy-on-write or SSD storage (docs/files.md).
func (b *LocalBackend) Delete(_ context.Context, key string) error {
	if !blobKeyRe.MatchString(key) {
		return nil
	}
	path := filepath.Join(b.dir, filepath.FromSlash(shard(key)))
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

// Ping implements Backend: the directory must exist and accept a file.
func (b *LocalBackend) Ping(context.Context) error {
	f, err := os.CreateTemp(filepath.Join(b.dir, ".tmp"), "ping-*")
	if err != nil {
		return errors.New("blob directory is not writable")
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
