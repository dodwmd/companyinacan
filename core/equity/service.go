// Package equity implements the in-house equity service: legal entities, share classes, plans
// and pools, stakeholders, option grants with vesting, exercises, an append-only share ledger,
// cap-table views as of any date, illustrative round modelling, valuation records and audited
// exports. The `equity` capability.
//
// Equity is a SENSITIVE data class (constitution V): in-house only, deny-by-default policy layer,
// per-field access, audited reads (fail closed), sealed sensitive columns, retention, legal hold
// and erasure.
//
// THIS SOFTWARE IS A TOOL, NOT LEGAL, TAX OR SECURITIES ADVICE. It records and computes what
// the data says. Jurisdiction-specific rules (US 409A and ISO/NSO limits, tax treatment, 83(b)
// elections, ESOP schemes, securities exemptions, share-register law) are NOT implemented; they
// are modelled only as data fields and documented limits (docs/equity.md).
package equity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// readTimeout bounds one read; writeTimeout bounds one write transaction. Both cap how long a
// request can hold the single database connection.
const (
	readTimeout  = 10 * time.Second
	writeTimeout = 15 * time.Second
)

// Options configure a Service.
type Options struct {
	Keyring   *Keyring
	Auditor   Auditor
	Retention map[string]RetentionRule
	Approvals ApprovalEngine // optional: where board approval requests are also registered
	Now       func() time.Time
}

// Service is the application layer: authorisation, storage, audit and events.
type Service struct {
	db         *sql.DB
	keys       *Keyring
	audit      Auditor
	deny       denyLimiter
	approvals  ApprovalEngine
	idemMu     sync.Mutex
	rotateHook func() // tests: runs between reading a value and its compare-and-swap
	retention  map[string]RetentionRule
	now        func() time.Time
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string, opt Options) (*Service, error) {
	if opt.Keyring == nil {
		return nil, errors.New("equity: a keyring is required (sensitive fields are always encrypted)")
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
	db.SetMaxOpenConns(1) // serialises writes (pool and balance checks rely on it) and keeps idle RAM low
	s := &Service{db: db, keys: opt.Keyring, audit: opt.Auditor, retention: opt.Retention, now: opt.Now, approvals: opt.Approvals}
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

func (s *Service) today() string { return s.now().UTC().Format(dateLayout) }

type sqlTx = sql.Tx

// tx runs fn in a write transaction bounded by writeTimeout.
func (s *Service) tx(ctx context.Context, fn func(ctx context.Context, tx *sqlTx) error) error {
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

// readCtx bounds one read.
func readCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, readTimeout)
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

// scrub removes erased data from places secure_delete does not reach: it rewrites the file
// (VACUUM, which also drops free pages) and truncates the write-ahead log, where the
// pre-erasure pages would otherwise survive until the next automatic checkpoint. Backups and
// snapshots taken earlier still hold the data until they expire (SPEC.md section 3.7; the E08
// erasure ledger re-applies the erasure after a restore).
func (s *Service) scrub(ctx context.Context, vacuum bool) error {
	if vacuum {
		if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
			return fmt.Errorf("vacuum: %w", err)
		}
	}
	var busy, logFrames, checkpointed int
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("wal checkpoint: %w", err)
	}
	if busy != 0 {
		return errors.New("wal checkpoint was blocked; erased data may remain in the write-ahead log")
	}
	return nil
}

func (a Actor) auditID() string {
	if a.PersonID != "" {
		return a.PersonID
	}
	return "sub:" + canonSubject(a.Subject)
}

// ResolveActor turns an authenticated Principal into an Actor. Roles were filtered by the
// authenticator; a caller with no known role is blocked by the policy layer.
func (s *Service) ResolveActor(_ context.Context, p *Principal) (Actor, error) {
	return Actor{Subject: p.Subject, Roles: p.Roles, PersonID: p.PersonID}, nil
}

// authorize returns ErrForbidden (and audits the denial) when the policy says no.
func (s *Service) authorize(ctx context.Context, a Actor, act Action, f Fact, resource string) error {
	if !Decide(a, act, f) {
		s.auditDeny(ctx, a, string(act), resource, nil)
		return ErrForbidden
	}
	return nil
}

// selfFact reports whether the stakeholder is the actor's own record.
func (s *Service) selfFact(ctx context.Context, q querier, a Actor, stakeholderID string) (Fact, error) {
	if a.PersonID == "" {
		return Fact{}, nil
	}
	var pid sql.NullString
	err := q.QueryRowContext(ctx, `SELECT person_id FROM stakeholders WHERE id = ?`, stakeholderID).Scan(&pid)
	if errors.Is(err, sql.ErrNoRows) {
		return Fact{}, nil
	}
	if err != nil {
		return Fact{}, err
	}
	return Fact{Self: pid.Valid && pid.String == a.PersonID}, nil
}

// ---- idempotency --------------------------------------------------------------------------

const maxIdemKey = 128

// idempotent runs create at most once per (actor, route, key). A repeat with the same body
// returns the id of the first result (the caller re-reads it under the caller's current
// rights); the same key with another body is a conflict. Without a key it just runs create.
func (s *Service) idempotent(ctx context.Context, a Actor, route, key string, body []byte, create func() (string, error)) (id string, replay bool, err error) {
	if key == "" {
		id, err = create()
		return id, false, err
	}
	s.idemMu.Lock() // one create per key at a time: two concurrent first requests cannot both create
	defer s.idemMu.Unlock()
	if len(key) > maxIdemKey || strings.ContainsAny(key, "\r\n\x00") {
		return "", false, invalid("Idempotency-Key must be at most %d characters on one line", maxIdemKey)
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	actor := a.auditID()
	var prevHash, prevID string
	err = s.db.QueryRowContext(ctx, `SELECT body_hash, resource_id FROM idempotency WHERE actor = ? AND route = ? AND key = ?`, actor, route, key).Scan(&prevHash, &prevID)
	switch {
	case err == nil:
		if prevHash != hash {
			return "", false, conflict("Idempotency-Key was already used with a different request")
		}
		return prevID, true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", false, err
	}
	id, err = create()
	if err != nil {
		return "", false, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO idempotency (key, actor, route, body_hash, resource_id, created_at) VALUES (?,?,?,?,?,?)`, key, actor, route, hash, id, s.now().Unix())
	return id, false, err
}

// pruneIdempotency drops keys older than a day.
func (s *Service) pruneIdempotency(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM idempotency WHERE created_at < ?`, s.now().Add(-24*time.Hour).Unix())
	return err
}
