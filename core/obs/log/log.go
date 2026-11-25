// Package obslog is the shared structured-logging setup: JSON records (text for local
// development), a request id carried on the context and added to every record, and redaction of
// secret-bearing keys and secret-shaped values. It depends only on the standard library.
package obslog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/dodwmd/companyinacan/core/obs/rid"
)

// Options configures New.
type Options struct {
	Service string // added as "service"
	Version string // added as "version"
	Level   string // debug, info (default), warn, error
	Format  string // "json" (default) or "text" (local development)
}

// New builds the logger. Records carry request_id when the context passed to the *Context
// methods has one (see WithRequestID).
func New(w io.Writer, o Options) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(o.Level)); err != nil {
		lv = slog.LevelInfo
	}
	ho := &slog.HandlerOptions{Level: lv, ReplaceAttr: redactAttr}
	var h slog.Handler
	if strings.EqualFold(o.Format, "text") {
		h = slog.NewTextHandler(w, ho)
	} else {
		h = slog.NewJSONHandler(w, ho)
	}
	l := slog.New(ctxHandler{h})
	if o.Service != "" {
		l = l.With("service", o.Service)
	}
	if o.Version != "" {
		l = l.With("version", o.Version)
	}
	return l
}

type ctxHandler struct{ slog.Handler }

func (h ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestID(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}
func (h ctxHandler) WithAttrs(a []slog.Attr) slog.Handler { return ctxHandler{h.Handler.WithAttrs(a)} }
func (h ctxHandler) WithGroup(n string) slog.Handler      { return ctxHandler{h.Handler.WithGroup(n)} }

// WithRequestID returns ctx carrying the request id.
func WithRequestID(ctx context.Context, id string) context.Context { return rid.With(ctx, id) }

// RequestID returns the id stored by WithRequestID, or "".
func RequestID(ctx context.Context) string { return rid.From(ctx) }

// MaxRequestIDLen bounds an accepted inbound X-Request-Id.
const MaxRequestIDLen = rid.MaxLen

// NewRequestID returns inbound when it is a safe id (1 to 64 characters of A-Z a-z 0-9 . _ -),
// otherwise a fresh random 16-hex-digit id.
func NewRequestID(inbound string) string { return rid.New(inbound) }

// Sanitize makes s safe for a free-form (non-JSON) log line: control characters become spaces and
// the result is cut to max bytes (on a rune boundary). JSON and text slog handlers escape on their
// own; use this for hand-built lines.
func Sanitize(s string, max int) string {
	if len(s) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "..."
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			return ' '
		}
		return r
	}, s)
}

// secretKeyParts are matched case-insensitively against attribute keys.
var secretKeyParts = []string{
	"password", "passwd", "secret", "token", "authorization", "cookie", "api_key", "apikey", "api-key",
	"private_key", "privatekey", "credential", "bearer", "totp", "csrf", "signature", "session_id", "sessionid",
}

// Redacted replaces redacted values.
const Redacted = "[redacted]"

var (
	reURLCred = regexp.MustCompile(`(://[^/\s:@]*:)[^@\s/]+@`)
	reBearer  = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	reSecret  = regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api[_-]?key|authorization|cookie)(\s*[=:]\s*)[^\s,;&"']+`)
)

// RedactString removes URL credentials, bearer tokens and key=value secrets from free text.
func RedactString(s string) string {
	if len(s) < 6 {
		return s
	}
	if strings.Contains(s, "@") && strings.Contains(s, "://") {
		s = reURLCred.ReplaceAllString(s, "${1}"+Redacted+"@")
	}
	l := strings.ToLower(s)
	if strings.Contains(l, "bearer") || strings.Contains(l, "basic") {
		s = reBearer.ReplaceAllString(s, "$1 "+Redacted)
	}
	if strings.ContainsAny(s, "=:") {
		for _, k := range []string{"password", "passwd", "secret", "token", "key", "authorization", "cookie"} {
			if strings.Contains(l, k) {
				s = reSecret.ReplaceAllString(s, "$1$2"+Redacted)
				break
			}
		}
	}
	return s
}

// anyText renders v like fmt's %+v, following pointers and showing []byte as text.
func anyText(v any) string {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer && !rv.IsNil() {
		rv = rv.Elem()
	}
	if !rv.IsValid() || !rv.CanInterface() {
		return ""
	}
	v = rv.Interface()
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprintf("%+v", v)
}

func secretKey(k string) bool {
	l := strings.ToLower(k)
	for _, p := range secretKeyParts {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

func redactAttr(_ []string, a slog.Attr) slog.Attr {
	if secretKey(a.Key) && a.Value.Kind() != slog.KindGroup {
		return slog.String(a.Key, Redacted)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		if s := a.Value.String(); s != "" {
			a.Value = slog.StringValue(RedactString(s))
		}
	case slog.KindAny:
		switch v := a.Value.Any().(type) {
		case nil:
		case error:
			a.Value = slog.StringValue(RedactString(v.Error()))
		default:
			// Maps, structs, http.Header, []byte, Stringers and pointers to them: render the value the way
			// fmt would and keep the original (so harmless values stay structured JSON) unless the text
			// carries something secret-shaped.
			if text := anyText(v); text != "" {
				if red := RedactString(text); red != text {
					a.Value = slog.StringValue(red)
				}
			}
		}
	}
	return a
}
