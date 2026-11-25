package board

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Errors mapped to HTTP statuses by the API layer.
var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
	ErrConflict  = errors.New("conflict")
)

// InvalidError is a client mistake (HTTP 400).
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &InvalidError{Msg: fmt.Sprintf(format, a...)} }

// ConflictError carries the reason for a 409.
type ConflictError struct{ Msg string }

func (e *ConflictError) Error() string { return e.Msg }
func (e *ConflictError) Unwrap() error { return ErrConflict }

func conflict(format string, a ...any) error { return &ConflictError{Msg: fmt.Sprintf(format, a...)} }

// ErrSeparation is returned when the same person would act twice in a chain that must have two.
// It maps to 403 separation_of_duties.
var ErrSeparation = errors.New("separation of duties: the person who proposed, drafted or is named in a record cannot be the one who approves it")

// ---- quorum and tally: exact integer arithmetic --------------------------------------------------

// QuorumRequired is the number of eligible members that must be present: the smallest integer that is at
// least num/den of the eligible members, at least min, and never less than one. Integers only: no
// float rounding can decide whether a meeting was quorate.
func QuorumRequired(eligible, num, den, min int) int {
	if den < 1 {
		den = 1
	}
	req := 0
	if eligible > 0 && num > 0 {
		req = int((int64(eligible)*int64(num) + int64(den) - 1) / int64(den))
	}
	if req < min {
		req = min
	}
	if req < 1 {
		req = 1
	}
	return req
}

// TallyPasses applies a pass threshold of num/den of the votes cast (for and against: abstentions are
// not votes cast). A strict threshold needs more than the fraction, an inclusive one at least the
// fraction. A resolution with no vote for it never passes. Simple majority is 1/2 strict; unanimity of
// those voting is 1/1 inclusive.
func TallyPasses(forN, against, _abstain, num, den int, inclusive bool) bool {
	_ = _abstain
	if forN <= 0 || den < 1 {
		return false
	}
	lhs := int64(forN) * int64(den)
	rhs := int64(num) * (int64(forN) + int64(against))
	if inclusive {
		return lhs >= rhs
	}
	return lhs > rhs
}

// ---- dates and text -----------------------------------------------------------------------

const dateLayout = "2006-01-02"

// minYear and maxYear bound every date so calendar loops stay bounded.
const (
	minYear = 1970
	maxYear = 2200
)

func parseDate(s string) (time.Time, error) {
	t, err := time.Parse(dateLayout, s)
	if err != nil || t.Year() < minYear || t.Year() > maxYear {
		return time.Time{}, invalid("date %q must be YYYY-MM-DD between %d and %d", s, minYear, maxYear)
	}
	return t, nil
}

func validText(name, v string, max int, required bool) error {
	if required && strings.TrimSpace(v) == "" {
		return invalid("%s is required", name)
	}
	if utf8.RuneCountInString(v) > max || !utf8.ValidString(v) {
		return invalid("%s is too long or not valid UTF-8 (max %d characters)", name, max)
	}
	for _, r := range v {
		if r < 0x20 && r != '\n' && r != '\t' {
			return invalid("%s contains control characters", name)
		}
	}
	return nil
}

func validLine(name, v string, max int, required bool) error {
	if strings.ContainsAny(v, "\n\r\t") {
		return invalid("%s must be a single line", name)
	}
	return validText(name, v, max, required)
}

// canonSubject folds a subject for comparison: Unicode compatibility normalisation, invisible
// format characters removed, trimmed, case-folded. "Alice", "alice " and a zero-width-space
// variant are one person for separation of duties.
func canonSubject(s string) string {
	s = norm.NFKC.String(s)
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	return cases.Fold().String(strings.TrimSpace(s))
}

var (
	idRe       = regexp.MustCompile(`^(bdy|mbr|mtg|agi|pki|cfl|res|vot|min|act|lge|reg|ctc|obl|fil|doc|scl)_[0-9a-f]{24}$`)
	personIDRe = regexp.MustCompile(`^pers_[0-9a-f]{24}$`)
	subjectRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+-]{0,127}$`)
	// opaque references to other modules: a document id, an equity class or entity id, a work issue id
	refRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
)

// validID returns ErrNotFound for a malformed id: it is indistinguishable from an unknown one.
func validID(prefix, id string) error {
	if !idRe.MatchString(id) || !strings.HasPrefix(id, prefix+"_") {
		return ErrNotFound
	}
	return nil
}

func validRef(name, v string, required bool) error {
	if v == "" && !required {
		return nil
	}
	if !refRe.MatchString(v) {
		return invalid("%s must be an opaque reference of 1 to 128 characters (letters, digits and . _ : / -)", name)
	}
	return nil
}

func validPerson(name, v string) error {
	if !personIDRe.MatchString(v) {
		return invalid("%s must be a directory person id (pers_ followed by 24 hex digits)", name)
	}
	return nil
}
