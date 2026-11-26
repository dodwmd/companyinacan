package modbackup

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the same driver the services use
)

// Manifest describes one dump set.
type Manifest struct {
	Format  int       `json:"format"` // 1
	Binary  string    `json:"binary"`
	Module  string    `json:"module,omitempty"`
	TakenAt time.Time `json:"taken_at"`
	// Database is the gzip of the copy; Size and SHA256 are of the uncompressed database.
	Database Part `json:"database"`
	// SchemaVersion is the newest migration applied to the database (0 in sets made before it
	// was recorded; restore then reads it from the database).
	SchemaVersion int        `json:"schema_version,omitempty"`
	Blobs         *BlobsPart `json:"blobs,omitempty"`
}

// Part is one file of the set.
type Part struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// BlobsPart lists the files the database refers to. The bytes are not in the dump: with the local
// backend they are in the volume backup; with s3 they are in the bucket and the list is only a
// record.
type BlobsPart struct {
	Backend string `json:"backend"`
	Count   int    `json:"count"`
	Bytes   int64  `json:"bytes"`
	Part
}

type blob struct {
	key    string
	size   int64
	sha    string
	record string // the id of the record that refers to the file
}

// retryPause is how long backup waits before a second look at a file that is missing (a purge
// removes the bytes just before it clears the reference).
var retryPause = 3 * time.Second

const attempts = 3

// runBackup builds the dump set in a temporary directory and uploads it, or leaves it in out.
func runBackup(ctx context.Context, s spec, l layout, module string, api *client, out string, stdout io.Writer) error {
	if fi, err := os.Stat(l.db); errors.Is(err, os.ErrNotExist) {
		return ErrNoData
	} else if err != nil {
		return err
	} else if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", l.db)
	}
	if out != "" {
		if err := os.MkdirAll(out, 0o700); err != nil {
			return err
		}
	}
	// The default temporary directory is the hook Job's /tmp volume: nothing is written next to the
	// live database.
	tmp, err := os.MkdirTemp(out, ".dump-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	copyPath := filepath.Join(tmp, "copy.db")
	var blobs []blob
	var taken time.Time
	var version int
	for try := 1; ; try++ {
		taken = time.Now().UTC()
		if err := snapshot(ctx, l.db, copyPath); err != nil {
			return err
		}
		if err := integrity(ctx, copyPath); err != nil {
			return fmt.Errorf("the copy of the database: %w", err)
		}
		if version, err = schemaVersion(ctx, copyPath); err != nil {
			return fmt.Errorf("refusing to dump %s: %w", l.db, err)
		}
		if s.blobs == nil {
			break
		}
		if blobs, err = listBlobs(ctx, copyPath, s.blobs.query); err != nil {
			return fmt.Errorf("list the files the database refers to: %w", err)
		}
		if l.backend != "local" {
			break
		}
		missing := absent(s.blobs, l.blobDir, blobs)
		if len(missing) == 0 {
			break
		}
		if try == attempts {
			return fmt.Errorf("%d referenced file(s) missing or of the wrong size under %s. %s", len(missing), l.blobDir, s.blobs.missingFiles(missing, records(blobs)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryPause):
		}
		if err := os.Remove(copyPath); err != nil {
			return err
		}
	}

	m := Manifest{Format: 1, Binary: s.binary, Module: module, TakenAt: taken, SchemaVersion: version}
	if m.Database, err = compress(copyPath, filepath.Join(tmp, DatabaseName), DatabaseName); err != nil {
		return err
	}
	if err := os.Remove(copyPath); err != nil {
		return err
	}
	files := []string{DatabaseName}
	if s.blobs != nil {
		list := filepath.Join(tmp, "blobs.tsv")
		var b strings.Builder
		var total int64
		for _, x := range blobs {
			fmt.Fprintf(&b, "%s\t%d\t%s\n", x.key, x.size, x.sha)
			total += x.size
		}
		if err := os.WriteFile(list, []byte(b.String()), 0o600); err != nil {
			return err
		}
		part, err := compress(list, filepath.Join(tmp, BlobsName), BlobsName)
		if err != nil {
			return err
		}
		m.Blobs = &BlobsPart{Backend: l.backend, Count: len(blobs), Bytes: total, Part: part}
		files = append(files, BlobsName)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, ManifestName), append(b, '\n'), 0o600); err != nil {
		return err
	}
	files = append(files, ManifestName) // last: a set with a manifest is complete

	for _, name := range files {
		if out != "" {
			if err := os.Rename(filepath.Join(tmp, name), filepath.Join(out, name)); err != nil {
				return err
			}
			continue
		}
		if err := api.put(ctx, name, filepath.Join(tmp, name)); err != nil {
			return fmt.Errorf("upload %s: %w", name, err)
		}
	}
	fmt.Fprintf(stdout, "backup ok: database %d bytes", m.Database.Size)
	if m.Blobs != nil {
		fmt.Fprintf(stdout, ", %d referenced file(s), %d bytes (%s backend)", m.Blobs.Count, m.Blobs.Bytes, m.Blobs.Backend)
	}
	fmt.Fprintln(stdout)
	return nil
}

