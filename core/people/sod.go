package people

import (
	"context"
	"database/sql"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Bounds on database work: every E14b read and write runs under a deadline, so a stuck
// statement cannot hold the single connection (and with it every request) forever.
const (
	readTimeout  = 10 * time.Second
	writeTimeout = 15 * time.Second
)

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

// sodOK is the single separation-of-duties rule for reviews and leave: two actions in one chain
// need two different people. Subjects are compared canonically, and a person is one person
// across their SSO subject and their directory person id. Everything that must be done by
// someone else calls this and nothing else. It is fail-closed: an actor with no identity at all
// is refused.
func sodOK(actor Actor, others ...string) bool {
	me, pid := canonSubject(actor.Subject), canonSubject(actor.PersonID)
	if me == "" && pid == "" {
		return false
	}
	for _, o := range others {
		o = canonSubject(o)
		if o == "" {
			continue
		}
		if o == me || (pid != "" && o == pid) {
			return false
		}
	}
	return true
}

// idents lists the identities of a person for separation-of-duties chains: the person id and
// the SSO subject, both canonical.
func (s *Service) idents(ctx context.Context, q querier, personID string) []string {
	out := []string{canonSubject(personID)}
	var subj sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT sso_subject FROM people WHERE id = ?`, personID).Scan(&subj); err == nil && subj.Valid {
		out = append(out, canonSubject(subj.String))
	}
	return out
}

// wtx runs fn in a write transaction bounded by writeTimeout. The audit entry, when the
// auditor can join the transaction (the default one can), commits with the change or not at all.
func (s *Service) wtx(ctx context.Context, fn func(ctx context.Context, tx *sqlTx) error) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// txAudit runs fn and records the audit entry in the same transaction: no entry, no mutation.
func (s *Service) txAudit(ctx context.Context, a Actor, action, resource string, fields []string, fn func(ctx context.Context, tx *sqlTx) error) error {
	return s.wtx(ctx, func(ctx context.Context, tx *sqlTx) error {
		if err := fn(ctx, tx); err != nil {
			return err
		}
		return s.auditIn(ctx, tx, a, action, resource, fields)
	})
}

// readCtx bounds one read.
func readCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, readTimeout)
}
