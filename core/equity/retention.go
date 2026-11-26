package equity

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Data classes held by this module (modules/equity/module.yaml). Class names follow the manifest
// pattern ^[a-z][a-z0-9-]*$ and double as the `class` of E08 erasure ledger entries.
const (
	ClassLedger      = "equity-ledger"      // issuances, transfers, exercises, grant terms, convertibles: kept by law
	ClassStakeholder = "equity-stakeholder" // holder identity: name, email, tax id, person link
	ClassGrants      = "equity-grants"      // free-form grant notes, payment references, drafts
	ClassValuation   = "equity-valuation"   // 409A and other valuation records
	ClassAudit       = "equity-audit"       // append-only audit trail (ids only)
)

// Classes lists every data class.
var Classes = []string{ClassLedger, ClassStakeholder, ClassGrants, ClassValuation, ClassAudit}

// PurgeClasses are the classes an erasure or retention sweep can purge. The ledger and the audit
// trail are retained by law: they hold stakeholder ids only, so erasing the identity (the
// stakeholder class) leaves them intact and pseudonymous.
var PurgeClasses = []string{ClassStakeholder, ClassGrants}

// RetentionRule is a parsed retention period. Legal means "as required by law": never purged
// automatically.
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

// DefaultRetention mirrors modules/equity/module.yaml. They are starting points the operator must
// review against local law, not advice.
func DefaultRetention() map[string]RetentionRule {
	return map[string]RetentionRule{
		ClassLedger:      {Legal: true},
		ClassStakeholder: {Days: 7 * 365},
		ClassGrants:      {Days: 7 * 365},
		ClassValuation:   {Days: 7 * 365},
		ClassAudit:       {Legal: true},
	}
}

// RetentionReport counts what a sweep purged, by class.
type RetentionReport struct {
	Purged  map[string]int `json:"purged"`
	Skipped map[string]int `json:"skipped_legal_hold"`
}

// purgeClass removes one data class for one stakeholder. It is idempotent and used by erasure,
// the retention sweep and the restore-time ledger replay. It never touches the ledger.
func (s *Service) purgeClass(ctx context.Context, q querier, id, class string) error {
	now := s.now().Unix()
	var stmts []string
	switch class {
	case ClassStakeholder:
		stmts = []string{
			fmt.Sprintf(`UPDATE stakeholders SET sens_name=NULL, sens_email=NULL, sens_tax_id=NULL, person_id=NULL, legal_hold_reason='',
				person_linked_by=NULL, person_verified_by=NULL, person_verified_on=NULL, identity_by=NULL, external_attested_by=NULL, external_attested_on=NULL,
				erased_at=COALESCE(erased_at, %d), updated_at=%d WHERE id=?`, now, now),
		}
	case ClassGrants:
		stmts = []string{
			`UPDATE grants SET sens_notes=NULL WHERE stakeholder_id=?`,
			`UPDATE exercises SET sens_payment_ref=NULL WHERE stakeholder_id=?`,
			fmt.Sprintf(`UPDATE exercises SET status='cancelled', updated_at=%d WHERE stakeholder_id=? AND status IN ('requested','approved')`, now),
			`DELETE FROM idempotency WHERE resource_id IN (SELECT id FROM grants WHERE stakeholder_id=?) OR resource_id IN (SELECT id FROM exercises WHERE stakeholder_id=?)`,
			`DELETE FROM grants WHERE stakeholder_id=? AND status='draft' AND supersedes_id IS NULL`,
		}
	case ClassLedger, ClassAudit, ClassValuation:
		return conflict("class %s is retained by law and cannot be purged per stakeholder", class)
	default:
		return invalid("unknown data class %q", class)
	}
	for _, st := range stmts {
		args := make([]any, strings.Count(st, "?"))
		for i := range args {
			args[i] = id
		}
		if _, err := q.ExecContext(ctx, st, args...); err != nil {
			return err
		}
	}
	return nil
}

