package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Data classes held by this module (modules/board/module.yaml and modules/entity/module.yaml). Class
// names follow the manifest pattern ^[a-z][a-z0-9-]*$ and double as the `class` of E08 erasure ledger
// entries.
const (
	ClassMinutes     = "board-minutes"     // minutes: statutory records, kept by law
	ClassResolutions = "board-resolutions" // resolutions, votes, consents: the record of decisions, kept by law
	ClassPacks       = "board-packs"       // board pack document lists and notes
	ClassPrivate     = "board-private"     // declared interests and their nature
	ClassAudit       = "board-audit"       // append-only audit trail (ids only), shared with the entity module
	ClassRegister    = "entity-register"   // statutory registers: kept by law
	ClassPerson      = "entity-person"     // individuals' addresses, dates of birth, agent details
	ClassFilings     = "entity-filings"    // the filings calendar (ids, dates, receipts)
	ClassEntityAudit = "entity-audit"      // the entity module's rows in the shared audit trail
)

// Classes lists every data class.
var Classes = []string{ClassMinutes, ClassResolutions, ClassPacks, ClassPrivate, ClassAudit, ClassRegister, ClassPerson, ClassFilings, ClassEntityAudit}

// BoardClasses and EntityClasses are the classes each module's manifest declares.
var (
	BoardClasses  = []string{ClassMinutes, ClassResolutions, ClassPacks, ClassPrivate, ClassAudit}
	EntityClasses = []string{ClassRegister, ClassPerson, ClassFilings, ClassEntityAudit}
)

// PurgeClasses are the classes an erasure or retention sweep can purge per person. Minutes, resolutions,
// votes, registers and the audit trail are the legal record: they refer to people by opaque id.
var PurgeClasses = []string{ClassPrivate, ClassPerson}

func legalClass(c string) bool {
	switch c {
	case ClassMinutes, ClassResolutions, ClassAudit, ClassRegister, ClassFilings, ClassEntityAudit:
		return true
	}
	return false
}

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

// DefaultRetention mirrors the manifests. They are starting points the operator must review against
// local law, not advice.
func DefaultRetention() map[string]RetentionRule {
	return map[string]RetentionRule{
		ClassMinutes: {Legal: true}, ClassResolutions: {Legal: true}, ClassPacks: {Days: 7 * 365}, ClassPrivate: {Days: 7 * 365},
		ClassAudit: {Legal: true}, ClassRegister: {Legal: true}, ClassPerson: {Days: 7 * 365}, ClassFilings: {Legal: true}, ClassEntityAudit: {Legal: true},
	}
}

// RetentionReport counts what a sweep purged, by class.
type RetentionReport struct {
	Purged  map[string]int `json:"purged"`
	Skipped map[string]int `json:"skipped_legal_hold"`
}

var personKeysToStrip = []string{"address", "date_of_birth", "email"}

// purgeClass removes one data class for one person. Idempotent; used by erasure, the retention sweep
// and the restore-time ledger replay. It never touches the legal record: the names in the registers
// stay (the law requires them), the personal details that are not required are stripped.
func (s *Service) purgeClass(ctx context.Context, q querier, person, class string) error {
	switch class {
	case ClassPrivate:
		_, err := q.ExecContext(ctx, `UPDATE conflicts SET sens_nature = NULL WHERE person_id = ?`, person)
		return err
	case ClassPerson:
		if _, err := q.ExecContext(ctx, `UPDATE entity_contacts SET sens_name = NULL, sens_address = NULL WHERE person_ref = ?`, person); err != nil {
			return err
		}
		rows, err := q.QueryContext(ctx, `SELECT id, sens_detail FROM register_entries WHERE party_ref = ?`, person)
		if err != nil {
			return err
		}
		type it struct{ id, sealed string }
		var items []it
		for rows.Next() {
			var x it
			if err := rows.Scan(&x.id, &x.sealed); err != nil {
				_ = rows.Close()
				return err
			}
			items = append(items, x)
		}
		_ = rows.Close()
		for _, x := range items {
			plain, err := s.keys.Open(x.sealed, x.id+":detail")
			if err != nil {
				return err
			}
			m := map[string]string{}
			if err := json.Unmarshal([]byte(plain), &m); err != nil {
				return err
			}
			for _, k := range personKeysToStrip {
				delete(m, k)
			}
			b, _ := json.Marshal(m)
			c, err := s.keys.Seal(string(b), x.id+":detail")
			if err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, `UPDATE register_entries SET sens_detail = ? WHERE id = ?`, c, x.id); err != nil {
				return err
			}
		}
		return nil
	case ClassMinutes, ClassResolutions, ClassAudit, ClassRegister, ClassFilings, ClassEntityAudit:
		return conflict("this class is retained by law and cannot be purged per person")
	case ClassPacks:
		return invalid("this class is not tied to a person")
	}
	return invalid("unknown data class")
}

// Retained says what an erasure kept, and why.
type Retained struct {
	Class  string `json:"class"`
	Count  int    `json:"count"`
	Reason string `json:"reason"`
}

