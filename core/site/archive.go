package site

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
)

// MaxArchive is the largest site tarball that is published: the site pod reads it from a ConfigMap,
// which is limited to 1 MiB and base64-encodes binary data (+33%).
const MaxArchive = 700 << 10

// Annotations of ConfigMap can-system/site-content, written by `can up` and by can-core.
const (
	AnnotationSHA     = "can.dev/site-sha256"        // the archive
	AnnotationBaseURL = "can.dev/site-base-url"      // the public origin it was built for
	AnnotationSource  = "can.dev/site-source-sha256" // SectionHash of the `site:` section it was built from
)

// Archive packs the built site, with its precompressed files and marker, as the gzip-compressed tar
// the site pod unpacks (`can-static -extract`). The same output always gives the same bytes. It
// returns the archive and the number of files in it.
func (o *Output) Archive() ([]byte, int, error) {
	tmp, err := os.MkdirTemp("", "can-site-")
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := o.Write(tmp, WriteOptions{Compress: true}); err != nil {
		return nil, 0, err
	}
	var names []string
	err = filepath.WalkDir(tmp, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(tmp, p)
		names = append(names, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Strings(names)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(tmp, filepath.FromSlash(n))) //nolint:gosec // walked from our own temp dir
		if err != nil {
			return nil, 0, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(b)), Format: tar.FormatPAX}); err != nil {
			return nil, 0, err
		}
		if _, err := tw.Write(b); err != nil {
			return nil, 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, 0, err
	}
	if err := gz.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), len(names), nil
}

// ArchiveLogo returns the logo a published archive holds, "" when it has none. can-core, which has
// no copy of the company repository, reuses it when it publishes.
func ArchiveLogo(tgz []byte) (name string, data []byte, err error) {
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return "", nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", nil, nil
		}
		if err != nil {
			return "", nil, err
		}
		if !IsLogoPath(h.Name) {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxLogoBytes+1))
		if err != nil {
			return "", nil, err
		}
		return h.Name, b, nil
	}
}

// IsLogoPath reports whether p is where a rendered site keeps its logo.
func IsLogoPath(p string) bool {
	return strings.HasPrefix(p, "assets/logo.") && logoExts[path.Ext(p)]
}

// SectionHash identifies the content of a `site:` section: two documents with the same hash render
// the same pages. It is what concurrent edits are detected with.
func SectionHash(s config.Site) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err) // strings, bools and slices of them
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