// Retained says what an erasure kept, and why.
type Retained struct {
	Class  string `json:"class"`
	Count  int    `json:"count"`
	Reason string `json:"reason"`
}

// Erasure is the result of an erasure request: an opaque subject id, the classes purged and
// what was retained under legal retention.
type Erasure struct {
	Subject  string     `json:"subject"`
	Classes  []string   `json:"classes"`
	Date     string     `json:"date"`
	Retained []Retained `json:"retained"`
}

// RequestErasure erases a stakeholder: the identity is pseudonymised and free text and payment
// references are cleared; issued share records, exercised grants and the ledger are kept, because
// legal retention overrides erasure for them (they refer to the stakeholder id only). A legal
// hold blocks it; so do live grants (end or cancel them first). admin only.
func (s *Service) RequestErasure(ctx context.Context, a Actor, id string) (*Erasure, error) {
	if err := validID("stk", id); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActErasure, Fact{}, "stakeholder:"+id); err != nil {
		return nil, err
	}
	st, err := s.stakeholder(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	// Nobody erases their own record: it removes person_id, which the holder checks compare.
	if !sodOK(a, st.PersonID) {
		s.auditDeny(ctx, a, "erase", "stakeholder:"+id, nil)
		return nil, ErrSeparation
	}
	if st.LegalHold {
		s.auditDeny(ctx, a, "erase", "stakeholder:"+id, nil)
		return nil, conflict("erasure is blocked by a legal hold")
	}
	var live int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM grants WHERE stakeholder_id = ? AND status IN ('pending_approval', 'approved', 'active')`, id).Scan(&live); err != nil {
		return nil, err
	}
	if live > 0 {
		return nil, conflict("the stakeholder has %d live grant(s): cancel or end them before erasure", live)
	}
	var inLedger, grants, exercises int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger WHERE stakeholder_id = ?`, id).Scan(&inLedger)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM grants WHERE stakeholder_id = ? AND status != 'draft'`, id).Scan(&grants)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM exercises WHERE stakeholder_id = ? AND status = 'issued'`, id).Scan(&exercises)
	date := s.today()
	err = s.txAudit(ctx, a, "erase", "stakeholder:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		// Who erased: they alone cannot then move the record's shares (guardHolders).
		if _, err := tx.ExecContext(ctx, `UPDATE stakeholders SET erased_by = COALESCE(erased_by, ?) WHERE id = ?`, canonSubject(a.Subject), id); err != nil {
			return err
		}
		for _, c := range PurgeClasses {
			if err := s.purgeClass(ctx, tx, id, c); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO erasures (subject, class, date, created_at) VALUES (?,?,?,?) ON CONFLICT (subject, class) DO UPDATE SET date = excluded.date, created_at = excluded.created_at`, id, c, date, s.now().Unix()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Audit first: the erasure has committed, so it must be on record even if the scrub fails.
	if err := s.scrub(ctx, true); err != nil {
		return nil, fmt.Errorf("the stakeholder is erased and audited, but scrubbing the database files failed (repeat the request: it is idempotent and retries the scrub): %w", err)
	}
	out := &Erasure{Subject: id, Classes: PurgeClasses, Date: date, Retained: []Retained{}}
	if inLedger > 0 {
		out.Retained = append(out.Retained, Retained{ClassLedger, inLedger, "issued share records are kept for the legal retention period; they refer to this stakeholder by opaque id only"})
	}
	if grants > 0 || exercises > 0 {
		out.Retained = append(out.Retained, Retained{ClassLedger, grants + exercises, "grant terms and exercise records that resulted in issued shares or were approved are part of the legal record"})
	}
	return out, nil
}

// SetLegalHold places or lifts a legal hold on a stakeholder. A hold blocks erasure and
// retention purges. admin only.
func (s *Service) SetLegalHold(ctx context.Context, a Actor, id string, hold bool, reason string) error {
	if err := validID("stk", id); err != nil {
		return err
	}
	if err := s.authorize(ctx, a, ActLegalHold, Fact{}, "stakeholder:"+id); err != nil {
		return err
	}
	st, err := s.stakeholder(ctx, s.db, id)
	if err != nil {
		return err
	}
	if st.Erased {
		return conflict("the stakeholder is already erased")
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
	return s.txAudit(ctx, a, "hold", "stakeholder:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `UPDATE stakeholders SET legal_hold = ?, legal_hold_reason = ?, updated_at = ? WHERE id = ?`, n, reason, s.now().Unix(), id)
		return err
	})
}

// ErasureEntry is one tombstone as can-core pulls it into the erasure ledger.
type ErasureEntry struct {
	Seq     int64  `json:"seq"`
	Subject string `json:"subject"`
	Class   string `json:"class"`
	Date    string `json:"date"`
	// At is when the data was erased: a restore leaves alone a backup made after it.
	At string `json:"at,omitempty"`
}

// ListErasures returns tombstones after a cursor. admin (the can-core service account holds it).
func (s *Service) ListErasures(ctx context.Context, a Actor, after int64) ([]ErasureEntry, error) {
	if err := s.authorize(ctx, a, ActRetention, Fact{}, "erasures"); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq, subject, class, date, created_at FROM erasures WHERE seq > ? ORDER BY seq LIMIT 1000`, after)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ErasureEntry{}
	for rows.Next() {
		var e ErasureEntry
		var created int64
		if err := rows.Scan(&e.Seq, &e.Subject, &e.Class, &e.Date, &created); err != nil {
			return nil, err
		}
		e.At = time.Unix(created, 0).UTC().Format(time.RFC3339)
		out = append(out, e)
	}
	return out, rows.Err()
}

