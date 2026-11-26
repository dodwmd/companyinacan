package modbackup

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type restoreOptions struct {
	from         string // directory with the dump set; "" downloads it
	blobs        string // directory to copy missing files from
	replace      bool
	allowMissing bool
}

// maxManifest and maxList bound what is read into memory.
const (
	maxManifest = 64 << 10
	maxList     = 256 << 20
)

// runRestore puts a dump set into the instance at l. Everything is verified before the database is
// installed; running it again is safe (with -replace once a database is there). The service must
// not be running.
func runRestore(ctx context.Context, s spec, l layout, module string, api *client, o restoreOptions, stdout io.Writer) error {
	dir := o.from
	if dir == "" {
		tmp, err := os.MkdirTemp("", ".restore-*")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		dir = tmp
		if err := api.get(ctx, ManifestName, filepath.Join(dir, ManifestName)); err != nil {
			return fmt.Errorf("download %s: %w", ManifestName, err)
		}
	}
	m, err := readManifest(filepath.Join(dir, ManifestName))
	if err != nil {
		return err
	}
	if m.Binary != s.binary {
		return fmt.Errorf("the dump was made by %s, not %s", m.Binary, s.binary)
	}
	if module != "" && m.Module != "" && m.Module != module {
		return fmt.Errorf("the dump is of module %q, not %q", m.Module, module)
	}
	if (m.Blobs != nil) != (s.blobs != nil) {
		return errors.New("the dump's list of files does not match this service")
	}
	if o.from == "" {
		names := []string{m.Database.Name}
		if m.Blobs != nil {
			names = append(names, m.Blobs.Name)
		}
		for _, n := range names {
			if err := api.get(ctx, n, filepath.Join(dir, n)); err != nil {
				return fmt.Errorf("download %s: %w", n, err)
			}
		}
	}
	if _, err := os.Lstat(l.db); err == nil && !o.replace {
		return fmt.Errorf("%s already exists: stop the service and pass -replace to overwrite it", l.db)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.db), 0o700); err != nil {
		return err
	}
	if m.SchemaVersion > s.latest {
		return newerSchema(m.SchemaVersion, s)
	}
	// Held until the new database is in place: nothing may be using the old one.
	release, err := lockDatabase(ctx, l.db)
	if err != nil {
		return err
	}
	defer release()

	// Unpacked next to the target, so the final rename is atomic.
	staged := l.db + ".restoring"
	_ = os.Remove(staged)
	defer func() { _ = os.Remove(staged) }()
	if err := unpack(filepath.Join(dir, DatabaseName), staged, m.Database); err != nil {
		return err
	}
	if err := integrity(ctx, staged); err != nil {
		return fmt.Errorf("the restored database: %w", err)
	}
	if v, err := schemaVersion(ctx, staged); err != nil {
		return fmt.Errorf("the dumped database: %w", err)
	} else if v > s.latest {
		return newerSchema(v, s)
	}

	missing, copied, err := restoreBlobs(ctx, s, l, m, dir, o)
	if err != nil {
		return err
	}
	guidance := ""
	if len(missing) > 0 {
		refs, err := listBlobs(ctx, staged, s.blobs.query)
		if err != nil {
			return fmt.Errorf("list the files the database refers to: %w", err)
		}
		guidance = s.blobs.missingFiles(missing, records(refs))
		if !o.allowMissing {
			return fmt.Errorf("%d of %d referenced file(s) are missing or damaged: restore the volume first or name it with -blobs; "+
				"-allow-missing-blobs continues without them (only right for files destroyed after the dump was taken). First: %s", len(missing), m.Blobs.Count, guidance)
		}
	}

	// A journal left over from the old database must not be applied to the new one.
	for _, ext := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(l.db + ext); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(staged, l.db); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(l.db)); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "restore ok: database %d bytes, dump of %s", m.Database.Size, m.TakenAt.Format("2006-01-02T15:04:05Z07:00"))
	if m.Blobs != nil && m.Blobs.Backend == "local" {
		fmt.Fprintf(stdout, ", %d file(s) verified (%d copied, %d missing)", m.Blobs.Count-len(missing), copied, len(missing))
	} else if m.Blobs != nil {
		fmt.Fprintf(stdout, ", %d file(s) are in the %s bucket and were NOT checked", m.Blobs.Count, m.Blobs.Backend)
	}
	fmt.Fprintln(stdout)
	if guidance != "" {
		fmt.Fprintf(stdout, "WARNING: the restored database still refers to %d missing file(s), and every backup of this module FAILS until each record is purged or its file is back. First: %s\n", len(missing), guidance)
	}
	return nil
}

