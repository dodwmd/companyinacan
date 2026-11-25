// Package diagnose builds the support bundle behind `can diagnose` and `can-core admin diagnose`:
// a small tar.gz an operator can attach to a support request without reviewing it line by line.
//
// The guarantees, each covered by tests that seed secrets into every input:
//   - an allowlist of file names: nothing else can be added, so no database, key file or
//     environment dump can ride along by accident;
//   - size caps per file and in total;
//   - every text passes Scrub (and config.yaml RedactConfig first): bearer tokens, URL credentials, key=value secrets, PEM blocks, JWTs,
//     well-known provider key prefixes, long random-looking tokens and email addresses are removed,
//     plus any extra redactor the caller supplies (the provider Redactor, which knows the secrets
//     this run resolved).
//
// Scrubbing is a safety net, not a licence to collect: collectors gather counts, versions and
// states, never rows of personal data.
package diagnose

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	obslog "github.com/dodwmd/companyinacan/core/obs/log"
)

// Default caps.
const (
	DefaultMaxFile  = 256 << 10
	DefaultMaxTotal = 2 << 20
)

// Allowed lists every file name a bundle may contain (manifest.json is added by WriteTo).
var Allowed = map[string]string{
	"versions.json":  "binary and runtime versions",
	"config.yaml":    "company.yaml with secrets and personal data redacted",
	"doctor.json":    "prerequisite and reachability checks",
	"modules.json":   "module states (no secrets, no settings)",
	"state.json":     "database sizes, schema version, counters, audit chain verification",
	"reconcile.json": "the last reconcile outcome (sanitised error text)",
	"metrics.txt":    "a metrics snapshot (bounded series, no personal data by construction)",
	"logs.txt":       "the last log lines, scrubbed",
	"backup.json":    "backup status",
}

// Bundle collects files. The zero value is not usable; use New.
type Bundle struct {
	MaxFile, MaxTotal int
	Now               func() time.Time

	extra []func(string) string
	files map[string][]byte
	total int
}

// New returns an empty bundle with the default caps.
func New() *Bundle {
	return &Bundle{MaxFile: DefaultMaxFile, MaxTotal: DefaultMaxTotal, Now: time.Now, files: map[string][]byte{}}
}

// AddRedactor adds a redactor applied after Scrub (for example provider.Redactor.Redact).
func (b *Bundle) AddRedactor(f func(string) string) { b.extra = append(b.extra, f) }

// AddText scrubs, caps and stores a file. The name must be on the Allowed list and used once.
func (b *Bundle) AddText(name, text string) error {
	if _, ok := Allowed[name]; !ok {
		return fmt.Errorf("diagnose: %q is not an allowed bundle file", name)
	}
	if _, dup := b.files[name]; dup {
		return fmt.Errorf("diagnose: %q added twice", name)
	}
	if name == "config.yaml" {
		text = RedactConfig(text)
	}
	text = Scrub(text)
	for _, f := range b.extra {
		text = f(text)
	}
	if len(text) > b.MaxFile {
		cut := b.MaxFile
		for cut > 0 && text[cut]&0xC0 == 0x80 {
			cut--
		}
		text = fmt.Sprintf("%s\n... [truncated: %d bytes omitted]\n", text[:cut], len(text)-cut)
	}
	if b.total+len(text) > b.MaxTotal {
		return fmt.Errorf("diagnose: bundle would exceed %d bytes; %q not added", b.MaxTotal, name)
	}
	b.total += len(text)
	b.files[name] = []byte(text)
	return nil
}

// AddJSON stores v as indented JSON (scrubbed like any text).
func (b *Bundle) AddJSON(name string, v any) error {
	j, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return b.AddText(name, string(j)+"\n")
}

// Names returns the stored file names, sorted.
func (b *Bundle) Names() []string {
	n := make([]string, 0, len(b.files))
	for k := range b.files {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// Encode writes the bundle as tar.gz, every entry mode 0600, preceded by manifest.json.
func (b *Bundle) Encode(w io.Writer) error {
	now := b.Now().UTC()
	type entry struct {
		Name  string `json:"name"`
		Bytes int    `json:"bytes"`
		About string `json:"about"`
	}
	m := struct {
		GeneratedAt string  `json:"generated_at"`
		Files       []entry `json:"files"`
		Note        string  `json:"note"`
	}{GeneratedAt: now.Format(time.RFC3339), Note: "Secrets, tokens, personal data and email addresses are removed before writing; review before sharing if in doubt."}
	for _, n := range b.Names() {
		m.Files = append(m.Files, entry{n, len(b.files[n]), Allowed[n]})
	}
	mj, _ := json.MarshalIndent(m, "", "  ")
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	write := func(name string, body []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), ModTime: now, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(body)
		return err
	}
	if err := write("manifest.json", append(mj, '\n')); err != nil {
		return err
	}
	for _, n := range b.Names() {
		if err := write(n, b.files[n]); err != nil {
			return err
		}
	}
	return errors.Join(tw.Close(), gz.Close())
}

var (
	rePEM    = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]+-----.*?(-----END [A-Z0-9 ]+-----|\z)`)
	reJWT    = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`)
	reKeyPfx = regexp.MustCompile(`\b(?:gh[pousr]_|github_pat_|sk_live_|sk_test_|pk_live_|xox[abprs]-|glpat-|AKIA|ASIA)[A-Za-z0-9_-]{8,}`)
	reLong   = regexp.MustCompile(`[A-Za-z0-9_+=-]{32,}`)
	reANSI   = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")
	reEmail  = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)
)

