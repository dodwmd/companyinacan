package staticsrv

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

// Extract unpacks a gzip-compressed tar archive of a built site into dir. It exists so the site
// pod can be fed from a ConfigMap (one tar.gz key) without a shell or tar in the image
// (`can-static -extract`, run by an init container, epic E24).
//
// Only regular files and directories are accepted; absolute paths, ".." and links are refused,
// and every write goes through an os.Root so nothing can land outside dir. maxBytes bounds the
// total unpacked size (a decompression bomb cannot fill the volume).
func Extract(archive io.Reader, dir string, maxBytes int64) error {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("site archive is not gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(gz)
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("site archive: %w", err)
		}
		name := path.Clean(strings.TrimSuffix(h.Name, "/"))
		if h.Name == "" || path.IsAbs(h.Name) || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(h.Name, "\x00") || strings.Contains(h.Name, "\\") {
			return fmt.Errorf("site archive: unsafe path %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if name == "." {
				continue
			}
			if err := mkdirAll(root, name); err != nil {
				return err
			}
		case tar.TypeReg:
			if name == "." {
				return fmt.Errorf("site archive: unsafe path %q", h.Name)
			}
			total += h.Size
			if h.Size < 0 || total > maxBytes {
				return fmt.Errorf("site archive: unpacked size exceeds the %d byte limit", maxBytes)
			}
			if d := path.Dir(name); d != "." {
				if err := mkdirAll(root, d); err != nil {
					return err
				}
			}
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				return fmt.Errorf("site archive: %w", err)
			}
			n, cerr := io.Copy(f, io.LimitReader(tr, h.Size))
			if err := f.Close(); cerr == nil {
				cerr = err
			}
			if cerr != nil || n != h.Size {
				return fmt.Errorf("site archive: writing %s: %v", name, cerr)
			}
		default:
			return fmt.Errorf("site archive: %q is a link or special file, which is not allowed", h.Name)
		}
	}
}

func mkdirAll(root *os.Root, name string) error {
	cur := ""
	for _, part := range strings.Split(name, "/") {
		cur = path.Join(cur, part)
		if err := root.Mkdir(cur, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("site archive: %w", err)
		}
	}
	return nil
}
