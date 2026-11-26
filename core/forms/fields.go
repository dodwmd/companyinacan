package forms

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// Body parsing errors; the HTTP layer maps each to a status code.
var (
	ErrUnsupportedMedia = errors.New("unsupported media type")
	ErrTooLarge         = errors.New("request body too large")
	ErrBadBody          = errors.New("malformed request body")
)

const maxInputFields = 64

// FieldError describes one invalid input. Code is a stable machine-readable value.
type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ParseBody reads a urlencoded or JSON body of flat string values, bounded by max bytes.
// Repeated keys, nested values and non-string JSON values are rejected so that what is
// validated is exactly what is stored.
func ParseBody(w http.ResponseWriter, r *http.Request, max int64) (map[string]string, error) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, ErrUnsupportedMedia
	}
	body := http.MaxBytesReader(w, r.Body, max)
	switch mt {
	case "application/x-www-form-urlencoded":
		b, err := io.ReadAll(body)
		if err != nil {
			return nil, bodyErr(err)
		}
		q, err := url.ParseQuery(string(b))
		if err != nil || len(q) > maxInputFields {
			return nil, ErrBadBody
		}
		out := make(map[string]string, len(q))
		for k, v := range q {
			if len(v) != 1 {
				return nil, ErrBadBody
			}
			out[k] = v[0]
		}
		return out, nil
	case "application/json":
		dec := json.NewDecoder(body)
		var raw map[string]any
		if err := dec.Decode(&raw); err != nil {
			return nil, bodyErr(err)
		}
		if _, err := dec.Token(); err != io.EOF {
			if err != nil {
				return nil, bodyErr(err)
			}
			return nil, ErrBadBody
		}
		if raw == nil || len(raw) > maxInputFields {
			return nil, ErrBadBody
		}
		out := make(map[string]string, len(raw))
		for k, v := range raw {
			s, ok := v.(string)
			if !ok {
				return nil, ErrBadBody
			}
			out[k] = s
		}
		return out, nil
	}
	return nil, ErrUnsupportedMedia
}

func bodyErr(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return ErrTooLarge
	}
	return ErrBadBody
}

// Validate checks input against the form definition: required, length, type, allow-list,
// and rejects fields the form does not declare. It returns the cleaned (trimmed) values;
// empty optional fields are dropped. The input must already have reserved keys (honeypot,
// token, captcha response) removed.
func (f *Form) Validate(in map[string]string) (map[string]string, []FieldError) {
	var errs []FieldError
	add := func(field, code, msg string) { errs = append(errs, FieldError{field, code, msg}) }
	clean := make(map[string]string, len(f.Fields))
	declared := make(map[string]bool, len(f.Fields))

	for _, fl := range f.Fields {
		declared[fl.Name] = true
		// A browser posts a line break as CR LF but counts it as one character against maxlength.
		v := strings.TrimSpace(strings.ReplaceAll(in[fl.Name], "\r\n", "\n"))
		if v == "" {
			if fl.Required {
				add(fl.Name, "required", "this field is required")
			}
			continue
		}
		if !utf8.ValidString(v) || hasControl(v) {
			add(fl.Name, "invalid_characters", "contains characters that are not allowed")
			continue
		}
		if fl.Type == TypeText && !fl.Multiline && strings.ContainsAny(v, "\r\n") {
			add(fl.Name, "invalid_characters", "line breaks are not allowed in this field")
			continue
		}
		n := utf8.RuneCountInString(v)
		if n > fl.MaxLength {
			add(fl.Name, "too_long", "too long")
			continue
		}
		if n < fl.MinLength {
			add(fl.Name, "too_short", "too short")
			continue
		}
		switch fl.Type {
		case TypeEmail:
			e, ok := cleanEmail(v)
			if !ok {
				add(fl.Name, "invalid_email", "not a valid email address")
				continue
			}
			v = e
		case TypeConsent:
			// A browser sends the checkbox's value only when it is ticked.
			switch v {
			case "yes", "on":
				v = "yes"
			case "no":
				continue
			default:
				add(fl.Name, "not_allowed", "not one of the allowed values")
				continue
			}
		case TypeSelect:
			if !slices.Contains(fl.Options, v) {
				add(fl.Name, "not_allowed", "not one of the allowed values")
				continue
			}
		}
		clean[fl.Name] = v
	}

	var unknown []string
	for k := range in {
		if !declared[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		// The name is not echoed back verbatim into any HTML; it is JSON-encoded by the caller.
		add(truncate(k, 40), "unknown_field", "this field is not accepted by the form")
	}
	return clean, errs
}

// Consents are the names of the consent checkboxes ticked in clean values.
func (f *Form) Consents(clean map[string]string) []string {
	var out []string
	for _, fl := range f.Fields {
		if fl.Type == TypeConsent && clean[fl.Name] == "yes" {
			out = append(out, fl.Name)
		}
	}
	return out
}

func hasControl(s string) bool {
	for _, r := range s {
		if r == 0x7f || (r < 0x20 && r != '\n' && r != '\r' && r != '\t') {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
