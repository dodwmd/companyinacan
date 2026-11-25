// Package identity is can-core's side of access removal for people who left (epic E13d,
// specs/028-identity-provision). A module that provides `identity.provision` may ask can-core to
// revoke the personal access tokens and the sessions of ONE person, and only when
//
//  1. the people module (the authenticated source of person.offboarded.v1, never a body field)
//     reported that person as offboarded, and the effective date has been reached, and
//  2. the account subject in the request is the one the directory linked to that person: the
//     event carries subject_ref, the SHA-256 prefix of the person's sso_subject, so the link is
//     checked without any personal data in the event.
//
// It is not a general "revoke anyone" call. Everything fails closed, is idempotent, is audited in
// the same transaction as the change (ids and counts only), and is rate limited per module.
package identity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Capability is what a module must provide to call Revoke.
const Capability = "identity.provision"

// PeopleSource is the only event source whose offboarding facts are recorded.
const PeopleSource = "people"

// Errors; the HTTP layer maps them to statuses.
var (
	ErrInvalid         = errors.New("invalid request")
	ErrNotEntitled     = errors.New("module does not provide identity.provision")
	ErrNotOffboarded   = errors.New("the person is not recorded as offboarded with a reached effective date")
	ErrSubjectMismatch = errors.New("the subject is not the account linked to that person")
	ErrRateLimited     = errors.New("too many revocations")
	ErrUnknown         = errors.New("no such account")
	ErrForbidden       = errors.New("only an owner may do that to an owner")
	ErrLastOwner       = errors.New("that is the last enabled owner")
)

var (
	partyRe   = regexp.MustCompile(`^pers_[0-9a-f]{24}$`)
	subjectRe = regexp.MustCompile(`^user:[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	refRe     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	dateRe    = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

// SubjectRef is the non-reversible reference to an account subject that person.offboarded.v1
// carries: the first 32 hex characters of its SHA-256.
func SubjectRef(subject string) string {
	h := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(h[:16])
}

// Service records offboarding facts and performs revocations.
type Service struct {
	DB    *store.DB
	Audit *audit.Log
	Now   func() time.Time
	Log   *slog.Logger
	// PerMinute bounds revocation calls per module and minute (default 120: a mass offboarding
	// revokes many people at once, and the caller treats a 429 as "wait", never as a failure).
	PerMinute int
	// Providers, when set, resolves a capability to the ids of the enabled modules providing it;
	// the caller must be one of them. Without it the caller's own manifest is checked.
	Providers func(ctx context.Context, capability string) ([]string, error)

	mu  sync.Mutex
	win map[string]*window
}

type window struct {
	start time.Time
	n     int
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// HandleOffboarded is the in-process subscriber of person.offboarded.v1. Events from any source
// other than the people module, and malformed ones, are ignored.
func (s *Service) HandleOffboarded(ctx context.Context, e events.Envelope) {
	if e.Source != PeopleSource {
		return
	}
	var d struct {
		PartyID       string `json:"party_id"`
		EffectiveDate string `json:"effective_date"`
		SubjectRef    string `json:"subject_ref"`
	}
	if json.Unmarshal(e.Data, &d) != nil || !partyRe.MatchString(d.PartyID) || !dateRe.MatchString(d.EffectiveDate) ||
		(d.SubjectRef != "" && !refRe.MatchString(d.SubjectRef)) {
		return
	}
	if _, err := time.Parse("2006-01-02", d.EffectiveDate); err != nil {
		return
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO offboarded_parties(party_id, subject_ref, effective_date, recorded_at) VALUES (?,?,?,?)
		ON CONFLICT(party_id) DO UPDATE SET subject_ref=excluded.subject_ref, effective_date=excluded.effective_date, recorded_at=excluded.recorded_at`,
		d.PartyID, d.SubjectRef, d.EffectiveDate, s.now().Unix())
	if err != nil && s.Log != nil {
		s.Log.Error("identity: recording an offboarding failed", "party_id", d.PartyID, "error", err)
	}
}

// Request names the person and the account to revoke.
type Request struct {
	PartyID string `json:"party_id"`
	Subject string `json:"subject"`
}

// Result counts what this call ended (zero on a repeat).
type Result struct {
	Tokens   int `json:"tokens"`
	Sessions int `json:"sessions"`
}

