package jobs

import (
	"context"
	"crypto/hmac"
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

	"github.com/dodwmd/companyinacan/core/obs/obssql"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// dbq is what both *sql.DB and *sql.Tx offer.
type dbq interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type scanner interface{ Scan(dest ...any) error }

// StoreOptions are the collaborators and policy a Store needs.
type StoreOptions struct {
	Keyring   *Keyring
	HashKey   []byte // HMAC key for the email dedupe hash (kept apart from the encryption keys)
	Config    *Config
	Now       func() time.Time
	Syndicate []Syndicator
	Approver  Approver
	Resumes   ResumeStore
	Scanner   Scanner
	EventsOn  bool
}

// Store is the SQLite-backed jobs store. All rules (visibility scope, lifecycle, sealing,
// retention) live here so every API path gets them.
type Store struct {
	db         *sql.DB
	rdb        *sql.DB // read-only pool for lists and feeds (WAL readers do not block the writer)
	keys       *Keyring
	hashKey    []byte
	cfg        *Config
	site       SiteInfo
	now        func() time.Time
	events     bool
	kick       chan struct{}
	synd       []Syndicator
	approver   Approver
	resumes    ResumeStore
	scanner    Scanner
	deny       denyLimiter
	auditMu    sync.Mutex
	rotateHook func()
}

// OpenStore opens (creating if needed) the database at path and applies migrations.
func OpenStore(path string, o StoreOptions) (*Store, error) {
	if o.Keyring == nil {
		return nil, errors.New("a keyring is required: candidate data is sealed before it is stored")
	}
	if len(o.HashKey) < 32 {
		return nil, errors.New("the email hash key must be at least 32 bytes")
	}
	if o.Config == nil {
		o.Config = DefaultConfig()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Approver == nil {
		o.Approver = AutoApprover{}
	}
	if o.Scanner == nil {
		o.Scanner = NoopScanner{}
	}
	if o.Syndicate == nil {
		o.Syndicate = DefaultSyndicators()
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "secure_delete(ON)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// One connection serialises writes (counters, audit chain) and keeps idle memory minimal.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, keys: o.Keyring, hashKey: o.HashKey, cfg: o.Config, site: o.Config.Site(), now: o.Now, events: o.EventsOn,
		kick: make(chan struct{}, 1), synd: o.Syndicate, approver: o.Approver, resumes: o.Resumes, scanner: o.Scanner}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	obssql.WatchDefault(s.db)
	if err := s.seedStages(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	rq := url.Values{}
	rq.Add("_pragma", "busy_timeout(5000)")
	rq.Add("_pragma", "query_only(1)")
	rdb, err := sql.Open("sqlite", "file:"+path+"?"+rq.Encode())
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	rdb.SetMaxOpenConns(3)
	rdb.SetConnMaxIdleTime(time.Minute)
	s.rdb = rdb
	return s, nil
}

// resumesOpen reports whether the apply form may take a resume: a store exists and, when scanning
// is required (the default), a real scanner is wired.
func (s *Store) resumesOpen() bool {
	if s.resumes == nil {
		return false
	}
	_, noop := s.scanner.(NoopScanner)
	return !s.cfg.Apply.ScannerRequired || !noop
}

// Close releases the database.
func (s *Store) Close() error { return errors.Join(s.rdb.Close(), s.db.Close()) }

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) migrate(ctx context.Context) error {
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
	cur, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	scrubAfter := false
	for _, m := range migs {
		if m.version <= cur {
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
		if m.version == 3 {
			scrubAfter = true // see migrations/003: dropped diversity rows must not linger in the file
		}
	}
	if scrubAfter {
		// VACUUM cannot run inside the migration transaction, so it follows the commit. It, and
		// the WAL truncation, remove the dropped per-response diversity rows from the file.
		return s.scrub(ctx, true)
	}
	return nil
}

// SchemaVersion returns the highest applied migration (0 when none).
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

// inTx runs fn in a transaction, committing on success. Only the tx may be used inside fn:
// the pool has one connection, so touching s.db would deadlock.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	select {
	case s.kick <- struct{}{}:
	default:
	}
	return nil
}

func ts(t time.Time) int64 { return t.Unix() }

func fromUnix(v int64) time.Time { return time.Unix(v, 0).UTC() }

func nullTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromUnix(v.Int64)
	return &t
}

func nullUnix(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}

func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

// emailHash is the dedupe key: HMAC-SHA256 under a key kept apart from the encryption keys,
// so a stolen database does not allow a dictionary check of known addresses.
func (s *Store) emailHash(email string) string {
	m := hmac.New(sha256.New, s.hashKey)
	m.Write([]byte(email))
	return hex.EncodeToString(m.Sum(nil))
}

// ---- sealing ---------------------------------------------------------------------------------

