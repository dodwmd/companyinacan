// Package store is can-core's SQLite state: migrations and the repository layer.
//
// The pool is limited to one connection (SQLite is a single-writer database and can-core's
// load is tiny), so code must never run a query on the DB while a transaction or an open Rows
// from the same goroutine is still active.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (research R1)
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// DB wraps the SQLite handle.
type DB struct {
	*sql.DB
	// Now is the clock; tests replace it.
	Now func() time.Time
}

// ErrNotFound is returned when a looked-up row does not exist.
var ErrNotFound = errors.New("not found")

// Open opens (creating if needed) the database at path and applies pending migrations.
// Use ":memory:" for an in-memory database.
func Open(ctx context.Context, path string) (*DB, error) {
	dsn := "file::memory:?_pragma=foreign_keys(1)"
	if path != ":memory:" {
		dsn = "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	}
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	sdb.SetMaxOpenConns(1)
	sdb.SetMaxIdleConns(1)
	sdb.SetConnMaxLifetime(0)
	db := &DB{DB: sdb, Now: time.Now}
	if err := db.PingContext(ctx); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	if err := db.migrate(ctx); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	return db, nil
}

// SchemaVersion returns the highest applied migration number.
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

func (db *DB) migrate(ctx context.Context) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	cur, err := db.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		num, err := strconv.Atoi(strings.SplitN(n, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %q: name must start with a number", n)
		}
		if num <= cur {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + n)
		if err != nil {
			return err
		}
		if err := db.InTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, num, db.Now().Unix())
			return err
		}); err != nil {
			return fmt.Errorf("migration %s: %w", n, err)
		}
	}
	return nil
}

// InTx runs fn in a transaction, committing on nil and rolling back otherwise.
func (db *DB) InTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