// Scrub removes secrets and personal identifiers from free text.
func Scrub(s string) string {
	s = reANSI.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		if r == 0x1b || r == 0x07 || r == 0 {
			return -1
		}
		return r
	}, s)
	s = rePEM.ReplaceAllString(s, "[redacted private key]")
	s = obslogRedact(s)
	s = reJWT.ReplaceAllString(s, "[redacted jwt]")
	s = reKeyPfx.ReplaceAllString(s, "[redacted key]")
	s = reEmail.ReplaceAllString(s, "[email]")
	s = reLong.ReplaceAllString(s, "[redacted token]")
	return s
}

// obslogRedact applies the shared log redactor line by line (its patterns are line oriented).
func obslogRedact(s string) string {
	if !strings.ContainsAny(s, "\n") {
		return obslog.RedactString(s)
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = obslog.RedactString(l)
	}
	return strings.Join(lines, "\n")
}

// secretKeyParts are case-insensitive substrings of key names whose values are removed. The list errs
// on the side of removing too much from a diagnostic copy.
var secretKeyParts = []string{
	"password", "passwd", "pass", "secret", "token", "api_key", "apikey", "api-key", "key", "private", "credential",
	"bearer", "dsn", "webhook", "url", "signing", "signature", "cookie", "auth", "email", "mail_to", "notify",
	// people data: contact details and names, where a config carries them
	"contact", "phone", "mobile", "tel", "owner", "person", "first_name", "last_name", "full_name", "surname", "address",
}

var reKeyLine = regexp.MustCompile(`^(\s*(?:-\s+)*)("?[A-Za-z0-9_.-]+"?)(\s*:\s*)(.*)$`)

func suspiciousKey(k string, indent int) bool {
	l := strings.ToLower(strings.Trim(k, `"`))
	if l == "name" {
		return indent > 0 // the company name at the top level is useful; a nested name is a person or a contact
	}
	if strings.HasSuffix(l, "_name") {
		return true
	}
	for _, p := range secretKeyParts {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

func indentOf(s string) int { return len(s) - len(strings.TrimLeft(s, " \t")) }

// RedactConfig removes sensitive values from YAML-ish text without parsing it (so it cannot fail on
// a file the schema would reject): the value, and everything nested under it, of any key that names a
// secret, a credential, a URL, an email or a person's contact details; and the body of every block
// scalar (a value starting with a pipe or a greater-than sign), which is free text that can say
// anything. Ordinary configuration (the company name, domains, hosts, numbers, booleans) is kept.
// Names and phone numbers inside values of keys it does not recognise can survive: this is a safety
// net, not a guarantee.
func RedactConfig(s string) string {
	lines := strings.Split(s, "\n")
	skipBelow := -1 // while >= 0, lines indented deeper than this belong to a redacted value
	for i, l := range lines {
		if skipBelow >= 0 {
			if strings.TrimSpace(l) == "" || indentOf(l) > skipBelow {
				lines[i] = ""
				continue
			}
			skipBelow = -1
		}
		m := reKeyLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		depth := len(m[1]) // a list dash counts as depth, so "- first_name" is nested
		val := strings.TrimSpace(m[4])
		block := strings.HasPrefix(val, "|") || strings.HasPrefix(val, ">")
		switch {
		case suspiciousKey(m[2], depth):
			lines[i] = m[1] + m[2] + m[3] + "[redacted]"
			skipBelow = indentOf(l)
		case block:
			lines[i] = m[1] + m[2] + m[3] + "[free text removed]"
			skipBelow = indentOf(l)
		}
	}
	return strings.Join(lines, "\n")
}

// ValidComposeProject reports whether name is a safe docker compose project name (it is passed to
// `docker compose -p`, so it must not look like a flag or contain separators).
func ValidComposeProject(name string) bool {
	if name == "" || len(name) > 64 || name[0] == '-' || name[0] == '_' {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// TailLines returns the last n lines of r, each cut to maxLine bytes. It streams, so a huge log
// costs n lines of memory.
func TailLines(r io.Reader, n, maxLine int) string {
	if n <= 0 {
		return ""
	}
	ring := make([]string, 0, n)
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			if len(line) > maxLine {
				line = line[:maxLine] + "..."
			}
			if len(ring) == n {
				copy(ring, ring[1:])
				ring = ring[:n-1]
			}
			ring = append(ring, line)
		}
		if err != nil {
			break
		}
	}
	var out bytes.Buffer
	for _, l := range ring {
		out.WriteString(l)
		out.WriteByte('\n')
	}
	return out.String()
}