// Erasure is the result of an erasure request: an opaque person id, the classes purged and what was
// retained under legal retention.
type Erasure struct {
	Subject  string     `json:"subject"`
	Classes  []string   `json:"classes"`
	Date     string     `json:"date"`
	Retained []Retained `json:"retained"`
}

// RequestErasure erases a person's private data: the nature of declared interests, and the addresses,
// dates of birth and contact details held for them in the entity records. Votes, attendance,
// membership terms, minutes and the statutory registers are kept, because legal retention overrides
// erasure for them (they refer to the person by opaque id, and the registers keep the name the law
// requires). A legal hold blocks it; so does a current term or office (end it first). admin.
func (s *Service) RequestErasure(ctx context.Context, a Actor, person string) (*Erasure, error) {
	if err := validPerson("person", person); err != nil {
		return nil, ErrNotFound
	}
	if err := s.authorize(ctx, a, ActErasure, Fact{}, "person:"+person); err != nil {
		return nil, err
	}
	held, err := s.exists(ctx, s.db, `SELECT 1 FROM person_holds WHERE person_id = ?`, person)
	if err != nil {
		return nil, err
	}
	if held {
		s.auditDeny(ctx, a, "erase", "person:"+person, nil)
		return nil, conflict("erasure is blocked by a legal hold")
	}
	live, err := s.exists(ctx, s.db, `SELECT 1 FROM body_members WHERE person_id = ? AND end_date IS NULL`, person)
	if err != nil {
		return nil, err
	}
	if live {
		return nil, conflict("the person has a current term on a body: end it before erasure")
	}
	var terms, votes, attend, entries int
	_ = s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM body_members WHERE person_id = ?), (SELECT COUNT(*) FROM votes WHERE voter_person = ?) + (SELECT COUNT(*) FROM resolution_voters WHERE person_id = ?),
		(SELECT COUNT(*) FROM attendance WHERE person_id = ?), (SELECT COUNT(*) FROM register_entries WHERE party_ref = ?)`, person, person, person, person, person).Scan(&terms, &votes, &attend, &entries)
	date := s.today()
	err = s.txAudit(ctx, a, "erase", "person:"+person, nil, func(ctx context.Context, tx *sqlTx) error {
		for _, c := range PurgeClasses {
			if err := s.purgeClass(ctx, tx, person, c); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO erasures (subject, class, date, created_at) VALUES (?,?,?,?)`, person, c, date, s.now().Unix()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := s.scrub(ctx, true); err != nil {
		return nil, fmt.Errorf("the person is erased and audited, but scrubbing the database files failed (repeat the request: it is idempotent and retries the scrub): %w", err)
	}
	out := &Erasure{Subject: person, Classes: PurgeClasses, Date: date, Retained: []Retained{}}
	if terms+votes+attend > 0 {
		out.Retained = append(out.Retained, Retained{ClassResolutions, terms + votes + attend, "membership terms, votes and attendance are part of the record of decisions and the minutes; they refer to the person by opaque id only"})
	}
	if entries > 0 {
		out.Retained = append(out.Retained, Retained{ClassRegister, entries, "statutory register entries are kept by law, including the name the register must show; addresses and dates of birth were removed"})
	}
	return out, nil
}

