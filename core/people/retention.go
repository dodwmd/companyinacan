package people

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Data classes held by this module (modules/people/module.yaml). Class names follow the
// manifest pattern ^[a-z][a-z0-9-]*$ and double as the `class` of E08 erasure ledger entries.
const (
	ClassBasic      = "directory-basic"
	ClassSensitive  = "people-sensitive"
	ClassChecklists = "people-checklists"
	ClassOneOnOne   = "performance-oneonone"
	ClassGoals      = "performance-goals"
	ClassReviews    = "performance-reviews" // review cycles' per-person content: self, manager, calibration, feedback
	ClassTimeOff    = "people-timeoff"      // leave requests, balances ledger
)

// Classes lists every data class in purge order.
var Classes = []string{ClassSensitive, ClassOneOnOne, ClassGoals, ClassReviews, ClassTimeOff, ClassChecklists, ClassBasic}

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
		ClassReviews:    {Days: 2 * 365},
		ClassTimeOff:    {Days: 7 * 365},
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
			`UPDATE checklists SET created_by='erased' WHERE created_by=?`,
			`UPDATE checklist_tasks SET done_by='erased' WHERE done_by=?`,
			`UPDATE goals SET created_by='erased' WHERE created_by=?`,
			`UPDATE action_items SET created_by='erased' WHERE created_by=?`,
			`UPDATE agenda_items SET author_id='erased' WHERE author_id=?`,
			`UPDATE review_participants SET mgr_author='erased' WHERE mgr_author=?`,
			`UPDATE review_participants SET calibrated_by='erased' WHERE calibrated_by=?`,
			`UPDATE review_participants SET release_requested_by='erased' WHERE release_requested_by=?`,
			`UPDATE review_participants SET release_decided_by='erased' WHERE release_decided_by=?`,
			`UPDATE review_requests SET proposed_by='erased' WHERE proposed_by=?`,
			`UPDATE review_requests SET decided_by='erased' WHERE decided_by=?`,
			`UPDATE leave_requests SET submitted_by='erased' WHERE submitted_by=?`,
			`UPDATE leave_requests SET decided_by='erased' WHERE decided_by=?`,
			`UPDATE leave_requests SET cancelled_by='erased' WHERE cancelled_by=?`,
			`UPDATE leave_ledger SET created_by='erased' WHERE created_by=?`,
			`UPDATE review_templates SET created_by='erased' WHERE created_by=?`,
			`UPDATE review_cycles SET created_by='erased' WHERE created_by=?`,
			`UPDATE blackouts SET created_by='erased' WHERE created_by=?`,
		}
	case ClassSensitive:
		stmts = []string{`UPDATE people SET sens_dob=NULL, sens_national_id=NULL, sens_address=NULL, sens_phone=NULL, sens_salary=NULL WHERE id=?`}
	case ClassReviews:
		// What the person wrote as a reviewer (attributed feedback) and the requests addressed to
		// them go; reviews about them go with their rows. Anonymous feedback they gave is not linked
		// to them and stays (it names nobody): see docs/people.md.
		stmts = []string{
			`DELETE FROM review_responses WHERE reviewer_id=?`,
			`DELETE FROM review_requests WHERE reviewer_id=?`,
			`DELETE FROM review_participants WHERE person_id=?`,
		}
	case ClassTimeOff:
		stmts = []string{
			`UPDATE ledger_guard SET open=1`,
			`DELETE FROM leave_ledger WHERE person_id=?`,
			`UPDATE ledger_guard SET open=0`,
			`DELETE FROM leave_requests WHERE person_id=?`,
		}
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
		var args []any
		for range strings.Count(st, "?") {
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
		if si.erased {
			return nil // repeat request: nothing new to announce
		}
		return s.enqueue(ctx, tx, EventPersonErased, map[string]any{"party_id": id, "erased_on": date})
	})
	if err != nil {
		return nil, err
	}
	// Audit first: the erasure has committed, so it must be on record even if the scrub fails.
	if err := s.auditWrite(ctx, a, "erase", "person:"+id, nil); err != nil {
		return nil, err
	}
	if err := s.scrub(ctx, true); err != nil {
		return nil, fmt.Errorf("the person is erased and audited, but scrubbing the database files failed (repeat the request: it is idempotent and retries the scrub): %w", err)
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
	if err := s.tx(ctx, func(tx *sqlTx) error { return s.purgeClass(ctx, tx, subject, class) }); err != nil {
		return err
	}
	return s.scrub(ctx, false)
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
		case ClassReviews:
			// Reviews of a closed cycle expire by age, unless their subject is under legal hold.
			n, err := s.execCount(ctx, `DELETE FROM review_participants WHERE cycle_id IN (SELECT id FROM review_cycles WHERE state = 'closed' AND closed_at <= ?)
				AND person_id NOT IN (SELECT id FROM people WHERE legal_hold = 1)`, cutoff)
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
	for _, n := range rep.Purged {
		if n > 0 {
			if err := s.scrub(ctx, true); err != nil {
				return rep, err
			}
			break
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
// keyring; when it returns 0 the old key can be removed. Each value is replaced with a
// compare-and-swap (the UPDATE only applies if the stored ciphertext is still the one that was
// read), so a concurrent write is never overwritten with stale data; values that changed in
// the meantime are retried on the next pass.
func (s *Service) RotateKeys(ctx context.Context) (int, error) {
	total := 0
	for pass := 0; pass < 5; pass++ {
		n, missed, err := s.rotateOnce(ctx)
		total += n
		if err != nil || missed == 0 {
			return total, err
		}
	}
	return total, errors.New("rotate-keys: values kept changing during rotation; run it again")
}

func (s *Service) rotateOnce(ctx context.Context) (n, missed int, _ error) {
	for field, col := range sensCols {
		rows, err := s.db.QueryContext(ctx, `SELECT id, `+col+` FROM people WHERE `+col+` IS NOT NULL`) //nolint:gosec // column from a fixed map
		if err != nil {
			return n, missed, err
		}
		type item struct{ id, sealed string }
		var items []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.id, &it.sealed); err != nil {
				_ = rows.Close()
				return n, missed, err
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
				return n, missed, fmt.Errorf("rotate %s: %w", aad, err)
			}
			c, err := s.keys.Seal(plain, aad)
			if err != nil {
				return n, missed, err
			}
			if s.rotateHook != nil {
				s.rotateHook()
			}
			res, err := s.db.ExecContext(ctx, `UPDATE people SET `+col+` = ? WHERE id = ? AND `+col+` = ?`, c, it.id, it.sealed) //nolint:gosec // column from a fixed map
			if err != nil {
				return n, missed, err
			}
			if k, _ := res.RowsAffected(); k == 0 {
				missed++
				continue
			}
			n++
		}
	}
	for _, field := range []string{"shared_notes", "private_notes"} {
		col := field + "_sealed"
		rows, err := s.db.QueryContext(ctx, `SELECT id, `+col+` FROM oneonone_meetings WHERE `+col+` IS NOT NULL`) //nolint:gosec // column from a fixed list
		if err != nil {
			return n, missed, err
		}
		type item struct{ id, sealed string }
		var items []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.id, &it.sealed); err != nil {
				_ = rows.Close()
				return n, missed, err
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
				return n, missed, fmt.Errorf("rotate %s: %w", aad, err)
			}
			c, err := s.keys.Seal(plain, aad)
			if err != nil {
				return n, missed, err
			}
			res, err := s.db.ExecContext(ctx, `UPDATE oneonone_meetings SET `+col+` = ? WHERE id = ? AND `+col+` = ?`, c, it.id, it.sealed) //nolint:gosec // column from a fixed list
			if err != nil {
				return n, missed, err
			}
			if k, _ := res.RowsAffected(); k == 0 {
				missed++
				continue
			}
			n++
		}
	}
	en, em, err := s.rotateE14b(ctx)
	n, missed = n+en, missed+em
	if err != nil {
		return n, missed, err
	}
	// status history notes (keyed by row sequence)
	rows, err := s.db.QueryContext(ctx, `SELECT seq, person_id, note FROM status_history WHERE note != ''`)
	if err != nil {
		return n, missed, err
	}
	type note struct {
		seq            int64
		person, sealed string
	}
	var notes []note
	for rows.Next() {
		var x note
		if err := rows.Scan(&x.seq, &x.person, &x.sealed); err != nil {
			_ = rows.Close()
			return n, missed, err
		}
		if s.keys.NeedsRotation(x.sealed) {
			notes = append(notes, x)
		}
	}
	_ = rows.Close()
	for _, x := range notes {
		aad := x.person + ":status_note"
		plain, err := s.keys.Open(x.sealed, aad)
		if err != nil {
			return n, missed, fmt.Errorf("rotate %s: %w", aad, err)
		}
		c, err := s.keys.Seal(plain, aad)
		if err != nil {
			return n, missed, err
		}
		res, err := s.db.ExecContext(ctx, `UPDATE status_history SET note = ? WHERE seq = ? AND note = ?`, c, x.seq, x.sealed)
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

// e14bSealed lists the sealed columns added by E14b: table, column, and the AAD around the row id.
var e14bSealed = []struct{ table, col, prefix, suffix string }{
	{"review_participants", "self_sealed", "review:", ":self"},
	{"review_participants", "mgr_sealed", "review:", ":mgr"},
	{"review_participants", "calib_sealed", "review:", ":calib"},
	{"review_participants", "final_sealed", "review:", ":final"},
	{"review_participants", "ack_sealed", "review:", ":ack"},
	{"review_participants", "release_note_sealed", "review:", ":relnote"},
	{"review_responses", "answers_sealed", "review_response:", ""},
	{"leave_requests", "note_sealed", "leave:", ":note"},
}

// rotateE14b re-seals the review and time-off columns with the same compare-and-swap as the rest.
func (s *Service) rotateE14b(ctx context.Context) (n, missed int, _ error) {
	for _, c := range e14bSealed {
		rows, err := s.db.QueryContext(ctx, `SELECT id, `+c.col+` FROM `+c.table+` WHERE `+c.col+` IS NOT NULL`) //nolint:gosec // names from a fixed list
		if err != nil {
			return n, missed, err
		}
		type item struct{ id, sealed string }
		var items []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.id, &it.sealed); err != nil {
				_ = rows.Close()
				return n, missed, err
			}
			if s.keys.NeedsRotation(it.sealed) {
				items = append(items, it)
			}
		}
		_ = rows.Close()
		for _, it := range items {
			aad := c.prefix + it.id + c.suffix
			plain, err := s.keys.Open(it.sealed, aad)
			if err != nil {
				return n, missed, fmt.Errorf("rotate %s: %w", aad, err)
			}
			sealed, err := s.keys.Seal(plain, aad)
			if err != nil {
				return n, missed, err
			}
			res, err := s.db.ExecContext(ctx, `UPDATE `+c.table+` SET `+c.col+` = ? WHERE id = ? AND `+c.col+` = ?`, sealed, it.id, it.sealed) //nolint:gosec // names from a fixed list
			if err != nil {
				return n, missed, err
			}
			if k, _ := res.RowsAffected(); k == 0 {
				missed++
				continue
			}
			n++
		}
	}
	// manual adjustment reasons in the ledger are keyed by their ref
	rows, err := s.db.QueryContext(ctx, `SELECT seq, ref, note_sealed FROM leave_ledger WHERE note_sealed IS NOT NULL`)
	if err != nil {
		return n, missed, err
	}
	type adj struct {
		seq             int64
		ref, sealedNote string
	}
	var adjs []adj
	for rows.Next() {
		var x adj
		if err := rows.Scan(&x.seq, &x.ref, &x.sealedNote); err != nil {
			_ = rows.Close()
			return n, missed, err
		}
		if s.keys.NeedsRotation(x.sealedNote) {
			adjs = append(adjs, x)
		}
	}
	_ = rows.Close()
	for _, x := range adjs {
		aad := "leave_ledger:" + x.ref + ":note"
		plain, err := s.keys.Open(x.sealedNote, aad)
		if err != nil {
			return n, missed, fmt.Errorf("rotate %s: %w", aad, err)
		}
		sealed, err := s.keys.Seal(plain, aad)
		if err != nil {
			return n, missed, err
		}
		res, err := s.db.ExecContext(ctx, `UPDATE leave_ledger SET note_sealed = ? WHERE seq = ? AND note_sealed = ?`, sealed, x.seq, x.sealedNote)
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