func newerSchema(v int, s spec) error {
	return fmt.Errorf("the dump has schema version %d, this %s knows up to %d: restore with the module version that made the dump, or a newer one", v, s.binary, s.latest)
}

func readManifest(path string) (*Manifest, error) {
	f, err := os.Open(path) //nolint:gosec // operator-named dump directory
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(f, maxManifest)).Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m.Format != 1 || m.Database.Name != DatabaseName || (m.Blobs != nil && (m.Blobs.Name != BlobsName || m.Blobs.Size > maxList)) {
		return nil, fmt.Errorf("%s: not a dump manifest this version understands", path)
	}
	return &m, nil
}

// unpack gunzips src into dst and checks it against what the manifest records.
func unpack(src, dst string, want Part) error {
	in, err := os.Open(src) //nolint:gosec // operator-named dump directory
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	zr, err := gzip.NewReader(in)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(src), err)
	}
	return writeVerified(dst, zr, want.Size, want.SHA256)
}

// writeVerified streams exactly size bytes hashing to sha into dst (0600, synced), atomically.
func writeVerified(dst string, r io.Reader, size int64, sha string) error {
	f, err := os.CreateTemp(filepath.Dir(dst), ".restore-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, size+1))
	if err != nil {
		return err
	}
	if n != size || hex.EncodeToString(h.Sum(nil)) != sha {
		return fmt.Errorf("%s: size or checksum does not match the dump", filepath.Base(dst))
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// matches reports whether path is a regular file with the recorded content.
func matches(path string, b blob) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != b.size {
		return false
	}
	f, err := os.Open(path) //nolint:gosec // path built from a validated key
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == b.sha
}

// restoreBlobs verifies every file the database refers to, copying it from o.blobs when needed.
func restoreBlobs(ctx context.Context, s spec, l layout, m *Manifest, dir string, o restoreOptions) (missing []string, copied int, err error) {
	if m.Blobs == nil || m.Blobs.Backend != "local" {
		return nil, 0, nil
	}
	if l.backend != "local" {
		return nil, 0, fmt.Errorf("the dump uses the local file backend, this instance is configured for %q", l.backend)
	}
	list, err := readList(filepath.Join(dir, BlobsName), m.Blobs.Part)
	if err != nil {
		return nil, 0, err
	}
	if len(list) != m.Blobs.Count {
		return nil, 0, errors.New("the list of files does not match the manifest")
	}
	if err := os.MkdirAll(l.blobDir, 0o700); err != nil {
		return nil, 0, err
	}
	for _, b := range list {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		dst := s.blobs.path(l.blobDir, b.key)
		if matches(dst, b) {
			continue
		}
		var src string
		if o.blobs != "" {
			src = s.blobs.path(o.blobs, b.key)
		}
		if src == "" || !matches(src, b) {
			missing = append(missing, b.key)
			continue
		}
		in, err := os.Open(src) //nolint:gosec // path built from a validated key
		if err != nil {
			return nil, 0, err
		}
		err = os.MkdirAll(filepath.Dir(dst), 0o700)
		if err == nil {
			err = writeVerified(dst, in, b.size, b.sha)
		}
		_ = in.Close()
		if err != nil {
			return nil, 0, err
		}
		copied++
	}
	return missing, copied, nil
}

func readList(path string, want Part) ([]blob, error) {
	in, err := os.Open(path) //nolint:gosec // operator-named dump directory
	if err != nil {
		return nil, err
	}
	defer func() { _ = in.Close() }()
	zr, err := gzip.NewReader(in)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", BlobsName, err)
	}
	raw, err := io.ReadAll(io.LimitReader(zr, want.Size+1))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	if int64(len(raw)) != want.Size || hex.EncodeToString(sum[:]) != want.SHA256 {
		return nil, fmt.Errorf("%s: size or checksum does not match the dump", BlobsName)
	}
	var out []blob
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 3 || !blobKeyRe.MatchString(f[0]) {
			return nil, fmt.Errorf("%s: malformed line", BlobsName)
		}
		n, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%s: malformed size", BlobsName)
		}
		out = append(out, blob{key: f[0], size: n, sha: f[2]})
	}
	return out, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // the data directory
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}
