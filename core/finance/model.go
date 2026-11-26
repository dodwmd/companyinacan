package finance

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Errors the store returns; the API maps them to HTTP statuses.
var (
	ErrNotFound            = errors.New("not found")
	ErrConflict            = errors.New("conflict")
	ErrVersionMismatch     = errors.New("version mismatch")
	ErrIdempotencyMismatch = errors.New("idempotency key reused with a different request")
	ErrForbidden           = errors.New("forbidden")
	// ErrSeparation is returned when someone tries to approve their own work.
	ErrSeparation = errors.New("separation of duties: the creator of a record cannot approve it")
	ErrLocked     = errors.New("the period is locked")
	// ErrNotImplemented is a documented adapter stub (xero, quickbooks, wave): no network call is made.
	ErrNotImplemented = errors.New("not implemented")
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

// Field limits.
const (
	maxName     = 200
	maxText     = 5000
	maxBody     = 100000
	maxSubject  = 254
	maxLines    = 200
	maxEntries  = 500
	maxVars     = 50
	maxPageSize = 100
)

var (
	projectRE  = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)
	taskRE     = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}-[1-9][0-9]{0,9}$`)
	seriesRE   = regexp.MustCompile(`^[A-Z]{1,6}$`)
	codeRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,29}$`)
	categoryRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,29}$`)
	varRE      = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	sha256RE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	filenameRE = regexp.MustCompile(`^[^/\\\x00-\x1f]{1,255}$`)
)

// reservedSeries are the counters of other document classes. An invoice cannot use them, so a
// purchase order or credit note number can never be burned (or duplicated) through an invoice.
var reservedSeries = []string{"CN", "PO", "JE"}

func validInvoiceSeries(s string) error {
	if !seriesRE.MatchString(s) {
		return invalid("series", "1 to 6 capital letters")
	}
	if slices.Contains(reservedSeries, s) {
		return invalid("series", "%s is reserved for credit notes and purchase orders", s)
	}
	return nil
}

type scanner interface{ Scan(...any) error }

// canonSubject folds a subject for comparison: Unicode compatibility normalisation, trimmed,
// case-folded, so "Alice", "alice" and "ALICE " are one person for separation of duties.
func canonSubject(s string) string {
	s = norm.NFKC.String(s)
	// Invisible format characters (zero-width space and joiners, bidi marks, soft hyphen) make
	// two subjects look identical to a person and different to a comparison.
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	return cases.Fold().String(strings.TrimSpace(s))
}

func sameSubject(a, b string) bool { return canonSubject(a) == canonSubject(b) }

func validSubject(s string) bool {
	return s != "" && utf8.RuneCountInString(s) <= maxSubject && !strings.ContainsAny(s, "\x00\r\n\t") && strings.TrimSpace(s) == s
}

func validText(s string, max int) bool {
	return utf8.ValidString(s) && utf8.RuneCountInString(s) <= max && !strings.ContainsRune(s, 0)
}

func oneLineText(s string, max int) bool {
	return validText(s, max) && !strings.ContainsAny(s, "\r\n")
}

func validDate(s string) bool {
	t, err := time.Parse(time.DateOnly, s)
	return err == nil && t.Format(time.DateOnly) == s
}

func validEmail(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > 254 || strings.ContainsAny(s, "\r\n<>") {
		return false
	}
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s
}

func dedupe[T comparable](ls []T) []T {
	out := make([]T, 0, len(ls))
	for _, l := range ls {
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return 50
	case n > maxPageSize:
		return maxPageSize
	}
	return n
}

// Opt is an optional JSON field that tells "absent" from "null" from "a value", which a
// PATCH needs: absent leaves the field alone, null clears it.
type Opt[T any] struct {
	Set  bool
	Null bool
	V    T
}

// UnmarshalJSON implements json.Unmarshaler.
func (o *Opt[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Null = true
		return nil
	}
	return json.Unmarshal(b, &o.V)
}

// today formats a time as a date in UTC.
func dateOf(t time.Time) string { return t.UTC().Format(time.DateOnly) }

func parseDate(s string) time.Time {
	t, _ := time.Parse(time.DateOnly, s)
	return t
}

// bounded runs a read-side database call under a deadline so one slow report cannot hold
// the pool.
const readTimeout = 5 * time.Second