func (s *Service) allow(module string) bool {
	max := s.PerMinute
	if max <= 0 {
		max = 120
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.win == nil {
		s.win = map[string]*window{}
	}
	w := s.win[module]
	if w == nil || now.Sub(w.start) >= time.Minute {
		w = &window{start: now}
		s.win[module] = w
	}
	w.n++
	return w.n <= max
}

// Revoke ends the personal access tokens and sessions of req.Subject for an offboarded person.
func (s *Service) Revoke(ctx context.Context, caller store.Module, req Request) (Result, error) {
	provides := false
	for _, p := range caller.Manifest.Provides {
		provides = provides || (p.Capability == Capability && p.Status != "planned")
	}
	if !provides {
		return Result{}, ErrNotEntitled
	}
	if s.Providers != nil {
		ids, err := s.Providers(ctx, Capability)
		if err != nil {
			return Result{}, err
		}
		if !slices.Contains(ids, caller.ID) {
			return Result{}, ErrNotEntitled // not the resolved provider, or not enabled
		}
	}
	if !partyRe.MatchString(req.PartyID) || !subjectRe.MatchString(req.Subject) {
		return Result{}, ErrInvalid
	}
	if !s.allow(caller.ID) {
		return Result{}, ErrRateLimited
	}
	var ref, date string
	err := s.DB.QueryRowContext(ctx, `SELECT subject_ref, effective_date FROM offboarded_parties WHERE party_id=?`, req.PartyID).Scan(&ref, &date)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, ErrNotOffboarded
	}
	if err != nil {
		return Result{}, err
	}
	if date > s.now().UTC().Format("2006-01-02") {
		return Result{}, ErrNotOffboarded
	}
	if ref == "" || ref != SubjectRef(req.Subject) {
		return Result{}, ErrSubjectMismatch
	}
	var res Result
	err = s.DB.InTx(ctx, func(tx *sql.Tx) error {
		var err error
		if res, err = s.end(ctx, tx, req.Subject); err != nil {
			return err
		}
		// Fail closed: a revocation that cannot be audited is rolled back and reported as failed.
		_, err = s.Audit.AppendTx(ctx, tx, audit.Entry{Actor: "module:" + caller.ID, Action: "identity.revoke", Target: req.PartyID,
			Detail: map[string]any{"tokens": res.Tokens, "sessions": res.Sessions}})
		return err
	})
	return res, err
}

// end revokes the subject's tokens, deletes their sessions and push subscriptions and disables the
// administrator account, in the caller's transaction. Idempotent.
func (s *Service) end(ctx context.Context, tx *sql.Tx, subject string) (Result, error) {
	username := strings.TrimPrefix(subject, "user:")
	now := s.now().Unix()
	if _, last, err := s.ownerState(ctx, tx, subject); err != nil {
		return Result{}, err
	} else if last {
		return Result{}, ErrLastOwner // the platform must keep one enabled owner
	}
	r1, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at=? WHERE subject=? AND revoked_at=0`, now, subject)
	if err != nil {
		return Result{}, err
	}
	r2, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id IN (SELECT id FROM admin_users WHERE username=?)`, username)
	if err != nil {
		return Result{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE admin_users SET disabled_at=? WHERE username=? AND disabled_at=0`, now, username); err != nil {
		return Result{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE subject=?`, subject); err != nil {
		return Result{}, err
	}
	// Module sessions that began with a launch ticket (E11c) end with the account.
	if _, err := tx.ExecContext(ctx, `UPDATE ui_launch_sessions SET revoked_at=? WHERE subject=? AND revoked_at=0`, now, subject); err != nil {
		return Result{}, err
	}
	n1, _ := r1.RowsAffected()
	n2, _ := r2.RowsAffected()
	return Result{Tokens: int(n1), Sessions: int(n2)}, nil
}

