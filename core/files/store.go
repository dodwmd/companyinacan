package files

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/obs/obssql"
	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrations is the embedded schema history ("migrations/NNN_name.sql"), for the backup and
// restore subcommands (core/modbackup).
func Migrations() fs.FS { return migrationFS }

// dbTimeout bounds every statement group, so a stuck query cannot hold the single connection.
const dbTimeout = 15 * time.Second

type dbq interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// StoreOptions are the collaborators of a Store.
type StoreOptions struct {
	Keyring    *Keyring
	Backend    Backend
	Scanner    Scanner
	LinkSecret []byte
	Config     *Config
	Now        func() time.Time
	// EventsOn queues integrity events in the outbox (a deployment with can-core).
	EventsOn bool
}

// Store is the files service: SQLite metadata, a blob Backend, sealed fields and the audit chain.
// Every method takes the Principal it acts for and decides access itself; handlers hold no policy.
type Store struct {
	db         *sql.DB
	keys       *Keyring
	backend    Backend
	scanner    Scanner
	linkSecret []byte
	cfg        *Config
	now        func() time.Time
	events     bool
	kick       chan struct{}
	deny       denyLimiter
	rotateHook func()
	shareFails *failLimiter
	stageDir   string
	uploads    chan struct{} // one slot per upload in progress
	uploadMu   sync.Mutex
	uploadsBy  map[string]int // subject -> slots held
}

// OpenStore opens (creating if needed) the database at path and applies migrations.
func OpenStore(path string, o StoreOptions) (*Store, error) {
	switch {
	case o.Keyring == nil:
		return nil, errors.New("a keyring is required: names and descriptions are sealed before they are stored")
	case o.Backend == nil:
		return nil, errors.New("a blob backend is required")
	case len(o.LinkSecret) < 32:
		return nil, errors.New("the share link secret must be at least 32 bytes")
	}
	if o.Config == nil {
		o.Config = DefaultConfig()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Scanner == nil {
		o.Scanner = NoopScanner{}
	}
	dsn := "file::memory:?_pragma=foreign_keys(1)"
	if path != ":memory:" {
		q := url.Values{}
		q.Add("_pragma", "busy_timeout(5000)")
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "foreign_keys(1)")
		q.Add("_pragma", "synchronous(NORMAL)")
		q.Add("_pragma", "secure_delete(ON)")
		dsn = "file:" + path + "?" + q.Encode()
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection serialises writes (the audit chain, quota checks) and keeps idle memory low.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	s := &Store{db: db, keys: o.Keyring, backend: o.Backend, scanner: o.Scanner, linkSecret: o.LinkSecret, cfg: o.Config, now: o.Now,
		events: o.EventsOn, kick: make(chan struct{}, 1), shareFails: newFailLimiter(o.Config.Share.FailuresPerMinute, o.Now)}
	s.uploads = make(chan struct{}, o.Config.Limits.MaxConcurrentUploads)
	s.uploadsBy = map[string]int{}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	obssql.WatchDefault(s.db)
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks that the database and the blob backend answer.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return err
	}
	return s.backend.Ping(ctx)
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	var cur sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&cur); err != nil {
		return err
	}
	ents, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		v, err := strconv.Atoi(strings.SplitN(n, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: bad name", n)
		}
		if int64(v) <= cur.Int64 {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + n)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", n, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?,?)`, v, s.now().Unix()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// inTx runs fn in a write transaction under dbTimeout. State changes and their audit entries are
// written in the same transaction, so an action that cannot be audited is not recorded as done.
// fn must use only tx: the store has one connection and a call on s.db would wait forever.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
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

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// idHex is the random part of a new id; the web UI's golden tests replace it so pages are stable.
var idHex = func() string { return randHex(12) }

func newID(prefix string) string { return prefix + "_" + idHex() }

func ts(t time.Time) int64 { return t.Unix() }

func fromUnix(n int64) time.Time { return time.Unix(n, 0).UTC() }

func nullTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromUnix(v.Int64)
	return &t
}

func (s *Store) wake() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// ---- audit -------------------------------------------------------------------------------------

// audit appends to the hash chain inside tx. Each entry's hash covers the previous one, so
// removing or editing an old entry breaks every later hash. Triggers refuse UPDATE and DELETE.
// Entries carry opaque ids and field names, never names or content.
func (s *Store) audit(ctx context.Context, q dbq, actor, action, resource string, fields []string, outcome string) error {
	var prev string
	if err := q.QueryRowContext(ctx, `SELECT hash FROM audit_log ORDER BY seq DESC LIMIT 1`).Scan(&prev); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	at := s.now().Unix()
	f := strings.Join(fields, ",")
	_, err := q.ExecContext(ctx, `INSERT INTO audit_log (at, actor, action, resource, fields, outcome, prev, hash) VALUES (?,?,?,?,?,?,?,?)`,
		at, actor, action, resource, f, outcome, prev, auditHash(prev, at, actor, action, resource, f, outcome))
	return err
}

