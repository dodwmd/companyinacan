package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LocalRepository reports whether repo is a restic repository in a local directory (no backend
// prefix): used to restore from a copy of the bucket, and by the end-to-end test.
func LocalRepository(repo string) bool { return filepath.IsAbs(repo) }

// FileStore is an ObjectStore in a directory: the ledger's place next to a local repository,
// mirroring the bucket layout (<dir>/restic and <dir>/ledger/...).
type FileStore struct{ Dir string }

// FileStoreFor returns the store that goes with the local repository repo.
func FileStoreFor(repo string) FileStore { return FileStore{Dir: filepath.Dir(filepath.Clean(repo))} }

func (s FileStore) path(key string) (string, error) {
	if key == "" || strings.Contains(key, "..") || filepath.IsAbs(key) {
		return "", fmt.Errorf("bad object key %q", key)
	}
	return filepath.Join(s.Dir, filepath.FromSlash(key)), nil
}

// Put implements ObjectStore.
func (s FileStore) Put(_ context.Context, key string, data []byte) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	return writeFileSync(p, data, 0o600)
}

// Get implements ObjectStore.
func (s FileStore) Get(_ context.Context, key string) ([]byte, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	// A missing directory is a wrong repository path, not "no ledger".
	if _, err := os.Stat(s.Dir); err != nil {
		return nil, fmt.Errorf("ledger store %s: %w", s.Dir, errors.Unwrap(err))
	}
	b, err := os.ReadFile(p) //nolint:gosec // operator-chosen directory
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}
