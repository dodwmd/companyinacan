package provision

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrations is the embedded schema history ("migrations/NNN_name.sql"), for the backup and
// restore subcommands (core/modbackup).
func Migrations() fs.FS { return migrationFS }

// dbTimeout bounds every statement group, so a stuck query cannot hold the single connection.
const dbTimeout = 15 * time.Second

// Store is the SQLite state of the module: runs, steps, accounts, tokens, groups, outbox, audit.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// OpenStore opens (creating if needed) the database at path and applies the migrations.
// ":memory:" gives a private in-memory database for tests.
func OpenStore(path string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	dsn := "file::memory:?_pragma=foreign_keys(1)"
	if path != ":memory:" {
		q := url.Values{}
		q.Add("_pragma", "busy_timeout(5000)")
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "foreign_keys(1)")
		q.Add("_pragma", "synchronous(NORMAL)")
		dsn = "file:" + path + "?" + q.Encode()
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection serialises writes (the audit chain, state transitions) and keeps memory low.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	s := &Store{db: db, now: now}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

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

// tx runs fn in a write transaction under dbTimeout. State changes and their audit entries are
// written in the same transaction, so an action that cannot be audited is not recorded as done.
func (s *Store) tx(ctx context.Context, fn func(ctx context.Context, tx *sql.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
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

func (s *Store) read(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, dbTimeout)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func newID(prefix string) string { return prefix + randHex(12) }

// ---- audit -------------------------------------------------------------------------------------

const auditGenesis = "genesis"

// AuditRecord is one stored entry of the chain.
type AuditRecord struct {
	Seq    int64          `json:"seq"`
	Time   time.Time      `json:"time"`
	Actor  string         `json:"actor"`
	Action string         `json:"action"`
	Target string         `json:"target"`
	Detail map[string]any `json:"detail"`
	Hash   string         `json:"hash"`
}

func auditHash(prev string, seq, at int64, actor, action, target, detail string) string {
	h := sha256.New()
	for _, f := range []string{"can-provision-audit-v1", prev, strconv.FormatInt(seq, 10), strconv.FormatInt(at, 10), actor, action, target, detail} {
		fmt.Fprintf(h, "%d:%s|", len(f), f)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// audit appends to the chain inside tx. detail carries ids and counts only; callers never put
// names, emails, tokens or free text in it.
func (s *Store) audit(ctx context.Context, tx *sql.Tx, actor, action, target string, detail map[string]any) error {
	d := "{}"
	if len(detail) > 0 {
		b, err := json.Marshal(detail)
		if err != nil {
			return err
		}
		d = string(b)
	}
	seq, prev := int64(0), auditGenesis
	if err := tx.QueryRowContext(ctx, `SELECT seq, hash FROM audit ORDER BY seq DESC LIMIT 1`).Scan(&seq, &prev); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	at := s.now().Unix()
	_, err := tx.ExecContext(ctx, `INSERT INTO audit(seq,time,actor,action,target,detail,prev_hash,hash) VALUES (?,?,?,?,?,?,?,?)`,
		seq+1, at, actor, action, target, d, prev, auditHash(prev, seq+1, at, actor, action, target, d))
	return err
}

// Audit appends one entry in its own transaction (for actions with no other state change).
func (s *Store) Audit(ctx context.Context, actor, action, target string, detail map[string]any) error {
	return s.tx(ctx, func(ctx context.Context, tx *sql.Tx) error { return s.audit(ctx, tx, actor, action, target, detail) })
}

// AuditList returns entries after seq, oldest first.
func (s *Store) AuditList(ctx context.Context, after int64, limit int) ([]AuditRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	ctx, cancel := s.read(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT seq,time,actor,action,target,detail,hash FROM audit WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AuditRecord
	for rows.Next() {
		var r AuditRecord
		var at int64
		var d string
		if err := rows.Scan(&r.Seq, &at, &r.Actor, &r.Action, &r.Target, &d, &r.Hash); err != nil {
			return nil, err
		}
		r.Time = time.Unix(at, 0).UTC()
		_ = json.Unmarshal([]byte(d), &r.Detail)
		out = append(out, r)
	}
	return out, rows.Err()
}

// VerifyAudit recomputes the chain and reports the first break.
func (s *Store) VerifyAudit(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT seq,time,actor,action,target,detail,prev_hash,hash FROM audit ORDER BY seq`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	prev, want := auditGenesis, int64(1)
	for rows.Next() {
		var seq, at int64
		var actor, action, target, detail, ph, h string
		if err := rows.Scan(&seq, &at, &actor, &action, &target, &detail, &ph, &h); err != nil {
			return err
		}
		if seq != want || ph != prev || h != auditHash(prev, seq, at, actor, action, target, detail) {
			return fmt.Errorf("audit chain broken at seq %d", seq)
		}
		prev, want = h, seq+1
	}
	return rows.Err()
}

// ---- outbox ------------------------------------------------------------------------------------

// enqueue stores an event for delivery inside tx. data holds ids and machine values only.
func (s *Store) enqueue(ctx context.Context, tx *sql.Tx, typ string, data map[string]any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO outbox(id,type,data,created_at) VALUES (?,?,?,?)`, "evt_"+randHex(12), typ, string(b), s.now().Unix())
	return err
}

// OutboxItem is a pending event.
type OutboxItem struct {
	ID, Type string
	Data     []byte
	Attempts int
}

// PendingEvents returns up to limit undelivered events that are due.
func (s *Store) PendingEvents(ctx context.Context, limit int) ([]OutboxItem, error) {
	ctx, cancel := s.read(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id,type,data,attempts FROM outbox WHERE delivered_at=0 AND next_at<=? AND attempts<10 ORDER BY created_at LIMIT ?`, s.now().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []OutboxItem
	for rows.Next() {
		var it OutboxItem
		var d string
		if err := rows.Scan(&it.ID, &it.Type, &d, &it.Attempts); err != nil {
			return nil, err
		}
		it.Data = []byte(d)
		out = append(out, it)
	}
	return out, rows.Err()
}

// MarkEvent records a delivery outcome; failures back off (30 s doubling) and stop after 10 tries.
func (s *Store) MarkEvent(ctx context.Context, id string, delivered bool) error {
	ctx, cancel := s.read(ctx)
	defer cancel()
	if delivered {
		_, err := s.db.ExecContext(ctx, `UPDATE outbox SET delivered_at=? WHERE id=?`, s.now().Unix(), id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE outbox SET attempts=attempts+1, next_at=? + 30*(1<<MIN(attempts,8)) WHERE id=?`, s.now().Unix(), id)
	return err
}

// Prune deletes delivered events and seen-event markers older than the retention.
func (s *Store) Prune(ctx context.Context, olderThan time.Duration) error {
	cut := s.now().Add(-olderThan).Unix()
	return s.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, q := range []string{`DELETE FROM outbox WHERE delivered_at>0 AND delivered_at<?`, `DELETE FROM seen_events WHERE seen_at<?`} {
			if _, err := tx.ExecContext(ctx, q, cut); err != nil {
				return err
			}
		}
		return nil
	})
}

// firstSeen records an event id; it reports false when the id was already recorded.
func (s *Store) firstSeen(ctx context.Context, id string) (bool, error) {
	ctx, cancel := s.read(ctx)
	defer cancel()
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO seen_events(event_id, seen_at) VALUES (?,?)`, id, s.now().Unix())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// forgetSeen removes the marker again when the event could not be handled, so its redelivery runs.
func (s *Store) forgetSeen(ctx context.Context, id string) {
	ctx, cancel := s.read(ctx)
	defer cancel()
	_, _ = s.db.ExecContext(ctx, `DELETE FROM seen_events WHERE event_id=?`, id)
}