// ownerState reports whether the subject holds the owner role and whether they are the last
// enabled owner (disabling them would leave nobody who can change owners).
func (s *Service) ownerState(ctx context.Context, tx *sql.Tx, subject string) (isOwner, last bool, err error) {
	var n int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rbac_assignments WHERE subject=? AND role=? AND scope=''`, subject, rbac.Owner).Scan(&n); err != nil || n == 0 {
		return false, false, err
	}
	var enabled int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users WHERE username=? AND disabled_at=0`, strings.TrimPrefix(subject, "user:")).Scan(&enabled); err != nil {
		return true, false, err
	}
	if enabled == 0 {
		return true, false, nil // already disabled: nothing is taken away
	}
	var others int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT a.subject) FROM rbac_assignments a JOIN admin_users u ON a.subject = 'user:' || u.username
		WHERE a.role=? AND a.scope='' AND a.subject<>? AND u.disabled_at=0`, rbac.Owner, subject).Scan(&others)
	return true, others == 0, err
}

// actorIsOwner: an unscoped owner, or the break-glass CLI actor.
func (s *Service) actorIsOwner(ctx context.Context, tx *sql.Tx, actor string) (bool, error) {
	if actor == rbac.CLIActor {
		return true, nil
	}
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rbac_assignments WHERE subject=? AND role=? AND scope=''`, "user:"+strings.TrimPrefix(actor, "user:"), rbac.Owner).Scan(&n)
	return n > 0, err
}

func (s *Service) userExists(ctx context.Context, tx *sql.Tx, subject string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users WHERE username=?`, strings.TrimPrefix(subject, "user:")).Scan(&n)
	return n > 0, err
}

// RevokeNow is the administrator's action for a termination that cannot wait for the people module
// to report the person offboarded (someone still "offboarding", flagged urgent): the same effect as
// Revoke, without the offboarding check, for a subject the administrator names. The caller must have
// checked rbac.manage. Disabling an owner needs an owner (or the break-glass CLI), the last enabled
// owner can never be disabled, and an unknown account is refused instead of audited as a no-op.
func (s *Service) RevokeNow(ctx context.Context, actor, subject string) (Result, error) {
	if !subjectRe.MatchString(subject) {
		return Result{}, ErrInvalid
	}
	var res Result
	err := s.DB.InTx(ctx, func(tx *sql.Tx) error {
		ok, err := s.userExists(ctx, tx, subject)
		if err != nil {
			return err
		}
		if !ok {
			return ErrUnknown
		}
		if isOwner, _, err := s.ownerState(ctx, tx, subject); err != nil {
			return err
		} else if isOwner {
			if ao, err := s.actorIsOwner(ctx, tx, actor); err != nil {
				return err
			} else if !ao {
				return ErrForbidden
			}
		}
		if res, err = s.end(ctx, tx, subject); err != nil {
			return err
		}
		_, err = s.Audit.AppendTx(ctx, tx, audit.Entry{Actor: actor, Action: "identity.revoke_now", Target: subject,
			Detail: map[string]any{"tokens": res.Tokens, "sessions": res.Sessions}})
		return err
	})
	return res, err
}

// Enable re-enables a disabled administrator account: owner only (or the break-glass CLI), audited.
// It does not restore tokens or sessions; the person signs in again.
func (s *Service) Enable(ctx context.Context, actor, subject string) error {
	if !subjectRe.MatchString(subject) {
		return ErrInvalid
	}
	return s.DB.InTx(ctx, func(tx *sql.Tx) error {
		ao, err := s.actorIsOwner(ctx, tx, actor)
		if err != nil {
			return err
		}
		if !ao {
			return ErrForbidden
		}
		res, err := tx.ExecContext(ctx, `UPDATE admin_users SET disabled_at=0 WHERE username=?`, strings.TrimPrefix(subject, "user:"))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrUnknown
		}
		_, err = s.Audit.AppendTx(ctx, tx, audit.Entry{Actor: actor, Action: "identity.enable", Target: subject})
		return err
	})
}

// Reconcile rebuilds the offboarding facts from the stored events of the people module, so a fact
// missed while this process was down (or lost with the table) is not lost. Call it at start.
func (s *Service) Reconcile(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, data_json FROM events WHERE type='person.offboarded.v1' AND source=? ORDER BY time`, PeopleSource)
	if err != nil {
		return err
	}
	type ev struct{ id, data string }
	var evs []ev
	for rows.Next() {
		var e ev
		if err := rows.Scan(&e.id, &e.data); err != nil {
			_ = rows.Close()
			return err
		}
		evs = append(evs, e)
	}
	_ = rows.Close()
	for _, e := range evs {
		s.HandleOffboarded(ctx, events.Envelope{ID: e.id, Type: "person.offboarded.v1", Source: PeopleSource, Data: json.RawMessage(e.data)})
	}
	return nil
}