// SetLegalHold places or lifts a legal hold on a person. A hold blocks erasure and retention purges. admin.
func (s *Service) SetLegalHold(ctx context.Context, a Actor, person string, hold bool, reason string) error {
	if err := validPerson("person", person); err != nil {
		return ErrNotFound
	}
	if err := s.authorize(ctx, a, ActLegalHold, Fact{}, "person:"+person); err != nil {
		return err
	}
	if err := validText("reason", reason, 500, false); err != nil {
		return err
	}
	return s.txAudit(ctx, a, "hold", "person:"+person, nil, func(ctx context.Context, tx *sqlTx) error {
		if hold {
			_, err := tx.ExecContext(ctx, `INSERT INTO person_holds (person_id, reason, placed_by, placed_at) VALUES (?,?,?,?) ON CONFLICT (person_id) DO UPDATE SET reason = excluded.reason`, person, reason, canonSubject(a.Subject), s.now().Unix())
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM person_holds WHERE person_id = ?`, person)
		return err
	})
}

// ErasureEntry is one tombstone as can-core pulls it into the erasure ledger.
type ErasureEntry struct {
	Seq     int64  `json:"seq"`
	Subject string `json:"subject"`
	Class   string `json:"class"`
	Date    string `json:"date"`
}

// ListErasures returns tombstones after a cursor. admin (the can-core service account holds it).
func (s *Service) ListErasures(ctx context.Context, a Actor, after int64) ([]ErasureEntry, error) {
	if err := s.authorize(ctx, a, ActRetention, Fact{}, "erasures"); err != nil {
		return nil, err
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

// PurgeSubject is the `purge(subject, class)` hook: idempotent, ignores legal holds (the erasure was
// already authorised when the tombstone was written) and is safe to replay after a restore. An unknown
// subject is a no-op.
func (s *Service) PurgeSubject(ctx context.Context, subject, class string) error {
	if !personIDRe.MatchString(subject) {
		return invalid("subject must be a directory person id")
	}
	if err := s.tx(ctx, func(ctx context.Context, tx *sqlTx) error { return s.purgeClass(ctx, tx, subject, class) }); err != nil {
		return err
	}
	return s.scrub(ctx, false)
}

// RunRetention enforces per-class retention for the classes that can be purged: declared-interest text
// older than the period, board pack titles and notes of old meetings, and contact details of ended
// agent and address records. A person under legal hold is skipped. The legal record is never touched.
func (s *Service) RunRetention(ctx context.Context) (*RetentionReport, error) {
	rep := &RetentionReport{Purged: map[string]int{}, Skipped: map[string]int{}}
	now := s.now()
	if rule, ok := s.retention[ClassPrivate]; ok && !rule.Legal {
		cutoff := now.AddDate(0, 0, -rule.Days).Unix()
		n, err := s.execCount(ctx, `UPDATE conflicts SET sens_nature = NULL WHERE sens_nature IS NOT NULL AND declared_at <= ? AND person_id NOT IN (SELECT person_id FROM person_holds)`, cutoff)
		if err != nil {
			return rep, err
		}
		rep.Purged[ClassPrivate] += n
		var held int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conflicts WHERE sens_nature IS NOT NULL AND declared_at <= ? AND person_id IN (SELECT person_id FROM person_holds)`, cutoff).Scan(&held); err != nil {
			return rep, err
		}
		rep.Skipped[ClassPrivate] += held
	}
	if rule, ok := s.retention[ClassPacks]; ok && !rule.Legal {
		cutoff := now.AddDate(0, 0, -rule.Days).Format(dateLayout)
		n, err := s.execCount(ctx, `UPDATE pack_items SET sens_title = NULL, sens_note = NULL WHERE (sens_title IS NOT NULL OR sens_note IS NOT NULL) AND meeting_id IN (SELECT id FROM meetings WHERE starts_on <= ?)`, cutoff)
		if err != nil {
			return rep, err
		}
		rep.Purged[ClassPacks] += n
	}
	if rule, ok := s.retention[ClassPerson]; ok && !rule.Legal {
		cutoff := now.AddDate(0, 0, -rule.Days).Format(dateLayout)
		n, err := s.execCount(ctx, `UPDATE entity_contacts SET sens_name = NULL, sens_address = NULL WHERE (sens_name IS NOT NULL OR sens_address IS NOT NULL) AND effective_to IS NOT NULL AND effective_to <= ?
			AND person_ref NOT IN (SELECT person_id FROM person_holds)`, cutoff)
		if err != nil {
			return rep, err
		}
		rep.Purged[ClassPerson] += n
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

// sealedColumn is one sealed column: its table, an SQL expression for the row's id (the AAD is built
// from it), the column and the AAD suffix.
type sealedColumn struct{ table, idExpr, col, aadSuffix string }

var sealedColumns = []sealedColumn{
	{"pack_items", "id", "sens_title", "title"}, {"pack_items", "id", "sens_note", "note"},
	{"resolution_versions", "resolution_id || '#' || version", "sens_title", "title"}, {"resolution_versions", "resolution_id || '#' || version", "sens_text", "text"},
	{"conflicts", "id", "sens_nature", "nature"}, {"ballots", "id", "sens_choice", "choice"}, {"ballot_staging", "id", "sens_choice", "choice"}, {"resolution_withdrawals", "resolution_id", "sens_reason", "reason"},
	{"minutes", "id", "sens_content", "content"}, {"action_items", "id", "sens_text", "text"},
	{"legal_entities", "id", "sens_reg_number", "reg_number"}, {"legal_entities", "id", "sens_tax_id", "tax_id"},
	{"entity_contacts", "id", "sens_name", "name"}, {"entity_contacts", "id", "sens_address", "address"},
	{"register_entries", "id", "sens_detail", "detail"}, {"entity_documents", "id", "sens_title", "title"},
}

// RotateKeys re-seals every sealed value written with a retired key under the active key. It returns
// the number of values re-sealed. Run it after adding a key at the top of the keyring; when it returns
// 0 the old key can be removed. Each value is replaced with a compare-and-swap (the UPDATE only applies
// if the stored ciphertext is still the one that was read), so a concurrent write is never overwritten
// with stale data; values that changed in the meantime are retried on the next pass.
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
		rows, err := s.db.QueryContext(ctx, `SELECT `+sc.idExpr+`, `+sc.col+` FROM `+sc.table+` WHERE `+sc.col+` IS NOT NULL`) //nolint:gosec // identifiers come from a fixed list
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
			res, err := s.db.ExecContext(ctx, `UPDATE `+sc.table+` SET `+sc.col+` = ? WHERE `+sc.idExpr+` = ? AND `+sc.col+` = ?`, c, it.id, it.sealed) //nolint:gosec // identifiers come from a fixed list
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

var _ = strings.TrimSpace