func open(path, query string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Opaque: (&url.URL{Path: path}).EscapedPath(), RawQuery: query}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// snapshot copies the database with VACUUM INTO over a read-only connection of its own: one read
// transaction, so the copy is a single point in time while the service keeps writing.
func snapshot(ctx context.Context, src, dst string) error {
	db, err := open(src, "mode=ro&_pragma=busy_timeout(30000)")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	return os.Chmod(dst, 0o600)
}

// integrity runs integrity_check over a database no one is writing.
func integrity(ctx context.Context, path string) error {
	db, err := open(path, "mode=ro&immutable=1")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	var res string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	if res != "ok" {
		return fmt.Errorf("integrity check failed: %s", res)
	}
	return nil
}

func listBlobs(ctx context.Context, path, query string) ([]blob, error) {
	db, err := open(path, "mode=ro&immutable=1")
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []blob
	for rows.Next() {
		var b blob
		if err := rows.Scan(&b.key, &b.size, &b.sha, &b.record); err != nil {
			return nil, err
		}
		if !blobKeyRe.MatchString(b.key) {
			return nil, errors.New("the database holds a file key this version does not understand")
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func records(blobs []blob) map[string]string {
	out := map[string]string{}
	for _, b := range blobs {
		out[b.key] = b.record
	}
	return out
}

// absent returns the keys whose file is not there with the recorded size. Contents are hashed at
// restore, not on every backup.
func absent(spec *blobSpec, dir string, blobs []blob) []string {
	var out []string
	for _, b := range blobs {
		fi, err := os.Lstat(spec.path(dir, b.key))
		if err != nil || !fi.Mode().IsRegular() || fi.Size() != b.size {
			out = append(out, b.key)
		}
	}
	return out
}

// compress writes the gzip of src to dst and returns the size and hash of the uncompressed bytes.
func compress(src, dst, name string) (Part, error) {
	in, err := os.Open(src) //nolint:gosec // our temporary file
	if err != nil {
		return Part{}, err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // our temporary file
	if err != nil {
		return Part{}, err
	}
	defer func() { _ = out.Close() }()
	bw := bufio.NewWriterSize(out, 64<<10)
	zw, err := gzip.NewWriterLevel(bw, gzip.BestSpeed) // small memory, little CPU
	if err != nil {
		return Part{}, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(zw, h), in)
	if err != nil {
		return Part{}, err
	}
	for _, f := range []func() error{zw.Close, bw.Flush, out.Close} {
		if err := f(); err != nil {
			return Part{}, fmt.Errorf("write %s: %w", strconv.Quote(name), err)
		}
	}
	return Part{Name: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}
