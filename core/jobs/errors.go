package jobs

import (
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Errors the store returns; the API maps them to HTTP statuses.
var (
	ErrNotFound        = errors.New("not found")
	ErrConflict        = errors.New("conflict")
	ErrVersionMismatch = errors.New("version mismatch")
	ErrForbidden       = errors.New("forbidden")
	ErrClosed          = errors.New("this job is not accepting applications")
)

// ProblemField names one invalid input.
type ProblemField struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// InvalidError is a validation failure carrying every problem found.
type InvalidError struct{ Problems []ProblemField }

func (e *InvalidError) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = p.Field + ": " + p.Message
	}
	return strings.Join(parts, "; ")
}

func invalid(field, format string, a ...any) *InvalidError {
	return &InvalidError{Problems: []ProblemField{{Field: field, Message: fmt.Sprintf(format, a...)}}}
}

type conflictError struct{ msg string }

func (e *conflictError) Error() string { return e.msg }
func (e *conflictError) Unwrap() error { return ErrConflict }

func conflict(format string, a ...any) error { return &conflictError{fmt.Sprintf(format, a...)} }

type problems []ProblemField

func (p *problems) add(field, format string, a ...any) {
	*p = append(*p, ProblemField{Field: field, Message: fmt.Sprintf(format, a...)})
}

func (p problems) err() error {
	if len(p) == 0 {
		return nil
	}
	return &InvalidError{Problems: p}
}

// text checks a free-text field: valid UTF-8, no control characters (newlines and tabs only
// when multiline), at most max runes.
func (p *problems) text(field, s string, max int, required, multiline bool) {
	if s == "" {
		if required {
			p.add(field, "required")
		}
		return
	}
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
		p.add(field, "must be valid text of at most %d characters", max)
		return
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			if multiline && (r == '\n' || r == '\t' || r == '\r') {
				continue
			}
			p.add(field, "must not contain control characters")
			return
		}
	}
	if strings.TrimSpace(s) == "" {
		p.add(field, "must not be blank")
	}
}

var emailRE = regexp.MustCompile(`^[^\s@<>"',;()\[\]\\]+@[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?\.[A-Za-z]{2,}$`)

// normEmail validates and lower-cases an address. Display names and comments are refused.
func normEmail(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) > 254 || !emailRE.MatchString(s) {
		return "", false
	}
	if a, err := mail.ParseAddress(s); err != nil || a.Address != s {
		return "", false
	}
	return strings.ToLower(s), true
}
