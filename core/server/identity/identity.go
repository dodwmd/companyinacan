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
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/events"
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
	// PerMinute bounds revocation calls per module and minute (default 30).
	PerMinute int

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
		max = 30
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
	username := strings.TrimPrefix(req.Subject, "user:")
	var res Result
	err = s.DB.InTx(ctx, func(tx *sql.Tx) error {
		r1, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at=? WHERE subject=? AND revoked_at=0`, s.now().Unix(), req.Subject)
		if err != nil {
			return err
		}
		n1, _ := r1.RowsAffected()
		r2, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id IN (SELECT id FROM admin_users WHERE username=?)`, username)
		if err != nil {
			return err
		}
		n2, _ := r2.RowsAffected()
		res = Result{Tokens: int(n1), Sessions: int(n2)}
		// Fail closed: a revocation that cannot be audited is rolled back and reported as failed.
		_, err = s.Audit.AppendTx(ctx, tx, audit.Entry{Actor: "module:" + caller.ID, Action: "identity.revoke", Target: req.PartyID,
			Detail: map[string]any{"tokens": res.Tokens, "sessions": res.Sessions}})
		return err
	})
	return res, err
}
