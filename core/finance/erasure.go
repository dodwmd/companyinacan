package finance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// Erasure tombstones of parties (specs/056-erasure-finance-work). can-core reads them into the
// erasure ledger; `can restore` replays them through `can-finance purge`.

// ClassPartyContact is the data class a party erasure removes, and the only one purge accepts.
const ClassPartyContact = "finance-party-contact"

// replayActor names a replayed erasure in the audit log.
const replayActor = "system:erasure-replay"

var erasureSubjectRe = regexp.MustCompile(`^party-([1-9][0-9]{0,17})-([0-9]{1,12})$`)

// erasureSubject names a party in a tombstone. It holds no personal data. The creation time keeps
// it apart from a later party that is given the same id after a restore of an older backup.
func erasureSubject(id int64, created time.Time) string {
	return "party-" + strconv.FormatInt(id, 10) + "-" + strconv.FormatInt(ts(created), 10)
}

// ErasureEntry is one tombstone as can-core pulls it into the erasure ledger.
type ErasureEntry struct {
	Seq     int64  `json:"seq"`
	Subject string `json:"subject"`
	Class   string `json:"class"`
	Date    string `json:"date"`
	// At is when the data was erased: a restore leaves alone a backup made after it.
	At string `json:"at"`
}

// ListErasures returns tombstones after a cursor, at most 1000.
func (s *Store) ListErasures(ctx context.Context, after int64) ([]ErasureEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, subject, class, created_at FROM erasures WHERE seq > ? ORDER BY seq LIMIT 1000`, after)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ErasureEntry{}
	for rows.Next() {
		var e ErasureEntry
		var at int64
		if err := rows.Scan(&e.Seq, &e.Subject, &e.Class, &at); err != nil {
			return nil, err
		}
		t := fromUnix(at).UTC()
		e.Date, e.At = t.Format(time.DateOnly), t.Format(time.RFC3339)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgeSubject replays one erasure: exactly what the live party erasure does, with the same legal
// retention. A party that is not in this database (created after the backup, or already deleted at
// the end of its retention) is not an error and gives a nil report; a different party that was
// given the same id is left alone. Repeating it removes nothing more.
func (s *Store) PurgeSubject(ctx context.Context, subject, class string, retentionYears int) (*ErasureReport, error) {
	if class != ClassPartyContact {
		return nil, fmt.Errorf("purge handles class %s only", ClassPartyContact)
	}
	m := erasureSubjectRe.FindStringSubmatch(subject)
	if m == nil {
		return nil, errors.New("the subject of a party erasure looks like party-<id>-<creation time>")
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return nil, err
	}
	var created int64
	err = s.db.QueryRowContext(ctx, `SELECT created_at FROM parties WHERE id = ?`, id).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && strconv.FormatInt(created, 10) != m[2]) {
		return nil, s.ResumeScrub(ctx)
	}
	if err != nil {
		return nil, err
	}
	return s.ErasePartyData(ctx, &Principal{Subject: replayActor, Permissions: []string{PermAdmin}}, id, retentionYears)
}

// ResumeScrub clears the free pages and truncates the write-ahead log if an erasure was committed and the truncation that
// should have followed it did not finish.
func (s *Store) ResumeScrub(ctx context.Context) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM erasures WHERE scrubbed = 0`).Scan(&n); err != nil || n == 0 {
		return err
	}
	return s.scrubErasures(ctx)
}

func (s *Store) scrubErasures(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	if err := s.Scrub(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE erasures SET scrubbed = 1 WHERE scrubbed = 0`); err != nil {
		return err
	}
	return s.Scrub(ctx) // the mark itself went to the log: leave it empty
}
