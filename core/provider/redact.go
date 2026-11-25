package provider

import (
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Redacted replaces every secret in output.
const Redacted = "[REDACTED]"

// Redactor removes known secrets and secret-looking assignments from text.
// Known secrets are matched literally; the patterns catch the rest
// (Authorization headers, token=..., password: ...).
type Redactor struct {
	mu      sync.Mutex
	secrets []string
}

var (
	reBearer = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
	reAssign = regexp.MustCompile(`(?i)((?:token|secret|password|passwd|authorization)["']?\s*[:=]\s*["']?(?:bearer\s+)?)[^\s"',;]{4,}`)
	reKey    = regexp.MustCompile(`(-----BEGIN [A-Z ]*PRIVATE KEY-----)[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----)`)
)

// NewRedactor returns an empty redactor.
func NewRedactor() *Redactor { return &Redactor{} }

// Add registers a literal secret. Very short values are ignored (they would
// shred unrelated output).
func (r *Redactor) Add(s string) {
	s = strings.TrimSpace(s)
	if len(s) < 6 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.secrets {
		if e == s {
			return
		}
	}
	r.secrets = append(r.secrets, s)
	// longest first so a secret containing another is removed whole
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
}

// Redact returns s with secrets replaced.
func (r *Redactor) Redact(s string) string {
	r.mu.Lock()
	known := append([]string(nil), r.secrets...)
	r.mu.Unlock()
	for _, k := range known {
		s = strings.ReplaceAll(s, k, Redacted)
	}
	s = reKey.ReplaceAllString(s, "${1} "+Redacted+" ${2}")
	s = reBearer.ReplaceAllString(s, "${1}"+Redacted)
	s = reAssign.ReplaceAllString(s, "${1}"+Redacted)
	return s
}

// Err redacts an error message, keeping nil as nil and the chain unwrappable.
func (r *Redactor) Err(err error) error {
	if err == nil {
		return nil
	}
	return redactedError{r.Redact(err.Error()), err}
}

type redactedError struct {
	msg string
	err error
}

func (e redactedError) Error() string { return e.msg }
func (e redactedError) Unwrap() error { return e.err }

// Writer wraps w so everything written is redacted. Output is line buffered so
// a secret split across Write calls within one line is still caught; call
// Flush on the result (it implements Flusher) when done.
func (r *Redactor) Writer(w io.Writer) io.Writer { return &redactWriter{r: r, w: w} }

type redactWriter struct {
	r   *Redactor
	w   io.Writer
	buf []byte
}

func (rw *redactWriter) Write(p []byte) (int, error) {
	rw.buf = append(rw.buf, p...)
	for {
		i := strings.IndexByte(string(rw.buf), '\n')
		if i < 0 {
			break
		}
		line := string(rw.buf[:i+1])
		rw.buf = rw.buf[i+1:]
		if _, err := io.WriteString(rw.w, rw.r.Redact(line)); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush writes any buffered partial line.
func (rw *redactWriter) Flush() error {
	if len(rw.buf) == 0 {
		return nil
	}
	_, err := io.WriteString(rw.w, rw.r.Redact(string(rw.buf)))
	rw.buf = nil
	return err
}

// Flusher is implemented by writers returned from Redactor.Writer.
type Flusher interface{ Flush() error }
