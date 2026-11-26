package modbackup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// latestVersion is the newest migration the binary ships ("migrations/NNN_name.sql").
func latestVersion(migrations fs.FS) (int, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return 0, fmt.Errorf("the binary's migrations: %w", err)
	}
	latest := 0
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil {
			return 0, fmt.Errorf("bad migration name %q", e.Name())
		}
		latest = max(latest, v)
	}
	if latest == 0 {
		return 0, errors.New("the binary ships no migrations")
	}
	return latest, nil
}

// schemaVersion is the highest migration applied to a database no one is writing. A database
// without one (an empty file, a half-made install, something else entirely) is an error: it must
// never replace a good dump or a good database.
func schemaVersion(ctx context.Context, path string) (int, error) {
	db, err := open(path, "mode=ro&immutable=1")
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	var v int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("it has no schema_migrations table, so it is not an initialised database of this service: %w", err)
	}
	if v == 0 {
		return 0, errors.New("its schema_migrations table is empty, so it is not an initialised database of this service")
	}
	return v, nil
}

// lockDatabase takes the database exclusively before it is replaced: a process that still has it
// open would go on writing to the replaced file, and those writes would be lost without a word.
// The lock is held until release. A missing or unreadable (corrupt) database has nothing to
// guard. It cannot see a process that has the file open but has never read it: stop the service.
func lockDatabase(ctx context.Context, path string) (release func(), err error) {
	none := func() {}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return none, nil
	} else if err != nil {
		return nil, err
	}
	db, err := open(path, "mode=rw&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err == nil {
		// In exclusive locking mode the lock is kept; leaving WAL also removes -wal and -shm
		// while no one else can be using them.
		for _, q := range []string{`PRAGMA locking_mode=EXCLUSIVE`, `BEGIN EXCLUSIVE`, `COMMIT`, `PRAGMA journal_mode=DELETE`, `BEGIN EXCLUSIVE`} {
			if _, err = conn.ExecContext(ctx, q); err != nil {
				break
			}
		}
	}
	closeAll := func() {
		if conn != nil {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
			_ = conn.Close()
		}
		_ = db.Close()
	}
	if err == nil {
		return closeAll, nil
	}
	closeAll()
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not a database") || strings.Contains(msg, "malformed"):
		return none, nil
	case strings.Contains(msg, "locked") || strings.Contains(msg, "SQLITE_BUSY"):
		return nil, fmt.Errorf("%s is in use by another process: stop the service (scale the module to 0) and run restore again", path)
	}
	return nil, fmt.Errorf("lock %s: %w", path, err)
}

// missingFiles describes referenced files that are not there, with what to do about them. It is
// short on purpose: only the end of a hook Job's log reaches the failed dump run's message.
func (b *blobSpec) missingFiles(keys []string, records map[string]string) string {
	id := records[keys[0]]
	more := ""
	if len(keys) > 1 {
		more = fmt.Sprintf(" (and %d more)", len(keys)-1)
	}
	return fmt.Sprintf("file %s of %s %s%s is missing. Purge the record (deleting it is not enough): POST /v1/purge {\"subject\":%q,\"class\":%q}, or put the file back",
		keys[0], b.record, id, more, id, b.class)
}