var stakeholderIDRe = regexp.MustCompile(`^stk_[0-9a-f]{24}$`)

// PurgeSubject is the `purge(subject, class)` hook: idempotent, ignores legal holds (the erasure
// was already authorised when the tombstone was written) and is safe to replay after a restore.
// An unknown subject is a no-op.
func (s *Service) PurgeSubject(ctx context.Context, subject, class string) error {
	if !stakeholderIDRe.MatchString(subject) {
		return invalid("subject must be a stakeholder id")
	}
	if err := s.tx(ctx, func(ctx context.Context, tx *sqlTx) error { return s.purgeClass(ctx, tx, subject, class) }); err != nil {
		return err
	}
	return s.scrub(ctx, false)
}

// RunRetention enforces per-class retention for the classes that can be purged. A stakeholder
// with no live grants and no legal hold whose last activity is older than the period has the
// identity and free text purged (the ledger stays); old terminal grants lose their notes and
// payment references; old valuation records lose their sealed price and notes.
func (s *Service) RunRetention(ctx context.Context) (*RetentionReport, error) {
	rep := &RetentionReport{Purged: map[string]int{}, Skipped: map[string]int{}}
	now := s.now()
	if rule, ok := s.retention[ClassStakeholder]; ok && !rule.Legal {
		cutoff := now.AddDate(0, 0, -rule.Days).Unix()
		rows, err := s.db.QueryContext(ctx, `SELECT id, legal_hold FROM stakeholders s WHERE erased_at IS NULL AND updated_at <= ?
			AND NOT EXISTS (SELECT 1 FROM grants g WHERE g.stakeholder_id = s.id AND (g.status IN ('draft','pending_approval','approved','active') OR g.updated_at > ?))
			AND NOT EXISTS (SELECT 1 FROM exercises e WHERE e.stakeholder_id = s.id AND e.updated_at > ?)
			AND NOT EXISTS (SELECT 1 FROM ledger l WHERE l.stakeholder_id = s.id AND l.recorded_at > ?)`, cutoff, cutoff, cutoff, cutoff)
		if err != nil {
			return nil, err
		}
		type cand struct {
			id   string
			hold bool
		}
		var cs []cand
		for rows.Next() {
			var c cand
			var h int
			if err := rows.Scan(&c.id, &h); err != nil {
				_ = rows.Close()
				return nil, err
			}
			c.hold = h == 1
			cs = append(cs, c)
		}
		_ = rows.Close()
		for _, c := range cs {
			if c.hold {
				rep.Skipped[ClassStakeholder]++
				continue
			}
			if err := s.tx(ctx, func(ctx context.Context, tx *sqlTx) error {
				for _, cl := range PurgeClasses {
					if err := s.purgeClass(ctx, tx, c.id, cl); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return rep, err
			}
			rep.Purged[ClassStakeholder]++
		}
	}
	if rule, ok := s.retention[ClassGrants]; ok && !rule.Legal {
		cutoff := now.AddDate(0, 0, -rule.Days).Format(dateLayout)
		n, err := s.execCount(ctx, `UPDATE grants SET sens_notes = NULL WHERE sens_notes IS NOT NULL AND status IN ('cancelled','forfeited','expired') AND terminal_on <= ?
			AND stakeholder_id NOT IN (SELECT id FROM stakeholders WHERE legal_hold = 1)`, cutoff)
		if err != nil {
			return rep, err
		}
		m, err := s.execCount(ctx, `UPDATE exercises SET sens_payment_ref = NULL WHERE sens_payment_ref IS NOT NULL AND status IN ('issued','rejected','cancelled') AND issued_on <= ?
			AND stakeholder_id NOT IN (SELECT id FROM stakeholders WHERE legal_hold = 1)`, cutoff)
		if err != nil {
			return rep, err
		}
		rep.Purged[ClassGrants] += n + m
	}
	if rule, ok := s.retention[ClassValuation]; ok && !rule.Legal {
		cutoff := now.AddDate(0, 0, -rule.Days).Format(dateLayout)
		n, err := s.execCount(ctx, `UPDATE valuations SET sens_fmv = NULL, sens_notes = NULL WHERE (sens_fmv IS NOT NULL OR sens_notes IS NOT NULL) AND effective_date <= ?`, cutoff)
		if err != nil {
			return rep, err
		}
		rep.Purged[ClassValuation] += n
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

// ---- key rotation -------------------------------------------------------------------------------

// sealedColumn is one sealed column: its table, the column, and how its AAD is built from the row id.
type sealedColumn struct{ table, col, aadSuffix string }

var sealedColumns = []sealedColumn{
	{"stakeholders", "sens_name", "name"}, {"stakeholders", "sens_email", "email"}, {"stakeholders", "sens_tax_id", "tax_id"},
	{"grants", "sens_notes", "notes"}, {"exercises", "sens_payment_ref", "payment_ref"},
	{"convertibles", "sens_terms", "terms"}, {"valuations", "sens_fmv", "fmv"}, {"valuations", "sens_notes", "notes"},
}

// RotateKeys re-seals every sealed value written with a retired key under the active key. It
// returns the number of values re-sealed. Run it after adding a key at the top of the keyring;
// when it returns 0 the old key can be removed. Each value is replaced with a compare-and-swap
// (the UPDATE only applies if the stored ciphertext is still the one that was read), so a
// concurrent write is never overwritten with stale data; values that changed in the meantime
// are retried on the next pass.
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
	for _, sc := range sealedColumns {
		rows, err := s.db.QueryContext(ctx, `SELECT id, `+sc.col+` FROM `+sc.table+` WHERE `+sc.col+` IS NOT NULL`) //nolint:gosec // identifiers come from a fixed list
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
			aad := it.id + ":" + sc.aadSuffix
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
			res, err := s.db.ExecContext(ctx, `UPDATE `+sc.table+` SET `+sc.col+` = ? WHERE id = ? AND `+sc.col+` = ?`, c, it.id, it.sealed) //nolint:gosec // identifiers come from a fixed list
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
	return n, missed, nil
}