func aad(table, id, col string) string { return table + ":" + id + ":" + col }

// seal encrypts a value for storage; empty stays NULL.
func (s *Store) seal(table, id, col, plain string) (sql.NullString, error) {
	if plain == "" {
		return sql.NullString{}, nil
	}
	c, err := s.keys.Seal(plain, aad(table, id, col))
	return sql.NullString{String: c, Valid: err == nil}, err
}

// unseal decrypts a stored value; NULL is "".
func (s *Store) unseal(table, id, col string, v sql.NullString) (string, error) {
	if !v.Valid || v.String == "" {
		return "", nil
	}
	return s.keys.Open(v.String, aad(table, id, col))
}

// sealedCol names one encrypted column for key rotation.
type sealedCol struct{ table, pk, col string }

var sealedCols = []sealedCol{
	{"candidates", "id", "email_sealed"}, {"candidates", "id", "phone_sealed"}, {"candidates", "id", "address_sealed"},
	{"candidates", "id", "salary_exp_sealed"}, {"candidates", "id", "notes_sealed"},
	{"applications", "id", "reject_note_sealed"}, {"applications", "id", "cover_letter_sealed"},
	{"scorecards", "id", "comments_sealed"}, {"stage_history", "seq", "note_sealed"},
}

// RotateKeys re-seals every value written with a retired key under the active key and returns
// how many it re-sealed. Each replacement is a compare-and-swap on the stored ciphertext, so a
// concurrent write is never overwritten with stale data; values that changed meanwhile are
// retried. When it returns 0 the old key can be removed from the keyring.
func (s *Store) RotateKeys(ctx context.Context) (int, error) {
	total := 0
	for pass := 0; pass < 5; pass++ {
		missed := 0
		for _, c := range sealedCols {
			n, m, err := s.rotateColumn(ctx, c)
			total += n
			missed += m
			if err != nil {
				return total, err
			}
		}
		if missed == 0 {
			return total, nil
		}
	}
	return total, errors.New("rotate-keys: values kept changing during rotation; run it again")
}

func (s *Store) rotateColumn(ctx context.Context, c sealedCol) (n, missed int, _ error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s, %s FROM %s WHERE %s IS NOT NULL`, c.pk, c.col, c.table, c.col)) //nolint:gosec // identifiers come from the fixed sealedCols list
	if err != nil {
		return 0, 0, err
	}
	type item struct{ id, sealed string }
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.sealed); err != nil {
			_ = rows.Close()
			return 0, 0, err
		}
		if s.keys.NeedsRotation(it.sealed) {
			items = append(items, it)
		}
	}
	_ = rows.Close()
	for _, it := range items {
		a := aad(c.table, it.id, c.col)
		plain, err := s.keys.Open(it.sealed, a)
		if err != nil {
			return n, missed, fmt.Errorf("rotate %s: %w", a, err)
		}
		fresh, err := s.keys.Seal(plain, a)
		if err != nil {
			return n, missed, err
		}
		if s.rotateHook != nil {
			s.rotateHook()
		}
		res, err := s.db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET %s = ? WHERE %s = ? AND %s = ?`, c.table, c.col, c.pk, c.col), fresh, it.id, it.sealed) //nolint:gosec // fixed identifiers
		if err != nil {
			return n, missed, err
		}
		if k, _ := res.RowsAffected(); k == 0 {
			missed++
			continue
		}
		n++
	}
	return n, missed, nil
}

// ---- audit -----------------------------------------------------------------------------------

// audit appends one hash-chained row. Each row's hash covers the previous hash, so editing or
// removing an old row breaks every later hash. Triggers refuse UPDATE and DELETE. Audit rows
// carry opaque ids and field names, never candidate data.
func (s *Store) audit(ctx context.Context, q dbq, actor, action, resource string, fields []string, outcome string) error {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	var prev string
	if err := q.QueryRowContext(ctx, `SELECT hash FROM audit_log ORDER BY seq DESC LIMIT 1`).Scan(&prev); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	at := s.now().Unix()
	f := strings.Join(fields, ",")
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%s|%s|%s|%s", prev, at, actor, action, resource, f, outcome)))
	_, err := q.ExecContext(ctx, `INSERT INTO audit_log (at, actor, action, resource, fields, outcome, prev, hash) VALUES (?,?,?,?,?,?,?,?)`,
		at, actor, action, resource, f, outcome, prev, hex.EncodeToString(sum[:]))
	return err
}

// auditRead records a read of candidate data in its own transaction and fails closed: when it
// errors the caller must not return the data.
func (s *Store) auditRead(ctx context.Context, p *Principal, resource string, fields []string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		return s.audit(ctx, tx, p.Subject, "read", resource, fields, "ok")
	})
}

