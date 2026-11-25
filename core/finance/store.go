package finance

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// dbq is what both *sql.DB and *sql.Tx offer; helpers take it so they work in either.
type dbq interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store is the SQLite-backed finance store. All rules (project scoping, hierarchy, workflow,
// concurrency) live here so every API path gets them.
type Store struct {
	db  *sql.DB
	rdb *sql.DB // read-only pool for searches and boards (WAL readers do not block the writer)
	now func() time.Time
	// events turns the outbox on; with no webhook configured nothing is queued.
	events bool
	kick   chan struct{}
	// base is the company's base currency; payInstr the default payment instructions text.
	base     string
	payInstr string
	approver Approver
}

// OpenStore opens (creating if needed) the database at path and applies migrations.
func OpenStore(path string) (*Store, error) {
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
	// One connection: serialises writes (so counters and rank neighbours cannot race) and
	// keeps idle memory minimal.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now, kick: make(chan struct{}, 1), base: "USD", approver: ScopeApprover{}}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
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

func nullStr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

// AuditEntry is one recorded admin action.
type AuditEntry struct {
	ID     int64           `json:"id"`
	Actor  string          `json:"actor"`
	Action string          `json:"action"`
	Target string          `json:"target"`
	Detail json.RawMessage `json:"detail"`
	At     time.Time       `json:"at"`
}

func (s *Store) audit(ctx context.Context, q dbq, actor, action, target string, detail any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO audit_log (actor, action, target, detail, at) VALUES (?, ?, ?, ?, ?)`, actor, action, target, string(b), ts(s.now()))
	return err
}

// ListAudit returns audit entries, newest first, after the cursor id (exclusive; 0 = start).
func (s *Store) ListAudit(ctx context.Context, before int64, limit int) ([]AuditEntry, error) {
	limit = clampLimit(limit)
	q := `SELECT id, actor, action, target, detail, at FROM audit_log`
	var args []any
	if before > 0 {
		q += ` WHERE id < ?`
		args = append(args, before)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var d string
		var at int64
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.Target, &d, &at); err != nil {
			return nil, err
		}
		e.Detail, e.At = json.RawMessage(d), fromUnix(at)
		out = append(out, e)
	}
	return out, rows.Err()
}
