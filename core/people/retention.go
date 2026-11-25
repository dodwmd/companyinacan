package people

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
)

// Data classes held by this module (modules/people/module.yaml). Class names follow the
// manifest pattern ^[a-z][a-z0-9-]*$ and double as the `class` of E08 erasure ledger entries.
const (
	ClassBasic      = "directory-basic"
	ClassSensitive  = "people-sensitive"
	ClassChecklists = "people-checklists"
	ClassOneOnOne   = "performance-oneonone"
	ClassGoals      = "performance-goals"
)

// Classes lists every data class in purge order.
var Classes = []string{ClassSensitive, ClassOneOnOne, ClassGoals, ClassChecklists, ClassBasic}

// RetentionRule is a parsed retention period. Legal means "as required by law": never purged
// automatically. Zero Days with !Legal means purge as soon as the trigger (leaving) happens.
type RetentionRule struct {
	Legal bool
	Days  int
}

var retentionRe = regexp.MustCompile(`^(0|legal|[1-9][0-9]*[dmy])$`)

// ParseRetention parses the manifest syntax: 30d, 12m, 7y, 0 or legal.
func ParseRetention(s string) (RetentionRule, error) {
	if !retentionRe.MatchString(s) {
		return RetentionRule{}, fmt.Errorf("retention %q must be Nd, Nm, Ny, 0 or legal", s)
	}
	switch s {
	case "legal":
		return RetentionRule{Legal: true}, nil
	case "0":
		return RetentionRule{}, nil
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n > 100000 {
		return RetentionRule{}, fmt.Errorf("retention %q is out of range", s)
	}
	mult := map[byte]int{'d': 1, 'm': 30, 'y': 365}[s[len(s)-1]]
	return RetentionRule{Days: n * mult}, nil
}

// DefaultRetention mirrors the defaults in modules/people/module.yaml; they are starting points
// the operator must review against local law, not advice.
func DefaultRetention() map[string]RetentionRule {
	return map[string]RetentionRule{
		ClassBasic:      {Days: 7 * 365},
		ClassSensitive:  {Days: 7 * 365},
		ClassChecklists: {Days: 2 * 365},
		ClassOneOnOne:   {Days: 2 * 365},
		ClassGoals:      {Days: 2 * 365},
	}
}

// RetentionReport counts what a sweep purged, by class.
type RetentionReport struct {
	Purged  map[string]int `json:"purged"`
	Skipped map[string]int `json:"skipped_legal_hold"`
}

// purgeClass removes one data class for one person. It is idempotent and is used by retention,
// erasure and the restore-time ledger replay.
func (s *Service) purgeClass(ctx context.Context, q querier, id, class string) error {
	now := s.now().Unix()
	var stmts []string
	switch class {
	case ClassBasic:
		stmts = []string{
			fmt.Sprintf(`UPDATE people SET given_name='[erased]', family_name='', preferred_name='', work_email='', work_phone='', location='', sso_subject=NULL,
				manager_id=NULL, department_id=NULL, team_id=NULL, position_id=NULL, start_date='', end_date='', legal_hold_reason='',
				erased_at=COALESCE(erased_at, %d), updated_at=%d WHERE id=?`, now, now),
			`UPDATE teams SET lead_id=NULL WHERE lead_id=?`,
			`UPDATE people SET manager_id=NULL WHERE manager_id=?`,
			`UPDATE status_history SET note='' WHERE person_id=?`,
			`UPDATE status_history SET recorded_by='erased' WHERE recorded_by=?`,
		}
	case ClassSensitive:
		stmts = []string{`UPDATE people SET sens_dob=NULL, sens_national_id=NULL, sens_address=NULL, sens_phone=NULL, sens_salary=NULL WHERE id=?`}
	case ClassChecklists:
		stmts = []string{`UPDATE checklist_tasks SET assignee_id=NULL WHERE assignee_id=?`, `DELETE FROM checklists WHERE person_id=?`}
	case ClassOneOnOne:
		stmts = []string{`DELETE FROM oneonone_series WHERE manager_id=? OR report_id=?`}
	case ClassGoals:
		stmts = []string{
			`UPDATE goals SET parent_id=NULL WHERE parent_id IN (SELECT id FROM goals WHERE owner_id=?)`,
			`DELETE FROM goals WHERE owner_id=?`,
			`UPDATE goal_updates SET author_id='erased', comment='' WHERE author_id=?`,
		}
	default:
		return invalid("unknown data class %q", class)
	}
	for _, st := range stmts {
		args := []any{id}
		if class == ClassOneOnOne {
			args = append(args, id)
		}
		if _, err := q.ExecContext(ctx, st, args...); err != nil {
			return err
		}
	}
	return nil
}

// Erasure is the result of an erasure request: an opaque subject id and the classes purged.
type Erasure struct {
	Subject string   `json:"subject"`
	Classes []string `json:"classes"`
	Date    string   `json:"date"`
}

// RequestErasure purges a former person or candidate and records tombstones. hr or admin only.
// A legal hold blocks it. The subject id is the person's own random id: opaque, with no
// personal data in it, and it satisfies the E08 ledger's subject pattern.
func (s *Service) RequestErasure(ctx context.Context, a Actor, id string) (*Erasure, error) {
	f, si, err := s.personFact(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if !Decide(a, ActErasure, f) {
		s.auditDeny(ctx, a, "erase", "person:"+id, nil)
		return nil, ErrForbidden
	}
	var hold int
	if err := s.db.QueryRowContext(ctx, `SELECT legal_hold FROM people WHERE id = ?`, id).Scan(&hold); err != nil {
		return nil, err
	}
	if hold == 1 {
		s.auditDeny(ctx, a, "erase", "person:"+id, nil)
		return nil, conflict("erasure is blocked by a legal hold")
	}
	if si.status != StFormer && si.status != StCandidate {
		return nil, conflict("only former people and candidates can be erased (status is %s); offboard first", si.status)
	}
	date := s.today()
	err = s.tx(ctx, func(tx *sqlTx) error {
		for _, c := range Classes {
			if err := s.purgeClass(ctx, tx, id, c); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO erasures (subject, class, date, created_at) VALUES (?,?,?,?)`, id, c, date, s.now().Unix()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := s.auditWrite(ctx, a, "erase", "person:"+id, nil); err != nil {
		return nil, err
	}
	return &Erasure{Subject: id, Classes: Classes, Date: date}, nil
}

// SetLegalHold places or lifts a legal hold. hr or admin only. A hold blocks erasure and
// retention purges.
func (s *Service) SetLegalHold(ctx context.Context, a Actor, id string, hold bool, reason string) error {
	f, si, err := s.personFact(ctx, a, id)
	if err != nil {
		return err
	}
	if !Decide(a, ActLegalHold, f) {
		s.auditDeny(ctx, a, "hold", "person:"+id, nil)
		return ErrForbidden
	}
	if si.erased {
		return conflict("the record is already erased")
	}
	if err := validText("reason", reason, 500, false); err != nil {
		return err
	}
	n := 0
	if hold {
		n = 1
	} else {
		reason = ""
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE people SET legal_hold = ?, legal_hold_reason = ?, updated_at = ? WHERE id = ?`, n, reason, s.now().Unix(), id); err != nil {
		return err
	}
	return s.auditWrite(ctx, a, "hold", "person:"+id, nil)
}

// ErasureEntry is one tombstone as can-core pulls it into the erasure ledger.
type ErasureEntry struct {
	Seq     int64  `json:"seq"`
	Subject string `json:"subject"`
	Class   string `json:"class"`
	Date    string `json:"date"`
}

// ListErasures returns tombstones after a cursor. hr/admin (the can-core service account
// holds admin).
func (s *Service) ListErasures(ctx context.Context, a Actor, after int64) ([]ErasureEntry, error) {
	if !Decide(a, ActRetention, Fact{}) {
		return nil, ErrForbidden
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq, subject, class, date FROM erasures WHERE seq > ? ORDER BY seq LIMIT 1000`, after)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ErasureEntry{}
	for rows.Next() {
		var e ErasureEntry
		if err := rows.Scan(&e.Seq, &e.Subject, &e.Class, &e.Date); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgeSubject is the `purge(subject, class)` hook: idempotent, ignores legal holds (the
// erasure was already authorised when the tombstone was written) and is safe to replay after
// a restore. An unknown subject is a no-op.
func (s *Service) PurgeSubject(ctx context.Context, subject, class string) error {
	if !personIDRe.MatchString(subject) {
		return invalid("subject must be a person id")
	}
	return s.tx(ctx, func(tx *sqlTx) error { return s.purgeClass(ctx, tx, subject, class) })
}

// RunRetention enforces per-class retention. A person's classes expire retention-period after
// they became former; 1:1 meetings and closed goals also expire by age. Legal holds skip.
func (s *Service) RunRetention(ctx context.Context) (*RetentionReport, error) {
	rep := &RetentionReport{Purged: map[string]int{}, Skipped: map[string]int{}}
	now := s.now()
	for _, class := range Classes {
		rule, ok := s.retention[class]
		if !ok || rule.Legal {
			continue
		}
		cutoff := now.AddDate(0, 0, -rule.Days).Unix()
		rows, err := s.db.QueryContext(ctx, `SELECT id, legal_hold, erased_at IS NOT NULL FROM people WHERE status = 'former' AND ended_at IS NOT NULL AND ended_at <= ?`, cutoff)
		if err != nil {
			return nil, err
		}
		type cand struct {
			id     string
			hold   bool
			erased bool
		}
		var cs []cand
		for rows.Next() {
			var c cand
			var h int
			if err := rows.Scan(&c.id, &h, &c.erased); err != nil {
				_ = rows.Close()
				return nil, err
			}
			c.hold = h == 1
			cs = append(cs, c)
		}
		_ = rows.Close()
		for _, c := range cs {
			if c.hold {
				rep.Skipped[class]++
				continue
			}
			if class == ClassBasic && c.erased {
				continue
			}
			if err := s.tx(ctx, func(tx *sqlTx) error { return s.purgeClass(ctx, tx, c.id, class) }); err != nil {
				return rep, err
			}
			rep.Purged[class]++
		}
		switch class {
		case ClassOneOnOne:
			n, err := s.execCount(ctx, `DELETE FROM oneonone_meetings WHERE scheduled_at <= ? AND series_id NOT IN (
				SELECT sr.id FROM oneonone_series sr JOIN people m ON m.id = sr.manager_id JOIN people r ON r.id = sr.report_id WHERE m.legal_hold = 1 OR r.legal_hold = 1)`, cutoff)
			if err != nil {
				return rep, err
			}
			rep.Purged[class] += n
		case ClassGoals:
			n, err := s.execCount(ctx, `DELETE FROM goals WHERE status IN ('achieved','abandoned') AND updated_at <= ? AND (owner_id IS NULL OR owner_id NOT IN (SELECT id FROM people WHERE legal_hold = 1))
				AND id NOT IN (SELECT parent_id FROM goals WHERE parent_id IS NOT NULL)`, cutoff)
			if err != nil {
				return rep, err
			}
			rep.Purged[class] += n
		}
	}
	return rep, nil
}

func (s *Service) execCount(ctx context.Context, q string, args ...any) (int, error) {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RotateKeys re-seals every sealed value written with a retired key under the active key.
// It returns the number of values re-sealed. Run it after adding a key at the top of the
// keyring; when it returns 0 the old key can be removed.
func (s *Service) RotateKeys(ctx context.Context) (int, error) {
	n := 0
	for field, col := range sensCols {
		rows, err := s.db.QueryContext(ctx, `SELECT id, `+col+` FROM people WHERE `+col+` IS NOT NULL`) //nolint:gosec // column from a fixed map
		if err != nil {
			return n, err
		}
		type item struct{ id, sealed string }
		var items []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.id, &it.sealed); err != nil {
				_ = rows.Close()
				return n, err
			}
			if s.keys.NeedsRotation(it.sealed) {
				items = append(items, it)
			}
		}
		_ = rows.Close()
		for _, it := range items {
			aad := sensAAD(it.id, field)
			plain, err := s.keys.Open(it.sealed, aad)
			if err != nil {
				return n, fmt.Errorf("rotate %s: %w", aad, err)
			}
			c, err := s.keys.Seal(plain, aad)
			if err != nil {
				return n, err
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE people SET `+col+` = ? WHERE id = ?`, c, it.id); err != nil { //nolint:gosec // column from a fixed map
				return n, err
			}
			n++
		}
	}
	for _, field := range []string{"shared_notes", "private_notes"} {
		col := field + "_sealed"
		rows, err := s.db.QueryContext(ctx, `SELECT id, `+col+` FROM oneonone_meetings WHERE `+col+` IS NOT NULL`) //nolint:gosec // column from a fixed list
		if err != nil {
			return n, err
		}
		type item struct{ id, sealed string }
		var items []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.id, &it.sealed); err != nil {
				_ = rows.Close()
				return n, err
			}
			if s.keys.NeedsRotation(it.sealed) {
				items = append(items, it)
			}
		}
		_ = rows.Close()
		for _, it := range items {
			aad := it.id + ":" + field
			plain, err := s.keys.Open(it.sealed, aad)
			if err != nil {
				return n, fmt.Errorf("rotate %s: %w", aad, err)
			}
			c, err := s.keys.Seal(plain, aad)
			if err != nil {
				return n, err
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE oneonone_meetings SET `+col+` = ? WHERE id = ?`, c, it.id); err != nil { //nolint:gosec // column from a fixed list
				return n, err
			}
			n++
		}
	}
	return n, nil
}