// auditDeny records a refused access, best effort and throttled per actor so a client cannot
// fill the log.
func (s *Store) auditDeny(ctx context.Context, p *Principal, action, resource string) {
	record, notice := s.deny.check(p.Subject, s.now())
	if notice {
		_ = s.inTx(ctx, func(tx *sql.Tx) error { return s.audit(ctx, tx, p.Subject, "deny-throttled", "-", nil, "denied") })
	}
	if !record {
		return
	}
	_ = s.inTx(ctx, func(tx *sql.Tx) error { return s.audit(ctx, tx, p.Subject, action, resource, nil, "denied") })
}

// VerifyAudit recomputes the hash chain and reports the first broken entry.
func (s *Store) VerifyAudit(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, at, actor, action, resource, fields, outcome, prev, hash FROM audit_log ORDER BY seq`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	prev := ""
	for rows.Next() {
		var seq, at int64
		var actor, action, resource, fields, outcome, p, hash string
		if err := rows.Scan(&seq, &at, &actor, &action, &resource, &fields, &outcome, &p, &hash); err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%s|%s|%s|%s", prev, at, actor, action, resource, fields, outcome)))
		if p != prev || hash != hex.EncodeToString(sum[:]) {
			return fmt.Errorf("audit log entry %d fails verification", seq)
		}
		prev = hash
	}
	return rows.Err()
}

// AuditEntry is one audit row as the admin API returns it.
type AuditEntry struct {
	Seq      int64     `json:"seq"`
	At       time.Time `json:"at"`
	Actor    string    `json:"actor"`
	Action   string    `json:"action"`
	Resource string    `json:"resource"`
	Fields   string    `json:"fields"`
	Outcome  string    `json:"outcome"`
}

// ListAudit pages through the audit log, newest first (admin only; the caller checks).
func (s *Store) ListAudit(ctx context.Context, before int64, limit int) ([]AuditEntry, int64, error) {
	limit = clampLimit(limit)
	if before <= 0 {
		before = 1 << 62
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT seq, at, actor, action, resource, fields, outcome FROM audit_log WHERE seq < ? ORDER BY seq DESC LIMIT ?`, before, limit+1)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var at int64
		if err := rows.Scan(&e.Seq, &at, &e.Actor, &e.Action, &e.Resource, &e.Fields, &e.Outcome); err != nil {
			return nil, 0, err
		}
		e.At = fromUnix(at)
		out = append(out, e)
	}
	var next int64
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].Seq
	}
	return out, next, rows.Err()
}

func clampLimit(n int) int {
	if n <= 0 {
		return 50
	}
	return min(n, 200)
}

// denyLimiter caps how many denial audit records one actor can generate per minute.
type denyLimiter struct {
	mu  sync.Mutex
	win map[string]*denyWindow
}

type denyWindow struct {
	start time.Time
	n     int
}

const denyAuditPerMinute = 30

func (d *denyLimiter) check(actor string, now time.Time) (record, notice bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.win == nil || len(d.win) > 10000 {
		d.win = map[string]*denyWindow{}
	}
	w := d.win[actor]
	if w == nil || now.Sub(w.start) >= time.Minute {
		w = &denyWindow{start: now}
		d.win[actor] = w
	}
	w.n++
	switch {
	case w.n <= denyAuditPerMinute:
		return true, false
	case w.n == denyAuditPerMinute+1:
		return false, true
	}
	return false, false
}

// scrub removes erased data from places secure_delete does not reach: it rewrites the file
// (VACUUM, which also drops free pages) and truncates the write-ahead log, where the
// pre-erasure pages would otherwise survive until the next automatic checkpoint. Backups and
// snapshots taken earlier still hold the data until they expire; the erasure ledger re-applies
// the erasure after a restore (SPEC.md section 3.7).
func (s *Store) scrub(ctx context.Context, vacuum bool) error {
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

// Approver is the requisition approval hook. A deployment with can-core wires the `approvals`
// platform capability here; the default approves at once (no approval workflow).
type Approver interface {
	// Request starts an approval for a job and returns a reference. When the decision is
	// immediate it returns ApprovalApproved; otherwise ApprovalPending and the decision
	// arrives later: from can-core as approval.decided.v1 (Store.ApplyApprovalDecision), or, for
	// approvers that decide by hand, through Store.DecideApproval. With CoreApprover the manual
	// route is closed: the decision belongs to can-core's engine.
	Request(ctx context.Context, j *Job, requestedBy string) (ref, state string, err error)
}

// AutoApprover approves immediately: the zero-configuration behaviour.
type AutoApprover struct{}

// Request implements Approver.
func (AutoApprover) Request(context.Context, *Job, string) (string, string, error) {
	return "auto", ApprovalApproved, nil
}
