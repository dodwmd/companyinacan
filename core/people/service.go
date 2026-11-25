// Package people implements the in-house people service: the `directory` capability
// (people records, org structure, employment lifecycle, onboarding checklists) and the
// `performance` capability (1:1s and goals). It holds the most sensitive data classes in the
// platform, so it is deny-by-default: every operation goes through the policy layer
// (policy.go), sensitive reads are audited per field, and sensitive columns are encrypted at
// rest.
//
// This software is a tool, not legal, HR or payroll advice. Compliance with employment and
// data-protection law is the operator's responsibility.
package people

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)
)

//go:embed migrations/*.sql
var migrationFS embed.FS

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

// Principal is an authenticated caller as the Authenticator reports it.
type Principal struct {
	Subject string // SSO subject; links to people.sso_subject
	Roles   []string
}

// Options configure a Service.
type Options struct {
	Keyring   *Keyring
	Auditor   Auditor
	Retention map[string]RetentionRule
	Now       func() time.Time
}

// Service is the application layer: authorisation, storage, audit and events.
type Service struct {
	db        *sql.DB
	keys      *Keyring
	audit     Auditor
	retention map[string]RetentionRule
	now       func() time.Time
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string, opt Options) (*Service, error) {
	if opt.Keyring == nil {
		return nil, errors.New("people: a keyring is required (sensitive fields are always encrypted)")
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "secure_delete(ON)") // erased data is overwritten, not left in free pages
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // serialises writes (cycle checks rely on it) and keeps idle RAM low
	s := &Service{db: db, keys: opt.Keyring, audit: opt.Auditor, retention: opt.Retention, now: opt.Now}
	if s.now == nil {
		s.now = time.Now
	}
	if s.retention == nil {
		s.retention = DefaultRetention()
	}
	if s.audit == nil {
		s.audit = &SQLAuditor{db: db, now: s.now}
	}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close releases the database.
func (s *Service) Close() error { return s.db.Close() }

// Ping checks that the database answers.
func (s *Service) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Service) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	type mig struct {
		version int
		name    string
	}
	var migs []mig
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil {
			return fmt.Errorf("bad migration name %q", e.Name())
		}
		migs = append(migs, mig{v, e.Name()})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	var cur sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&cur); err != nil {
		return err
	}
	for _, m := range migs {
		if int64(m.version) <= cur.Int64 {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + m.name)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, m.version, s.now().Unix()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ---- helpers ------------------------------------------------------------------------------

func newID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

var (
	personIDRe = regexp.MustCompile(`^pers_[0-9a-f]{24}$`)
	issueIDRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
	subjectRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+-]{0,127}$`)
)

const dateLayout = "2006-01-02"

func (s *Service) today() string { return s.now().UTC().Format(dateLayout) }

func validDate(d string) error {
	if _, err := time.Parse(dateLayout, d); err != nil {
		return invalid("date %q must be YYYY-MM-DD", d)
	}
	return nil
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

func validEmail(v string) error {
	if v == "" {
		return nil
	}
	a, err := mail.ParseAddress(v)
	if err != nil || a.Address != v || len(v) > 254 {
		return invalid("work_email is not a valid address")
	}
	return nil
}

type sqlTx = sql.Tx

func (s *Service) tx(ctx context.Context, fn func(tx *sqlTx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

type querier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---- actor resolution and relationship facts -----------------------------------------------

// ResolveActor links a Principal to their person record. A subject with a person record that is
// erased resolves as a blocked actor (status former).
func (s *Service) ResolveActor(ctx context.Context, p *Principal) (Actor, error) {
	a := Actor{Subject: p.Subject, Roles: p.Roles}
	if p.Subject == "" {
		return a, nil
	}
	var id, status string
	err := s.db.QueryRowContext(ctx, `SELECT id, status FROM people WHERE sso_subject = ?`, p.Subject).Scan(&id, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	a.PersonID, a.HasPerson, a.Status = id, true, Status(status)
	return a, nil
}

func (a Actor) auditID() string {
	if a.HasPerson {
		return a.PersonID
	}
	return "svc:" + a.Subject
}

const maxChainDepth = 64

// managerChain returns the ids of the manager, the manager's manager, and so on. It stops at
// maxChainDepth, so even a corrupted cyclic chain terminates.
func managerChain(ctx context.Context, q querier, personID string) ([]string, error) {
	var chain []string
	seen := map[string]bool{personID: true}
	cur := personID
	for i := 0; i < maxChainDepth; i++ {
		var m sql.NullString
		err := q.QueryRowContext(ctx, `SELECT manager_id FROM people WHERE id = ?`, cur).Scan(&m)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !m.Valid) {
			return chain, nil
		}
		if err != nil {
			return nil, err
		}
		if seen[m.String] {
			return chain, nil
		}
		seen[m.String] = true
		chain = append(chain, m.String)
		cur = m.String
	}
	return chain, nil
}

type subjectInfo struct {
	status  Status
	teamID  string
	manager string
	chain   []string
	erased  bool
}

func (s *Service) subject(ctx context.Context, q querier, id string) (*subjectInfo, error) {
	var st string
	var team, mgr sql.NullString
	var erased sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT status, team_id, manager_id, erased_at FROM people WHERE id = ?`, id).Scan(&st, &team, &mgr, &erased)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	chain, err := managerChain(ctx, q, id)
	if err != nil {
		return nil, err
	}
	return &subjectInfo{status: Status(st), teamID: team.String, manager: mgr.String, chain: chain, erased: erased.Valid}, nil
}

// factFor computes the relationship of the actor to a person.
func (s *Service) factFor(ctx context.Context, q querier, a Actor, personID string) (Fact, *subjectInfo, error) {
	si, err := s.subject(ctx, q, personID)
	if err != nil {
		return Fact{}, nil, err
	}
	f := Fact{Status: si.status}
	if a.HasPerson {
		f.Self = a.PersonID == personID
		f.Direct = si.manager != "" && si.manager == a.PersonID
		for _, c := range si.chain {
			if c == a.PersonID {
				f.Chain = true
			}
		}
		var myTeam sql.NullString
		if err := q.QueryRowContext(ctx, `SELECT team_id FROM people WHERE id = ?`, a.PersonID).Scan(&myTeam); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return f, si, err
		}
		f.SameTeam = si.teamID != "" && myTeam.String == si.teamID
	}
	return f, si, nil
}
