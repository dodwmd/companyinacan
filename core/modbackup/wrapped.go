package modbackup

// The dump set of a wrapped module (specs/046-wrapped-backups): its SQLite database, the
// pg_dump of its database in the shared Postgres, and an archive of its volume. These run as
// `can-core module-data backup|restore` in a Job in the module's namespace, so no image beyond
// the ones already on the node is needed. pg_dump and pg_restore themselves run in the Postgres
// image, in a second container of the same pod, over a shared work directory.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// File names of a wrapped module's dump set (besides ManifestName and DatabaseName).
const (
	PGDumpName = "database.pgdump" // pg_dump --format=custom, already compressed
	FilesName  = "files.tar.gz"
	// WrappedBinary is the manifest's binary: it tells these sets from the in-house ones.
	WrappedBinary = "can-core"
)

// Scratch entries of a file restore, at the top of the volume; never archived.
const (
	stageDir = ".can-restore-new"
	oldDir   = ".can-restore-old"
)

// wrappedManifest describes a set. Unlike Manifest, every size and hash is of the file as it was
// uploaded (compressed), so that it is computed while streaming.
type wrappedManifest struct {
	Format   int       `json:"format"` // 1
	Binary   string    `json:"binary"`
	Module   string    `json:"module"`
	TakenAt  time.Time `json:"taken_at"`
	Database *Part     `json:"database,omitempty"` // the SQLite copy
	Postgres *Part     `json:"postgres,omitempty"`
	Files    *Part     `json:"files,omitempty"`
}

// Wrapped says what a wrapped module's data is.
type Wrapped struct {
	DataDir string // where the module's volume is mounted
	SQLite  string // database file relative to DataDir, "" for none
	Files   bool   // archive the whole volume
	PGDump  string // the pg_dump file: read by Backup, written by Restore; "" for none
	WorkDir string // scratch space on disk ("" = the system's)
}

func (w Wrapped) check() error {
	if w.SQLite != "" && (w.SQLite != filepath.Base(w.SQLite) || strings.HasPrefix(w.SQLite, ".")) {
		return fmt.Errorf("%q is not a file name", w.SQLite)
	}
	if w.SQLite != "" && w.Files {
		return errors.New("a volume is either a SQLite database or archived files, not both")
	}
	if (w.SQLite != "" || w.Files) && w.DataDir == "" {
		return errors.New("no data directory")
	}
	if w.SQLite == "" && !w.Files && w.PGDump == "" {
		return errors.New("nothing to back up")
	}
	return nil
}

// BackupWrapped uploads the set of w. The pg_dump file, if any, was written before it started, so
// the files are always archived after the database was dumped: every file a dumped row refers to
// is in the archive unless the app deleted it in between.
func BackupWrapped(ctx context.Context, w Wrapped, getenv func(string) string, stdout io.Writer) error {
	if err := w.check(); err != nil {
		return err
	}
	api := newClient(getenv)
	if err := api.check(); err != nil {
		return err
	}
	return backupWrapped(ctx, w, api, stdout)
}

