package files

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SniffSize is how much of an upload is read before anything is stored: enough for every magic
// number below and for the text check.
const SniffSize = 4096

type typeSpec struct {
	ct    string
	magic func(head []byte) bool
}

func prefix(p string) func([]byte) bool {
	return func(b []byte) bool { return bytes.HasPrefix(b, []byte(p)) }
}

var zipMagic = prefix("PK\x03\x04")

// fileTypes is the closed set of types the service can ever accept, by extension. The declared
// Content-Type of an upload is never used. No archive, script, markup or executable type is here:
// an SVG or HTML file is active content, and a zip is something somebody later unpacks.
var fileTypes = map[string]typeSpec{
	"pdf":  {"application/pdf", prefix("%PDF-")},
	"png":  {"image/png", prefix("\x89PNG\r\n\x1a\n")},
	"jpg":  {"image/jpeg", prefix("\xff\xd8\xff")},
	"jpeg": {"image/jpeg", prefix("\xff\xd8\xff")},
	"gif": {"image/gif", func(b []byte) bool {
		return bytes.HasPrefix(b, []byte("GIF87a")) || bytes.HasPrefix(b, []byte("GIF89a"))
	}},
	"webp": {"image/webp", func(b []byte) bool {
		return len(b) >= 12 && bytes.HasPrefix(b, []byte("RIFF")) && string(b[8:12]) == "WEBP"
	}},
	"docx": {"application/vnd.openxmlformats-officedocument.wordprocessingml.document", zipMagic},
	"xlsx": {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", zipMagic},
	"pptx": {"application/vnd.openxmlformats-officedocument.presentationml.presentation", zipMagic},
	"odt":  {"application/vnd.oasis.opendocument.text", zipMagic},
	"ods":  {"application/vnd.oasis.opendocument.spreadsheet", zipMagic},
	"odp":  {"application/vnd.oasis.opendocument.presentation", zipMagic},
	"rtf":  {"application/rtf", prefix(`{\rtf`)},
	"txt":  {"text/plain; charset=utf-8", isText},
	"csv":  {"text/csv; charset=utf-8", isText},
	"md":   {"text/markdown; charset=utf-8", isText},
}

// inlineSafe are the extensions that may ever be shown inline: raster images only, never PDF
// (its viewer runs script) and never text.
var inlineSafe = []string{"png", "jpg", "jpeg", "gif", "webp"}

func isText(b []byte) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	if utf8.Valid(b) {
		return true
	}
	if len(b) < SniffSize { // the whole file fits and is not UTF-8
		return false
	}
	for trim := 1; trim <= 3; trim++ { // the sniff window may have cut a rune
		if utf8.Valid(b[:len(b)-trim]) {
			return true
		}
	}
	return false
}

// extOf returns the lower-case extension of a sanitised name ("" when there is none).
func extOf(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i < 1 || i == len(name)-1 {
		return ""
	}
	return strings.ToLower(name[i+1:])
}

// DetectType checks that the file's extension is on the allow-list and that its leading bytes
// really are that kind of file, and returns the content type to store. Any failure is ErrType.
func DetectType(name string, head []byte, allowed []string) (string, error) {
	ext := extOf(name)
	spec, ok := fileTypes[ext]
	if ext == "" || !ok || !slices.Contains(allowed, ext) {
		return "", fmt.Errorf("%w: the extension is not allowed", ErrType)
	}
	if !spec.magic(head) {
		return "", fmt.Errorf("%w: the content does not match .%s", ErrType, ext)
	}
	return spec.ct, nil
}

// SafeFilename reduces an uploaded name to a short, single-line base name for display and
// Content-Disposition. It never influences where anything is stored (keys are random). Path
// parts, control and format characters (bidi overrides included), quotes and semicolons go;
// leading dots go. It may return "": the caller must refuse an empty name.
func SafeFilename(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '"' || r == ';' || r < 0x20 || r == 0x7f:
			return ' '
		case unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Co, r) || r == utf8.RuneError || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029:
			return -1
		}
		return r
	}, name)
	name = strings.Join(strings.Fields(name), " ")
	name = strings.TrimLeft(name, ". ")
	if len(name) > 120 {
		ext := ""
		if i := strings.LastIndexByte(name, '.'); i > 0 && len(name)-i <= 8 {
			ext = name[i:]
		}
		stem := name[:len(name)-len(ext)]
		for len(stem) > 120-len(ext) || !utf8.ValidString(stem) {
			stem = stem[:len(stem)-1]
		}
		name = strings.TrimSpace(stem) + ext
	}
	return name
}

// ContentDisposition builds the header: attachment unless inline was explicitly allowed for the
// type. The quoted name is ASCII only; the full name goes in the RFC 5987 form.
func ContentDisposition(name string, inline bool) string {
	kind := "attachment"
	if inline {
		kind = "inline"
	}
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' || r == ';' {
			return '_'
		}
		return r
	}, name)
	return kind + `; filename="` + ascii + `"; filename*=UTF-8''` + url.PathEscape(name)
}

func newBlobKey() string { return randHex(16) }

func hexSHA(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// VerifiedCopy streams r to w, which must produce exactly size bytes hashing to wantSHA. The
// last chunk is held back until the whole object has been read and checked, so a corrupted or
// truncated object is reported (ErrIntegrity) before its end reaches the client. Memory is two
// fixed buffers whatever the size.
func VerifiedCopy(w io.Writer, r io.Reader, size int64, wantSHA string) error {
	const chunk = 32 << 10
	h := sha256.New()
	bufA, bufB := make([]byte, chunk), make([]byte, chunk)
	cur, held := bufA, bufB
	var total int64
	heldN := 0
	for {
		n, err := io.ReadFull(r, cur)
		if n > 0 {
			total += int64(n)
			if total > size {
				return ErrIntegrity
			}
			h.Write(cur[:n])
			if heldN > 0 {
				if _, werr := w.Write(held[:heldN]); werr != nil {
					return werr
				}
			}
			cur, held = held, cur
			heldN = n
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if total != size || hex.EncodeToString(h.Sum(nil)) != wantSHA {
		return ErrIntegrity
	}
	if heldN > 0 {
		if _, err := w.Write(held[:heldN]); err != nil {
			return err
		}
	}
	return nil
}