func auditHash(prev string, at int64, actor, action, resource, fields, outcome string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%s|%s|%s|%s", prev, at, actor, action, resource, fields, outcome)))
	return hex.EncodeToString(sum[:])
}

// auditRead records a read in its own transaction and fails closed: when it errors the caller
// must not return the data (ErrUnavailable).
func (s *Store) auditRead(ctx context.Context, actor, action, resource string) error {
	if err := s.inTx(ctx, func(tx *sql.Tx) error { return s.audit(ctx, tx, actor, action, resource, nil, "ok") }); err != nil {
		return fmt.Errorf("%w: the access could not be recorded", ErrUnavailable)
	}
	return nil
}

// auditDeny records a refused access, best effort and throttled per actor so a client cannot
// fill the log.
func (s *Store) auditDeny(ctx context.Context, actor, action, resource string) {
	record, notice := s.deny.check(actor, s.now())
	if notice {
		_ = s.inTx(ctx, func(tx *sql.Tx) error { return s.audit(ctx, tx, actor, "deny-throttled", "-", nil, "denied") })
	}
	if record {
		_ = s.inTx(ctx, func(tx *sql.Tx) error { return s.audit(ctx, tx, actor, action, resource, nil, "denied") })
	}
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
		if p != prev || hash != auditHash(prev, at, actor, action, resource, fields, outcome) {
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
	Fields   string    `json:"fields,omitempty"`
	Outcome  string    `json:"outcome"`
}

// ListAudit pages through the audit log, newest first. Admin only.
func (s *Store) ListAudit(ctx context.Context, p *Principal, before int64, limit int) (Page[AuditEntry], error) {
	if !p.Has(PermAdmin) {
		return Page[AuditEntry]{}, ErrForbidden
	}
	limit = clampLimit(limit)
	if before <= 0 {
		before = 1 << 62
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT seq, at, actor, action, resource, fields, outcome FROM audit_log WHERE (seq < ?) ORDER BY seq DESC LIMIT ?`, before, limit+1)
	if err != nil {
		return Page[AuditEntry]{}, err
	}
	defer func() { _ = rows.Close() }()
	out := Page[AuditEntry]{Items: []AuditEntry{}}
	for rows.Next() {
		var e AuditEntry
		var at int64
		if err := rows.Scan(&e.Seq, &at, &e.Actor, &e.Action, &e.Resource, &e.Fields, &e.Outcome); err != nil {
			return out, err
		}
		e.At = fromUnix(at)
		out.Items = append(out.Items, e)
	}
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		out.Next = strconv.FormatInt(out.Items[limit-1].Seq, 10)
	}
	return out, rows.Err()
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

// failLimiter counts FAILED attempts per client address. A request is only ever refused because
// of failures it follows: a valid credential is checked first and never throttled.
type failLimiter struct {
	mu    sync.Mutex
	limit int
	now   func() time.Time
	win   map[string]*denyWindow
}

func newFailLimiter(limit int, now func() time.Time) *failLimiter {
	return &failLimiter{limit: limit, now: now, win: map[string]*denyWindow{}}
}

// blocked reports whether key already used up its failures this minute.
func (l *failLimiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.win[key]
	return w != nil && l.now().Sub(w.start) < time.Minute && w.n >= l.limit
}

// fail records one failure.
func (l *failLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.win) >= 10000 { // bounded: forget the old windows first, then everything
		for k, w := range l.win {
			if now.Sub(w.start) >= time.Minute {
				delete(l.win, k)
			}
		}
		if len(l.win) >= 10000 {
			l.win = map[string]*denyWindow{}
		}
	}
	w := l.win[key]
	if w == nil || now.Sub(w.start) >= time.Minute {
		w = &denyWindow{start: now}
		l.win[key] = w
	}
	w.n++
}

// scrub removes erased data from places secure_delete does not reach: it rewrites the file
// (VACUUM, which also drops free pages) and truncates the write-ahead log, where the
// pre-erasure pages would otherwise survive until the next automatic checkpoint. Backups taken
// earlier still hold the data until they expire; the erasure ledger re-applies the erasure
// after a restore (SPEC.md section 3.7).
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

// ---- sealing -----------------------------------------------------------------------------------

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

type sealedCol struct{ table, pk, col string }

var sealedCols = []sealedCol{
	{"spaces", "id", "name_sealed"}, {"folders", "id", "name_sealed"},
	{"files", "id", "name_sealed"}, {"files", "id", "description_sealed"}, {"shares", "id", "label_sealed"},
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
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s, %s FROM %s WHERE (%s IS NOT NULL)`, c.pk, c.col, c.table, c.col)) //nolint:gosec // identifiers come from the fixed sealedCols list
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
		res, err := s.db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET %s = ? WHERE (%s = ?) AND (%s = ?)`, c.table, c.col, c.pk, c.col), fresh, it.id, it.sealed) //nolint:gosec // fixed identifiers
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