func backupWrapped(ctx context.Context, w Wrapped, api *client, stdout io.Writer) error {
	tmp, err := os.MkdirTemp(w.WorkDir, ".dump-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	m := wrappedManifest{Format: 1, Binary: WrappedBinary, Module: api.module, TakenAt: time.Now().UTC()}
	put := func(name, file string) (*Part, error) {
		part, err := partOf(name, file)
		if err != nil {
			return nil, err
		}
		if err := api.put(ctx, name, file); err != nil {
			return nil, fmt.Errorf("upload %s: %w", name, err)
		}
		return &part, nil
	}

	if w.PGDump != "" {
		if err := checkPGDump(w.PGDump); err != nil {
			return err
		}
		if m.Postgres, err = put(PGDumpName, w.PGDump); err != nil {
			return err
		}
	}
	if w.SQLite != "" {
		src := filepath.Join(w.DataDir, w.SQLite)
		if fi, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			// An empty volume was never used. A volume with other things in it but without the
			// database is a mistake (a wrong file name), which must not pass as "nothing to do".
			names, rerr := topEntries(w.DataDir)
			if rerr != nil {
				return rerr
			}
			if len(names) == 0 {
				return ErrNoData
			}
			return fmt.Errorf("%s is missing although the volume holds %s", src, strings.Join(names, ", "))
		} else if err != nil {
			return err
		} else if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", src)
		}
		copyPath := filepath.Join(tmp, "copy.db")
		if err := snapshot(ctx, src, copyPath); err != nil {
			return err
		}
		if err := integrity(ctx, copyPath); err != nil {
			return fmt.Errorf("the copy of the database: %w", err)
		}
		gz := filepath.Join(tmp, DatabaseName)
		if _, err := compress(copyPath, gz, DatabaseName); err != nil {
			return err
		}
		if err := os.Remove(copyPath); err != nil {
			return err
		}
		if m.Database, err = put(DatabaseName, gz); err != nil {
			return err
		}
	}
	if w.Files {
		part, err := api.putStream(ctx, FilesName, func(out io.Writer) error { return archive(ctx, w.DataDir, out, stdout) })
		if err != nil {
			return fmt.Errorf("upload %s: %w", FilesName, err)
		}
		m.Files = &part
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	file := filepath.Join(tmp, ManifestName)
	if err := os.WriteFile(file, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := api.put(ctx, ManifestName, file); err != nil { // last: a set with a manifest is complete
		return fmt.Errorf("upload %s: %w", ManifestName, err)
	}
	for _, p := range []*Part{m.Postgres, m.Database, m.Files} {
		if p != nil {
			fmt.Fprintf(stdout, "backup ok: %s %d bytes\n", p.Name, p.Size)
		}
	}
	return nil
}

func checkPGDump(file string) error {
	f, err := os.Open(file) //nolint:gosec // the work directory
	if err != nil {
		return fmt.Errorf("the database dump: %w", err)
	}
	defer func() { _ = f.Close() }()
	magic := make([]byte, 5)
	if _, err := io.ReadFull(f, magic); err != nil || string(magic) != "PGDMP" {
		return fmt.Errorf("%s is not a pg_dump archive", file)
	}
	return nil
}

func partOf(name, file string) (Part, error) {
	f, err := os.Open(file) //nolint:gosec // our temporary file
	if err != nil {
		return Part{}, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return Part{}, err
	}
	return Part{Name: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// counter counts and hashes what goes through it.
type counter struct {
	w io.Writer
	h hash.Hash
	n int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.h.Write(p[:n])
	c.n += int64(n)
	return n, err
}

// putStream uploads what write produces, without holding it anywhere. If write fails, the request
// is broken off and can-core keeps nothing of it. A retry calls write again.
func (c *client) putStream(ctx context.Context, name string, write func(io.Writer) error) (Part, error) {
	var part Part
	var done chan error
	var pr *io.PipeReader
	// settle ends the writer of the previous attempt.
	settle := func() error {
		if done == nil {
			return nil
		}
		_ = pr.Close()
		err := <-done
		done = nil
		return err
	}
	defer func() { _ = settle() }()
	resp, err := c.do(ctx, name, func(target string) (*http.Request, error) {
		_ = settle()
		r, pw := io.Pipe()
		ch := make(chan error, 1)
		pr, done = r, ch
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, r)
		if err != nil {
			done = nil
			return nil, err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		go func() {
			cw := &counter{w: pw, h: sha256.New()}
			err := write(cw)
			part = Part{Name: name, Size: cw.n, SHA256: hex.EncodeToString(cw.h.Sum(nil))}
			_ = pw.CloseWithError(err) // nil closes normally: the end of the body
			ch <- err
		}()
		return req, nil
	})
	if err != nil {
		if werr := settle(); werr != nil && !errors.Is(werr, io.ErrClosedPipe) {
			return Part{}, werr
		}
		return Part{}, err
	}
	_ = resp.Body.Close()
	werr := <-done
	done = nil
	if werr != nil {
		return Part{}, werr
	}
	return part, nil
}

// topEntries lists a volume's top level without the restore's scratch entries and lost+found.
func topEntries(dir string) ([]string, error) {
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range es {
		switch e.Name() {
		case stageDir, oldDir, "lost+found":
		default:
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// archive writes a gzipped tar of dir: directories, regular files and symbolic links. Nothing is
// followed out of dir. Sockets and other special files are left out. A file that cannot be read
// fails the archive; one that is deleted while the walk runs is left out.
func archive(ctx context.Context, dir string, out io.Writer, log io.Writer) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	zw, err := gzip.NewWriterLevel(out, gzip.BestSpeed)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(zw)
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p != "." {
				return nil
			}
			return err
		}
		if p == "." {
			return nil
		}
		if p == stageDir || p == oldDir || p == "lost+found" {
			return fs.SkipDir
		}
		fi, err := root.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		h := &tar.Header{Name: p, Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime(), Format: tar.FormatPAX}
		switch {
		case fi.IsDir():
			h.Typeflag, h.Name = tar.TypeDir, p+"/"
			return tw.WriteHeader(h)
		case fi.Mode()&fs.ModeSymlink != 0:
			h.Typeflag = tar.TypeSymlink
			if h.Linkname, err = root.Readlink(p); errors.Is(err, fs.ErrNotExist) {
				return nil
			} else if err != nil {
				return err
			}
			return tw.WriteHeader(h)
		case !fi.Mode().IsRegular():
			return nil
		}
		f, err := root.Open(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		h.Typeflag, h.Size = tar.TypeReg, fi.Size()
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		n, err := io.Copy(tw, io.LimitReader(f, h.Size))
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if n < h.Size { // truncated while being read (a log): the entry keeps its declared size
			fmt.Fprintf(log, "warning: %s shrank while it was archived\n", p)
			_, err = io.CopyN(tw, zeros{}, h.Size-n)
		}
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// RestoreWrapped downloads the module's set (allowed only in restore mode), verifies every part
// before changing anything, then replaces the SQLite database or the volume's content and leaves
// the pg_dump file at w.PGDump for pg_restore. The module must not be running. Running it again
// is safe.
func RestoreWrapped(ctx context.Context, w Wrapped, getenv func(string) string, stdout io.Writer) error {
	if err := w.check(); err != nil {
		return err
	}
	api := newClient(getenv)
	if err := api.check(); err != nil {
		return err
	}
	return restoreWrapped(ctx, w, api, stdout)
}

func restoreWrapped(ctx context.Context, w Wrapped, api *client, stdout io.Writer) error {
	tmp, err := os.MkdirTemp(w.WorkDir, ".restore-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	mf := filepath.Join(tmp, ManifestName)
	if err := api.get(ctx, ManifestName, mf); err != nil {
		return fmt.Errorf("download %s: %w", ManifestName, err)
	}
	b, err := os.ReadFile(mf) //nolint:gosec // our temporary file
	if err != nil {
		return err
	}
	var m wrappedManifest
	if len(b) > maxManifest || json.Unmarshal(b, &m) != nil || m.Format != 1 || m.Binary != WrappedBinary {
		return fmt.Errorf("%s is not the manifest of a wrapped module's dump", ManifestName)
	}
	if m.Module != api.module {
		return fmt.Errorf("the dump is of module %q, not %q", m.Module, api.module)
	}
	// Download and verify everything first.
	fetch := func(p *Part, name string, wanted bool) (string, error) {
		if (p != nil) != wanted {
			return "", fmt.Errorf("the dump and the module disagree about %s", name)
		}
		if !wanted {
			return "", nil
		}
		file := filepath.Join(tmp, name)
		if err := api.get(ctx, name, file); err != nil {
			return "", fmt.Errorf("download %s: %w", name, err)
		}
		got, err := partOf(name, file)
		if err != nil {
			return "", err
		}
		if got != *p {
			return "", fmt.Errorf("%s: size or checksum does not match the dump", name)
		}
		return file, nil
	}
	pg, err := fetch(m.Postgres, PGDumpName, w.PGDump != "")
	if err != nil {
		return err
	}
	db, err := fetch(m.Database, DatabaseName, w.SQLite != "")
	if err != nil {
		return err
	}
	files, err := fetch(m.Files, FilesName, w.Files)
	if err != nil {
		return err
	}
	if pg != "" {
		// The group (the pod's fsGroup) reads it: pg_restore runs as another user.
		if err := os.Chmod(pg, 0o640); err != nil {
			return err
		}
		if err := os.Rename(pg, w.PGDump); err != nil {
			return err
		}
	}
	if db != "" {
		if err := installSQLite(ctx, db, filepath.Join(w.DataDir, w.SQLite)); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "restore ok: %s\n", w.SQLite)
	}
	if files != "" {
		n, err := installFiles(ctx, files, w.DataDir)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "restore ok: %d file(s)\n", n)
	}
	fmt.Fprintf(stdout, "dump of %s verified\n", m.TakenAt.Format(time.RFC3339))
	return nil
}

func installSQLite(ctx context.Context, gz, dst string) error {
	staged := dst + ".restoring"
	_ = os.Remove(staged)
	defer func() { _ = os.Remove(staged) }()
	if err := gunzipTo(gz, staged); err != nil {
		return err
	}
	if err := integrity(ctx, staged); err != nil {
		return fmt.Errorf("the restored database: %w", err)
	}
	// A journal left over from the old database must not be applied to the new one.
	for _, ext := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(dst + ext); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(staged, dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

func gunzipTo(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // our temporary file
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	zr, err := gzip.NewReader(in)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the data directory
	if err != nil {
		return err
	}
	_, err = io.Copy(out, zr) //nolint:gosec // our own verified dump
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// installFiles replaces the content of dir with the archive. It is unpacked completely beside the
// old content first, so that a broken archive or a full disk leaves the volume as it was.
func installFiles(ctx context.Context, file, dir string) (int, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, err
	}
	defer func() { _ = root.Close() }()
	for _, d := range []string{stageDir, oldDir} {
		if err := root.RemoveAll(d); err != nil {
			return 0, err
		}
	}
	if err := root.Mkdir(stageDir, 0o750); err != nil {
		return 0, err
	}
	n, err := unarchive(ctx, file, filepath.Join(dir, stageDir))
	if err != nil {
		_ = root.RemoveAll(stageDir)
		return 0, fmt.Errorf("unpack %s (the volume is unchanged): %w", FilesName, err)
	}
	old, err := topEntries(dir)
	if err != nil {
		return 0, err
	}
	if err := root.Mkdir(oldDir, 0o750); err != nil {
		return 0, err
	}
	for _, name := range old {
		if err := root.Rename(name, path.Join(oldDir, name)); err != nil {
			return 0, err
		}
	}
	es, err := os.ReadDir(filepath.Join(dir, stageDir))
	if err != nil {
		return 0, err
	}
	for _, e := range es {
		if err := root.Rename(path.Join(stageDir, e.Name()), e.Name()); err != nil {
			return 0, err
		}
	}
	for _, d := range []string{stageDir, oldDir} {
		if err := root.RemoveAll(d); err != nil {
			return 0, err
		}
	}
	return n, syncDir(dir)
}

// unarchive unpacks into dir and returns the number of regular files. Nothing can be written
// outside dir: every operation goes through an os.Root, which refuses to follow links out of it.
func unarchive(ctx context.Context, file, dir string) (int, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, err
	}
	defer func() { _ = root.Close() }()
	in, err := os.Open(file) //nolint:gosec // our temporary file
	if err != nil {
		return 0, err
	}
	defer func() { _ = in.Close() }()
	zr, err := gzip.NewReader(in)
	if err != nil {
		return 0, err
	}
	tr := tar.NewReader(zr)
	files := 0
	type dirMode struct {
		name string
		mode fs.FileMode
	}
	var dirs []dirMode
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
		name := path.Clean(h.Name)
		if !filepath.IsLocal(name) || name == "." {
			return 0, fmt.Errorf("the archive holds an unacceptable name %q", h.Name)
		}
		mode := fs.FileMode(h.Mode).Perm() //nolint:gosec // permission bits
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o700); err != nil {
				return 0, err
			}
			dirs = append(dirs, dirMode{name, mode})
		case tar.TypeSymlink:
			if err := root.Symlink(h.Linkname, name); err != nil {
				return 0, err
			}
		case tar.TypeReg:
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return 0, err
			}
			_, err = io.Copy(f, tr) //nolint:gosec // our own verified dump
			if err == nil {
				err = f.Chmod(mode) // not masked by the umask
			}
			if err == nil {
				err = f.Sync()
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return 0, err
			}
			files++
		default:
			return 0, fmt.Errorf("the archive holds an entry of type %q", string(h.Typeflag))
		}
	}
	// Directory modes last, deepest first: a read-only directory could not have been filled.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := root.Chmod(dirs[i].name, dirs[i].mode); err != nil {
			return 0, err
		}
	}
	// A truncated gzip stream must not pass: read it to its checksum.
	_, err = io.Copy(io.Discard, zr) //nolint:gosec // our own verified dump
	return files, err
}
